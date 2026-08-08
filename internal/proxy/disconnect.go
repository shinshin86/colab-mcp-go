package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const DisconnectToolName = "disconnect_colab_runtime"

// unassignMarkerPrefix tags the cell added by this tool; a per-call nonce is
// appended so the newest disconnect cell is distinguishable from cells left
// behind by earlier disconnects.
const unassignMarkerPrefix = "colab-mcp-go: disconnect_colab_runtime"

// unassignRunTimeout bounds the final run step: the kernel usually dies
// mid-execution, so the call may never get a response.
const unassignRunTimeout = 30 * time.Second

// Outcomes reported in the structured result. "unknown" covers the expected
// success shape (the kernel terminates while executing the cell, so the run
// step errors or times out) and must be verified in the Colab UI. Failures
// before the cell is issued are reported as tool errors, not as an outcome.
const (
	outcomeCompleted = "completed"
	outcomeUnknown   = "unknown"
)

var runToolNames = []string{"run_code_cell", "execute_cell"}

var cellIDArgNames = []string{"cellId", "cell_id"}

const (
	addToolName = "add_code_cell"
	getToolName = "get_cells"
)

// runtime.unassign() is the official API behind
// "Runtime > Disconnect and delete runtime".
func unassignCode(marker string) string {
	return "# " + marker + "\nfrom google.colab import runtime\nruntime.unassign()"
}

// executionPlan is how the unassign snippet will be executed remotely: either
// a runner that takes code directly, or add_code_cell followed by a runner
// that takes a cell id.
type executionPlan struct {
	runTool   string
	direct    bool
	cellIDArg string
	addTool   string
	getTool   string
}

func (m *Manager) disconnectColabRuntime(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !m.disconnecting.CompareAndSwap(false, true) {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("another %s call is already in progress", DisconnectToolName))
		return &res, nil
	}
	defer m.disconnecting.Store(false)

	m.mu.RLock()
	session := m.remoteSession
	m.mu.RUnlock()
	if session == nil || !m.ws.Live() {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("Colab is not connected. Run %s first.", InjectedToolName))
		return &res, nil
	}

	plan, err := findExecutionPlan(ctx, session)
	if err != nil {
		var res mcp.CallToolResult
		res.SetError(err)
		return &res, nil
	}

	marker := fmt.Sprintf("%s %d", unassignMarkerPrefix, time.Now().UnixNano())
	code := unassignCode(marker)

	if plan.direct {
		return m.issueUnassign(ctx, session, plan.runTool, map[string]any{"code": code})
	}

	addRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: plan.addTool, Arguments: map[string]any{"code": code}})
	if err != nil {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("%s failed: %w", plan.addTool, err))
		return &res, nil
	}
	if addRes.IsError {
		return addRes, nil
	}

	// The marker lookup via get_cells is authoritative: it proves the id
	// belongs to the cell this call just added. When get_cells responds
	// successfully, a marker match is required — an id from the add result,
	// even a unique one, could be a request id or a stale cell id. The
	// add-result id is only trusted when get_cells is missing or failed.
	candidate, ambiguous := candidateCellID(addRes)
	cellID := ""
	lookupSucceeded := false
	if plan.getTool != "" {
		if getRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: plan.getTool, Arguments: map[string]any{}}); err == nil && !getRes.IsError {
			lookupSucceeded = true
			cellID = findMarkedCellID(getRes, marker)
		}
	}
	if cellID == "" && !lookupSucceeded && !ambiguous {
		cellID = candidate
	}
	if cellID == "" {
		var res mcp.CallToolResult
		reason := "could not determine the id of the added cell"
		switch {
		case lookupSucceeded:
			reason = fmt.Sprintf("%s returned no cell containing this call's marker, so no id could be verified", plan.getTool)
		case ambiguous:
			reason = "the add result contained multiple candidate cell ids"
		}
		res.SetError(fmt.Errorf("added the unassign cell but %s; run it manually via %s (%s)", reason, CallToolName, plan.runTool))
		return &res, nil
	}

	return m.issueUnassign(ctx, session, plan.runTool, map[string]any{plan.cellIDArg: cellID})
}

// issueUnassign runs the unassign cell. Because runtime.unassign() kills the
// kernel while the cell is executing, an error or timeout from this call is
// the expected shape of success; only the Colab UI can confirm it visually.
// Once the request has been dispatched there is no failure that proves the
// cell did not execute, so every post-dispatch error maps to "unknown"; only
// a context cancelled before dispatch is reported as a plain tool error.
func (m *Manager) issueUnassign(ctx context.Context, session *mcp.ClientSession, toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	if ctx.Err() != nil {
		var res mcp.CallToolResult
		res.SetError(fmt.Errorf("cancelled before issuing %s: %w", toolName, ctx.Err()))
		return &res, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, unassignRunTimeout)
	defer cancel()

	res, err := session.CallTool(runCtx, &mcp.CallToolParams{Name: toolName, Arguments: args})
	var outcome, detail string
	switch {
	case err != nil && ctx.Err() != nil:
		outcome = outcomeUnknown
		detail = fmt.Sprintf("%s was cancelled after the request was dispatched (%v); the runtime may already have been released", toolName, err)
	case err != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		outcome = outcomeUnknown
		detail = fmt.Sprintf("%s did not respond within %s; this is expected when the runtime terminates mid-execution", toolName, unassignRunTimeout)
	case err != nil:
		outcome = outcomeUnknown
		detail = fmt.Sprintf("%s got no response (%v); this can happen when the runtime terminates mid-execution", toolName, err)
	case res.IsError:
		outcome = outcomeUnknown
		detail = fmt.Sprintf("%s reported an error (%s); this can mean the kernel died mid-execution, but check the message for a genuine failure such as a missing cell", toolName, resultText(res))
	default:
		outcome = outcomeCompleted
		detail = fmt.Sprintf("%s completed", toolName)
	}
	detail += ". Verify in the Colab UI that the runtime is disconnected; do NOT run more cells to check - that can assign a fresh runtime."
	m.logger.Info("issued runtime unassign", "tool", toolName, "outcome", outcome, "detail", detail)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: detail}},
		StructuredContent: map[string]any{"issued": true, "outcome": outcome, "detail": detail},
	}, nil
}

// findExecutionPlan picks the execution path by fixed priority (runToolNames
// order), accepting a runner only when its input schema confirms the argument
// it will be called with.
func findExecutionPlan(ctx context.Context, session *mcp.ClientSession) (executionPlan, error) {
	byName := map[string]*mcp.Tool{}
	var names []string
	for tool, iterErr := range session.Tools(ctx, nil) {
		if iterErr != nil {
			return executionPlan{}, iterErr
		}
		if tool == nil {
			continue
		}
		names = append(names, tool.Name)
		t := *tool
		byName[tool.Name] = &t
	}

	addOK := false
	if t := byName[addToolName]; t != nil && schemaAccepts(t.InputSchema, "code") {
		addOK = true
	}
	getName := ""
	if byName[getToolName] != nil {
		getName = getToolName
	}

	for _, name := range runToolNames {
		t := byName[name]
		if t == nil {
			continue
		}
		if schemaAccepts(t.InputSchema, "code") {
			return executionPlan{runTool: name, direct: true, getTool: getName}, nil
		}
		for _, arg := range cellIDArgNames {
			if schemaAccepts(t.InputSchema, arg) && addOK {
				return executionPlan{runTool: name, cellIDArg: arg, addTool: addToolName, getTool: getName}, nil
			}
		}
	}
	return executionPlan{}, fmt.Errorf("no usable cell execution plan: need one of [%s] taking code, or %s(code) plus a runner taking a cell id; available tools: %s",
		strings.Join(runToolNames, ", "), addToolName, strings.Join(names, ", "))
}

// schemaAccepts reports whether a call passing exactly the provided argument
// names can satisfy the schema: every provided name must exist in properties,
// and every top-level required name must be among the provided ones.
func schemaAccepts(schema any, provided ...string) bool {
	obj, ok := toJSONValue(schema).(map[string]any)
	if !ok {
		return false
	}
	props, ok := obj["properties"].(map[string]any)
	if !ok {
		return false
	}
	providedSet := map[string]bool{}
	for _, name := range provided {
		if _, ok := props[name]; !ok {
			return false
		}
		providedSet[name] = true
	}
	if required, ok := obj["required"].([]any); ok {
		for _, r := range required {
			name, ok := r.(string)
			if !ok {
				continue
			}
			if !providedSet[name] {
				return false
			}
		}
	}
	return true
}

var cellIDTextPattern = regexp.MustCompile(`(?i)cell.{0,10}?id[^0-9A-Za-z_-]{1,4}([0-9A-Za-z_-]+)`)

// candidateCellID extracts the id of the just-added cell from the add result.
// Explicit cellId/cell_id keys win over bare id keys, which win over a
// textual "cell id: <value>" pattern. If the preferred tier holds more than
// one distinct value there is no way to tell which cell is ours, so the
// result is reported as ambiguous rather than guessed at.
func candidateCellID(res *mcp.CallToolResult) (id string, ambiguous bool) {
	var explicit, bare, textual []string
	record := func(key, val string) {
		switch strings.ToLower(key) {
		case "cellid", "cell_id":
			explicit = appendUnique(explicit, val)
		case "id":
			bare = appendUnique(bare, val)
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
		for _, match := range cellIDTextPattern.FindAllStringSubmatch(text.Text, -1) {
			textual = appendUnique(textual, match[1])
		}
	}
	for _, tier := range [][]string{explicit, bare, textual} {
		if len(tier) == 1 {
			return tier[0], false
		}
		if len(tier) > 1 {
			return "", true
		}
	}
	return "", false
}

// markerSourceKeys are the only fields the marker is searched in: the marker
// is a code comment, so it can only legitimately appear in a cell's code.
var markerSourceKeys = map[string]bool{"source": true, "code": true, "text": true, "content": true}

// findMarkedCellID looks for the object describing the cell whose code
// contains marker (the cell added by this call) and returns its id.
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
	for key, val := range obj {
		if !markerSourceKeys[strings.ToLower(key)] {
			continue
		}
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

func appendUnique(list []string, val string) []string {
	if val == "" {
		return list
	}
	for _, existing := range list {
		if existing == val {
			return list
		}
	}
	return append(list, val)
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
