package access

import (
	"encoding/json"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

func gr(subject, token string) roster.Grant { return roster.Grant{Subject: subject, Token: token} }

var parent = []roster.Grant{gr("", "guardians"), gr("e_child1", "guardians"), gr("e_parent1", "subject")}
var child = []roster.Grant{gr("e_child1", "subject"), gr("e_child2", "siblings")}

func TestVisible(t *testing.T) {
	for _, c := range []struct {
		g        []roster.Grant
		subject  string
		audience []string
		want     bool
	}{
		{parent, "e_child1", []string{"guardians"}, true},
		{child, "e_child1", []string{"guardians"}, false}, // guardians-only never reaches the subject
		{child, "e_child1", []string{"subject", "guardians"}, true},
		{child, "e_child2", []string{"subject"}, false},
		{child, "e_child2", []string{"siblings"}, true},
		{parent, "", []string{"guardians"}, true},
		{child, "", []string{"guardians"}, false},
		{child, "e_other", []string{"public"}, true},
		{nil, "e_child1", []string{"subject"}, false},
		{parent, "e_child1", nil, false},
	} {
		if got := Visible(c.g, c.subject, c.audience); got != c.want {
			t.Errorf("Visible(%v, %q, %v) = %v", c.g, c.subject, c.audience, got)
		}
	}
}

func TestPairs(t *testing.T) {
	s, tk := Pairs(parent)
	if len(s) != 3 || s[1] != "e_child1" || tk[1] != "guardians" {
		t.Errorf("%v %v", s, tk)
	}
}

func TestText(t *testing.T) {
	v := json.RawMessage(`"hello"`)
	if string(Text(v, record.VerdictPass)) != `"hello"` {
		t.Error("a passing value was withheld")
	}
	for _, verdict := range []string{"quarantine", record.VerdictError, ""} {
		if string(Text(v, verdict)) != `"withheld: failed content scan"` {
			t.Errorf("verdict %q released the value", verdict)
		}
	}
	if Text(nil, "") != nil {
		t.Error("null became a marker")
	}
}

func TestOldValue(t *testing.T) {
	old := json.RawMessage(`"secret"`)
	const withheld = `"withheld: different audience"`
	if string(OldValue(child, "e_child1", nil, false, old)) != `"secret"` {
		t.Error("no audience change: the old value is the reader's")
	}
	if string(OldValue(child, "e_child1", []string{"guardians"}, false, old)) != `"secret"` {
		t.Error("prev is not consulted when the audience did not change")
	}
	if string(OldValue(child, "e_child1", []string{"guardians"}, true, old)) != withheld {
		t.Error("a value from a guardians-only version reached the subject")
	}
	if string(OldValue(parent, "e_child1", []string{"guardians"}, true, old)) != `"secret"` {
		t.Error("a reader matching both audiences lost the old value")
	}
	if string(OldValue(child, "e_child1", []string{"guardians"}, true, nil)) != withheld {
		t.Error("even a null old value is not the reader's to see")
	}
	// An empty or nil previous audience with a change matches nobody: it
	// never releases the old value.
	for _, prev := range [][]string{{}, nil} {
		for _, g := range [][]roster.Grant{parent, child, nil} {
			if string(OldValue(g, "e_child1", prev, true, old)) != withheld {
				t.Errorf("changed with prev %#v released the old value to %v", prev, g)
			}
		}
	}
}

func TestNotices(t *testing.T) {
	n := []rules.Notice{{Token: "guardians", Mode: "push"}, {Token: "subject", Mode: "prompt", Purpose: "attendance_followup"}}
	if got := Notices(parent, "e_child1", n); len(got) != 1 || got[0].Token != "guardians" {
		t.Errorf("parent: %v", got)
	}
	if got := Notices(child, "e_child1", n); len(got) != 1 || got[0].Mode != "prompt" {
		t.Errorf("child: %v", got)
	}
	if got := Notices(child, "e_child2", n); len(got) != 0 {
		t.Errorf("sibling: %v", got)
	}
}
