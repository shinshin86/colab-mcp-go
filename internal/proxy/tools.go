// Portions of this file are based on googlecolab/colab-mcp,
// licensed under the Apache License, Version 2.0.
// This file has been adapted for the Go implementation.

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (m *Manager) openColabBrowserConnection(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		NotebookURL string `json:"notebook_url"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			var res mcp.CallToolResult
			res.SetError(err)
			return &res, nil
		}
	}
	if m.IsConnected() {
		if strings.TrimSpace(in.NotebookURL) != "" {
			var res mcp.CallToolResult
			res.SetError(fmt.Errorf("a Colab browser session is already connected; close that browser connection before opening another notebook"))
			return &res, nil
		}
		return boolToolResult(true), nil
	}

	token := req.Params.GetProgressToken()
	if token != nil {
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      1,
			Total:         3,
			Message:       "The user is not connected to the Colab UI",
		})
	}

	browserURL, err := m.ws.BrowserURL(in.NotebookURL)
	if err != nil {
		var res mcp.CallToolResult
		res.SetError(err)
		return &res, nil
	}
	if err := m.opener.Open(ctx, browserURL); err != nil {
		m.logger.Warn("failed to open Colab browser URL", "error", err)
	} else {
		m.logger.Info("opened Colab browser URL")
	}

	if token != nil {
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      2,
			Total:         3,
			Message:       fmt.Sprintf("Waiting for user to connect in Colab - will wait for %gs", m.timeout.Seconds()),
		})
	}

	waitCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	ok := m.WaitConnected(waitCtx)
	if token != nil {
		msg := "Timeout while waiting for the user to connect."
		if ok {
			msg = "The Colab UI is successfully connected!"
		}
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      3,
			Total:         3,
			Message:       msg,
		})
	}
	return boolToolResult(ok), nil
}

func (m *Manager) listColabTools(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.RLock()
	session := m.remoteSession
	m.mu.RUnlock()
	if session == nil || !m.ws.Live() {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("Colab is not connected. Run %s first.", InjectedToolName))
		return &res, nil
	}
	var tools []map[string]any
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			var res mcp.CallToolResult
			res.SetError(err)
			return &res, nil
		}
		if tool == nil || isReservedTool(tool.Name) {
			continue
		}
		tools = append(tools, map[string]any{
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.InputSchema,
		})
	}
	data, _ := json.Marshal(tools)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
		StructuredContent: map[string]any{"tools": tools},
	}, nil
}

func (m *Manager) callColabTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			var res mcp.CallToolResult
			res.SetError(err)
			return &res, nil
		}
	}
	if in.Name == "" || isReservedTool(in.Name) {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("%q is not a proxied Colab notebook tool", in.Name))
		return &res, nil
	}
	callReq := *req
	callParams := *req.Params
	callParams.Name = in.Name
	callParams.Arguments = in.Arguments
	callReq.Params = &callParams
	return m.forwardToolCall(ctx, in.Name, &callReq)
}

func (m *Manager) getColabConnectionStatus(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.RLock()
	remoteSessionActive := m.remoteSession != nil
	remoteToolCount := len(m.remoteToolNames)
	m.mu.RUnlock()

	uptime := int64(time.Since(m.ws.StartedAt()).Seconds())
	if uptime < 0 {
		uptime = 0
	}
	status := map[string]any{
		"pid":                   os.Getpid(),
		"port":                  m.ws.Port(),
		"browser_connected":     m.ws.Live(),
		"remote_session_active": remoteSessionActive,
		"remote_tool_count":     remoteToolCount,
		"uptime_seconds":        uptime,
		"version":               Version,
	}
	data, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
		StructuredContent: status,
	}, nil
}

func boolToolResult(v bool) *mcp.CallToolResult {
	text := "false"
	if v {
		text = "true"
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: map[string]any{"result": v},
	}
}
