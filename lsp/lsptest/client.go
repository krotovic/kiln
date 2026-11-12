// Package lsptest is a test's end of a language server session: a client over a WebSocket or a stream that sends
// requests and notifications and waits for answers and diagnostics. Hosts use it to test the server they configure.
package lsptest

import (
	"encoding/json"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/krotovic/kiln/lsp"
)

// Message is any JSON-RPC message the server sends.
type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Client is one session.
type Client struct {
	t      testing.TB
	conn   lsp.Conn
	nextID int
	// In has every message the server sends that Call and Diagnostics haven't taken; it closes with the session.
	In chan Message
	// Timeout bounds every wait (default 5 s).
	Timeout time.Duration
}

// Dial opens a session at url (ws://…), closed when the test ends.
func Dial(t testing.TB, url string) *Client {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(t, wsConn{conn})
}

// Stream opens a session on a stream (a server's standard output and input, say): messages framed by their
// Content-Length header, read from in and written to out, closed when the test ends.
func Stream(t testing.TB, in io.Reader, out io.Writer) *Client {
	return New(t, lsp.NewStreamConn(in, out))
}

// New opens a session on conn, closed when the test ends.
func New(t testing.TB, conn lsp.Conn) *Client {
	c := &Client{t: t, conn: conn, In: make(chan Message, 64), Timeout: 5 * time.Second}
	go func() {
		defer close(c.In)
		for {
			data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m Message
			_ = json.Unmarshal(data, &m)
			c.In <- m
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return c
}

// wsConn is a WebSocket as an lsp.Conn.
type wsConn struct{ conn *websocket.Conn }

func (c wsConn) ReadMessage() ([]byte, error) {
	_, data, err := c.conn.ReadMessage()
	return data, err
}

func (c wsConn) WriteMessage(data []byte) error {
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c wsConn) Close() error { return c.conn.Close() }

// Send sends v as one message, as it is: a test's malformed request, say. Its answer comes on In.
func (c *Client) Send(v any) { c.send(v) }

func (c *Client) send(v any) {
	data, _ := json.Marshal(v)
	if err := c.conn.WriteMessage(data); err != nil {
		c.t.Fatal(err)
	}
}

// Notify sends a notification.
func (c *Client) Notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Call sends a request and waits for its response (notifications in between are dropped). A session that closes
// first answers an error "closed".
func (c *Client) Call(method string, params any) Message {
	c.t.Helper()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	timeout := time.After(c.Timeout)
	for {
		select {
		case m, ok := <-c.In:
			if !ok {
				return Message{Error: &Error{Message: "closed"}}
			}
			if string(m.ID) == id {
				return m
			}
		case <-timeout:
			c.t.Fatalf("no answer to %s", method)
		}
	}
}

// Initialize sends initialize with options as its initializationOptions.
func (c *Client) Initialize(options any) Message {
	return c.Call("initialize", map[string]any{"initializationOptions": options})
}

// Open opens a document.
func (c *Client) Open(uri, languageID, text string) {
	c.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": languageID, "version": 1, "text": text}})
}

// Diagnostics waits for the next diagnostics of uri.
func (c *Client) Diagnostics(uri string) []lsp.Diagnostic {
	c.t.Helper()
	timeout := time.After(c.Timeout)
	for {
		select {
		case m, ok := <-c.In:
			if !ok {
				c.t.Fatal("closed")
			}
			if m.Method != "textDocument/publishDiagnostics" {
				continue
			}
			var p lsp.PublishDiagnosticsParams
			_ = json.Unmarshal(m.Params, &p)
			if p.URI == uri {
				return p.Diagnostics
			}
		case <-timeout:
			c.t.Fatalf("no diagnostics for %s", uri)
		}
	}
}

// WaitClosed waits for the server to end the session.
func (c *Client) WaitClosed(what string) {
	c.t.Helper()
	timeout := time.After(c.Timeout)
	for {
		select {
		case _, ok := <-c.In:
			if !ok {
				return
			}
		case <-timeout:
			c.t.Fatalf("%s: the session stayed open", what)
		}
	}
}

// Pos is the params of a request at a position.
func Pos(uri string, line, char int) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": line, "character": char}}
}
