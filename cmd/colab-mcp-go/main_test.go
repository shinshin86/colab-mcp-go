package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"testing"
)

func TestDoctorJSONOutput(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "--host=127.0.0.1", "--port", fmt.Sprint(port), "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for a non-running bridge; stderr=%s", code, stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("doctor output is not JSON: %v\n%s", err, stdout.String())
	}
	if report["diagnosis_code"] != "not_running" || report["recommended_action"] == "" {
		t.Fatalf("report = %#v", report)
	}
}
