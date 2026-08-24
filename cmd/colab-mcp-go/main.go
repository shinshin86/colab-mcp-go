package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shinshin86/colab-mcp-go/internal/app"
)

func main() {
	cfg := app.DefaultConfig()
	var showVersion bool
	flag.StringVar(&cfg.LogDir, "log", "", "log file directory")
	flag.StringVar(&cfg.Host, "host", "localhost", "WebSocket bind host")
	flag.IntVar(&cfg.Port, "port", 0, "WebSocket bind port (0 chooses an ephemeral port)")
	flag.StringVar(&cfg.TokenFile, "token-file", "", "persist the browser connection token in this file")
	flag.DurationVar(&cfg.ConnectTimeout, "connect-timeout", 60*time.Second, "Colab UI connection timeout")
	flag.BoolVar(&cfg.NoBrowser, "no-browser", false, "do not open a browser when the connection tool is called")
	flag.BoolVar(&cfg.EnableProxy, "enable-proxy", true, "enable the Colab browser session proxy")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println(app.Version)
		return
	}

	logger, cleanup, err := app.InitLogger(cfg.LogDir)
	if err != nil {
		log.Fatalf("init logger: %v", err)
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.New(cfg, logger).Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("server exited", "error", err)
		fmt.Fprintf(os.Stderr, "colab-mcp-go: %s\n", safeErrorMessage(err))
		os.Exit(1)
	}
}

func safeErrorMessage(err error) string {
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "listen" {
		return "server exited unexpectedly; see the log for details"
	}
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return "failed to start WebSocket server: address already in use"
	case errors.Is(err, syscall.EACCES):
		return "failed to start WebSocket server: permission denied"
	}
	return "failed to start WebSocket server; see the log for details"
}
