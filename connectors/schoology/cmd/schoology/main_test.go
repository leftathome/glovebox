package main

import (
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// TestRunOptions_ReadinessUnchanged: Schoology keeps ReadyAfterFirstPoll
// once connector.Run starts (its catch-up poll is bounded and returns nil
// on transient failures, and readiness also gates the trigger Service); it
// passes the Status its pre-run wait reported into.
func TestRunOptions_ReadinessUnchanged(t *testing.T) {
	status := connector.NewStatus()
	env := map[string]string{"GLOVEBOX_STATE_DIR": "/state", "GLOVEBOX_STAGING_DIR": "/staging"}
	o := runOptions(nil, "/etc/connector/config.json", status, func(k string) string { return env[k] })
	if o.Readiness != connector.ReadyAfterFirstPoll {
		t.Errorf("Readiness = %v, want ReadyAfterFirstPoll", o.Readiness)
	}
	if o.Status != status {
		t.Error("Status is not the one the wait reported into")
	}
	if o.Name != "schoology" || o.StateDir != "/state" || o.StagingDir != "/staging" ||
		o.PollInterval != 30*24*time.Hour || o.Setup == nil {
		t.Errorf("options changed: %+v", o)
	}
	if listenerAddr != ":8081" || healthAddr != ":8080" {
		t.Error("the stand-in must bind the framework's health port and the listener port (HealthPort+1)")
	}
}
