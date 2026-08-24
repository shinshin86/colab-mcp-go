// Portions of this file are based on googlecolab/colab-mcp,
// licensed under the Apache License, Version 2.0.
// This file has been adapted for the Go implementation.

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
				return err
			}
			defer func() {
				if err := stateLock.Close(); err != nil {
					a.Logger.Warn("failed to clean up process state", "error", err)
				}
			}()
		}
		var err error
		ws, err = colabws.NewWithOptions(colabws.Options{
			Host:      a.Config.Host,
			Port:      a.Config.Port,
			Token:     token,
			StartedAt: startedAt,
			Version:   Version,
		}, a.Logger)
		if err != nil {
			return err
		}
		if stateLock != nil {
			ws.OnConnectionChange(func(connected bool) {
				if err := stateLock.Update(ws.Port(), connected); err != nil {
					a.Logger.Warn("failed to update browser connection state", "error", err)
				}
			})
		}
		if err := ws.Start(appCtx); err != nil {
			return err
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
