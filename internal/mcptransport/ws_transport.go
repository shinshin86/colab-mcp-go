// Portions of this file are based on googlecolab/colab-mcp,
// licensed under the Apache License, Version 2.0.
// This file has been adapted for the Go implementation.

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

type connectedTransport struct {
	conn *colabws.Connection
}

func New(acceptor WebSocketAcceptor) *Transport {
	return &Transport{Acceptor: acceptor}
}

// NewConnected binds an already accepted browser connection to one MCP
// handshake. Accepting it separately lets the proxy bound that handshake.
func NewConnected(conn *colabws.Connection) mcp.Transport {
	return &connectedTransport{conn: conn}
}

func (t *Transport) Connect(ctx context.Context) (mcp.Connection, error) {
	return t.Acceptor.WaitConnection(ctx)
}

func (t *connectedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t.conn, nil
}
