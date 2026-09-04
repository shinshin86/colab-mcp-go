package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shinshin86/colab-mcp-go/internal/app"
	"github.com/shinshin86/colab-mcp-go/internal/doctor"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "doctor" {
		return runDoctor(args[1:], stdout, stderr)
	}
	return runServer(args, stdout, stderr)
}

func runServer(args []string, stdout, stderr io.Writer) int {
	cfg := app.DefaultConfig()
	var showVersion bool
	flags := flag.NewFlagSet("colab-mcp-go", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.LogDir, "log", "", "log file directory")
	flags.StringVar(&cfg.Host, "host", "localhost", "WebSocket bind host")
	flags.IntVar(&cfg.Port, "port", 0, "WebSocket bind port (0 chooses an ephemeral port)")
	flags.StringVar(&cfg.TokenFile, "token-file", "", "persist the browser connection token in this file")
	flags.DurationVar(&cfg.ConnectTimeout, "connect-timeout", 60*time.Second, "Colab UI connection timeout")
	flags.BoolVar(&cfg.NoBrowser, "no-browser", false, "do not open a browser when the connection tool is called")
	flags.BoolVar(&cfg.EnableProxy, "enable-proxy", true, "enable the Colab browser session proxy")
	flags.BoolVar(&cfg.NoFallback, "no-fallback", false, "exit instead of continuing on an ephemeral port when the configured port or instance lock is unavailable")
	flags.BoolVar(&showVersion, "version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "colab-mcp-go: unexpected argument %q\n", flags.Arg(0))
		return 2
	}

	if showVersion {
		fmt.Fprintln(stdout, app.Version)
		return 0
	}

	if cfg.LogDir == "" {
		dir, err := app.DefaultLogDir(cfg.TokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "colab-mcp-go: resolve log directory: %v\n", err)
			return 1
		}
		cfg.LogDir = dir
	}
	logger, cleanup, err := app.InitLogger(cfg.LogDir)
	if err != nil {
		log.New(stderr, "", log.LstdFlags).Printf("init logger: %v", err)
		return 1
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.New(cfg, logger).Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("server exited", "error", err)
		fmt.Fprintf(stderr, "colab-mcp-go: %s\n", safeErrorMessage(err))
		return 1
	}
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	var options doctor.Options
	var jsonOutput bool
	flags := flag.NewFlagSet("colab-mcp-go doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.Host, "host", "localhost", "WebSocket host to inspect")
	flags.IntVar(&options.Port, "port", 0, "WebSocket port to inspect (0 infers it from state.json)")
	flags.StringVar(&options.TokenFile, "token-file", "", "persistent browser connection token file")
	flags.StringVar(&options.Log, "log", "", "log file or directory to inspect")
	flags.BoolVar(&jsonOutput, "json", false, "print machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "colab-mcp-go doctor: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if options.Log == "" {
		// Mirror the server default: logs live beside the token file unless
		// --log was given. Only inspect the directory when it exists.
		if dir, err := app.DefaultLogDir(options.TokenFile); err == nil && dir != "" {
			if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
				options.Log = dir
			}
		}
	}
	report := doctor.Run(context.Background(), options)
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "colab-mcp-go doctor: encode report: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "Status: %s\n", report.Status)
		fmt.Fprintf(stdout, "Diagnosis: %s\n", report.Diagnosis)
		fmt.Fprintf(stdout, "Recommended action: %s\n", report.RecommendedAction)
		if report.Port.Number > 0 {
			fmt.Fprintf(stdout, "Port: %s:%d (listening: %t)\n", report.Port.Host, report.Port.Number, report.Port.Listening)
		}
		if report.Port.OwnerPID > 0 {
			fmt.Fprintf(stdout, "Owner: %s (pid %d)\n", report.Port.OwnerProcess, report.Port.OwnerPID)
		}
		if report.State.Configured {
			fmt.Fprintf(stdout, "State: exists=%t valid=%t pid_alive=%t\n", report.State.Exists, report.State.Valid, report.State.PIDAlive)
		}
		if report.Token.Configured {
			fmt.Fprintf(stdout, "Token file: exists=%t valid=%t permissions=%s\n", report.Token.Exists, report.Token.Valid, report.Token.Permissions)
		}
		if len(report.Log.KnownErrors) > 0 {
			fmt.Fprintf(stdout, "Known log errors: %s\n", strings.Join(report.Log.KnownErrors, ", "))
		}
	}
	if report.Status == "ok" {
		return 0
	}
	return 1
}

func safeErrorMessage(err error) string {
	var safe interface{ SafeMessage() string }
	if errors.As(err, &safe) {
		return safe.SafeMessage()
	}
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
