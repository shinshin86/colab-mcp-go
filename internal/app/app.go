package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shinshin86/colab-mcp-go/internal/browser"
	"github.com/shinshin86/colab-mcp-go/internal/colabws"
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
	server := mcp.NewServer(&mcp.Implementation{Name: "ColabMCP", Version: Version}, &mcp.ServerOptions{
		Logger:       a.Logger,
		Instructions: "Connects to a user's Google Colab session in a browser and allows interactions with their Google Colab notebook.",
	})

	var ws *colabws.Server
	var mgr *proxy.Manager
	if a.Config.EnableProxy {
		var err error
		ws, err = colabws.New(a.Config.Host, a.Logger)
		if err != nil {
			return err
		}
		if err := ws.Start(ctx); err != nil {
			return err
		}
		defer ws.Close()

		var opener browser.Opener = browser.OSOpener{}
		if a.Config.NoBrowser {
			opener = loggingNoopOpener{logger: a.Logger}
		}
		proxy.Version = Version
		mgr = proxy.NewManager(server, ws, opener, a.Config.ConnectTimeout, a.Logger)
		mgr.RegisterInjectedTools()
		go mgr.Run(ctx)
	}

	return server.Run(ctx, &mcp.StdioTransport{})
}

type loggingNoopOpener struct {
	logger *slog.Logger
}

func (o loggingNoopOpener) Open(_ context.Context, url string) error {
	o.logger.Info("not opening browser because --no-browser is set", "url", url)
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
