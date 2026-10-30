package schoology

import "github.com/leftathome/glovebox/internal/journal/record"

// The record types this connector writes to the journal (spec 5.2). They
// are defined here, once; the chart's journal-record-types/schoology.json
// is generated from RecordTypes (decision S14).
const (
	TypeLMSAssignment       = "lms_assignment"
	TypeLMSAssignmentDetail = "lms_assignment_detail"
)

// recordSource is the scope's source and the types' owner.
const recordSource = "schoology"

// recordSchemaVersion is the schema version of both types. A later version
// may only add optional fields (Part A 4.7).
const recordSchemaVersion = 1

// RecordTypes returns the two Schoology record types. No field is
// required: a value the page did not show is null.
//
// Decision T1: lms_assignment_detail has no teacher field in version 1.
// schoology-go v0.3.0's AssignmentDetail has none (a parent's page does not
// show one); when the library exposes it, version 2 adds it as optional.
func RecordTypes() []record.Type {
	subjectAndGuardians := []string{"subject", "guardians"}
	def := func(t record.FieldType) record.FieldDef { return record.FieldDef{Type: t} }
	return []record.Type{
		{
			Name: TypeLMSAssignment, Owner: recordSource, SchemaVersion: recordSchemaVersion,
			Fields: map[string]record.FieldDef{
				"title":  def(record.String),
				"course": def(record.String),
				"status": def(record.String),
				"link":   def(record.String),
				"due":    def(record.Timestamp),
			},
			DefaultAudience: subjectAndGuardians,
		},
		{
			Name: TypeLMSAssignmentDetail, Owner: recordSource, SchemaVersion: recordSchemaVersion,
			Fields: map[string]record.FieldDef{
				"description": def(record.Text),
				"comments":    def(record.Text),
			},
			DefaultAudience: subjectAndGuardians,
		},
	}
}
