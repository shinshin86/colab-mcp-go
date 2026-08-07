package proxy

import (
	"context"
	"encoding/json"
	"errors"
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

func TestDisconnectRuntimeNoExecutionTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, _ := newDisconnectTestManager(t, ctx)
	res := callDisconnect(t, mgr)
	if !res.IsError {
		t.Fatalf("expected error result, got %#v", res)
	}
	if !contains(resultText(res), "no cell execution tool") {
		t.Fatalf("unexpected error text: %s", resultText(res))
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
	if !contains(addedCode, "runtime.unassign()") {
		t.Fatalf("added code = %q, want unassign snippet", addedCode)
	}
	if ranCellID != "cell-1" {
		t.Fatalf("ran cell id = %q, want cell-1", ranCellID)
	}
	sc := res.StructuredContent.(map[string]any)
	if sc["issued"] != true {
		t.Fatalf("structured content = %#v, want issued true", sc)
	}
}

func TestDisconnectRuntimeRunErrorStillIssued(t *testing.T) {
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
	sc := res.StructuredContent.(map[string]any)
	if sc["issued"] != true {
		t.Fatalf("structured content = %#v, want issued true", sc)
	}
	if !contains(resultText(res), "Verify in the Colab UI") {
		t.Fatalf("detail should point to UI verification: %s", resultText(res))
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

func TestDisconnectRuntimeGetCellsFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, remote := newDisconnectTestManager(t, ctx)

	var ranCellID string
	remote.AddTool(&mcp.Tool{Name: "add_code_cell", InputSchema: cellSchema("code")}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "cell added"}}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "get_cells", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{"cells": []any{
				map[string]any{"id": "other", "source": "print(1)"},
				map[string]any{"id": "cell-9", "source": "# " + unassignMarker + "\nfrom google.colab import runtime"},
			}},
			Content: []mcp.Content{&mcp.TextContent{Text: "cells"}},
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
	if ranCellID != "cell-9" {
		t.Fatalf("ran cell id = %q, want cell-9", ranCellID)
	}
}

func TestFindCellIDFromText(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Added cell with id: abc-123"}}}
	if got := findCellID(res); got != "abc-123" {
		t.Fatalf("findCellID = %q, want abc-123", got)
	}
}
