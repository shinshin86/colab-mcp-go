package app

import (
	"path/filepath"
	"time"

	"github.com/shinshin86/colab-mcp-go/internal/colabws"
)

const Version = "v0.1.0"

type Config struct {
	LogDir         string
	Host           string
	Port           int
	TokenFile      string
	ConnectTimeout time.Duration
	NoBrowser      bool
	EnableProxy    bool
	// NoFallback makes the bridge exit when the configured port or the
	// single-instance lock is unavailable instead of continuing on an
	// ephemeral port.
	NoFallback bool
}

func DefaultConfig() Config {
	return Config{
		Host:           "localhost",
		ConnectTimeout: 60 * time.Second,
		EnableProxy:    true,
	}
}

// DefaultLogDir returns the persistent log directory that is used when
// --token-file is set but --log is not: a logs directory beside the token
// file. It returns an empty string when no token file is configured.
func DefaultLogDir(tokenFile string) (string, error) {
	if tokenFile == "" {
		return "", nil
	}
	path, err := colabws.ResolveTokenPath(tokenFile)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil
	}
	return filepath.Join(filepath.Dir(path), "logs"), nil
}
