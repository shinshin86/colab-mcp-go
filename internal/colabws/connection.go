package colabws

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

var ErrConnectionClosed = errors.New("websocket connection closed")

const writeTimeout = 2 * time.Second

// Connection wraps a Colab WebSocket as an MCP JSON-RPC connection.
type Connection struct {
	ws           *websocket.Conn
	onClose      func()
	readCh       chan readResult
	writeMu      sync.Mutex
	closeOnce    sync.Once
	closeErr     error
	closed       chan struct{}
	closedSignal sync.Once
}

type readResult struct {
	msg jsonrpc.Message
	err error
}

func newConnection(ws *websocket.Conn, onClose func()) *Connection {
	c := &Connection{ws: ws, onClose: onClose, readCh: make(chan readResult, 16), closed: make(chan struct{})}
	go c.readLoop()
	return c
}

func (c *Connection) readLoop() {
	defer close(c.readCh)
	for {
		typ, data, err := c.ws.ReadMessage()
		if err != nil {
			c.readCh <- readResult{err: err}
			c.Close()
			return
		}
		if typ != websocket.TextMessage && typ != websocket.BinaryMessage {
			c.readCh <- readResult{err: fmt.Errorf("unsupported websocket message type %d", typ)}
			continue
		}
		msg, err := jsonrpc.DecodeMessage(data)
		c.readCh <- readResult{msg: msg, err: err}
	}
}

func (c *Connection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, ErrConnectionClosed
	case res, ok := <-c.readCh:
		if !ok {
			return nil, ErrConnectionClosed
		}
		return res.msg, res.err
	}
}

func (c *Connection) Write(ctx context.Context, msg jsonrpc.Message) error {
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		select {
		case <-ctx.Done():
			done <- ctx.Err()
			return
		case <-c.closed:
			done <- ErrConnectionClosed
			return
		default:
		}
		deadline := time.Now().Add(writeTimeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		if err := c.ws.SetWriteDeadline(deadline); err != nil {
			done <- err
			return
		}
		done <- c.ws.WriteMessage(websocket.TextMessage, data)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrConnectionClosed
	case err := <-done:
		if err != nil {
			c.Close()
		}
		return err
	}
}

func (c *Connection) Close() error {
	didClose := false
	c.closeOnce.Do(func() {
		c.signalClosed()
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		c.closeErr = c.ws.Close()
		didClose = true
	})
	// The callback may close the MCP session, which closes this connection
	// again. Invoke it after sync.Once has finished to allow that reentry.
	if didClose && c.onClose != nil {
		c.onClose()
	}
	return c.closeErr
}

func (c *Connection) signalClosed() {
	c.closedSignal.Do(func() { close(c.closed) })
}

func (c *Connection) SessionID() string { return "" }

func (c *Connection) Done() <-chan struct{} { return c.closed }
