package powerschool

import "github.com/leftathome/glovebox/internal/journal/record"

// The record types this connector writes to the journal (spec 5.1). They
// are defined here, once; the chart's journal-record-types/powerschool.json
// is generated from RecordTypes (decision S14).
const (
	TypeCourseMark      = "course_mark"
	TypeAssignment      = "assignment"
	TypeAttendanceTotal = "attendance_total"
	TypeAttendanceMark  = "attendance_mark"
	TypeAttendanceDay   = "attendance_day"
	TypeTermGrade       = "term_grade"
	TypeTermComment     = "term_comment"
)

// recordSchemaVersion is the schema version of every type below. A later
// version may only add optional fields (Part A 4.7).
const recordSchemaVersion = 1

// RecordTypes returns the seven PowerSchool record types. No field is
// required: every field is nullable, so a value the page did not show is
// null rather than a refused snapshot.
func RecordTypes() []record.Type {
	fields := func(byType map[record.FieldType][]string) map[string]record.FieldDef {
		out := map[string]record.FieldDef{}
		for ft, names := range byType {
			for _, n := range names {
				out[n] = record.FieldDef{Type: ft}
			}
		}
		return out
	}
	subjectAndGuardians := []string{"subject", "guardians"}
	types := []record.Type{
		{Name: TypeCourseMark, Fields: fields(map[record.FieldType][]string{
			record.String: {"course", "period", "teacher", "q1_letter", "q2_letter", "s1_letter", "q3_letter", "q4_letter", "s2_letter"},
			record.Number: {"q1_percent", "q2_percent", "s1_percent", "q3_percent", "q4_percent", "s2_percent"},
		}), DefaultAudience: subjectAndGuardians},
		{Name: TypeAssignment, Fields: fields(map[record.FieldType][]string{
			record.String: {"course", "category", "title", "letter"},
			record.Date:   {"due"},
			record.Number: {"score", "max_score", "percent"},
			// absent: decision S9.
			record.Bool: {"missing", "late", "incomplete", "exempt", "collected", "absent"},
			record.Text: {"teacher_comment", "description", "category_description"},
		}), DefaultAudience: subjectAndGuardians},
		{Name: TypeAttendanceTotal, Fields: fields(map[record.FieldType][]string{
			record.String: {"course"},
			record.Int:    {"absences", "tardies"},
		}), DefaultAudience: subjectAndGuardians},
		{Name: TypeAttendanceMark, Fields: fields(map[record.FieldType][]string{
			record.Date:   {"date"},
			record.String: {"course", "period", "code", "code_description", "kind"},
			record.Bool:   {"excused"},
		}), DefaultAudience: subjectAndGuardians},
		{Name: TypeAttendanceDay, Fields: fields(map[record.FieldType][]string{
			record.Date: {"date"},
			record.Int: {"periods_met", "periods_unmarked", "absent_unexcused", "absent_excused", "late", "left_early",
				"authorized", "unknown_codes"},
			record.Bool:   {"any_unexcused"},
			record.String: {"classification"},
		}), DefaultAudience: subjectAndGuardians},
		{Name: TypeTermGrade, Fields: fields(map[record.FieldType][]string{
			record.String: {"course", "term", "grade"},
			record.Number: {"earned", "attempted"},
		}), DefaultAudience: subjectAndGuardians},
		{Name: TypeTermComment, Fields: fields(map[record.FieldType][]string{
			record.String: {"course", "term"},
			record.Text:   {"comment"},
		}), DefaultAudience: []string{"guardians"}},
	}
	for i := range types {
		types[i].Owner, types[i].SchemaVersion = sourceName, recordSchemaVersion
	}
	return types
}
