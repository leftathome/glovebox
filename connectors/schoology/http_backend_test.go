package schoology

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	schoologylib "github.com/leftathome/schoology-go"
)

// In-cluster the framework delivers over HTTP ingest (GLOVEBOX_INGEST_URL),
// where ConnectorContext.Writer -- the deprecated filesystem writer -- is nil
// and only Backend is set. The connector used to require Writer and so exited
// at startup the first time it was deployed. This wires it exactly as the
// framework does in that mode and checks an item actually reaches the ingest
// endpoint.
func TestConnector_WiresAndStagesOverHTTPIngest(t *testing.T) {
	var (
		mu     sync.Mutex
		posts  int
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if r.Method == http.MethodPost && r.URL.Path == "/v1/ingest" {
			posts++
			bodies = append(bodies, string(b))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return []*schoologylib.Assignment{{
				ID:          1001,
				Title:       "Read Chapter 3",
				CourseTitle: "English 7",
				DueAt:       time.Date(2026, 5, 30, 23, 59, 0, 0, time.UTC),
				URL:         "/assignment/1001",
				Status:      schoologylib.AssignmentStatusOverdue,
			}}, nil, nil
		},
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
	}
	c := newTestConnector(t, client)

	metrics, err := connector.NewMetrics("schoology-http-backend-test")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })

	backend := connector.NewHTTPStagingBackend(srv.URL+"/v1/ingest", "schoology", srv.Client())
	err = c.Wire(connector.ConnectorContext{
		// Writer deliberately nil: this is the HTTP-ingest shape.
		Backend: backend,
		Matcher: connector.NewRuleMatcher([]connector.Rule{
			{Match: "schoology:k1:assignment", Destination: "main", DataSubject: "e_test01", Audience: []string{"subject", "guardians"}},
		}),
		Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("Wire with Backend only (HTTP ingest mode): %v", err)
	}

	if err := c.pollNow(context.Background(), newTestCheckpoint(t), "scheduled", 0); err != nil {
		t.Fatalf("pollNow: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("ingest endpoint received %d POSTs, want 1", posts)
	}
	for _, want := range []string{"e_test01", "guardians", "Read Chapter 3"} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("ingest payload is missing %q", want)
		}
	}
}

func TestConnector_WireRejectsNoBackend(t *testing.T) {
	c := newTestConnector(t, &fakeClient{})
	metrics, err := connector.NewMetrics("schoology-no-backend-test")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })
	err = c.Wire(connector.ConnectorContext{
		Matcher: connector.NewRuleMatcher(nil),
		Metrics: metrics,
	})
	if err == nil || !strings.Contains(err.Error(), "no staging backend") {
		t.Fatalf("Wire with neither Backend nor Writer: err = %v, want a 'no staging backend' error", err)
	}
}
