package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultLogDir(t *testing.T) {
	if dir, err := DefaultLogDir(""); err != nil || dir != "" {
		t.Fatalf("no token file should give no default log dir, got %q, %v", dir, err)
	}

	dir, err := DefaultLogDir(filepath.Join("/some", "dir", "connection-token"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/some", "dir", "logs"); dir != want {
		t.Fatalf("DefaultLogDir = %q, want %q", dir, want)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("home directory is not available")
	}
	dir, err = DefaultLogDir("~/.config/colab-mcp-go/connection-token")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "colab-mcp-go", "logs"); dir != want {
		t.Fatalf("DefaultLogDir with ~ = %q, want %q", dir, want)
	}
}
