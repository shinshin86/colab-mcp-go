// Portions of this file are based on googlecolab/colab-mcp,
// licensed under the Apache License, Version 2.0.
// This file has been adapted for the Go implementation.

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shinshin86/colab-mcp-go/internal/browser"
	"github.com/shinshin86/colab-mcp-go/internal/colabws"
	"github.com/shinshin86/colab-mcp-go/internal/mcptransport"
)

const (
	InjectedToolName = "open_colab_browser_connection"
	ListToolsName    = "list_colab_tools"
	CallToolName     = "call_colab_tool"
	StatusToolName   = "get_colab_connection_status"
	// A browser WebSocket that never completes MCP initialization must not
	// prevent this bridge from accepting future reconnects indefinitely.
	remoteHandshakeTimeout = 30 * time.Second
)

var emptyObjectSchema = map[string]any{"type": "object"}

const (
	// InstanceModePrimary means the bridge is bound to its configured port
	// and, when a token file is configured, owns the shared state lock.
	InstanceModePrimary = "primary"
	// InstanceModeFallback means the configured port or the state lock was
	// unavailable at startup and the bridge continued on an ephemeral port.
	InstanceModeFallback = "fallback"
)

// InstanceInfo is non-secret startup information reported by
// get_colab_connection_status.
type InstanceInfo struct {
	Mode           string
	ConfiguredPort int
	FallbackReason string
}

type Manager struct {
	appCtx           context.Context
	server           *mcp.Server
	ws               *colabws.Server
	opener           browser.Opener
	timeout          time.Duration
	handshakeTimeout time.Duration
	logger           *slog.Logger
	info             InstanceInfo

	mu              sync.RWMutex
	remoteSession   *mcp.ClientSession
	remoteToolNames map[string]struct{}
	connectedCh     chan struct{}
	progress        map[string]progressTarget
	refreshing      atomic.Bool
	progressSeq     atomic.Uint64
	disconnecting   atomic.Bool
}

type progressTarget struct {
	session *mcp.ServerSession
	token   any
}

func NewManager(appCtx context.Context, server *mcp.Server, ws *colabws.Server, opener browser.Opener, timeout time.Duration, logger *slog.Logger) *Manager {
	if appCtx == nil {
		appCtx = context.Background()
	}
	if opener == nil {
		opener = browser.OSOpener{}
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		appCtx:           appCtx,
		server:           server,
		ws:               ws,
		opener:           opener,
		timeout:          timeout,
		handshakeTimeout: remoteHandshakeTimeout,
		logger:           logger,
		remoteToolNames:  map[string]struct{}{},
		connectedCh:      make(chan struct{}),
		progress:         map[string]progressTarget{},
	}
	ws.OnDisconnect(m.handleDisconnect)
	return m
}

// SetInstanceInfo records how this bridge instance started. Call it before
// RegisterInjectedTools.
func (m *Manager) SetInstanceInfo(info InstanceInfo) {
	if info.Mode == "" {
		info.Mode = InstanceModePrimary
	}
	m.info = info
}

func (m *Manager) RegisterInjectedTools() {
	m.server.AddTool(&mcp.Tool{
		Name:        InjectedToolName,
		Description: "Opens a connection to a Google Colab browser session and unlocks notebook editing tools. Pass notebook_url to reconnect an existing notebook. Returns a boolean representing whether the connection attempt succeeded",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"notebook_url": map[string]any{
					"type":        "string",
					"format":      "uri",
					"description": "Optional existing Google Colab notebook URL to open instead of a blank notebook",
				},
			},
		},
	}, m.openColabBrowserConnection)

	m.server.AddTool(&mcp.Tool{
		Name:        ListToolsName,
		Description: "Lists tools exposed by the connected Google Colab notebook. Use this for MCP clients that do not refresh tools after notifications/tools/list_changed.",
		InputSchema: emptyObjectSchema,
	}, m.listColabTools)

	m.server.AddTool(&mcp.Tool{
		Name:        CallToolName,
		Description: "Calls a tool exposed by the connected Google Colab notebook by name. Use list_colab_tools first to discover available tool names and argument schemas when the MCP client cannot refresh dynamic tools.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"name"},
			"properties": map[string]any{
				"name":      map[string]any{"type": "string"},
				"arguments": map[string]any{"type": "object"},
			},
		},
	}, m.callColabTool)

	m.server.AddTool(&mcp.Tool{
		Name:        StatusToolName,
		Description: "Reports local Colab bridge connection state without calling the remote notebook. instance_mode is 'primary' when the bridge owns its configured port and state lock, or 'fallback' when another bridge held them at startup and this process continued on an ephemeral port (fallback_reason explains why; the bridge still works normally).",
		InputSchema: emptyObjectSchema,
		OutputSchema: map[string]any{
			"type": "object",
			"required": []string{
				"pid", "port", "browser_connected", "remote_session_active",
				"remote_tool_count", "uptime_seconds", "version",
				"instance_mode", "configured_port", "fallback_reason",
			},
			"properties": map[string]any{
				"pid":                   map[string]any{"type": "integer"},
				"port":                  map[string]any{"type": "integer"},
				"browser_connected":     map[string]any{"type": "boolean"},
				"remote_session_active": map[string]any{"type": "boolean"},
				"remote_tool_count":     map[string]any{"type": "integer"},
				"uptime_seconds":        map[string]any{"type": "integer"},
				"version":               map[string]any{"type": "string"},
				"instance_mode":         map[string]any{"type": "string", "enum": []string{InstanceModePrimary, InstanceModeFallback}},
				"configured_port":       map[string]any{"type": "integer"},
				"fallback_reason":       map[string]any{"type": "string"},
			},
		},
	}, m.getColabConnectionStatus)

	m.server.AddTool(&mcp.Tool{
		Name:        DisconnectToolName,
		Description: "Disconnects and deletes the currently assigned Colab runtime (same as 'Runtime > Disconnect and delete runtime') by executing google.colab.runtime.unassign() in the kernel. Destructive: in-memory state and files outside mounted Drive are lost. The result reports outcome: 'completed', or 'unknown' (the kernel terminated while executing the cell - the expected success shape - or a genuine failure; check the detail). Failures before the cell is issued are reported as tool errors. Verify in the Colab UI. Do not run additional cells to verify, and do not retry after a likely success - either can assign a fresh runtime.",
		InputSchema: emptyObjectSchema,
	}, m.disconnectColabRuntime)
}

func (m *Manager) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "colab-mcp-go-proxy", Version: Version}, &mcp.ClientOptions{
			ToolListChangedHandler:      m.remoteToolListChanged,
			ProgressNotificationHandler: m.remoteProgress,
			Logger:                      m.logger,
		})
		session, release, err := m.connectRemote(ctx, client)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.logger.Warn("remote MCP client connect failed", "error", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		m.setRemoteSession(session)
		if err := m.RefreshTools(ctx); err != nil {
			m.logger.Warn("initial remote tools refresh failed", "error", err)
		}
		waitDone := make(chan error, 1)
		go func() { waitDone <- session.Wait() }()
		select {
		case err = <-waitDone:
		case <-ctx.Done():
			_ = session.Close()
			err = <-waitDone
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Info("remote MCP session ended", "error", err)
		}
		m.handleDisconnect()
		release()
		if ctx.Err() != nil {
			return
		}
	}
}

func (m *Manager) connectRemote(ctx context.Context, client *mcp.Client) (*mcp.ClientSession, context.CancelFunc, error) {
	conn, err := m.ws.WaitConnection(ctx)
	if err != nil {
		return nil, nil, err
	}

	// The SDK uses the Connect context for the entire session lifetime. Keep
	// that context alive after initialization, but bound the initial handshake.
	sessionCtx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-conn.Done():
			cancel()
		case <-sessionCtx.Done():
		}
	}()
	timer := time.AfterFunc(m.handshakeTimeout, func() {
		m.logger.Warn("remote MCP initialization timed out; closing browser connection")
		cancel()
		_ = conn.Close()
	})
	session, err := client.Connect(sessionCtx, mcptransport.NewConnected(conn), nil)
	timer.Stop()
	if err != nil {
		cancel()
		_ = conn.Close()
		return nil, nil, err
	}
	return session, cancel, nil
}

func (m *Manager) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.remoteSession != nil && m.ws.Live()
}

func (m *Manager) WaitConnected(ctx context.Context) bool {
	ctx, cancel := m.withAppCancellation(ctx)
	defer cancel()

	m.mu.RLock()
	if m.remoteSession != nil && m.ws.Live() {
		m.mu.RUnlock()
		return true
	}
	ch := m.connectedCh
	m.mu.RUnlock()
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}

func (m *Manager) withAppCancellation(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.appCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (m *Manager) setRemoteSession(session *mcp.ClientSession) {
	m.mu.Lock()
	m.remoteSession = session
	select {
	case <-m.connectedCh:
	default:
		close(m.connectedCh)
	}
	m.mu.Unlock()
}

func (m *Manager) remoteToolListChanged(_ context.Context, _ *mcp.ToolListChangedRequest) {
	if !m.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.refreshing.Store(false)
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-m.appCtx.Done():
			return
		}
		if err := m.RefreshTools(m.appCtx); err != nil && m.appCtx.Err() == nil {
			m.logger.Warn("remote tools refresh failed", "error", err)
		}
	}()
}

func (m *Manager) RefreshTools(ctx context.Context) error {
	m.mu.RLock()
	session := m.remoteSession
	m.mu.RUnlock()
	if session == nil {
		return nil
	}

	tools := map[string]*mcp.Tool{}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		if tool == nil {
			continue
		}
		if isReservedTool(tool.Name) {
			m.logger.Warn("skipping remote tool with reserved name", "name", tool.Name)
			continue
		}
		normalized, ok := m.normalizedTool(tool)
		if !ok {
			continue
		}
		tools[normalized.Name] = normalized
	}

	m.mu.Lock()
	old := m.remoteToolNames
	added := make([]*mcp.Tool, 0)
	removed := make([]string, 0)
	for name := range old {
		if _, ok := tools[name]; !ok {
			removed = append(removed, name)
		}
	}
	for name, tool := range tools {
		if _, ok := old[name]; !ok {
			added = append(added, tool)
			continue
		}
		added = append(added, tool)
	}
	next := make(map[string]struct{}, len(tools))
	for name := range tools {
		next[name] = struct{}{}
	}
	m.remoteToolNames = next
	m.mu.Unlock()

	if len(removed) > 0 {
		m.server.RemoveTools(removed...)
	}
	for _, tool := range added {
		name := tool.Name
		t := *tool
		m.server.AddTool(&t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return m.forwardToolCall(ctx, name, req)
		})
	}
	return nil
}

func (m *Manager) handleDisconnect() {
	var session *mcp.ClientSession
	var names []string
	m.mu.Lock()
	session = m.remoteSession
	m.remoteSession = nil
	for name := range m.remoteToolNames {
		names = append(names, name)
	}
	m.remoteToolNames = map[string]struct{}{}
	m.progress = map[string]progressTarget{}
	select {
	case <-m.connectedCh:
		m.connectedCh = make(chan struct{})
	default:
	}
	m.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}
	if len(names) > 0 {
		m.server.RemoveTools(names...)
	}
}

func (m *Manager) forwardToolCall(ctx context.Context, name string, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := m.withAppCancellation(ctx)
	defer cancel()

	m.mu.RLock()
	session := m.remoteSession
	m.mu.RUnlock()
	if session == nil || !m.ws.Live() {
		var res mcp.CallToolResult
		res.SetError(errors.New("Colab UI is not connected"))
		return &res, nil
	}

	var args any = map[string]any{}
	if len(req.Params.Arguments) > 0 {
		args = json.RawMessage(req.Params.Arguments)
	}
	params := &mcp.CallToolParams{Name: name, Arguments: args}

	localToken := req.Params.GetProgressToken()
	remoteToken := any(nil)
	if localToken != nil {
		remoteToken = fmt.Sprintf("colab-mcp-go:%d", m.progressSeq.Add(1))
		params.SetProgressToken(remoteToken)
		m.registerProgress(remoteToken, progressTarget{session: req.Session, token: localToken})
		defer func() {
			time.AfterFunc(2*time.Second, func() {
				m.unregisterProgress(remoteToken)
			})
		}()
	}

	res, err := session.CallTool(ctx, params)
	if err != nil {
		var toolErr mcp.CallToolResult
		toolErr.SetError(err)
		return &toolErr, nil
	}
	return res, nil
}

func (m *Manager) registerProgress(token any, target progressTarget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.progress[tokenKey(token)] = target
}

func (m *Manager) unregisterProgress(token any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.progress, tokenKey(token))
}

func (m *Manager) remoteProgress(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}
	m.mu.RLock()
	target, ok := m.progress[tokenKey(req.Params.ProgressToken)]
	m.mu.RUnlock()
	if !ok || target.session == nil {
		return
	}
	params := *req.Params
	params.ProgressToken = target.token
	_ = target.session.NotifyProgress(ctx, &params)
}

func (m *Manager) normalizedTool(tool *mcp.Tool) (*mcp.Tool, bool) {
	t := *tool
	if t.InputSchema == nil {
		m.logger.Warn("remote tool missing input schema; normalizing to empty object", "name", t.Name)
		t.InputSchema = emptyObjectSchema
	} else if !schemaIsObject(t.InputSchema) {
		m.logger.Warn("skipping remote tool with non-object input schema", "name", t.Name)
		return nil, false
	}
	if t.OutputSchema != nil && !schemaIsObject(t.OutputSchema) {
		m.logger.Warn("dropping non-object output schema from remote tool", "name", t.Name)
		t.OutputSchema = nil
	}
	return &t, true
}

func schemaIsObject(schema any) bool {
	data, err := json.Marshal(schema)
	if err != nil {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return false
	}
	return obj["type"] == "object"
}

func tokenKey(token any) string {
	switch v := token.(type) {
	case string:
		return "s:" + v
	case int:
		return fmt.Sprintf("i:%d", v)
	case int32:
		return fmt.Sprintf("i32:%d", v)
	case int64:
		return fmt.Sprintf("i64:%d", v)
	case float64:
		return fmt.Sprintf("f64:%g", v)
	default:
		return fmt.Sprintf("%T:%v", token, token)
	}
}

func isReservedTool(name string) bool {
	return name == InjectedToolName || name == ListToolsName || name == CallToolName || name == StatusToolName || name == DisconnectToolName
}

var Version = "v0.1.0"
