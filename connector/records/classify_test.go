package records

import (
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func TestClassifyMatchesTheJournalTable(t *testing.T) {
	cases := []struct {
		code   int
		status string
		reason string
		want   Action
	}{
		{200, "applied", "", ActDelete},
		{200, "duplicate", "", ActDelete},
		{200, "late", "", ActDelete},
		{200, "held", "", ActDelete},
		{200, "discarded", "", ActDelete},
		{410, "rejected", journalapi.ReasonErasedSubject, ActErased},
		{403, "rejected", journalapi.ReasonPolicy, ActReject},
		{413, "rejected", journalapi.ReasonTooLarge, ActReject},
		{422, "rejected", journalapi.ReasonSchema, ActReject}, // also owner != source (A2)
		{422, "rejected", journalapi.ReasonFutureObservedAt, ActReject},
		{422, "rejected", journalapi.ReasonDuplicateKey, ActReject},
		{422, "rejected", journalapi.ReasonSnapshotIDReused, ActReject},
		{409, "rejected", journalapi.ReasonUnknownSubject, ActRetryScope},
		{400, "rejected", journalapi.ReasonBadRequest, ActRetryAlert},
		{401, "rejected", journalapi.ReasonUnauthenticated, ActRetryAlert},
		{403, "rejected", journalapi.ReasonForbidden, ActRetryAlert},
		{404, "rejected", journalapi.ReasonNotFound, ActRetryAlert},
		{503, "rejected", journalapi.ReasonUnavailable, ActRetry},
		{503, "rejected", journalapi.ReasonAuthUnavailable, ActRetry},
		{503, "", "", ActRetry},
		{500, "rejected", journalapi.ReasonInternal, ActRetry},
		{500, "", "", ActRetry},
		{502, "", "", ActRetry},
		{504, "", "", ActRetry},
		{0, "", "", ActRetry}, // transport error
		// Not in the table: kept and alerted on.
		{200, "rejected", "", ActRetryAlert},
		{200, "", "", ActRetryAlert},
		{200, "something-new", "", ActRetryAlert},
		{200, "Applied", "", ActRetryAlert},
		{201, "applied", "", ActRetryAlert},
		{307, "", "", ActRetryAlert},
		{422, "rejected", "brand_new_reason", ActRetryAlert},
		{422, "rejected", "", ActRetryAlert},
		{422, "rejected", journalapi.ReasonPolicy, ActRetryAlert},
		{403, "rejected", journalapi.ReasonSchema, ActRetryAlert},
		{403, "rejected", "", ActRetryAlert},
		{413, "rejected", "", ActRetryAlert},
		{409, "rejected", journalapi.ReasonConflict, ActRetryAlert},
		{409, "rejected", "", ActRetryAlert},
		{429, "", "", ActRetryAlert},
		{410, "rejected", "", ActRetryAlert},
		{410, "rejected", journalapi.ReasonPolicy, ActRetryAlert},
		{400, "rejected", journalapi.ReasonSchema, ActRetryAlert},
		{401, "rejected", "", ActRetryAlert},
		{503, "rejected", "brand_new_reason", ActRetryAlert},
		{503, "rejected", journalapi.ReasonErasedSubject, ActRetryAlert},
	}
	for _, c := range cases {
		if got := Classify(c.code, c.status, c.reason); got != c.want {
			t.Errorf("%d %s/%s = %v, want %v", c.code, c.status, c.reason, got, c.want)
		}
	}
}

// Only the permanent reasons of the table ever reject: exhaustively, no
// code and reason outside that set leads to rejected/.
func TestOnlyPermanentReasonsReject(t *testing.T) {
	permanentRows := map[[2]any]bool{
		{403, journalapi.ReasonPolicy}: true, {413, journalapi.ReasonTooLarge}: true,
		{422, journalapi.ReasonSchema}: true, {422, journalapi.ReasonFutureObservedAt}: true,
		{422, journalapi.ReasonDuplicateKey}: true, {422, journalapi.ReasonSnapshotIDReused}: true,
	}
	reasons := []string{"", "other"}
	for r := range knownReasons {
		reasons = append(reasons, r)
	}
	for code := 0; code < 600; code++ {
		for _, r := range reasons {
			got := Classify(code, journalapi.StatusRejected, r)
			if (got == ActReject) != permanentRows[[2]any{code, r}] {
				t.Errorf("%d %q = %v", code, r, got)
			}
			if got == ActErased && !(code == 410 && r == journalapi.ReasonErasedSubject) {
				t.Errorf("%d %q purges a subject", code, r)
			}
			if got == ActDelete && code != 200 {
				t.Errorf("%d %q deletes", code, r)
			}
		}
	}
}

func TestReasonLabelIsBounded(t *testing.T) {
	if ReasonLabel("x\ny") != "other" || ReasonLabel(journalapi.ReasonPolicy) != "policy" || ReasonLabel("") != "none" {
		t.Fatal("reason label must come from a fixed set")
	}
}

func TestActionNames(t *testing.T) {
	want := map[Action]string{ActDelete: "delete", ActErased: "erased", ActReject: "reject",
		ActRetryScope: "retry_scope", ActRetry: "retry", ActRetryAlert: "retry_alert", Action(99): "unknown"}
	for a, s := range want {
		if a.String() != s {
			t.Errorf("%d = %s, want %s", a, a.String(), s)
		}
	}
}
