package browser

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

type Opener interface {
	Open(ctx context.Context, url string) error
}

type OSOpener struct{}

func (OSOpener) Open(ctx context.Context, url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	return cmd.Process.Release()
}

type NoopOpener struct{}

func (NoopOpener) Open(context.Context, string) error { return nil }
