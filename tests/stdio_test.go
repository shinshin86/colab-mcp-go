package tests

import (
	"bufio"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubprocessStdoutContainsOnlyMCPMessages(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "colab-mcp-go")
	build := exec.Command("go", "build", "-o", bin, "../cmd/colab-mcp-go")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "--no-browser", "--connect-timeout=10ms", "--log", tmp)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	reader := bufio.NewReader(stdout)
	writeLine(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"stdio-test","version":"0"}}}`)
	initLine := readLine(t, reader)
	assertJSONRPCLine(t, initLine)
	if !strings.Contains(initLine, `"id":1`) || !strings.Contains(initLine, `"serverInfo"`) {
		t.Fatalf("unexpected initialize response: %s", initLine)
	}

	writeLine(t, stdin, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	writeLine(t, stdin, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	listLine := readLine(t, reader)
	assertJSONRPCLine(t, listLine)
	if !strings.Contains(listLine, "open_colab_browser_connection") {
		t.Fatalf("tools/list response did not include injected tool: %s", listLine)
	}

	files, err := filepath.Glob(filepath.Join(tmp, "colab-mcp-go.*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("expected log file to be written")
	}
}

func writeLine(t *testing.T, stdin io.Writer, line string) {
	t.Helper()
	if _, err := stdin.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
}

func readLine(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		ch <- result{line: strings.TrimSpace(line), err: err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatal(res.err)
		}
		return res.line
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for stdout")
		return ""
	}
}

func assertJSONRPCLine(t *testing.T, line string) {
	t.Helper()
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("stdout line is not JSON-RPC JSON: %q: %v", line, err)
	}
	if msg["jsonrpc"] != "2.0" {
		t.Fatalf("stdout line is not JSON-RPC 2.0: %s", line)
	}
}
