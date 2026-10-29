package powerschool

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// PowerSchoolRecords is the connector's "records" block: the shared spool
// and journal settings, plus the one PowerSchool-only setting.
type PowerSchoolRecords struct {
	records.Config
	// AttendanceDaySettleSchoolDays: a day with unmarked cells becomes an
	// attendance_day record once this many school days have followed it
	// (decision S1; default 3, 1-10).
	AttendanceDaySettleSchoolDays int `json:"attendance_day_settle_school_days"`
}

// DefaultAttendanceDaySettleSchoolDays is the settling time of decision S1.
const DefaultAttendanceDaySettleSchoolDays = 3

// applyRecordsDefaults fills the records block, only when it is enabled: a
// disabled block stays as it was written.
func applyRecordsDefaults(r *PowerSchoolRecords) {
	if !r.Enabled {
		return
	}
	r.Config.ApplyDefaults()
	if r.AttendanceDaySettleSchoolDays == 0 {
		r.AttendanceDaySettleSchoolDays = DefaultAttendanceDaySettleSchoolDays
	}
}

// recordKinds are the attendance kinds an attendance_day record can count
// (decision S11). With records enabled no other kind may be configured.
var recordKinds = []string{
	"absence_excused", "absence_unexcused", "late_excused", "late_unexcused",
	"left_early_excused", "left_early_unexcused", "authorized",
}

// validateRecords checks the records block. A disabled block is never
// checked, so the connector behaves exactly as before records existed.
func validateRecords(c *Config) error {
	if !c.Records.Enabled {
		return nil
	}
	if err := c.Records.Config.Validate(); err != nil {
		return err
	}
	if n := c.Records.AttendanceDaySettleSchoolDays; n < 1 || n > 10 {
		return fmt.Errorf("records.attendance_day_settle_school_days must be between 1 and 10")
	}
	for kind := range c.AttendanceDays.Kinds {
		if !slices.Contains(recordKinds, kind) {
			return fmt.Errorf("records: attendance_days.kinds[%s] is not one of the kinds attendance_day records count %v", kind, recordKinds)
		}
	}
	return nil
}

// Binding is who a kid's records on one surface are about and who may see
// them: the data_subject and audience of the rule that routes the kid's
// items on that surface. Records and items share one routing decision.
type Binding struct {
	DataSubject string
	Audience    []string
	// OnSkip counts a row or snapshot a builder left out, by reason
	// (powerschool_record_rows_skipped_total). nil: not counted.
	OnSkip func(reason string)
}

func (b Binding) skip(reason string) {
	if b.OnSkip != nil {
		b.OnSkip(reason)
	}
}

// scope is a scope of this binding's subject on one record surface.
func (b Binding) scope(surface string, extra ...journalapi.ScopePart) []journalapi.ScopePart {
	return records.Scope(sourceName, surface, b.DataSubject, extra...)
}

// record is one record about the binding's subject, with its audience.
func (b Binding) record(key string, f *records.Fields) journalapi.Record {
	return journalapi.Record{Key: key, DataSubject: b.DataSubject, Audience: slices.Clone(b.Audience), Fields: f.Map()}
}

// maxSnapshotBytes is the journal's body limit (spec 4.3).
const maxSnapshotBytes = 8 << 20

// fits reports whether the journal would take the snapshot's size. A
// snapshot over a limit is refused whole (413, permanent) and would be
// refused again at every poll; it is left out here, logged and counted
// instead. The log line names the record type only.
func (b Binding) fits(s journalapi.Snapshot) bool {
	ok := len(s.Records) <= record.MaxRecords
	if ok {
		raw, err := json.Marshal(s)
		ok = err == nil && len(raw) <= maxSnapshotBytes
	}
	if !ok {
		b.skip("snapshot_too_large")
		slog.Warn("powerschool record snapshot over the journal's size limits; not sent", "record_type", s.RecordType)
	}
	return ok
}

// subjectFor resolves the binding of a kid's surface. Without a rule, or a
// rule without a data_subject, there are no records for the surface, as
// there are no items.
func subjectFor(m *connector.RuleMatcher, kid Kid, surface string) (Binding, bool) {
	if m == nil {
		return Binding{}, false
	}
	rule, ok := m.Match(MatchKey(kid.Name, surface))
	if !ok || rule.DataSubject == "" {
		return Binding{}, false
	}
	return Binding{DataSubject: rule.DataSubject, Audience: slices.Clone(rule.Audience)}, true
}
