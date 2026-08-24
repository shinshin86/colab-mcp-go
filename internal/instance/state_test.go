package instance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockPreventsSecondInstanceAndCleansUpState(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "connection-token")
	if err := os.WriteFile(tokenFile, []byte("configured_token_1234567890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Truncate(time.Nanosecond)
	first, err := Acquire(tokenFile, State{PID: os.Getpid(), Port: 8765, StartedAt: startedAt})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	state, statePath, err := Read(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if state.PID != os.Getpid() || state.Port != 8765 || !state.StartedAt.Equal(startedAt) {
		t.Fatalf("state = %#v", state)
	}
	if state.WSConnected {
		t.Fatal("initial browser connection state should be false")
	}

	_, err = Acquire(tokenFile, State{PID: os.Getpid() + 1, Port: 9999})
	var running *AlreadyRunningError
	if !errors.As(err, &running) {
		t.Fatalf("second acquire error = %T %v, want AlreadyRunningError", err, err)
	}
	if running.Owner == nil || running.Owner.PID != os.Getpid() || running.Owner.StartedAt.IsZero() {
		t.Fatalf("lock owner = %#v", running.Owner)
	}

	if err := first.Update(8766, true); err != nil {
		t.Fatal(err)
	}
	updated, _, err := Read(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Port != 8766 || !updated.WSConnected || updated.LastUpdated.Before(state.LastUpdated) {
		t.Fatalf("updated state = %#v", updated)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file still exists after close: %v", err)
	}
	replacement, err := Acquire(tokenFile, State{PID: os.Getpid(), Port: 8767})
	if err != nil {
		t.Fatalf("replacement did not acquire released lock: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}
