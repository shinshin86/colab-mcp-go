package colabws

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
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
