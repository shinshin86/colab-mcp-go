package colabws

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLoadOrCreateTokenPersistsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "connection-token")
	first, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("token changed between reads")
	}
	if err := validateToken(first); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestLoadOrCreateTokenIsConcurrentSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connection-token")
	const workers = 16
	tokens := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := LoadOrCreateToken(path)
			if err != nil {
				errs <- err
				return
			}
			tokens <- token
		}()
	}
	wg.Wait()
	close(tokens)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var want string
	for token := range tokens {
		if want == "" {
			want = token
		}
		if token != want {
			t.Fatal("concurrent callers received different tokens")
		}
	}
}

func TestLoadOrCreateTokenRejectsInvalidToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connection-token")
	if err := os.WriteFile(path, []byte("too-short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateToken(path); err == nil {
		t.Fatal("invalid token unexpectedly accepted")
	}
}

func TestLoadOrCreateTokenRejectsLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	path := filepath.Join(t.TempDir(), "connection-token")
	if err := os.WriteFile(path, []byte("configured_token_1234567890\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateToken(path); err == nil {
		t.Fatal("loosely permissioned token file unexpectedly accepted")
	}
}
