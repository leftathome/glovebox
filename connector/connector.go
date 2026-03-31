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
	Writer       *StagingWriter
	Matcher      *RuleMatcher
	Metrics      *Metrics
	FetchCounter *FetchCounter
}

// SetupFunc is an optional initialization callback. If Options.Setup is set,
// it is called after the runner initializes the staging writer and rule matcher,
// allowing the connector to receive these resources.
type SetupFunc func(cc ConnectorContext) error

type Options struct {
	Name         string
	StagingDir   string
	StateDir     string
	ConfigFile   string
	Connector    Connector
	Setup        SetupFunc
	PollInterval time.Duration // 0 = poll once and exit
	HealthPort   int
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
