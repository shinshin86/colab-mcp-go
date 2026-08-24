package doctor

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shinshin86/colab-mcp-go/internal/colabws"
	"github.com/shinshin86/colab-mcp-go/internal/instance"
)

const doctorTestToken = "doctor_test_token_1234567890"

func TestDoctorMajorDiagnosticPaths(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		tokenFile := writeDoctorToken(t)
		startedAt := time.Now().UTC().Add(-2 * time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server, err := colabws.NewWithOptions(colabws.Options{
			Host:      "127.0.0.1",
			Token:     doctorTestToken,
			StartedAt: startedAt,
			Version:   "test-version",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		lock, err := instance.Acquire(tokenFile, instance.State{PID: os.Getpid(), Port: server.Port(), StartedAt: startedAt})
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()

		report := Run(context.Background(), Options{Host: "127.0.0.1", TokenFile: tokenFile})
		if report.DiagnosisCode != "healthy" || report.Status != "ok" {
			t.Fatalf("report = %#v", report)
		}
		if !report.Port.Inferred || report.Port.Health == nil || report.Port.Health.PID != os.Getpid() {
			t.Fatalf("port check = %#v", report.Port)
		}
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), doctorTestToken) {
			t.Fatal("doctor report exposed the token")
		}
	})

	t.Run("not running", func(t *testing.T) {
		port := unusedPort(t)
		report := Run(context.Background(), Options{Host: "127.0.0.1", Port: port, TokenFile: writeDoctorToken(t)})
		if report.DiagnosisCode != "not_running" || report.Port.Listening {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("port owned by another process", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.NotFoundHandler()}
		go func() { _ = server.Serve(listener) }()
		defer server.Close()
		port := listener.Addr().(*net.TCPAddr).Port

		report := Run(context.Background(), Options{Host: "127.0.0.1", Port: port, TokenFile: writeDoctorToken(t)})
		if report.DiagnosisCode != "port_in_use_by_other_process" || !report.Port.Listening || report.Port.Health != nil {
			t.Fatalf("report = %#v", report)
		}
	})

	t.Run("stale state", func(t *testing.T) {
		tokenFile := writeDoctorToken(t)
		_, statePath, _, err := instance.Paths(tokenFile)
		if err != nil {
			t.Fatal(err)
		}
		state := instance.State{
			PID:         1 << 30,
			Port:        unusedPort(t),
			StartedAt:   time.Now().UTC().Add(-time.Hour),
			LastUpdated: time.Now().UTC().Add(-time.Hour),
		}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, data, 0o600); err != nil {
			t.Fatal(err)
		}

		report := Run(context.Background(), Options{Host: "127.0.0.1", TokenFile: tokenFile})
		if report.DiagnosisCode != "stale_state" || report.State.PIDAlive {
			t.Fatalf("report = %#v", report)
		}
	})
}

func TestDoctorExtractsKnownErrorsWithoutEchoingLogLines(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "colab-mcp-go.test.log")
	contents := "server exited error=address already in use secret=" + doctorTestToken
	if err := os.WriteFile(logFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	report := Run(context.Background(), Options{Port: unusedPort(t), Log: dir})
	if report.DiagnosisCode != "recent_startup_error" || len(report.Log.KnownErrors) != 1 {
		t.Fatalf("report = %#v", report)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), doctorTestToken) {
		t.Fatal("doctor report echoed a secret-bearing log line")
	}
}

func writeDoctorToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "connection-token")
	if err := os.WriteFile(path, []byte(doctorTestToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func unusedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}
