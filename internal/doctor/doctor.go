// Package doctor performs read-only local diagnostics for colab-mcp-go.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shinshin86/colab-mcp-go/internal/colabws"
	"github.com/shinshin86/colab-mcp-go/internal/instance"
)

const healthName = "colab-mcp-go"

type Options struct {
	Host      string
	Port      int
	TokenFile string
	Log       string
}

type Report struct {
	Status            string     `json:"status"`
	DiagnosisCode     string     `json:"diagnosis_code"`
	Diagnosis         string     `json:"diagnosis"`
	RecommendedAction string     `json:"recommended_action"`
	Port              PortCheck  `json:"port"`
	State             StateCheck `json:"state"`
	Token             TokenCheck `json:"token_file"`
	Log               LogCheck   `json:"log"`
}

type PortCheck struct {
	Host          string          `json:"host"`
	Number        int             `json:"number"`
	Inferred      bool            `json:"inferred_from_state"`
	Listening     bool            `json:"listening"`
	OwnerPID      int             `json:"owner_pid,omitempty"`
	OwnerProcess  string          `json:"owner_process,omitempty"`
	Health        *HealthResponse `json:"health,omitempty"`
	HealthError   string          `json:"health_error,omitempty"`
	InspectionErr string          `json:"inspection_error,omitempty"`
}

type HealthResponse struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	PID         int       `json:"pid"`
	WSConnected bool      `json:"ws_connected"`
	StartedAt   time.Time `json:"started_at"`
}

type StateCheck struct {
	Configured bool            `json:"configured"`
	Path       string          `json:"path,omitempty"`
	Exists     bool            `json:"exists"`
	Valid      bool            `json:"valid"`
	PIDAlive   bool            `json:"pid_alive"`
	Data       *instance.State `json:"data,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type TokenCheck struct {
	Configured  bool   `json:"configured"`
	Path        string `json:"path,omitempty"`
	Exists      bool   `json:"exists"`
	Valid       bool   `json:"valid"`
	Permissions string `json:"permissions,omitempty"`
	Error       string `json:"error,omitempty"`
}

type LogCheck struct {
	Configured  bool     `json:"configured"`
	File        string   `json:"file,omitempty"`
	KnownErrors []string `json:"known_errors"`
	Error       string   `json:"error,omitempty"`
}

func Run(ctx context.Context, options Options) Report {
	if options.Host == "" {
		options.Host = "localhost"
	}
	report := Report{
		Port:  PortCheck{Host: options.Host, Number: options.Port},
		State: inspectState(options.TokenFile),
		Token: inspectToken(options.TokenFile),
		Log:   inspectLog(options.Log),
	}
	if report.Port.Number == 0 && report.State.Valid && report.State.Data.Port > 0 {
		report.Port.Number = report.State.Data.Port
		report.Port.Inferred = true
	}
	inspectPort(ctx, &report.Port)
	diagnose(&report)
	return report
}

func inspectState(tokenFile string) StateCheck {
	check := StateCheck{Configured: tokenFile != ""}
	if tokenFile == "" {
		return check
	}
	_, statePath, _, pathErr := instance.Paths(tokenFile)
	if pathErr != nil {
		check.Error = pathErr.Error()
		return check
	}
	check.Path = statePath
	state, _, err := instance.Read(tokenFile)
	if errors.Is(err, os.ErrNotExist) {
		return check
	}
	check.Exists = true
	if err != nil {
		check.Error = err.Error()
		return check
	}
	check.Valid = true
	check.PIDAlive = processAlive(state.PID)
	check.Data = &state
	return check
}

func inspectToken(tokenFile string) TokenCheck {
	check := TokenCheck{Configured: tokenFile != ""}
	if tokenFile == "" {
		return check
	}
	path, err := colabws.ResolveTokenPath(tokenFile)
	if err != nil {
		check.Error = err.Error()
		return check
	}
	check.Path = path
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return check
	}
	if err != nil {
		check.Error = err.Error()
		return check
	}
	check.Exists = true
	check.Permissions = info.Mode().Perm().String()
	if err := colabws.ValidateTokenFile(path); err != nil {
		check.Error = err.Error()
		return check
	}
	check.Valid = true
	return check
}

func inspectPort(ctx context.Context, check *PortCheck) {
	if check.Number <= 0 || check.Number > 65535 {
		if check.Number != 0 {
			check.InspectionErr = "port must be between 1 and 65535"
		}
		return
	}
	host := probeHost(check.Host)
	address := net.JoinHostPort(host, strconv.Itoa(check.Number))
	dialer := &net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return
	}
	check.Listening = true
	_ = conn.Close()

	health, healthErr := fetchHealth(ctx, dialer, host, check.Number)
	if healthErr == nil && health.Name == healthName {
		check.Health = &health
		check.OwnerPID = health.PID
		check.OwnerProcess = healthName
	} else if healthErr != nil {
		check.HealthError = healthErr.Error()
	} else {
		check.HealthError = fmt.Sprintf("health endpoint identified %q, not %q", health.Name, healthName)
	}
	if check.OwnerPID == 0 {
		check.OwnerPID, check.OwnerProcess = inspectOwner(ctx, check.Number)
	}
}

func fetchHealth(ctx context.Context, dialer *net.Dialer, host string, port int) (HealthResponse, error) {
	transport := &http.Transport{
		Proxy:       nil,
		DialContext: dialer.DialContext,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   750 * time.Millisecond,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("health endpoint redirected")
		},
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/healthz"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return HealthResponse{}, err
	}
	response, err := client.Do(req)
	if err != nil {
		return HealthResponse{}, fmt.Errorf("health check failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return HealthResponse{}, fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
	}
	var health HealthResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health); err != nil {
		return HealthResponse{}, fmt.Errorf("health response was not valid JSON: %w", err)
	}
	return health, nil
}

func inspectOwner(ctx context.Context, port int) (int, string) {
	path, err := exec.LookPath("lsof")
	if err != nil {
		return 0, ""
	}
	commandCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := exec.CommandContext(commandCtx, path, "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-Fpc").Output()
	if err != nil {
		return 0, ""
	}
	var pid int
	var process string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			if pid == 0 {
				pid, _ = strconv.Atoi(line[1:])
			}
		case 'c':
			if process == "" {
				process = line[1:]
			}
		}
	}
	return pid, process
}

func inspectLog(name string) LogCheck {
	check := LogCheck{Configured: name != "", KnownErrors: []string{}}
	if name == "" {
		return check
	}
	path, err := resolveLogFile(name)
	if err != nil {
		check.Error = err.Error()
		return check
	}
	check.File = path
	f, err := os.Open(path)
	if err != nil {
		check.Error = err.Error()
		return check
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		check.Error = err.Error()
		return check
	}
	const tailSize = int64(64 << 10)
	start := info.Size() - tailSize
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		check.Error = err.Error()
		return check
	}
	data, err := io.ReadAll(io.LimitReader(f, tailSize))
	if err != nil {
		check.Error = err.Error()
		return check
	}
	lower := strings.ToLower(string(data))
	patterns := []string{
		"address already in use",
		"another colab-mcp-go instance is already running",
		"permission denied",
		"invalid token file",
		"browser connection token",
		"remote mcp client connect failed",
		"websocket server stopped",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			check.KnownErrors = append(check.KnownErrors, pattern)
		}
	}
	return check
}

func resolveLogFile(name string) (string, error) {
	info, err := os.Stat(name)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return name, nil
	}
	entries, err := filepath.Glob(filepath.Join(name, "colab-mcp-go.*.log"))
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no colab-mcp-go log files found")
	}
	sort.Slice(entries, func(i, j int) bool {
		left, leftErr := os.Stat(entries[i])
		right, rightErr := os.Stat(entries[j])
		if leftErr != nil || rightErr != nil {
			return entries[i] > entries[j]
		}
		return left.ModTime().After(right.ModTime())
	})
	return entries[0], nil
}

func diagnose(report *Report) {
	set := func(status, code, diagnosis, action string) {
		report.Status = status
		report.DiagnosisCode = code
		report.Diagnosis = diagnosis
		report.RecommendedAction = action
	}

	state := report.State
	port := report.Port
	if port.Listening && port.Health == nil {
		owner := "another process"
		if port.OwnerPID > 0 && port.OwnerProcess != "" {
			owner = fmt.Sprintf("%s (pid %d)", port.OwnerProcess, port.OwnerPID)
		} else if port.OwnerPID > 0 {
			owner = fmt.Sprintf("pid %d", port.OwnerPID)
		}
		set("error", "port_in_use_by_other_process", fmt.Sprintf("Port %d is listening, but its owner is %s and does not identify as colab-mcp-go.", port.Number, owner), "Stop the identified process manually if appropriate, or configure colab-mcp-go to use a different port.")
		return
	}
	if state.Exists && (!state.Valid || !state.PIDAlive) {
		set("error", "stale_state", "state.json is invalid or its recorded process is no longer running.", "Confirm that no colab-mcp-go process is using the configured port, then remove state.json manually before restarting.")
		return
	}
	if port.Health != nil && state.Configured {
		if !state.Exists {
			set("warning", "missing_state", "colab-mcp-go is responding, but state.json is missing.", "Restart the bridge normally so it can recreate state.json while holding the instance lock.")
			return
		}
		if state.Data.PID != port.Health.PID || state.Data.Port != port.Number {
			set("error", "state_mismatch", "The live colab-mcp-go process does not match the PID or port recorded in state.json.", "Stop the intended bridge process manually, verify the configured token file and port, then start one instance.")
			return
		}
	}
	if state.Valid && state.PIDAlive && !port.Listening {
		set("error", "state_without_listener", "state.json records a live process, but the expected port is not listening.", "Inspect the process and log, then stop or restart the bridge manually if it is stuck.")
		return
	}
	if report.Token.Exists && !report.Token.Valid {
		set("error", "invalid_token_file", "The configured token file is invalid or has unsafe permissions.", "Replace the token with at least 22 URL-safe characters and restrict the file to owner-only access before restarting.")
		return
	}
	if port.Health != nil {
		if report.Token.Configured && !report.Token.Exists {
			set("warning", "missing_token_file", "colab-mcp-go is responding, but the configured token file is missing.", "Verify that doctor and the running bridge use the same --token-file value, then restart the intended bridge if necessary.")
			return
		}
		set("ok", "healthy", "colab-mcp-go is listening and its local state is consistent.", "No action is required.")
		return
	}
	if port.Number == 0 {
		set("warning", "port_unknown", "No port was supplied and no valid state.json was available to infer one.", "Run doctor with the same --port and --token-file values used to start colab-mcp-go.")
		return
	}
	if len(report.Log.KnownErrors) > 0 {
		set("error", "recent_startup_error", fmt.Sprintf("No process is listening and the log contains a known error: %s.", report.Log.KnownErrors[len(report.Log.KnownErrors)-1]), "Correct the reported configuration or port conflict, then start the bridge manually and run doctor again.")
		return
	}
	set("warning", "not_running", fmt.Sprintf("No process is listening on port %d and no live instance state was found.", port.Number), "Start colab-mcp-go with the intended port and token file, then run doctor again.")
}

func probeHost(host string) string {
	switch host {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::", "[::]":
		return "::1"
	default:
		return strings.Trim(host, "[]")
	}
}
