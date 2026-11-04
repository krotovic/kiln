package lsp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Conn carries a session's JSON-RPC messages, one message per read and per write. ServeConn serves a WebSocket and
// ServeStream a byte stream with the Language Server Protocol's base framing; a host with another transport gives
// Serve a Conn of its own.
type Conn interface {
	// ReadMessage blocks until the next message; an error ends the session.
	ReadMessage() ([]byte, error)
	// WriteMessage sends one message. Writes come one at a time.
	WriteMessage(data []byte) error
	// Close ends the transport, so a ReadMessage that is waiting returns an error when it can.
	Close() error
}

// maxStreamMessage bounds one message on a stream: the stream is the host's own (a pipe to its process), and it
// sends the stored scripts as one notification.
const maxStreamMessage = 64 << 20

// wsConn is a WebSocket: a text frame per message, at most maxMessage.
type wsConn struct{ conn *websocket.Conn }

func (c wsConn) ReadMessage() ([]byte, error) {
	_, data, err := c.conn.ReadMessage()
	return data, err
}

func (c wsConn) WriteMessage(data []byte) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c wsConn) Close() error { return c.conn.Close() }

// streamConn is a byte stream framed as the Language Server Protocol's base protocol: headers (Content-Length,
// and an optional Content-Type), an empty line, then the message.
type streamConn struct {
	in  *bufio.Reader
	out io.Writer

	mu     sync.Mutex
	closed bool
	closer func() error
}

// NewStreamConn makes a Conn of a stream: messages read from in and written to out, each framed by its
// Content-Length header. Close closes out when it is an io.Closer, and in when it is one.
func NewStreamConn(in io.Reader, out io.Writer) Conn {
	c := &streamConn{in: bufio.NewReader(in), out: out}
	c.closer = func() error {
		var errs []error
		if w, ok := out.(io.Closer); ok {
			errs = append(errs, w.Close())
		}
		if r, ok := in.(io.Closer); ok {
			errs = append(errs, r.Close())
		}
		return errors.Join(errs...)
	}
	return c
}

func (c *streamConn) ReadMessage() ([]byte, error) {
	headers, err := textproto.NewReader(c.in).ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	length := headers.Get("Content-Length")
	if length == "" {
		return nil, errors.New("a message without a Content-Length header")
	}
	n, err := strconv.Atoi(strings.TrimSpace(length))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("a bad Content-Length %q", length)
	}
	if n > maxStreamMessage {
		return nil, fmt.Errorf("a message of %d bytes, over %d", n, maxStreamMessage)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(c.in, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *streamConn) WriteMessage(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("closed")
	}
	if _, err := fmt.Fprintf(c.out, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
		return err
	}
	_, err := c.out.Write(data)
	return err
}

func (c *streamConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.closer()
}
