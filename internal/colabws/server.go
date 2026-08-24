// Portions of this file are based on googlecolab/colab-mcp,
// licensed under the Apache License, Version 2.0.
// This file has been adapted for the Go implementation.

package colabws

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Server struct {
	Host   string
	Logger *slog.Logger

	token    string
	port     int
	bindPort int

	httpServer *http.Server
	listener   net.Listener
	upgrader   websocket.Upgrader

	mu        sync.RWMutex
	active    *Connection
	live      bool
	accepted  chan *Connection
	onDisc    []func()
	onChange  []func(bool)
	closeOnce sync.Once
	startedAt time.Time
	version   string
}

type Options struct {
	Host      string
	Port      int
	Token     string
	StartedAt time.Time
	Version   string
}

func New(host string, logger *slog.Logger) (*Server, error) {
	return NewWithOptions(Options{Host: host}, logger)
}

func NewWithOptions(options Options, logger *slog.Logger) (*Server, error) {
	if options.Host == "" {
		options.Host = "localhost"
	}
	if options.Port < 0 || options.Port > 65535 {
		return nil, fmt.Errorf("port must be between 0 and 65535")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if options.StartedAt.IsZero() {
		options.StartedAt = time.Now().UTC()
	}
	token := options.Token
	if token == "" {
		var err error
		token, err = newToken()
		if err != nil {
			return nil, err
		}
	} else if err := validateToken(token); err != nil {
		return nil, err
	}
	s := &Server{
		Host:      options.Host,
		Logger:    logger,
		token:     token,
		bindPort:  options.Port,
		accepted:  make(chan *Connection, 1),
		startedAt: options.StartedAt,
		version:   options.Version,
	}
	s.upgrader = websocket.Upgrader{
		Subprotocols: []string{Subprotocol},
		CheckOrigin:  func(r *http.Request) bool { return isAllowedOrigin(r.Header.Get("Origin")) },
	}
	return s, nil
}

func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.bindPort)))
	if err != nil {
		return err
	}
	s.listener = ln
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		s.port = tcp.Port
	} else {
		return fmt.Errorf("unexpected listener address %T", ln.Addr())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", s.handleWebSocket)
	s.httpServer = &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := s.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Logger.Error("websocket server stopped", "error", err)
		}
	}()
	s.Logger.Info("websocket server started", "host", s.Host, "port", s.port)
	return nil
}

func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if c := s.ActiveConnection(); c != nil {
			_ = c.Close()
		}
		if s.httpServer != nil {
			err = s.httpServer.Close()
		}
	})
	return err
}

func (s *Server) WaitConnection(ctx context.Context) (*Connection, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case c := <-s.accepted:
		return c, nil
	}
}

func (s *Server) Token() string { return s.token }

func (s *Server) Port() int { return s.port }

func (s *Server) StartedAt() time.Time { return s.startedAt }

func (s *Server) Live() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.live
}

func (s *Server) ActiveConnection() *Connection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

func (s *Server) OnDisconnect(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDisc = append(s.onDisc, fn)
}

// OnConnectionChange registers a callback for browser WebSocket connection
// and disconnection transitions. Callbacks run without holding the server
// mutex.
func (s *Server) OnConnectionChange(fn func(bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = append(s.onChange, fn)
}

func (s *Server) BrowserURL(notebookURL string) (string, error) {
	target := strings.TrimSpace(notebookURL)
	if target == "" {
		target = ColabBaseURL + ScratchPath
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !isAllowedColabHost(u.Hostname()) {
		return "", fmt.Errorf("notebook_url must be an HTTPS URL on colab.research.google.com or colab.google.com")
	}
	fragment := url.Values{}
	fragment.Set("mcpProxyToken", s.token)
	fragment.Set("mcpProxyPort", strconv.Itoa(s.port))
	u.Fragment = fragment.Encode()
	u.RawFragment = ""
	return u.String(), nil
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !isAllowedOrigin(r.Header.Get("Origin")) {
		http.Error(w, "Forbidden origin", http.StatusForbidden)
		return
	}
	if !hasMCPSubprotocol(r.Header.Values("Sec-Websocket-Protocol")) {
		http.Error(w, "Missing mcp subprotocol", http.StatusBadRequest)
		return
	}
	if status, msg := s.validateAuthorization(r); status != http.StatusOK {
		http.Error(w, msg, status)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.Logger.Warn("websocket upgrade failed", "error", err)
		return
	}

	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(BusyCloseCode, BusyCloseReason), time.Now().Add(time.Second))
		_ = ws.Close()
		return
	}
	conn := newConnection(ws, s.clearConnection)
	s.active = conn
	s.live = true
	callbacks := append([]func(bool){}, s.onChange...)
	s.mu.Unlock()
	for _, fn := range callbacks {
		fn(true)
	}

	select {
	case s.accepted <- conn:
	default:
		s.Logger.Warn("accepted connection queue full; closing connection")
		_ = conn.Close()
	}
}

func (s *Server) clearConnection() {
	var callbacks []func()
	var changeCallbacks []func(bool)
	s.mu.Lock()
	if s.active != nil {
		s.active.signalClosed()
	}
	s.active = nil
	if s.live {
		s.live = false
		callbacks = append(callbacks, s.onDisc...)
		changeCallbacks = append(changeCallbacks, s.onChange...)
	}
	s.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
	for _, fn := range changeCallbacks {
		fn(false)
	}
}

type healthResponse struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	PID         int       `json:"pid"`
	WSConnected bool      `json:"ws_connected"`
	StartedAt   time.Time `json:"started_at"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(healthResponse{
		Name:        "colab-mcp-go",
		Version:     s.version,
		PID:         os.Getpid(),
		WSConnected: s.Live(),
		StartedAt:   s.startedAt,
	})
}

func (s *Server) validateAuthorization(r *http.Request) (int, string) {
	if r.URL.Query().Get(accessTokenQueryName) == s.token {
		return http.StatusOK, ""
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return http.StatusUnauthorized, "Missing authorization"
	}
	parts := strings.Fields(auth)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		return http.StatusBadRequest, "Invalid authorization header"
	}
	if parts[1] != s.token {
		return http.StatusForbidden, "Bad authorization token"
	}
	return http.StatusOK, ""
}

func isAllowedOrigin(origin string) bool {
	return origin == ColabBaseURL || origin == ColabAlternativeURL
}

func isAllowedColabHost(host string) bool {
	return strings.EqualFold(host, "colab.research.google.com") || strings.EqualFold(host, "colab.google.com")
}

func hasMCPSubprotocol(values []string) bool {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) == Subprotocol {
				return true
			}
		}
	}
	return false
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func validateToken(token string) error {
	if len(token) < 22 {
		return fmt.Errorf("browser connection token must contain at least 22 URL-safe characters")
	}
	for _, r := range token {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return fmt.Errorf("browser connection token must contain only URL-safe letters, digits, '-' or '_'")
	}
	return nil
}
