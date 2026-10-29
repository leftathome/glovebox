package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func decodeRefusal(t *testing.T, rec *httptest.ResponseRecorder) journalapi.Response {
	t.Helper()
	var resp journalapi.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	return resp
}

// The read-error mapping of the Task 19 review, used by Task 26's handlers.
func TestReadErrMapping(t *testing.T) {
	r := newAuthRig(t)
	for name, c := range map[string]struct {
		err    error
		status int
		reason string
	}{
		"unknown reader": {fmt.Errorf("state: %w", journal.ErrUnknownReader), 403, journalapi.ReasonPolicy},
		"no roster":      {journal.ErrNoRoster, 503, journalapi.ReasonUnavailable},
		"bad query":      {fmt.Errorf("x: %w", journal.ErrBadQuery), 400, journalapi.ReasonBadRequest},
		"anything else":  {errors.New("pq: secret detail"), 500, journalapi.ReasonInternal},
	} {
		rec := httptest.NewRecorder()
		r.s.readErr(rec, "test", c.err)
		resp := decodeRefusal(t, rec)
		if rec.Code != c.status || resp.Reason != c.reason || resp.Status != journalapi.StatusRejected {
			t.Errorf("%s: %d %+v", name, rec.Code, resp)
		}
		if c.status == 500 && resp.Detail != "" {
			t.Errorf("%s: the 500 carries a detail: %q", name, resp.Detail)
		}
	}
}

func TestReaderOK(t *testing.T) {
	r := newAuthRig(t)
	rec := httptest.NewRecorder()
	if _, ok := r.s.readerOK(rec, "", true, "x"); ok || rec.Code != 400 || decodeRefusal(t, rec).Reason != journalapi.ReasonBadRequest {
		t.Errorf("no reader: %v %d", ok, rec.Code)
	}
	rec = httptest.NewRecorder()
	if _, ok := r.s.readerOK(rec, "e_parent1", false, "the detail"); ok || rec.Code != 403 || decodeRefusal(t, rec).Reason != journalapi.ReasonPolicy || decodeRefusal(t, rec).Detail != "the detail" {
		t.Errorf("not allowed: %v %d", ok, rec.Code)
	}
	rec = httptest.NewRecorder()
	if got, ok := r.s.readerOK(rec, "e_parent1", true, "x"); !ok || got != "e_parent1" || rec.Body.Len() != 0 {
		t.Errorf("allowed: %q %v %s", got, ok, rec.Body.String())
	}
}

func TestQueryParams(t *testing.T) {
	for q, ok := range map[string]bool{
		"":                           true,
		"reader=e_parent1":           true,
		"reader=e_parent1&limit=5":   true,
		"reader=e_parent1&grants=x":  false,
		"reader=a&reader=b":          false,
		"audience=guardians":         false,
		"reader=%zz":                 false,
		"reader=e_parent1;limit=5":   false,
		"Reader=e_parent1":           false,
		"reader=e_parent1&limit=5&x": false,
	} {
		req := httptest.NewRequest("GET", journalapi.PathState+"?"+q, nil)
		_, err := queryParams(req, "reader", "limit")
		if (err == nil) != ok {
			t.Errorf("%q: %v", q, err)
		}
	}
}

func TestIsJSON(t *testing.T) {
	for ct, ok := range map[string]bool{
		"application/json": true, "application/json; charset=utf-8": true, "Application/JSON": true,
		"": false, "text/json": false, "application/json-seq": false, "application/json;;": false,
	} {
		req := httptest.NewRequest("POST", journalapi.PathRecords, nil)
		req.Header.Set("Content-Type", ct)
		if isJSON(req) != ok {
			t.Errorf("%q", ct)
		}
	}
}
