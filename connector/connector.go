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

type Options struct {
	Name         string
	StagingDir   string
	StateDir     string
	ConfigFile   string
	Connector    Connector
	PollInterval time.Duration
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
