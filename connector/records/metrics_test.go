package records

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/leftathome/glovebox/connector/records/recordstest"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// labelAllowlist is every label a spool metric may carry. Each label is
// from a fixed set: never a subject id, key, scope value or field value.
var labelAllowlist = map[string][]string{
	metricFiles:       {"connector", "dir"},
	metricBytes:       {"connector"},
	metricOldest:      {"connector"},
	metricDropped:     {"connector", "reason"},
	metricRejected:    {"connector", "reason"},
	metricAnswers:     {"action", "code", "connector", "reason"},
	metricWriteErrors: {"connector"},
	metricCut:         {"connector"},
}

// allowedValues bounds every label's values.
func allowedValue(label, v string) bool {
	switch label {
	case "connector":
		return v == "test"
	case "dir":
		return v == "pending" || v == "rejected"
	case "action":
		return regexp.MustCompile(`^(delete|erased|reject|retry_scope|retry|retry_alert)$`).MatchString(v)
	case "code":
		return regexp.MustCompile(`^(0|token|[1-5][0-9][0-9])$`).MatchString(v)
	case "reason":
		switch v {
		case "none", "other", RejectUnreadable, DropMaxAge, DropMaxBytes, DropUnknownSubjectExpired, DropErasedSubject:
			return true
		}
		return knownReasons[v]
	}
	return false
}

func labelSets(ms []metricdata.Metrics) map[string][]attribute.Set {
	out := map[string][]attribute.Set{}
	for _, m := range ms {
		switch d := m.Data.(type) {
		case metricdata.Sum[int64]:
			for _, p := range d.DataPoints {
				out[m.Name] = append(out[m.Name], p.Attributes)
			}
		case metricdata.Gauge[int64]:
			for _, p := range d.DataPoints {
				out[m.Name] = append(out[m.Name], p.Attributes)
			}
		case metricdata.Gauge[float64]:
			for _, p := range d.DataPoints {
				out[m.Name] = append(out[m.Name], p.Attributes)
			}
		default:
			out[m.Name] = append(out[m.Name], attribute.NewSet(attribute.String("unexpected_kind", "x")))
		}
	}
	return out
}

// exerciseAll drives every instrument through the real code paths, with
// tell-tale values in subjects, keys, scopes and fields.
func exerciseAll(t *testing.T) (*testMetrics, string) {
	r := newSenderRig(t)
	r.j.Unknown("e_secretsubj2", true)
	r.j.Erase("e_secretsubj3")
	mk := func(subject string, m int) {
		f := NewFields(r.m.cutOne)
		f.String("a", "secretvalue")
		f.Date("d", at(13, m))
		f.Text("x", strings.Repeat("y", 70<<10)) // cut
		delete(f.m, "x")
		s := NewSnapshot("t1", 1, Scope("src", "s", subject, Part("course", "secretcourse")), true, nil, at(13, m),
			[]journalapi.Record{{Key: "secretkey", DataSubject: subject, Fields: f.Map()}})
		if err := r.spool.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	mk("e_secretsubj1", 0) // applied
	mk("e_secretsubj1", 1) // rejected: scripted schema
	mk("e_secretsubj2", 2) // 409
	mk("e_secretsubj3", 3) // 410
	mk("e_secretsubj4", 4) // transport-ish: scripted 503 then a weird reason
	r.j.Next(recordstest.Answer{Code: 200, Status: "applied"}, recordstest.Answer{Code: 422, Reason: journalapi.ReasonSchema})
	r.sender.Drain(context.Background())
	r.j.Next(recordstest.Answer{Code: 422, Reason: "secret\nreason"})
	r.clock.Advance(time.Hour)
	r.sender.Drain(context.Background())
	// A write error, a quarantine, and a max_age drop.
	_ = r.spool.Put(journalapi.Snapshot{SnapshotID: "bad"})
	mustWrite(t, spoolDir(r.dir)+"/junk.json", "x")
	_, _ = r.spool.Pending()
	r.clock.Advance(8 * 24 * time.Hour)
	_ = r.spool.Enforce()
	return r.m, r.logs.String()
}

func TestEveryMetricCarriesOnlyAllowedLabels(t *testing.T) {
	m, _ := exerciseAll(t)
	got := labelSets(m.collect(t))
	for name, sets := range got {
		want, ok := labelAllowlist[name]
		if !ok {
			t.Errorf("metric %s is not in the label allowlist: add it, with its labels reviewed for privacy", name)
			continue
		}
		want = append([]string{}, want...)
		sort.Strings(want)
		for _, set := range sets {
			var keys []string
			for _, kv := range set.ToSlice() {
				keys = append(keys, string(kv.Key))
				if !allowedValue(string(kv.Key), kv.Value.String()) {
					t.Errorf("metric %s label %s has value %q outside its fixed set", name, kv.Key, kv.Value.String())
				}
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != strings.Join(want, ",") {
				t.Errorf("metric %s has labels %v, allowlist says %v", name, keys, want)
			}
		}
	}
	for name := range labelAllowlist {
		if _, ok := got[name]; !ok {
			t.Errorf("allowlisted metric %s was not recorded by the exercise", name)
		}
	}
}

func TestAllowlistCatchesASubjectLabel(t *testing.T) {
	if allowedValue("data_subject", "e_child1") || allowedValue("reason", "e_child1") || allowedValue("code", "abc") {
		t.Fatal("the value check is vacuous")
	}
}

func TestSpoolMetricValues(t *testing.T) {
	r := newSenderRig(t)
	r.j.Unknown("e_child2", true)
	r.put(t, "e_child1", "c1", at(13, 0))
	r.put(t, "e_child1", "c2", at(13, 1))
	r.put(t, "e_child2", "c1", at(13, 2))
	if v := r.m.value(t, metricFiles, "dir", "pending"); v != 3 {
		t.Fatalf("pending gauge %v", v)
	}
	if v := r.m.value(t, metricOldest); v != 2*3600 {
		t.Fatalf("oldest %v, want the age of the 13:00 file at 15:00", v)
	}
	r.j.Next(recordstest.Answer{Code: 200, Status: "applied"}, recordstest.Answer{Code: 422, Reason: journalapi.ReasonSchema})
	r.sender.Drain(context.Background())
	if r.m.value(t, metricFiles, "dir", "pending") != 1 || r.m.value(t, metricFiles, "dir", "rejected") != 1 {
		t.Fatal("file gauges")
	}
	es, _ := r.spool.Pending()
	rej, _ := r.spool.Rejected()
	if r.m.value(t, metricBytes) != float64(es[0].Size+rej[0].Size) {
		t.Fatal("bytes gauge")
	}
	if r.m.value(t, metricAnswers, "code", "200", "action", "delete") != 1 ||
		r.m.value(t, metricAnswers, "code", "422", "reason", "schema", "action", "reject") != 1 ||
		r.m.value(t, metricAnswers, "code", "409", "reason", "unknown_subject", "action", "retry_scope") != 1 {
		t.Fatal("answers")
	}
	r.j.Close()
	r.clock.Advance(time.Hour)
	r.sender.Drain(context.Background())
	if r.m.value(t, metricAnswers, "code", "0", "reason", "none", "action", "retry") != 1 {
		t.Fatal("transport answer")
	}
	_ = r.spool.Delete(es[0])
	if v := r.m.value(t, metricOldest); v != 0 {
		t.Fatalf("oldest of an empty spool %v", v)
	}
}

func TestNilMetricsAreSafe(t *testing.T) {
	var m *Metrics
	m.setGauges(1, 1, 1, 1)
	m.droppedN("x", 1)
	m.rejectedOne("x")
	m.answer("200", "", ActDelete)
	m.writeError()
	m.cutOne()
}
