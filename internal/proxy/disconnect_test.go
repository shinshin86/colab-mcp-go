package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shinshin86/colab-mcp-go/internal/colabws"
)

func newDisconnectTestManager(t *testing.T, ctx context.Context) (*Manager, *mcp.Server) {
	t.Helper()
	ws := startWS(t, ctx)
	keepWSLive(t, ws)
	remoteServer, remoteSession := startMutableRemoteMCP(t, ctx)
	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	mgr.setRemoteSession(remoteSession)
	return mgr, remoteServer
}

func callDisconnect(t *testing.T, mgr *Manager) *mcp.CallToolResult {
	t.Helper()
	res, err := mgr.disconnectColabRuntime(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: DisconnectToolName},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func cellSchema(props ...string) map[string]any {
	properties := map[string]any{}
	for _, p := range props {
		properties[p] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": properties}
}

// observedColabToolSchemas loads the exact input schemas captured from a live
// Colab MCP session so future schema drift breaks this regression test.
func observedColabToolSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/colab_tool_schemas.json")
	if err != nil {
		t.Fatal(err)
	}
	var schemas map[string]map[string]any
	if err := json.Unmarshal(data, &schemas); err != nil {
		t.Fatal(err)
	}
	return schemas
}

func decodeArgs(t *testing.T, req *mcp.CallToolRequest) map[string]any {
	t.Helper()
	args := map[string]any{}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			t.Fatal(err)
		}
	}
	return args
}

func disconnectOutcome(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v", res.StructuredContent)
	}
	if sc["issued"] != true {
		t.Fatalf("structured content = %#v, want issued true", sc)
	}
	outcome, _ := sc["outcome"].(string)
	return outcome
}

func TestDisconnectRuntimeNotConnected(t *testing.T) {
	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	ws, err := colabws.New("localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	res := callDisconnect(t, mgr)
	if !res.IsError {
		t.Fatalf("expected error result, got %#v", res)
	}
}

func TestDisconnectRuntimeNoExecutionPlan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, _ := newDisconnectTestManager(t, ctx)
	res := callDisconnect(t, mgr)
	if !res.IsError {
		t.Fatalf("expected error result, got %#v", res)
	}
	if !contains(resultText(res), "no usable cell execution plan") {
		t.Fatalf("unexpected error text: %s", resultText(res))
	}
}

func TestDisconnectRuntimeRunnerWithoutAddToolFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	res := callDisconnect(t, mgr)
	if !res.IsError {
		t.Fatalf("expected error result, got %#v", res)
	}
}

func TestDisconnectRuntimeAddsAndRunsCell(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	var addedCode, ranCellID string
	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		addedCode, _ = decodeArgs(t, req)["code"].(string)
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: `{"cellId":"cell-1"}`}},
			StructuredContent: map[string]any{"cellId": "cell-1"},
		}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ranCellID, _ = decodeArgs(t, req)["cellId"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if !contains(addedCode, "runtime.unassign()") || !contains(addedCode, unassignMarkerPrefix) {
		t.Fatalf("added code = %q, want marked unassign snippet", addedCode)
	}
	if ranCellID != "cell-1" {
		t.Fatalf("ran cell id = %q, want cell-1", ranCellID)
	}
	if outcome := disconnectOutcome(t, res); outcome != outcomeCompleted {
		t.Fatalf("outcome = %q, want %q", outcome, outcomeCompleted)
	}
}

func TestDisconnectRuntimeObservedColabSchemas(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)
	schemas := observedColabToolSchemas(t)

	var addedCode string
	var addArgs map[string]any
	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: schemas["add_code_cell"]}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		addArgs = decodeArgs(t, req)
		addedCode, _ = addArgs["code"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "cell added"}}}, nil
	})

	getCalls := 0
	remote.AddTool(&mcp.Tool{Name: "get_cells", InputSchema: schemas["get_cells"]}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		getCalls++
		cells := []any{
			map[string]any{"id": "cell-1", "source": "print(1)"},
			map[string]any{"id": "cell-2", "source": "print(2)"},
		}
		if getCalls > 1 {
			cells = append(cells, map[string]any{"id": "disconnect-cell", "source": addedCode})
		}
		encoded, err := json.Marshal(cells)
		if err != nil {
			t.Fatal(err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		}, nil
	})

	var ranCellID string
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: schemas["run_code_cell"]}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ranCellID, _ = decodeArgs(t, req)["cellId"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if addArgs["cellIndex"] != float64(2) {
		t.Fatalf("add args = %#v, want cellIndex 2", addArgs)
	}
	if addArgs["language"] != "python" {
		t.Fatalf("add args = %#v, want language python", addArgs)
	}
	if !contains(addedCode, "runtime.unassign()") || !contains(addedCode, unassignMarkerPrefix) {
		t.Fatalf("added code = %q, want marked unassign snippet", addedCode)
	}
	if getCalls != 2 {
		t.Fatalf("get_cells calls = %d, want 2 (append index and marker verification)", getCalls)
	}
	if ranCellID != "disconnect-cell" {
		t.Fatalf("ran cell id = %q, want disconnect-cell", ranCellID)
	}
}

func TestDisconnectRuntimeCellIndexSchemaRequiresGetCells(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)
	schemas := observedColabToolSchemas(t)

	addCalled := false
	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: schemas["add_code_cell"]}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		addCalled = true
		return &mcp.CallToolResult{}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: schemas["run_code_cell"]}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})

	res := callDisconnect(t, mgr)
	if !res.IsError || !contains(resultText(res), "no usable cell execution plan") {
		t.Fatalf("expected no-plan error, got %#v", res)
	}
	if addCalled {
		t.Fatal("must not add a cell when its insertion index cannot be obtained")
	}
}

func TestDisconnectRuntimeUnrecognizedCellsResultStopsBeforeAdd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)
	schemas := observedColabToolSchemas(t)

	addCalled := false
	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: schemas["add_code_cell"]}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		addCalled = true
		return &mcp.CallToolResult{}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "get_cells", InputSchema: schemas["get_cells"]}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "cells unavailable"}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: schemas["run_code_cell"]}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})

	res := callDisconnect(t, mgr)
	if !res.IsError || !contains(resultText(res), "no recognizable cells array") {
		t.Fatalf("expected cell-count error, got %#v", res)
	}
	if addCalled {
		t.Fatal("must not add a cell when get_cells cannot provide an insertion index")
	}
}

func TestDisconnectRuntimeRunTransportErrorIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"cellId": "cell-2"}, Content: []mcp.Content{&mcp.TextContent{Text: "added"}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("kernel connection lost")
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("run-step error must not fail the tool: %s", resultText(res))
	}
	if outcome := disconnectOutcome(t, res); outcome != outcomeUnknown {
		t.Fatalf("outcome = %q, want %q", outcome, outcomeUnknown)
	}
	if !contains(resultText(res), "Verify in the Colab UI") {
		t.Fatalf("detail should point to UI verification: %s", resultText(res))
	}
}

func TestDisconnectRuntimeRunErrorResultIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"cellId": "cell-3"}, Content: []mcp.Content{&mcp.TextContent{Text: "added"}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res := &mcp.CallToolResult{}
		res.SetError(errors.New("kernel disconnected during execution"))
		return res, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("run-step tool error must not fail the tool: %s", resultText(res))
	}
	if outcome := disconnectOutcome(t, res); outcome != outcomeUnknown {
		t.Fatalf("outcome = %q, want %q", outcome, outcomeUnknown)
	}
	if !contains(resultText(res), "kernel disconnected during execution") {
		t.Fatalf("detail should surface the remote error: %s", resultText(res))
	}
}

func TestDisconnectRuntimeDirectCodeTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	var ranCode string
	remote.AddTool(&mcp.Tool{Name: "execute_cell", InputSchema: cellSchema("code")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ranCode, _ = decodeArgs(t, req)["code"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if !contains(ranCode, "runtime.unassign()") {
		t.Fatalf("ran code = %q, want unassign snippet", ranCode)
	}
}

func TestDisconnectRuntimePriorityPrefersRunCodeCell(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	executeCalled := false
	remote.AddTool(&mcp.Tool{Name: "execute_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		executeCalled = true
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"cellId": "cell-4"}, Content: []mcp.Content{&mcp.TextContent{Text: "added"}}}, nil
	})
	var ranCellID string
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ranCellID, _ = decodeArgs(t, req)["cellId"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if executeCalled {
		t.Fatal("execute_cell should not be used when run_code_cell is available")
	}
	if ranCellID != "cell-4" {
		t.Fatalf("ran cell id = %q, want cell-4", ranCellID)
	}
}

func TestDisconnectRuntimeCellIDArgVariant(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"cell_id": "cell-5"}, Content: []mcp.Content{&mcp.TextContent{Text: "added"}}}, nil
	})
	var gotArgs map[string]any
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cell_id")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		gotArgs = decodeArgs(t, req)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if gotArgs["cell_id"] != "cell-5" {
		t.Fatalf("args = %#v, want cell_id cell-5", gotArgs)
	}
}

func TestDisconnectRuntimeExplicitIDBeatsBareIDs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{
				"cellId": "cell-6",
				"cells":  []any{map[string]any{"id": "other-1"}, map[string]any{"id": "other-2"}},
			},
			Content: []mcp.Content{&mcp.TextContent{Text: "added"}},
		}, nil
	})
	var ranCellID string
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ranCellID, _ = decodeArgs(t, req)["cellId"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if ranCellID != "cell-6" {
		t.Fatalf("ran cell id = %q, want cell-6", ranCellID)
	}
}

func TestDisconnectRuntimeAmbiguousIDsWithoutGetCellsFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{"cells": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}},
			Content:           []mcp.Content{&mcp.TextContent{Text: "added"}},
		}, nil
	})
	runCalled := false
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		runCalled = true
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if !res.IsError {
		t.Fatalf("ambiguous ids must fail, got %#v", res)
	}
	if runCalled {
		t.Fatal("must not run a guessed cell id")
	}
}

func TestDisconnectRuntimeMarkerLookupResolvesAmbiguity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	var addedCode string
	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		addedCode, _ = decodeArgs(t, req)["code"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "cell added"}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "get_cells", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{"cells": []any{
				map[string]any{"id": "other", "source": "print(1)"},
				map[string]any{"id": "decoy", "title": addedCode},
				map[string]any{"id": "cell-9", "source": addedCode},
			}},
			Content: []mcp.Content{&mcp.TextContent{Text: "cells"}},
		}, nil
	})
	var ranCellID string
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ranCellID, _ = decodeArgs(t, req)["cellId"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if ranCellID != "cell-9" {
		t.Fatalf("ran cell id = %q, want cell-9 (marker in source, not in other fields)", ranCellID)
	}
}

func TestDisconnectRuntimeMarkerBeatsAddResultID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	var addedCode string
	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		addedCode, _ = decodeArgs(t, req)["code"].(string)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"cellId": "stale-guess"}, Content: []mcp.Content{&mcp.TextContent{Text: "added"}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "get_cells", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{"cells": []any{map[string]any{"id": "verified", "source": []any{addedCode}}}},
			Content:           []mcp.Content{&mcp.TextContent{Text: "cells"}},
		}, nil
	})
	var ranCellID string
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ranCellID, _ = decodeArgs(t, req)["cellId"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if ranCellID != "verified" {
		t.Fatalf("ran cell id = %q, want marker-verified id", ranCellID)
	}
}

func TestDisconnectRuntimeConcurrentCallRejected(t *testing.T) {
	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	ws, err := colabws.New("localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	mgr.disconnecting.Store(true)
	res := callDisconnect(t, mgr)
	if !res.IsError {
		t.Fatalf("expected in-progress rejection, got %#v", res)
	}
	if !contains(resultText(res), "already in progress") {
		t.Fatalf("unexpected error text: %s", resultText(res))
	}
	mgr.disconnecting.Store(false)
	res = callDisconnect(t, mgr)
	if !contains(resultText(res), "not connected") {
		t.Fatalf("flag must be released for later calls: %s", resultText(res))
	}
}

func TestDisconnectToolNameIsReserved(t *testing.T) {
	if !isReservedTool(DisconnectToolName) {
		t.Fatal("disconnect tool name should be reserved")
	}
}

func TestCandidateCellIDFromText(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Added cell with id: abc-123"}}}
	id, ambiguous := candidateCellID(res)
	if id != "abc-123" || ambiguous {
		t.Fatalf("candidateCellID = %q ambiguous=%v, want abc-123 false", id, ambiguous)
	}
}

func TestCandidateCellIDAmbiguousExplicit(t *testing.T) {
	res := &mcp.CallToolResult{StructuredContent: map[string]any{
		"cells": []any{map[string]any{"cellId": "x"}, map[string]any{"cellId": "y"}},
	}}
	id, ambiguous := candidateCellID(res)
	if id != "" || !ambiguous {
		t.Fatalf("candidateCellID = %q ambiguous=%v, want ambiguous", id, ambiguous)
	}
}

func TestDisconnectRuntimeUnverifiedIDRejectedWhenGetCellsSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"cellId": "stale-guess"}, Content: []mcp.Content{&mcp.TextContent{Text: "added"}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "get_cells", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{"cells": []any{map[string]any{"id": "other", "source": "print(1)"}}},
			Content:           []mcp.Content{&mcp.TextContent{Text: "cells"}},
		}, nil
	})
	runCalled := false
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		runCalled = true
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if !res.IsError {
		t.Fatalf("unverified id must be rejected when get_cells succeeds, got %#v", res)
	}
	if runCalled {
		t.Fatal("must not run an id that get_cells could not verify")
	}
	if !contains(resultText(res), "marker") {
		t.Fatalf("error should mention the marker: %s", resultText(res))
	}
}

func TestDisconnectRuntimeGetCellsErrorFallsBackToCandidate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"cellId": "cell-7"}, Content: []mcp.Content{&mcp.TextContent{Text: "added"}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "get_cells", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res := &mcp.CallToolResult{}
		res.SetError(errors.New("get_cells unavailable"))
		return res, nil
	})
	var ranCellID string
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ranCellID, _ = decodeArgs(t, req)["cellId"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if ranCellID != "cell-7" {
		t.Fatalf("ran cell id = %q, want add-result fallback cell-7", ranCellID)
	}
}

func TestDisconnectRuntimeRunnerWithExtraRequiredArgSkipped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	runCodeCalled := false
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cellId": map[string]any{"type": "string"},
			"mode":   map[string]any{"type": "string"},
		},
		"required": []any{"cellId", "mode"},
	}}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		runCodeCalled = true
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	var ranCode string
	remote.AddTool(&mcp.Tool{Name: "execute_cell", InputSchema: cellSchema("code")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ranCode, _ = decodeArgs(t, req)["code"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	res := callDisconnect(t, mgr)
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if runCodeCalled {
		t.Fatal("runner with unsatisfiable required args must be skipped")
	}
	if !contains(ranCode, "runtime.unassign()") {
		t.Fatalf("ran code = %q, want unassign snippet", ranCode)
	}
}

func TestIssueUnassignPreCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	called := false
	remote.AddTool(&mcp.Tool{Name: "run_code_cell", InputSchema: cellSchema("cellId")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	res, err := mgr.issueUnassign(cancelledCtx, mgr.remoteSession, "run_code_cell", map[string]any{"cellId": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("pre-cancelled context must be a tool error, got %#v", res)
	}
	if called {
		t.Fatal("must not dispatch after the context was cancelled")
	}
}

func TestDisconnectRuntimeCancelAfterDispatchIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	remote.AddTool(&mcp.Tool{Name: "execute_cell", InputSchema: cellSchema("code")}, func(handlerCtx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		select {
		case <-handlerCtx.Done():
		case <-release:
		}
		return nil, handlerCtx.Err()
	})

	callCtx, callCancel := context.WithCancel(context.Background())
	defer callCancel()
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := mgr.disconnectColabRuntime(callCtx, &mcp.CallToolRequest{
			Params: &mcp.CallToolParamsRaw{Name: DisconnectToolName},
		})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("execute_cell was not dispatched")
	}
	callCancel()

	select {
	case res := <-done:
		if res == nil {
			t.Fatal("no result")
		}
		if res.IsError {
			t.Fatalf("post-dispatch cancel must not be a tool error: %s", resultText(res))
		}
		if outcome := disconnectOutcome(t, res); outcome != outcomeUnknown {
			t.Fatalf("outcome = %q, want %q", outcome, outcomeUnknown)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not return after cancellation")
	}
}
