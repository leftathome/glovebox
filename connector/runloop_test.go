package connector

import (
	"context"
	"testing"
	"time"
)

func TestRunPollLoop_ExportedWrapperPollsConnector(t *testing.T) {
	port := pickPort(t)
	mock := &mockPollConnector{}
	opts := newFrameworkTestOpts(t, "fw-runpoll", port, mock)
	opts.PollInterval = 50 * time.Millisecond

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunPollLoop(ctx, fw, mock)
		close(done)
	}()

	time.Sleep(180 * time.Millisecond)
	cancel()
	<-done

	if got := mock.pollCount.Load(); got < 2 {
		t.Errorf("expected at least 2 polls via RunPollLoop, got %d", got)
	}
	if !fw.Ready.Load() {
		t.Error("expected Ready to be flipped by RunPollLoop")
	}
}

func TestRunWatchLoop_ExportedWrapperStartsWatch(t *testing.T) {
	port := pickPort(t)
	watchStarted := make(chan struct{})
	mock := &mockWatchConnector{watchStarted: watchStarted}
	opts := newFrameworkTestOpts(t, "fw-runwatch", port, mock)
	opts.PollInterval = 5 * time.Second

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunWatchLoop(ctx, fw, mock)
		close(done)
	}()

	select {
	case <-watchStarted:
		// good
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("Watch was not started by RunWatchLoop")
	}
	cancel()
	<-done
}
