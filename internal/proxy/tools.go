package proxy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (m *Manager) openColabBrowserConnection(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.IsConnected() {
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

	url := m.ws.BrowserURL()
	if err := m.opener.Open(ctx, url); err != nil {
		m.logger.Warn("failed to open Colab browser URL", "error", err, "url", url)
	} else {
		m.logger.Info("opened Colab browser URL", "url", url)
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
