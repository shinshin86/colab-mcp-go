package colabws

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

func startTestServer(t *testing.T) (*Server, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := New("localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	return s, cancel
}

func dial(t *testing.T, s *Server, origin, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	header := http.Header{}
	header.Set("Origin", origin)
	header.Set("Authorization", "Bearer "+token)
	return testDialer().Dial("ws://localhost:"+itoa(s.Port()), header)
}

func TestAllowedOrigins(t *testing.T) {
	for _, origin := range []string{ColabAlternativeURL, ColabBaseURL} {
		t.Run(origin, func(t *testing.T) {
			s, _ := startTestServer(t)
			c, _, err := dial(t, s, origin, s.Token())
			if err != nil {
				t.Fatal(err)
			}
			if !s.Live() {
				t.Fatal("connection should be live")
			}
			_ = c.Close()
			waitFalse(t, s.Live)
		})
	}
}

func TestRejectedOriginsAndAuth(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		auth   string
		query  string
		status int
	}{
		{"bad origin", "https://wrong.example", "Bearer good", "", http.StatusForbidden},
		{"bad token", ColabAlternativeURL, "Bearer bad", "", http.StatusForbidden},
		{"no auth", ColabAlternativeURL, "", "", http.StatusUnauthorized},
		{"malformed auth", ColabAlternativeURL, "Bearer?token", "", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := startTestServer(t)
			header := http.Header{}
			header.Set("Origin", tt.origin)
			if tt.auth == "Bearer good" {
				tt.auth = "Bearer " + s.Token()
			}
			if tt.auth != "" {
				header.Set("Authorization", tt.auth)
			}
			_, resp, err := testDialer().Dial("ws://localhost:"+itoa(s.Port())+tt.query, header)
			if err == nil {
				t.Fatal("dial unexpectedly succeeded")
			}
			if resp == nil || resp.StatusCode != tt.status {
				t.Fatalf("status = %v, want %d", status(resp), tt.status)
			}
		})
	}
}

func TestTokenInURL(t *testing.T) {
	s, _ := startTestServer(t)
	header := http.Header{}
	header.Set("Origin", ColabAlternativeURL)
	c, _, err := testDialer().Dial("ws://localhost:"+itoa(s.Port())+"?access_token="+s.Token(), header)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !s.Live() {
		t.Fatal("connection should be live")
	}
}

func TestConfiguredPortAndToken(t *testing.T) {
	probe, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	const token = "configured_token_1234567890"
	s, err := NewWithOptions(Options{Host: "localhost", Port: port, Token: token}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Port() != port {
		t.Fatalf("port = %d, want %d", s.Port(), port)
	}
	if s.Token() != token {
		t.Fatal("server did not use the configured token")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()

	restartCtx, restartCancel := context.WithCancel(context.Background())
	defer restartCancel()
	restarted, err := NewWithOptions(Options{Host: "localhost", Port: port, Token: token}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Start(restartCtx); err != nil {
		t.Fatalf("restart on stable port failed: %v", err)
	}
	defer restarted.Close()
	if restarted.Port() != port || restarted.Token() != token {
		t.Fatal("port or token changed after restart")
	}
}

func TestHealthzReportsLocalStateWithoutToken(t *testing.T) {
	startedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Nanosecond)
	const token = "health_token_123456789012345"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := NewWithOptions(Options{
		Host:      "127.0.0.1",
		Token:     token,
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

	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(server.Port()) + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
	var health healthResponse
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Name != "colab-mcp-go" || health.Version != "test-version" || health.PID <= 0 || health.WSConnected || !health.StartedAt.Equal(startedAt) {
		t.Fatalf("health = %#v", health)
	}
	data, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("health response exposed the token")
	}

	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(server.Port())+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	postResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer postResponse.Body.Close()
	if postResponse.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST health status = %d", postResponse.StatusCode)
	}
}

func TestBrowserURL(t *testing.T) {
	s, _ := startTestServer(t)
	tests := []struct {
		name       string
		notebook   string
		wantPrefix string
		wantErr    bool
	}{
		{"scratch", "", ColabBaseURL + ScratchPath, false},
		{"existing notebook", ColabBaseURL + "/drive/example-notebook#scrollTo=cell", ColabBaseURL + "/drive/example-notebook", false},
		{"alternative host", ColabAlternativeURL + "/github/example/repo/blob/main/demo.ipynb", ColabAlternativeURL + "/github/example/repo/blob/main/demo.ipynb", false},
		{"non Colab host", "https://example.com/notebook.ipynb", "", true},
		{"non HTTPS URL", "http://colab.research.google.com/drive/example", "", true},
		{"URL with credentials", "https://@colab.research.google.com/drive/example", "", true},
		{"URL with port", "https://colab.research.google.com:443/drive/example", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.BrowserURL(tt.notebook)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("BrowserURL(%q) unexpectedly succeeded: %s", tt.notebook, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(got, tt.wantPrefix) {
				t.Fatalf("URL = %q, want prefix %q", got, tt.wantPrefix)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			fragment, err := url.ParseQuery(u.Fragment)
			if err != nil {
				t.Fatal(err)
			}
			if fragment.Get("mcpProxyToken") != s.Token() || fragment.Get("mcpProxyPort") != strconv.Itoa(s.Port()) {
				t.Fatalf("connection fragment = %q", u.Fragment)
			}
		})
	}
}

func TestSecondConnectionRejected(t *testing.T) {
	s, _ := startTestServer(t)
	c1, _, err := dial(t, s, ColabAlternativeURL, s.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, _, err := dial(t, s, ColabAlternativeURL, s.Token())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c2.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("err = %T %v, want CloseError", err, err)
	}
	if closeErr.Code != BusyCloseCode || closeErr.Text != BusyCloseReason {
		t.Fatalf("close = %d %q", closeErr.Code, closeErr.Text)
	}
}

func TestReadWriteAndMalformed(t *testing.T) {
	s, _ := startTestServer(t)
	c, _, err := dial(t, s, ColabAlternativeURL, s.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	conn := waitConn(t, s)

	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":"abc","result":{"ok":true}}`)); err != nil {
		t.Fatal(err)
	}
	msg, err := conn.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if msg == nil {
		t.Fatal("nil message")
	}

	out, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"test","params":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["method"] != "test" {
		t.Fatalf("method = %v", got["method"])
	}

	if err := c.WriteMessage(websocket.TextMessage, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(context.Background()); err == nil {
		t.Fatal("malformed message should return an error")
	}
}

func TestDisconnectClearsLive(t *testing.T) {
	s, _ := startTestServer(t)
	c, _, err := dial(t, s, ColabAlternativeURL, s.Token())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Live() {
		t.Fatal("connection should be live")
	}
	_ = c.Close()
	waitFalse(t, s.Live)
}

func TestWaitConnectionSkipsBrowserThatAlreadyDisconnected(t *testing.T) {
	s, _ := startTestServer(t)
	browser, _, err := dial(t, s, ColabAlternativeURL, s.Token())
	if err != nil {
		t.Fatal(err)
	}
	_ = browser.Close()
	waitFalse(t, s.Live)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.WaitConnection(ctx); err != context.DeadlineExceeded {
		t.Fatalf("stale connection returned instead of waiting: %v", err)
	}

	fresh, _, err := dial(t, s, ColabAlternativeURL, s.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	conn := waitConn(t, s)
	select {
	case <-conn.Done():
		t.Fatal("fresh connection was closed")
	default:
	}
}

func waitConn(t *testing.T, s *Server) *Connection {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := s.WaitConnection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitFalse(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition remained true")
}

func status(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

func testDialer() *websocket.Dialer {
	d := *websocket.DefaultDialer
	d.Subprotocols = []string{Subprotocol}
	return &d
}
