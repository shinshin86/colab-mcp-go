package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	initLine   string
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

	t.Run("second process falls back to an ephemeral port when the fixed port is in use", func(t *testing.T) {
		port := availableTCPPort(t)
		first, firstDir, _ := startLifecycleProcess(t, bin, port)

		secondDir := t.TempDir()
		secondTokenFile := filepath.Join(secondDir, "connection-token")
		writeLifecycleToken(t, secondTokenFile)
		second := startLifecycleProcessWithOptions(t, bin, port, secondTokenFile, secondDir)
		status := callStatusTool(t, second)
		if status["instance_mode"] != "fallback" || status["configured_port"] != float64(port) {
			t.Fatalf("second process status = %#v", status)
		}
		boundPort, _ := status["port"].(float64)
		if boundPort <= 0 || int(boundPort) == port {
			t.Fatalf("second process should be bound to a different ephemeral port, got %#v", status["port"])
		}
		if reason, _ := status["fallback_reason"].(string); !strings.Contains(reason, fmt.Sprintf("port %d", port)) || !strings.Contains(reason, "address already in use") {
			t.Fatalf("fallback_reason = %q", reason)
		}
		if state := readStateJSON(t, secondDir); state["pid"] != float64(second.cmd.Process.Pid) || state["port"] != boundPort {
			t.Fatalf("second process state.json = %#v", state)
		}
		assertHealthz(t, int(boundPort))
		if !strings.Contains(readLogs(t, secondDir), "continuing on an ephemeral port") {
			t.Fatal("second process log did not record the port fallback")
		}
		finishLifecycleProcess(t, second)
		assertLifecycleOutputHasNoToken(t, second, secondDir)

		finishLifecycleProcess(t, first)
		assertLifecycleOutputHasNoToken(t, first, firstDir)
	})

	t.Run("second process reports address conflict safely with --no-fallback", func(t *testing.T) {
		port := availableTCPPort(t)
		first, firstDir, _ := startLifecycleProcess(t, bin, port)

		secondDir := t.TempDir()
		secondTokenFile := filepath.Join(secondDir, "connection-token")
		writeLifecycleToken(t, secondTokenFile)
		var stdout, stderr bytes.Buffer
		second := exec.Command(bin,
			"--no-browser",
			"--no-fallback",
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

		finishLifecycleProcess(t, first)
		assertLifecycleOutputHasNoToken(t, first, firstDir)
	})

	t.Run("second process with same token file falls back without the instance lock", func(t *testing.T) {
		port := availableTCPPort(t)
		first, firstDir, _ := startLifecycleProcess(t, bin, port)
		tokenFile := filepath.Join(firstDir, "connection-token")

		secondDir := t.TempDir()
		second := startLifecycleProcessWithOptions(t, bin, port, tokenFile, secondDir)
		status := callStatusTool(t, second)
		if status["instance_mode"] != "fallback" || status["configured_port"] != float64(port) {
			t.Fatalf("second process status = %#v", status)
		}
		boundPort, _ := status["port"].(float64)
		if boundPort <= 0 || int(boundPort) == port {
			t.Fatalf("second process should be bound to a different ephemeral port, got %#v", status["port"])
		}
		reason, _ := status["fallback_reason"].(string)
		if !strings.Contains(reason, "instance lock unavailable") || !strings.Contains(reason, fmt.Sprintf("pid %d", first.cmd.Process.Pid)) {
			t.Fatalf("fallback_reason = %q", reason)
		}
		if strings.Contains(reason, lifecycleToken) {
			t.Fatal("fallback_reason exposed the connection token")
		}
		// The lock owner keeps state.json; the fallback process never writes it.
		if state := readStateJSON(t, firstDir); state["pid"] != float64(first.cmd.Process.Pid) || state["port"] != float64(port) {
			t.Fatalf("state.json no longer describes the lock owner: %#v", state)
		}
		assertHealthz(t, int(boundPort))
		if !strings.Contains(readLogs(t, secondDir), "instance lock is held by another bridge") {
			t.Fatal("second process log did not record the lock fallback")
		}

		finishLifecycleProcess(t, second)
		assertLifecycleOutputHasNoToken(t, second, secondDir)
		if _, err := os.Stat(filepath.Join(firstDir, "state.json")); err != nil {
			t.Fatalf("state.json should survive the fallback process exit: %v", err)
		}

		finishLifecycleProcess(t, first)
		if _, err := os.Stat(filepath.Join(firstDir, "state.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("state.json was not removed after the lock owner exited: %v", err)
		}
		assertLifecycleOutputHasNoToken(t, first, firstDir)
	})

	t.Run("second process with same token file is rejected by instance lock with --no-fallback", func(t *testing.T) {
		first, firstDir, _ := startLifecycleProcess(t, bin, 0)
		tokenFile := filepath.Join(firstDir, "connection-token")

		secondDir := t.TempDir()
		var stdout, stderr bytes.Buffer
		second := exec.Command(bin,
			"--no-browser",
			"--no-fallback",
			"--host=127.0.0.1",
			"--port=0",
			"--token-file", tokenFile,
			"--log", secondDir,
		)
		second.Stdin = strings.NewReader("")
		second.Stdout = &stdout
		second.Stderr = &stderr
		if err := second.Start(); err != nil {
			t.Fatal(err)
		}
		err := waitForProcessExit(t, second, 3*time.Second)
		if err == nil {
			t.Fatal("second process unexpectedly acquired the instance lock")
		}
		if !strings.Contains(stderr.String(), "already running") || !strings.Contains(stderr.String(), fmt.Sprintf("pid %d", first.cmd.Process.Pid)) || !strings.Contains(stderr.String(), "started at") {
			t.Fatalf("stderr did not identify the lock owner safely: %q", stderr.String())
		}
		if strings.Contains(stdout.String(), lifecycleToken) || strings.Contains(stderr.String(), lifecycleToken) {
			t.Fatal("connection token was exposed by the rejected process")
		}

		finishLifecycleProcess(t, first)
		if _, err := os.Stat(filepath.Join(firstDir, "state.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("state.json was not removed after normal exit: %v", err)
		}
		assertLifecycleOutputHasNoToken(t, first, firstDir)
	})

	t.Run("logs default to a directory beside the token file", func(t *testing.T) {
		dir := t.TempDir()
		tokenFile := filepath.Join(dir, "connection-token")
		writeLifecycleToken(t, tokenFile)
		proc := &lifecycleProcess{}
		proc.cmd = exec.Command(bin,
			"--no-browser",
			"--host=127.0.0.1",
			"--port=0",
			"--token-file", tokenFile,
		)
		proc.cmd.Stdin = strings.NewReader("")
		proc.cmd.Stderr = &proc.stderr
		if err := proc.cmd.Run(); err != nil {
			t.Fatalf("process exit: %v; stderr=%s", err, proc.stderr.String())
		}
		files, err := filepath.Glob(filepath.Join(dir, "logs", "colab-mcp-go.*.log"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Fatalf("expected one log file beside the token file, got %v", files)
		}
		assertLogDirHasNoToken(t, filepath.Join(dir, "logs"))
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
	proc := startLifecycleProcessWithOptions(t, bin, port, tokenFile, dir)
	drainLifecycleStdout(proc)
	return proc, dir, proc.initLine
}

// startLifecycleProcessWithOptions starts the bridge, completes the MCP
// initialize handshake, and leaves stdout readable so tests can exchange more
// messages. Call finishLifecycleProcess (or drainLifecycleStdout) before
// assertLifecycleOutputHasNoToken.
func startLifecycleProcessWithOptions(t *testing.T, bin string, port int, tokenFile, logDir string, extraArgs ...string) *lifecycleProcess {
	t.Helper()
	proc := &lifecycleProcess{}
	args := []string{
		"--no-browser",
		"--connect-timeout=3600s",
		"--host=127.0.0.1",
		fmt.Sprintf("--port=%d", port),
		"--token-file", tokenFile,
		"--log", logDir,
	}
	args = append(args, extraArgs...)
	proc.cmd = exec.Command(bin, args...)
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
		t.Fatalf("unexpected initialize response: %s; stderr=%s", initLine, proc.stderr.String())
	}
	writeLine(t, proc.stdin, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	proc.initLine = initLine
	return proc
}

// drainLifecycleStdout starts collecting the remaining stdout so
// assertLifecycleOutputHasNoToken can inspect it after exit.
func drainLifecycleStdout(proc *lifecycleProcess) {
	if proc.stdoutDone != nil {
		return
	}
	proc.stdoutDone = make(chan readAllResult, 1)
	go func() {
		data, err := io.ReadAll(proc.stdout)
		proc.stdoutDone <- readAllResult{data: data, err: err}
	}()
}

// finishLifecycleProcess closes stdin and waits for a clean exit.
func finishLifecycleProcess(t *testing.T, proc *lifecycleProcess) {
	t.Helper()
	drainLifecycleStdout(proc)
	if err := proc.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitForProcessExit(t, proc.cmd, 5*time.Second); err != nil {
		t.Fatalf("process exit: %v; stderr=%s", err, proc.stderr.String())
	}
}

// callStatusTool calls get_colab_connection_status and returns its
// structured content.
func callStatusTool(t *testing.T, proc *lifecycleProcess) map[string]any {
	t.Helper()
	writeLine(t, proc.stdin, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_colab_connection_status","arguments":{}}}`)
	for {
		line := readLine(t, proc.stdout)
		assertJSONRPCLine(t, line)
		var msg struct {
			ID     any `json:"id"`
			Result struct {
				IsError           bool           `json:"isError"`
				StructuredContent map[string]any `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if msg.ID != float64(2) {
			continue // notifications such as tools/list_changed
		}
		if msg.Result.IsError || msg.Result.StructuredContent == nil {
			t.Fatalf("status call failed: %s", line)
		}
		return msg.Result.StructuredContent
	}
}

func readStateJSON(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func readLogs(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "colab-mcp-go.*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

func assertHealthz(t *testing.T, port int) {
	t.Helper()
	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		t.Fatalf("healthz on port %d: %v", port, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz on port %d returned %d", port, res.StatusCode)
	}
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
