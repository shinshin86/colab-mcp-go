package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const DisconnectToolName = "disconnect_colab_runtime"

const unassignMarker = "colab-mcp-go: disconnect_colab_runtime"

// unassignCode is executed in the Colab kernel. runtime.unassign() is the
// official API behind "Runtime > Disconnect and delete runtime".
const unassignCode = "# " + unassignMarker + "\nfrom google.colab import runtime\nruntime.unassign()"

// unassignRunTimeout bounds the final run step: the kernel usually dies
// mid-execution, so the call may never get a response.
const unassignRunTimeout = 30 * time.Second

var runToolNames = []string{"run_code_cell", "execute_cell"}

const (
	addToolName = "add_code_cell"
	getToolName = "get_cells"
)

func (m *Manager) disconnectColabRuntime(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.RLock()
	session := m.remoteSession
	m.mu.RUnlock()
	if session == nil || !m.ws.Live() {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("Colab is not connected. Run %s first.", InjectedToolName))
		return &res, nil
	}

	runTool, addTool, getTool, err := findExecutionTools(ctx, session)
	if err != nil {
		var res mcp.CallToolResult
		res.SetError(err)
		return &res, nil
	}

	// Older Colab builds exposed an execution tool that takes code directly.
	if schemaHasProperty(runTool.InputSchema, "code") {
		return m.issueUnassign(ctx, session, runTool.Name, map[string]any{"code": unassignCode})
	}

	// Current builds run existing cells by id: add a cell, then run it.
	if addTool == nil {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("remote tool %q runs cells by id but %q is not available; use %s manually", runTool.Name, addToolName, CallToolName))
		return &res, nil
	}
	addRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: addTool.Name, Arguments: map[string]any{"code": unassignCode}})
	if err != nil {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("%s failed: %w", addTool.Name, err))
		return &res, nil
	}
	if addRes.IsError {
		return addRes, nil
	}

	cellID := findCellID(addRes)
	if cellID == "" && getTool != nil {
		if getRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: getTool.Name, Arguments: map[string]any{}}); err == nil && !getRes.IsError {
			cellID = findMarkedCellID(getRes, unassignMarker)
		}
	}
	if cellID == "" {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("added the unassign cell but could not determine its cell id; run it manually via %s (%s)", CallToolName, runTool.Name))
		return &res, nil
	}

	return m.issueUnassign(ctx, session, runTool.Name, map[string]any{"cellId": cellID})
}

// issueUnassign runs the unassign cell. Because runtime.unassign() kills the
// kernel while the cell is executing, an error or timeout from this call is
// the expected shape of success; only the Colab UI can confirm it visually.
func (m *Manager) issueUnassign(ctx context.Context, session *mcp.ClientSession, toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, unassignRunTimeout)
	defer cancel()

	detail := ""
	res, err := session.CallTool(runCtx, &mcp.CallToolParams{Name: toolName, Arguments: args})
	switch {
	case err != nil:
		detail = fmt.Sprintf("%s got no response (%v); this is expected when the runtime terminates mid-execution", toolName, err)
	case res.IsError:
		detail = fmt.Sprintf("%s reported an error (%s); this can still mean the runtime terminated mid-execution", toolName, resultText(res))
	default:
		detail = fmt.Sprintf("%s completed", toolName)
	}
	detail += ". Verify in the Colab UI that the runtime is disconnected; do NOT run more cells to check - that can assign a fresh runtime."
	m.logger.Info("issued runtime unassign", "tool", toolName, "detail", detail)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: detail}},
		StructuredContent: map[string]any{"issued": true, "detail": detail},
	}, nil
}

func findExecutionTools(ctx context.Context, session *mcp.ClientSession) (runTool, addTool, getTool *mcp.Tool, err error) {
	var names []string
	for tool, iterErr := range session.Tools(ctx, nil) {
		if iterErr != nil {
			return nil, nil, nil, iterErr
		}
		if tool == nil {
			continue
		}
		names = append(names, tool.Name)
		switch {
		case tool.Name == addToolName:
			t := *tool
			addTool = &t
		case tool.Name == getToolName:
			t := *tool
			getTool = &t
		default:
			for _, name := range runToolNames {
				if tool.Name == name && runTool == nil {
					t := *tool
					runTool = &t
				}
			}
		}
	}
	if runTool == nil {
		return nil, nil, nil, fmt.Errorf("no cell execution tool (%s) exposed by the Colab notebook; available: %s", strings.Join(runToolNames, ", "), strings.Join(names, ", "))
	}
	return runTool, addTool, getTool, nil
}

func schemaHasProperty(schema any, name string) bool {
	obj, ok := toJSONValue(schema).(map[string]any)
	if !ok {
		return false
	}
	props, ok := obj["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = props[name]
	return ok
}

var cellIDTextPattern = regexp.MustCompile(`(?i)cell.{0,10}?id[^0-9A-Za-z_-]{1,4}([0-9A-Za-z_-]+)`)

// findCellID extracts a cell id from a tool result, preferring explicit
// cellId/cell_id keys over bare id keys, and falling back to a textual
// "cell id: <value>" pattern for non-JSON results.
func findCellID(res *mcp.CallToolResult) string {
	var cellID, plainID string
	record := func(key, val string) {
		switch strings.ToLower(key) {
		case "cellid", "cell_id":
			if cellID == "" {
				cellID = val
			}
		case "id":
			if plainID == "" {
				plainID = val
			}
		}
	}
	if res.StructuredContent != nil {
		walkStrings(toJSONValue(res.StructuredContent), record)
	}
	for _, c := range res.Content {
		text, ok := c.(*mcp.TextContent)
		if !ok {
			continue
		}
		var v any
		if json.Unmarshal([]byte(text.Text), &v) == nil {
			walkStrings(v, record)
			continue
		}
		if cellID == "" {
			if match := cellIDTextPattern.FindStringSubmatch(text.Text); match != nil {
				cellID = match[1]
			}
		}
	}
	if cellID != "" {
		return cellID
	}
	return plainID
}

// findMarkedCellID looks for the object describing the cell whose source
// contains marker (the cell added by this tool) and returns its id.
func findMarkedCellID(res *mcp.CallToolResult, marker string) string {
	var found string
	var visit func(v any)
	visit = func(v any) {
		if found != "" {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			if containsMarker(t, marker) {
				if id := idFromMap(t); id != "" {
					found = id
					return
				}
			}
			for _, val := range t {
				visit(val)
			}
		case []any:
			for _, val := range t {
				visit(val)
			}
		}
	}
	if res.StructuredContent != nil {
		visit(toJSONValue(res.StructuredContent))
	}
	for _, c := range res.Content {
		if found != "" {
			break
		}
		if text, ok := c.(*mcp.TextContent); ok {
			var v any
			if json.Unmarshal([]byte(text.Text), &v) == nil {
				visit(v)
			}
		}
	}
	return found
}

func containsMarker(obj map[string]any, marker string) bool {
	for _, val := range obj {
		switch t := val.(type) {
		case string:
			if strings.Contains(t, marker) {
				return true
			}
		case []any:
			for _, item := range t {
				if s, ok := item.(string); ok && strings.Contains(s, marker) {
					return true
				}
			}
		}
	}
	return false
}

func idFromMap(obj map[string]any) string {
	for _, key := range []string{"cellId", "cell_id", "id"} {
		for k, val := range obj {
			if strings.EqualFold(k, key) {
				if s, ok := val.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func walkStrings(v any, fn func(key, val string)) {
	switch t := v.(type) {
	case map[string]any:
		for key, val := range t {
			if s, ok := val.(string); ok {
				fn(key, s)
				continue
			}
			walkStrings(val, fn)
		}
	case []any:
		for _, val := range t {
			walkStrings(val, fn)
		}
	}
}

// toJSONValue normalizes typed values (e.g. schema structs) into plain
// map[string]any / []any trees so they can be walked uniformly.
func toJSONValue(v any) any {
	switch v.(type) {
	case map[string]any, []any, string, float64, bool, nil:
		return v
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, " ")
}
