package journal

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
)

const schemaDoc = "../../docs/journal-schema.md"

// Every migration updates docs/journal-schema.md (spec 4.2). A migration
// the document does not name fails here.
func TestSchemaDocNamesEveryMigration(t *testing.T) {
	doc, err := os.ReadFile(schemaDoc)
	if err != nil {
		t.Fatal(err)
	}
	names, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if !strings.Contains(string(doc), "`"+n+"`") {
			t.Errorf("docs/journal-schema.md does not name migration %s in its history", n)
		}
	}
}

// Every column of every table is described under its table's heading.
func TestSchemaDocDescribesEveryColumn(t *testing.T) {
	doc, err := os.ReadFile(schemaDoc)
	if err != nil {
		t.Fatal(err)
	}
	db := journaltest.NewDB(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'public' ORDER BY table_name, ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		section := docSection(string(doc), "### `"+table+"`")
		if section == "" {
			t.Errorf("docs/journal-schema.md has no section for table %s", table)
			continue
		}
		if !strings.Contains(section, "| `"+column+"` |") {
			t.Errorf("docs/journal-schema.md does not describe %s.%s", table, column)
		}
	}
}

// docSection returns the text from heading to the next "### " heading.
func docSection(doc, heading string) string {
	i := strings.Index(doc, heading)
	if i < 0 {
		return ""
	}
	rest := doc[i+len(heading):]
	if j := strings.Index(rest, "\n### "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
