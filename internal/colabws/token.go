package colabws

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var errTokenFileIncomplete = errors.New("token file is incomplete")

// LoadOrCreateToken reads a persistent browser connection token or creates one
// with owner-only permissions. The file content is intentionally never logged.
func LoadOrCreateToken(name string) (string, error) {
	path, err := ResolveTokenPath(name)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("token file path must not be empty")
	}

	if token, err := readTokenFileWithRetry(path); err == nil {
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create token file directory: %w", err)
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readTokenFileWithRetry(path)
	}
	if err != nil {
		return "", fmt.Errorf("create token file: %w", err)
	}
	removeOnError := true
	defer func() {
		_ = f.Close()
		if removeOnError {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.WriteString(token + "\n"); err != nil {
		return "", fmt.Errorf("write token file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close token file: %w", err)
	}
	removeOnError = false
	return token, nil
}

// ResolveTokenPath expands a leading home-directory marker without reading or
// creating the token file.
func ResolveTokenPath(name string) (string, error) {
	return expandHome(name)
}

// ValidateTokenFile checks an existing token file without changing it. The
// token itself is deliberately not returned to callers such as doctor.
func ValidateTokenFile(name string) error {
	path, err := ResolveTokenPath(name)
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("token file path must not be empty")
	}
	_, err = readTokenFile(path)
	return err
}

func readTokenFileWithRetry(path string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		token, err := readTokenFile(path)
		if err == nil || !errors.Is(err, errTokenFileIncomplete) {
			return token, err
		}
		lastErr = err
		time.Sleep(5 * time.Millisecond)
	}
	return "", lastErr
}

func readTokenFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("token file must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file permissions must not allow group or other access")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 22 {
		return "", fmt.Errorf("%w: browser connection token must contain at least 22 URL-safe characters", errTokenFileIncomplete)
	}
	if err := validateToken(token); err != nil {
		return "", fmt.Errorf("invalid token file: %w", err)
	}
	return token, nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
