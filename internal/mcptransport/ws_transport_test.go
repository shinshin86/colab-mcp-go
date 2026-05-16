package mcptransport

import (
	"context"
	"testing"

	"github.com/shinshin86/colab-mcp-go/internal/colabws"
)

type fakeAcceptor struct {
	conn *colabws.Connection
	err  error
}

func (f fakeAcceptor) WaitConnection(ctx context.Context) (*colabws.Connection, error) {
	if f.err != nil {
		return nil, f.err
	}
	<-ctx.Done()
	return f.conn, ctx.Err()
}

func TestConnectHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(fakeAcceptor{}).Connect(ctx)
	if err == nil {
		t.Fatal("Connect should return context cancellation")
	}
}
