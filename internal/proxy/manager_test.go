package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shinshin86/colab-mcp-go/internal/colabws"
)

type fakeOpener struct {
	urls []string
}

func (f *fakeOpener) Open(_ context.Context, url string) error {
	f.urls = append(f.urls, url)
	return nil
}

func TestOpenToolDisconnectedOpensURLAndTimesOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := startWS(t, ctx)
	local := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	opener := &fakeOpener{}
	mgr := NewManager(local, ws, opener, 10*time.Millisecond, nil)

	res, err := mgr.openColabBrowserConnection(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: InjectedToolName},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opener.urls) != 1 {
		t.Fatalf("opener called %d times, want 1", len(opener.urls))
	}
	if !contains(opener.urls[0], "mcpProxyToken="+ws.Token()) || !contains(opener.urls[0], "mcpProxyPort=") {
		t.Fatalf("URL missing token/port: %s", opener.urls[0])
	}
	if res.StructuredContent.(map[string]any)["result"] != false {
		t.Fatalf("result = %#v, want false", res.StructuredContent)
	}
}

func TestOpenToolAlreadyConnectedDoesNotOpenBrowser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := startWS(t, ctx)
	keepWSLive(t, ws)
	local := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	opener := &fakeOpener{}
	mgr := NewManager(local, ws, opener, time.Second, nil)
	mgr.setRemoteSession(startRemoteMCP(t, ctx))

	res, err := mgr.openColabBrowserConnection(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: InjectedToolName},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opener.urls) != 0 {
		t.Fatalf("opener called %d times, want 0", len(opener.urls))
	}
	if res.StructuredContent.(map[string]any)["result"] != true {
		t.Fatalf("result = %#v, want true", res.StructuredContent)
	}
}

func TestOpenToolConnectionArrivesBeforeTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := startWS(t, ctx)
	local := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	opener := &fakeOpener{}
	mgr := NewManager(local, ws, opener, time.Second, nil)

	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, _ := mgr.openColabBrowserConnection(context.Background(), &mcp.CallToolRequest{
			Params: &mcp.CallToolParamsRaw{Name: InjectedToolName},
		})
		done <- res
	}()
	keepWSLive(t, ws)
	mgr.setRemoteSession(startRemoteMCP(t, ctx))
	select {
	case res := <-done:
		if res.StructuredContent.(map[string]any)["result"] != true {
			t.Fatalf("result = %#v, want true", res.StructuredContent)
		}
	case <-time.After(time.Second):
		t.Fatal("open tool did not return")
	}
}

func TestOpenToolSendsProgressNotifications(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := startWS(t, ctx)
	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	mgr := NewManager(localServer, ws, &fakeOpener{}, 10*time.Millisecond, nil)
	mgr.RegisterInjectedTools()
	progress := make(chan *mcp.ProgressNotificationParams, 3)
	client := startLocalClient(t, ctx, localServer, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			progress <- req.Params
		},
	})
	params := &mcp.CallToolParams{Name: InjectedToolName, Arguments: map[string]any{}}
	params.SetProgressToken("open-token")
	res, err := client.CallTool(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if res.StructuredContent.(map[string]any)["result"] != false {
		t.Fatalf("result = %#v, want false", res.StructuredContent)
	}
	wantMessages := []string{
		"The user is not connected to the Colab UI",
		"Waiting for user to connect in Colab - will wait for 0.01s",
		"Timeout while waiting for the user to connect.",
	}
	for _, want := range wantMessages {
		select {
		case got := <-progress:
			if got.ProgressToken != "open-token" || got.Message != want {
				t.Fatalf("progress = %#v, want token open-token message %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing progress message %q", want)
		}
	}
}

func TestRefreshRegistersRemoteToolAndForwardsCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ws := startWS(t, ctx)
	keepWSLive(t, ws)

	remoteSession := startRemoteMCP(t, ctx)
	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	mgr.RegisterInjectedTools()
	mgr.setRemoteSession(remoteSession)
	if err := mgr.RefreshTools(ctx); err != nil {
		t.Fatal(err)
	}

	client := startLocalClient(t, ctx, localServer, nil)
	list, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTool(list.Tools, "echo") {
		t.Fatalf("echo tool not registered: %#v", toolNames(list.Tools))
	}

	res, err := client.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %#v", res.Content)
	}
	if len(res.Content) != 1 || res.Content[0].(*mcp.TextContent).Text != "hello" {
		t.Fatalf("content = %#v", res.Content)
	}
}

func TestDisconnectRemovesRemoteToolsAndReconnectRegistersAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := startWS(t, ctx)
	keepWSLive(t, ws)
	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	mgr.RegisterInjectedTools()
	mgr.setRemoteSession(startRemoteMCP(t, ctx))
	if err := mgr.RefreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	client := startLocalClient(t, ctx, localServer, nil)
	assertToolEventually(t, ctx, client, "echo", true)

	mgr.handleDisconnect()
	assertToolEventually(t, ctx, client, "echo", false)
	assertToolEventually(t, ctx, client, InjectedToolName, true)

	mgr.setRemoteSession(startRemoteMCP(t, ctx))
	if err := mgr.RefreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	assertToolEventually(t, ctx, client, "echo", true)
}

func TestRemoteToolListChangedRefreshesRegistry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := startWS(t, ctx)
	keepWSLive(t, ws)

	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	remoteServer, remoteSession := startMutableRemoteMCPWithOptions(t, ctx, &mcp.ClientOptions{
		ToolListChangedHandler:      mgr.remoteToolListChanged,
		ProgressNotificationHandler: mgr.remoteProgress,
	})
	mgr.RegisterInjectedTools()
	mgr.setRemoteSession(remoteSession)
	if err := mgr.RefreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	client := startLocalClient(t, ctx, localServer, nil)
	assertToolEventually(t, ctx, client, "echo", true)

	addEchoTool(remoteServer, "later")
	assertToolEventually(t, ctx, client, "later", true)
}

func TestRemoteProgressNotificationIsForwarded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := startWS(t, ctx)
	keepWSLive(t, ws)

	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	_, remoteSession := startMutableRemoteMCPWithOptions(t, ctx, &mcp.ClientOptions{
		ToolListChangedHandler:      mgr.remoteToolListChanged,
		ProgressNotificationHandler: mgr.remoteProgress,
	})
	mgr.RegisterInjectedTools()
	mgr.setRemoteSession(remoteSession)
	if err := mgr.RefreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	progress := make(chan *mcp.ProgressNotificationParams, 1)
	client := startLocalClient(t, ctx, localServer, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			progress <- req.Params
		},
	})
	params := &mcp.CallToolParams{Name: "progress", Arguments: map[string]any{}}
	params.SetProgressToken("local-token")
	if _, err := client.CallTool(ctx, params); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-progress:
		if got.ProgressToken != "local-token" || got.Progress != 1 || got.Total != 2 {
			t.Fatalf("progress = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("progress notification was not forwarded")
	}
}

func TestReservedRemoteToolNameIsSkipped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := startWS(t, ctx)
	keepWSLive(t, ws)
	remoteServer, remoteSession := startMutableRemoteMCP(t, ctx)
	addEchoTool(remoteServer, InjectedToolName)

	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	mgr.RegisterInjectedTools()
	mgr.setRemoteSession(remoteSession)
	if err := mgr.RefreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	client := startLocalClient(t, ctx, localServer, nil)
	list, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, tool := range list.Tools {
		if tool.Name == InjectedToolName {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reserved tool count = %d, want 1; tools=%v", count, toolNames(list.Tools))
	}
}

func TestConcurrentToolCallsAndDisconnectMidCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := startWS(t, ctx)
	keepWSLive(t, ws)
	remoteSession := startRemoteMCP(t, ctx)
	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	mgr.RegisterInjectedTools()
	mgr.setRemoteSession(remoteSession)
	if err := mgr.RefreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	client := startLocalClient(t, ctx, localServer, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = client.CallTool(ctx, &mcp.CallToolParams{
				Name:      "echo",
				Arguments: map[string]any{"message": strconv.Itoa(i)},
			})
		}(i)
	}
	mgr.handleDisconnect()
	wg.Wait()

	res, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "after"}})
	if err == nil && !res.IsError {
		t.Fatalf("stale call should fail as tool error, got %#v", res)
	}
}

func TestReservedRemoteToolIsSkipped(t *testing.T) {
	localServer := mcp.NewServer(&mcp.Implementation{Name: "local"}, nil)
	ws, _ := colabws.New("localhost", nil)
	mgr := NewManager(localServer, ws, &fakeOpener{}, time.Second, nil)
	if !isReservedTool(InjectedToolName) {
		t.Fatal("injected tool should be reserved")
	}
	if _, ok := mgr.normalizedTool(&mcp.Tool{Name: "bad", InputSchema: map[string]any{"type": "string"}}); ok {
		t.Fatal("non-object input schema should be skipped")
	}
}

func startRemoteMCP(t *testing.T, ctx context.Context) *mcp.ClientSession {
	t.Helper()
	_, session := startMutableRemoteMCP(t, ctx)
	return session
}

func startMutableRemoteMCP(t *testing.T, ctx context.Context) (*mcp.Server, *mcp.ClientSession) {
	return startMutableRemoteMCPWithOptions(t, ctx, nil)
}

func startMutableRemoteMCPWithOptions(t *testing.T, ctx context.Context, opts *mcp.ClientOptions) (*mcp.Server, *mcp.ClientSession) {
	t.Helper()
	remote := mcp.NewServer(&mcp.Implementation{Name: "remote"}, nil)
	addEchoTool(remote, "echo")
	addProgressTool(remote)
	serverT, clientT := mcp.NewInMemoryTransports()
	go func() { _ = remote.Run(ctx, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, opts)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return remote, session
}

func addEchoTool(server *mcp.Server, name string) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Message string `json:"message"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: in.Message}},
			StructuredContent: map[string]any{"message": in.Message},
		}, nil, nil
	})
}

func addProgressTool(server *mcp.Server) {
	server.AddTool(&mcp.Tool{Name: "progress", Description: "progress", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if token := req.Params.GetProgressToken(); token != nil {
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token,
				Progress:      1,
				Total:         2,
				Message:       "remote progress",
			})
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
	})
}

func startLocalClient(t *testing.T, ctx context.Context, server *mcp.Server, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	serverT, clientT := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-local-client"}, opts)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func startWS(t *testing.T, ctx context.Context) *colabws.Server {
	t.Helper()
	ws, err := colabws.New("localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func keepWSLive(t *testing.T, ws *colabws.Server) {
	t.Helper()
	header := http.Header{}
	header.Set("Origin", colabws.ColabAlternativeURL)
	header.Set("Authorization", "Bearer "+ws.Token())
	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = []string{colabws.Subprotocol}
	conn, _, err := dialer.Dial("ws://localhost:"+strconv.Itoa(ws.Port()), header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ws.Live() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("websocket did not become live")
}

func hasTool(tools []*mcp.Tool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func toolNames(tools []*mcp.Tool) []string {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func assertToolEventually(t *testing.T, ctx context.Context, client *mcp.ClientSession, name string, want bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		list, err := client.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if hasTool(list.Tools, name) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, _ := client.ListTools(ctx, nil)
	t.Fatalf("tool %q presence did not become %v; tools=%v", name, want, toolNames(list.Tools))
}
