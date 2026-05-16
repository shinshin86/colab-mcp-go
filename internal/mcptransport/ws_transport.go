package mcptransport

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shinshin86/colab-mcp-go/internal/colabws"
)

type WebSocketAcceptor interface {
	WaitConnection(context.Context) (*colabws.Connection, error)
}

type Transport struct {
	Acceptor WebSocketAcceptor
}

func New(acceptor WebSocketAcceptor) *Transport {
	return &Transport{Acceptor: acceptor}
}

func (t *Transport) Connect(ctx context.Context) (mcp.Connection, error) {
	return t.Acceptor.WaitConnection(ctx)
}
