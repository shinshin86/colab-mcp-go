package tests

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const lifecycleToken = "shutdown-lifecycle-token-1234"

type lifecycleProcess struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	stdoutDone chan readAllResult
	stderr     bytes.Buffer
}

type readAllResult struct {
	data []byte
	err  error
}

func TestShutdownLifecycle(t *testing.T) {
	bin := buildLifecycleBinary(t)

	t.Run("stdin EOF cancels connection wait", func(t *testing.T) {
		proc, dir, _ := startLifecycleProcess(t, bin, 0)
		startConnectionWait(t, proc, dir)

		started := time.Now()
		if err := proc.stdin.Close(); err != nil {
			t.Fatal(err)
		}
		if err := waitForProcessExit(t, proc.cmd, 5*time.Second); err != nil {
			t.Fatalf("process exited with an error after stdin EOF: %v; stderr=%s", err, proc.stderr.String())
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("process took %s to exit after stdin EOF", elapsed)
		}
		assertLifecycleOutputHasNoToken(t, proc, dir)
	})

	t.Run("SIGTERM cancels connection wait", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("SIGTERM is not supported on Windows")
		}
		proc, dir, _ := startLifecycleProcess(t, bin, 0)
		startConnectionWait(t, proc, dir)

		started := time.Now()
		if err := proc.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := waitForProcessExit(t, proc.cmd, 5*time.Second); err != nil {
			t.Fatalf("process exited with an error after SIGTERM: %v; stderr=%s", err, proc.stderr.String())
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("process took %s to exit after SIGTERM", elapsed)
		}
		assertLifecycleOutputHasNoToken(t, proc, dir)
	})

	t.Run("fixed port can be reused immediately", func(t *testing.T) {
		port := availableTCPPort(t)
		first, firstDir, _ := startLifecycleProcess(t, bin, port)
		startConnectionWait(t, first, firstDir)
		if err := first.stdin.Close(); err != nil {
			t.Fatal(err)
		}
		if err := waitForProcessExit(t, first.cmd, 5*time.Second); err != nil {
			t.Fatalf("first process exit: %v; stderr=%s", err, first.stderr.String())
		}

		second, secondDir, _ := startLifecycleProcess(t, bin, port)
		if err := second.stdin.Close(); err != nil {
			t.Fatal(err)
		}
		if err := waitForProcessExit(t, second.cmd, 5*time.Second); err != nil {
			t.Fatalf("replacement process could not reuse port %d: %v; stderr=%s", port, err, second.stderr.String())
		}
		assertLifecycleOutputHasNoToken(t, first, firstDir)
		assertLifecycleOutputHasNoToken(t, second, secondDir)
	})

	t.Run("second process reports address conflict safely", func(t *testing.T) {
		port := availableTCPPort(t)
		first, firstDir, _ := startLifecycleProcess(t, bin, port)

		secondDir := t.TempDir()
		secondTokenFile := filepath.Join(secondDir, "connection-token")
		writeLifecycleToken(t, secondTokenFile)
		var stdout, stderr bytes.Buffer
		second := exec.Command(bin,
			"--no-browser",
			"--connect-timeout=3600s",
			"--host=127.0.0.1",
			fmt.Sprintf("--port=%d", port),
			"--token-file", secondTokenFile,
			"--log", secondDir,
		)
		second.Stdin = strings.NewReader("")
		second.Stdout = &stdout
		second.Stderr = &stderr
		started := time.Now()
		if err := second.Start(); err != nil {
			t.Fatal(err)
		}
		err := waitForProcessExit(t, second, 3*time.Second)
		if err == nil {
			t.Fatal("second process unexpectedly succeeded on an occupied port")
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("second process took %s to report the port conflict", elapsed)
		}
		if !strings.Contains(stderr.String(), "address already in use") {
			t.Fatalf("stderr did not report the port conflict: %q", stderr.String())
		}
		if strings.Contains(stdout.String(), lifecycleToken) || strings.Contains(stderr.String(), lifecycleToken) {
			t.Fatal("connection token was exposed by the failed process")
		}
		assertLogDirHasNoToken(t, secondDir)

		if err := first.stdin.Close(); err != nil {
			t.Fatal(err)
		}
		if err := waitForProcessExit(t, first.cmd, 5*time.Second); err != nil {
			t.Fatalf("first process exit: %v; stderr=%s", err, first.stderr.String())
		}
		assertLifecycleOutputHasNoToken(t, first, firstDir)
	})
}

func buildLifecycleBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "colab-mcp-go")
	build := exec.Command("go", "build", "-o", bin, "../cmd/colab-mcp-go")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return bin
}

func startLifecycleProcess(t *testing.T, bin string, port int) (*lifecycleProcess, string, string) {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "connection-token")
	writeLifecycleToken(t, tokenFile)
	proc := &lifecycleProcess{}
	proc.cmd = exec.Command(bin,
		"--no-browser",
		"--connect-timeout=3600s",
		"--host=127.0.0.1",
		fmt.Sprintf("--port=%d", port),
		"--token-file", tokenFile,
		"--log", dir,
	)
	stdin, err := proc.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := proc.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	proc.stdin = stdin
	proc.stdout = bufio.NewReader(stdout)
	proc.cmd.Stderr = &proc.stderr
	if err := proc.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if proc.cmd.ProcessState == nil {
			_ = proc.cmd.Process.Kill()
			_, _ = proc.cmd.Process.Wait()
		}
	})

	writeLine(t, proc.stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"lifecycle-test","version":"0"}}}`)
	initLine := readLine(t, proc.stdout)
	assertJSONRPCLine(t, initLine)
	if !strings.Contains(initLine, `"id":1`) {
		t.Fatalf("unexpected initialize response: %s", initLine)
	}
	writeLine(t, proc.stdin, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	proc.stdoutDone = make(chan readAllResult, 1)
	go func() {
		data, err := io.ReadAll(proc.stdout)
		proc.stdoutDone <- readAllResult{data: data, err: err}
	}()
	return proc, dir, initLine
}

func startConnectionWait(t *testing.T, proc *lifecycleProcess, logDir string) {
	t.Helper()
	writeLine(t, proc.stdin, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"open_colab_browser_connection","arguments":{}}}`)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(logDir, "colab-mcp-go.*.log"))
		for _, name := range files {
			data, err := os.ReadFile(name)
			if err == nil && strings.Contains(string(data), "opened Colab browser URL") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("connection-waiting tool handler did not start")
}

func waitForProcessExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("process did not exit within %s", timeout)
		return nil
	}
}

func availableTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func writeLifecycleToken(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(lifecycleToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertLifecycleOutputHasNoToken(t *testing.T, proc *lifecycleProcess, logDir string) {
	t.Helper()
	stdout := <-proc.stdoutDone
	if stdout.err != nil {
		t.Fatal(stdout.err)
	}
	if strings.Contains(string(stdout.data), lifecycleToken) || strings.Contains(proc.stderr.String(), lifecycleToken) {
		t.Fatal("connection token was exposed in process output")
	}
	assertLogDirHasNoToken(t, logDir)
}

func assertLogDirHasNoToken(t *testing.T, logDir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(logDir, "colab-mcp-go.*.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), lifecycleToken) {
			t.Fatalf("connection token was exposed in %s", filepath.Base(name))
		}
	}
}
