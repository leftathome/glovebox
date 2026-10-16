package connector

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type Connector interface {
	Poll(ctx context.Context, checkpoint Checkpoint) error
}

type Watcher interface {
	Watch(ctx context.Context, checkpoint Checkpoint) error
}

type Listener interface {
	Handler() http.Handler
}

// ConnectorContext is passed to connectors during setup, providing
// framework-initialized resources.
type ConnectorContext struct {
	// Deprecated: Use Backend instead. Writer will be removed once all
	// connectors migrate to the StagingBackend interface.
	Writer       *StagingWriter
	Backend      StagingBackend
	Matcher      *RuleMatcher
	Metrics      *Metrics
	FetchCounter *FetchCounter
	// Status is the framework's condition set, served on /statusz and
	// summarised in the /readyz body. Connectors report "degraded but
	// ready" states here (see Readiness).
	Status *Status
}

// SetupFunc is an optional initialization callback. If Options.Setup is set,
// it is called after the runner initializes the staging writer and rule matcher,
// allowing the connector to receive these resources.
type SetupFunc func(cc ConnectorContext) error

type Options struct {
	Name string

	// Tier declares what kind of channel this connector is: TierFeed or
	// TierPersonal. REQUIRED -- NewFramework refuses to start without a valid
	// declaration, so a connector cannot silently inherit the wrong routing.
	// See connector/tier.go for why this lives here rather than in a central
	// list downstream (openclaw-iw1s).
	Tier Tier

	StagingDir   string // required for filesystem mode; unused when GLOVEBOX_INGEST_URL is set
	StateDir     string
	ConfigFile   string
	Connector    Connector
	Setup        SetupFunc
	PollInterval time.Duration // 0 = poll once and exit
	HealthPort   int

	// Readiness selects what /readyz reports; the zero value is
	// ReadyAfterFirstPoll (the historical behaviour).
	Readiness Readiness
	// Status, when set, is the condition set the health server reports;
	// a binary that recorded conditions before connector.Run (while it
	// waited for a session) passes it here so they carry over. Nil means
	// a new, empty one.
	Status *Status
}

type permanentError struct {
	err error
}

func (e *permanentError) Error() string {
	return e.err.Error()
}

func (e *permanentError) Unwrap() error {
	return e.err
}

func PermanentError(err error) error {
	return &permanentError{err: err}
}

func IsPermanent(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}
