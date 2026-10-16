package record

// OwnerHousehold owns record types written through /v1/notes (4.9a).
const OwnerHousehold = "household"

// FieldDef declares one field of a record type.
type FieldDef struct {
	Type     FieldType `json:"type"`
	Required bool      `json:"required,omitempty"`
}

// Type is one schema version of a record type (spec section 3, 4.7).
type Type struct {
	Name            string              `json:"name"`
	SchemaVersion   int                 `json:"schema_version"`
	Owner           string              `json:"owner"`
	Fields          map[string]FieldDef `json:"fields"`
	Renames         map[string]string   `json:"renames,omitempty"`
	DefaultAudience []string            `json:"default_audience,omitempty"`
	FixedAudience   []string            `json:"fixed_audience,omitempty"`
	AllowNoSubject  bool                `json:"allow_no_subject,omitempty"`
}
