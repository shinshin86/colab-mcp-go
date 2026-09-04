// Portions of this file are based on googlecolab/colab-mcp,
// licensed under the Apache License, Version 2.0.
// This file has been adapted for the Go implementation.

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shinshin86/colab-mcp-go/internal/browser"
	"github.com/shinshin86/colab-mcp-go/internal/colabws"
	"github.com/shinshin86/colab-mcp-go/internal/instance"
	"github.com/shinshin86/colab-mcp-go/internal/proxy"
)

type App struct {
	Config Config
	Logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	return &App{Config: cfg, Logger: logger}
}

func (a *App) Run(ctx context.Context) error {
	appCtx, stopApp := context.WithCancel(ctx)
	defer stopApp()

	server := mcp.NewServer(&mcp.Implementation{Name: "ColabMCP", Version: Version}, &mcp.ServerOptions{
		Logger:       a.Logger,
		Instructions: "Connects to a user's Google Colab session in a browser and allows interactions with their Google Colab notebook.",
	})

	var ws *colabws.Server
	var mgr *proxy.Manager
	if a.Config.EnableProxy {
		startedAt := time.Now().UTC()
		token := ""
		bindPort := a.Config.Port
		info := proxy.InstanceInfo{Mode: proxy.InstanceModePrimary, ConfiguredPort: a.Config.Port}
		var stateLock *instance.Lock
		if a.Config.TokenFile != "" {
			var err error
			token, err = colabws.LoadOrCreateToken(a.Config.TokenFile)
			if err != nil {
				return fmt.Errorf("load browser connection token: %w", err)
			}
			stateLock, err = instance.Acquire(a.Config.TokenFile, instance.State{
				PID:       os.Getpid(),
				Port:      a.Config.Port,
				StartedAt: startedAt,
			})
			if err != nil {
				var already *instance.AlreadyRunningError
				if a.Config.NoFallback || !errors.As(err, &already) {
					return err
				}
				// Another bridge owns the shared state lock (and most likely
				// the configured port). Keep serving this MCP client on an
				// ephemeral port instead of exiting, so the client still gets
				// a working set of tools. The lock owner keeps state.json.
				stateLock = nil
				bindPort = 0
				info.Mode = proxy.InstanceModeFallback
				info.FallbackReason = "instance lock unavailable: " + already.SafeMessage()
				attrs := []any{"configured_port", a.Config.Port}
				if already.Owner != nil {
					attrs = append(attrs, "owner_pid", already.Owner.PID, "owner_started_at", already.Owner.StartedAt)
				}
				a.Logger.Warn("instance lock is held by another bridge; continuing on an ephemeral port without the shared state lock", attrs...)
			} else {
				defer func() {
					if err := stateLock.Close(); err != nil {
						a.Logger.Warn("failed to clean up process state", "error", err)
					}
				}()
			}
		}
		var onChange func(port int, connected bool)
		if stateLock != nil {
			onChange = func(port int, connected bool) {
				if err := stateLock.Update(port, connected); err != nil {
					a.Logger.Warn("failed to update browser connection state", "error", err)
				}
			}
		}
		var err error
		ws, err = a.startWebSocketServer(appCtx, bindPort, token, startedAt, onChange)
		if err != nil {
			if bindPort == 0 || a.Config.NoFallback || !isListenError(err) {
				return err
			}
			reason := fmt.Sprintf("port %d unavailable: %s", bindPort, listenErrorReason(err))
			a.Logger.Warn("configured port is unavailable; continuing on an ephemeral port", "port", bindPort, "reason", listenErrorReason(err))
			ws, err = a.startWebSocketServer(appCtx, 0, token, startedAt, onChange)
			if err != nil {
				return err
			}
			info.Mode = proxy.InstanceModeFallback
			info.FallbackReason = reason
		}
		defer ws.Close()
		if stateLock != nil {
			if err := stateLock.Update(ws.Port(), ws.Live()); err != nil {
				return fmt.Errorf("update process state: %w", err)
			}
		}

		var opener browser.Opener = browser.OSOpener{}
		if a.Config.NoBrowser {
			opener = loggingNoopOpener{logger: a.Logger}
		}
		proxy.Version = Version
		mgr = proxy.NewManager(appCtx, server, ws, opener, a.Config.ConnectTimeout, a.Logger)
		mgr.SetInstanceInfo(info)
		mgr.RegisterInjectedTools()
	}

	managerDone := make(chan struct{})
	if mgr != nil {
		go func() {
			defer close(managerDone)
			mgr.Run(appCtx)
		}()
	} else {
		close(managerDone)
	}

	transport := &cancelOnReadErrorTransport{
		transport: &mcp.StdioTransport{},
		cancel:    stopApp,
	}
	err := server.Run(appCtx, transport)
	stopApp()
	<-managerDone
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// startWebSocketServer creates and starts the browser WebSocket server on the
// requested port. onChange, when set, is registered before the listener opens
// so no browser connection transition can be missed.
func (a *App) startWebSocketServer(ctx context.Context, port int, token string, startedAt time.Time, onChange func(port int, connected bool)) (*colabws.Server, error) {
	ws, err := colabws.NewWithOptions(colabws.Options{
		Host:      a.Config.Host,
		Port:      port,
		Token:     token,
		StartedAt: startedAt,
		Version:   Version,
	}, a.Logger)
	if err != nil {
		return nil, err
	}
	if onChange != nil {
		ws.OnConnectionChange(func(connected bool) { onChange(ws.Port(), connected) })
	}
	if err := ws.Start(ctx); err != nil {
		return nil, err
	}
	return ws, nil
}

func isListenError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "listen"
}

// listenErrorReason describes a listen failure without echoing the full
// operating system error text.
func listenErrorReason(err error) string {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return "address already in use"
	case errors.Is(err, syscall.EACCES):
		return "permission denied"
	default:
		return "listen failed"
	}
}

// cancelOnReadErrorTransport turns stdio EOF (or another terminal read error)
// into application cancellation. The MCP SDK otherwise waits for active tool
// handlers before server.Run returns, so those handlers need this independent
// shutdown signal to finish.
type cancelOnReadErrorTransport struct {
	transport mcp.Transport
	cancel    context.CancelFunc
}

func (t *cancelOnReadErrorTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &cancelOnReadErrorConnection{Connection: conn, cancel: t.cancel}, nil
}

type cancelOnReadErrorConnection struct {
	mcp.Connection
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnReadErrorConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err != nil {
		c.once.Do(c.cancel)
	}
	return msg, err
}

type loggingNoopOpener struct {
	logger *slog.Logger
}

func (o loggingNoopOpener) Open(_ context.Context, _ string) error {
	o.logger.Info("not opening browser because --no-browser is set")
	return nil
}

func InitLogger(logDir string) (*slog.Logger, func(), error) {
	if logDir == "" {
		var err error
		logDir, err = os.MkdirTemp("", "colab-mcp-go-logs-")
		if err != nil {
			return nil, nil, err
		}
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, nil, err
	}
	name := filepath.Join(logDir, fmt.Sprintf("colab-mcp-go.%s.log", time.Now().Format("2006-01-02_15-04-05")))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, func() { _ = f.Close() }, nil
}
