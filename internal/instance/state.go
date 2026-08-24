package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shinshin86/colab-mcp-go/internal/colabws"
)

const (
	stateFileName = "state.json"
	lockFileName  = "state.json.lock"
)

// State is the non-secret local process state used by the doctor command.
type State struct {
	PID         int       `json:"pid"`
	Port        int       `json:"port"`
	StartedAt   time.Time `json:"started_at"`
	WSConnected bool      `json:"ws_connected"`
	LastUpdated time.Time `json:"last_updated"`
}

// Lock owns the single-instance lock and keeps state.json current.
type Lock struct {
	mu       sync.Mutex
	file     *os.File
	platform *platformLock
	path     string
	state    State
	closed   bool
}

// AlreadyRunningError reports the public, non-secret identity of the process
// that currently owns the single-instance lock.
type AlreadyRunningError struct {
	Owner *State
}

func (e *AlreadyRunningError) Error() string { return e.SafeMessage() }

func (e *AlreadyRunningError) SafeMessage() string {
	if e.Owner == nil {
		return "another colab-mcp-go instance owns the state lock"
	}
	if e.Owner.PID > 0 && !e.Owner.StartedAt.IsZero() {
		return fmt.Sprintf("another colab-mcp-go instance is already running (pid %d, started at %s)", e.Owner.PID, e.Owner.StartedAt.Format(time.RFC3339))
	}
	if e.Owner.PID > 0 {
		return fmt.Sprintf("another colab-mcp-go instance is already running (pid %d)", e.Owner.PID)
	}
	return "another colab-mcp-go instance owns the state lock"
}

// Paths returns the resolved token, state, and lock file paths. No files are
// read or created.
func Paths(tokenFile string) (tokenPath, statePath, lockPath string, err error) {
	tokenPath, err = colabws.ResolveTokenPath(tokenFile)
	if err != nil {
		return "", "", "", err
	}
	if tokenPath == "" {
		return "", "", "", fmt.Errorf("token file path must not be empty")
	}
	dir := filepath.Dir(tokenPath)
	return tokenPath, filepath.Join(dir, stateFileName), filepath.Join(dir, lockFileName), nil
}

// Read reads state.json without taking the single-instance lock.
func Read(tokenFile string) (State, string, error) {
	_, statePath, _, err := Paths(tokenFile)
	if err != nil {
		return State{}, "", err
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return State{}, statePath, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, statePath, fmt.Errorf("decode state file: %w", err)
	}
	if state.PID <= 0 || state.Port < 0 || state.Port > 65535 || state.StartedAt.IsZero() || state.LastUpdated.IsZero() {
		return State{}, statePath, fmt.Errorf("state file contains invalid process metadata")
	}
	return state, statePath, nil
}

// Acquire obtains the OS-backed single-instance lock and creates state.json.
// The separate persistent lock inode lets state updates use atomic rename and
// makes normal state-file removal race-free.
func Acquire(tokenFile string, initial State) (*Lock, error) {
	_, statePath, lockPath, err := Paths(tokenFile)
	if err != nil {
		return nil, err
	}
	if initial.PID <= 0 {
		initial.PID = os.Getpid()
	}
	if initial.StartedAt.IsZero() {
		initial.StartedAt = time.Now().UTC()
	}
	if initial.LastUpdated.IsZero() {
		initial.LastUpdated = initial.StartedAt
	}

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	platform, err := tryPlatformLock(file)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, errLockHeld) {
			owner, _, readErr := Read(tokenFile)
			if readErr != nil {
				return nil, &AlreadyRunningError{}
			}
			return nil, &AlreadyRunningError{Owner: &owner}
		}
		return nil, fmt.Errorf("acquire instance lock: %w", err)
	}

	lock := &Lock{file: file, platform: platform, path: statePath, state: initial}
	if err := lock.writeLocked(); err != nil {
		_ = unlockPlatform(file, platform)
		_ = file.Close()
		return nil, err
	}
	return lock, nil
}

// Update records the current bound port and browser connection state.
func (l *Lock) Update(port int, wsConnected bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.state.Port = port
	l.state.WSConnected = wsConnected
	l.state.LastUpdated = time.Now().UTC()
	return l.writeLocked()
}

func (l *Lock) writeLocked() error {
	data, err := json.MarshalIndent(l.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state file: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".state.json-*")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpPath := tmp.Name()
	remove := true
	defer func() {
		_ = tmp.Close()
		if remove {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set state file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state file: %w", err)
	}
	if err := os.Rename(tmpPath, l.path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	remove = false
	return nil
}

// Close removes state.json before releasing the OS lock. The persistent lock
// file is intentionally kept so later processes always lock the same inode.
func (l *Lock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	removeErr := os.Remove(l.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	unlockErr := unlockPlatform(l.file, l.platform)
	closeErr := l.file.Close()
	return errors.Join(removeErr, unlockErr, closeErr)
}
