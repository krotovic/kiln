package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"adventurers-life-online/server/internal/engine/language"
	"adventurers-life-online/server/internal/game/scripting"
	"adventurers-life-online/server/pkg/auth"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const utilScript = `// Shouts a word.
export function shout(s: string): string { return s.toUpperCase() + "!" }
`

const greeterScript = `import { Behaviour, Context, log } from "game"
import { shout } from "script/util"
export default class Greeter implements Behaviour {
  greeting = "hi"
  onStart(ctx: Context): void { log(shout(this.greeting) + ctx.zone) }
}
`

type fixture struct {
	t       *testing.T
	url     string
	tokens  *auth.TokenManager
	library *scripting.Library
	src     scripting.Static
	revoked atomic.Bool
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, tokens: auth.NewTokenManager("secret", time.Hour),
		src: scripting.Static{"script/util": utilScript, "script/greeter": greeterScript}}
	f.library = scripting.NewLibrary(f.src, nil)
	if err := f.library.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(f.library, f.tokens, func(*auth.SessionClaims) bool { return f.revoked.Load() }, []*language.HostModule{scripting.GameModule()})
	srv.debounce, srv.watch = 10*time.Millisecond, 20*time.Millisecond
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	f.url = "ws" + strings.TrimPrefix(hs.URL, "http") + "/lsp"
	return f
}

func (f *fixture) token(role string, perms ...string) string {
	tok, err := f.tokens.GenerateSessionToken(uuid.New(), role+"@example.com", role, perms)
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

// client is a test's end of a session.
type client struct {
	t      *testing.T
	conn   *websocket.Conn
	nextID int
	in     chan message
}

func (f *fixture) dial() *client {
	conn, _, err := websocket.DefaultDialer.Dial(f.url, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	c := &client{t: f.t, conn: conn, in: make(chan message, 64)}
	go func() {
		defer close(c.in)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m message
			_ = json.Unmarshal(data, &m)
			c.in <- m
		}
	}()
	f.t.Cleanup(func() { _ = conn.Close() })
	return c
}

func (c *client) send(v any) {
	data, _ := json.Marshal(v)
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// call sends a request and waits for its response (notifications in between are dropped).
func (c *client) call(method string, params any) message {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.in:
			if !ok {
				return message{Error: &rpcError{Message: "closed"}}
			}
			if string(m.ID) == strings.TrimSpace(string(mustJSON(id))) {
				return m
			}
		case <-timeout:
			c.t.Fatalf("no answer to %s", method)
		}
	}
}

// diagnostics waits for the diagnostics of uri.
func (c *client) diagnostics(uri string) []Diagnostic {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.in:
			if !ok {
				c.t.Fatal("closed")
			}
			if m.Method != "textDocument/publishDiagnostics" {
				continue
			}
			var p PublishDiagnosticsParams
			_ = json.Unmarshal(m.Params, &p)
			if p.URI == uri {
				return p.Diagnostics
			}
		case <-timeout:
			c.t.Fatalf("no diagnostics for %s", uri)
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (c *client) initialize(token string) message {
	return c.call("initialize", map[string]any{"initializationOptions": map[string]any{"token": token}})
}

func (c *client) open(id, text string) {
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": "alo:/" + id, "languageId": "alo-script", "version": 1, "text": text}})
}

func pos(uri string, line, char int) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": line, "character": char}}
}

func TestSessionsNeedAGameMastersToken(t *testing.T) {
	f := newFixture(t)
	c := f.dial()
	if m := c.call("textDocument/hover", pos("alo:/script/util", 0, 0)); m.Error == nil || m.Error.Code != codeServerNotInitialized {
		t.Fatalf("a request before initialize: %+v", m)
	}
	if m := c.initialize(""); m.Error == nil {
		t.Fatal("initialize without a token was taken")
	}
	c = f.dial()
	if m := c.initialize(f.token("player")); m.Error == nil || !strings.Contains(m.Error.Message, "world.edit") {
		t.Fatalf("a player's token: %+v", m)
	}
	c = f.dial()
	m := c.initialize(f.token("gm", auth.PermWorldEdit))
	if m.Error != nil || !strings.Contains(string(m.Result), `"hoverProvider":true`) {
		t.Fatalf("a game master's token: %s %+v", m.Result, m.Error)
	}
	// A token revoked later ends the session
	f.revoked.Store(true)
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.in:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("the session of a revoked token went on")
		}
	}
}

func TestDiagnosticsFollowOpenDocumentsAndWhatImportsThem(t *testing.T) {
	f := newFixture(t)
	c := f.dial()
	c.initialize(f.token("gm", auth.PermWorldEdit))
	c.open("script/greeter", greeterScript)
	if d := c.diagnostics("alo:/script/greeter"); len(d) != 0 {
		t.Fatalf("a good script has diagnostics: %+v", d)
	}
	// Breaking the import shows in the open script that uses it
	c.open("script/util", "export function shout(n: number): number { return n }\n")
	got := map[string][]Diagnostic{}
	for len(got) < 2 {
		m := <-c.in
		if m.Method == "textDocument/publishDiagnostics" {
			var p PublishDiagnosticsParams
			_ = json.Unmarshal(m.Params, &p)
			got[p.URI] = p.Diagnostics
		}
	}
	d := got["alo:/script/greeter"]
	if len(d) != 1 || !strings.Contains(d[0].Message, "argument 1 of shout") || d[0].Range.Start != (Position{Line: 4, Character: 47}) {
		t.Fatalf("greeter's diagnostics: %+v", d)
	}
	// A type error as the game master types, with its range
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": "alo:/script/util", "version": 2},
		"contentChanges": []map[string]any{{"text": "export function shout(s: string): string { return nope }\n"}}})
	for {
		d := c.diagnostics("alo:/script/util")
		if len(d) == 1 {
			if d[0].Message != "unknown name nope" || d[0].Range != (Range{Start: Position{0, 50}, End: Position{0, 54}}) {
				t.Fatalf("util's diagnostics: %+v", d)
			}
			break
		}
	}
	c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": "alo:/script/util"}})
	if d := c.diagnostics("alo:/script/util"); len(d) != 0 {
		t.Fatalf("a closed document keeps diagnostics: %+v", d)
	}
}

func TestLanguageFeatures(t *testing.T) {
	f := newFixture(t)
	c := f.dial()
	c.initialize(f.token("gm", auth.PermWorldEdit))
	c.open("script/greeter", greeterScript)
	uri := "alo:/script/greeter"

	m := c.call("textDocument/hover", pos(uri, 4, 37))
	if !strings.Contains(string(m.Result), "function shout(s: string): string") || !strings.Contains(string(m.Result), "Shouts a word.") {
		t.Fatalf("hover: %s", m.Result)
	}
	m = c.call("textDocument/definition", pos(uri, 4, 37))
	var locs []Location
	_ = json.Unmarshal(m.Result, &locs)
	if len(locs) != 1 || locs[0].URI != "alo:/script/util" || locs[0].Range.Start != (Position{Line: 1, Character: 16}) {
		t.Fatalf("definition: %s", m.Result)
	}
	// ctx. offers the Context's members
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 2},
		"contentChanges": []map[string]any{{"text": strings.Replace(greeterScript, "ctx.zone", "ctx.", 1)}}})
	m = c.call("textDocument/completion", pos(uri, 4, 63))
	for _, want := range []string{`"label":"zone"`, `"label":"object"`, `"label":"hour"`} {
		if !strings.Contains(string(m.Result), want) {
			t.Fatalf("completion lacks %s: %s", want, m.Result)
		}
	}
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 3},
		"contentChanges": []map[string]any{{"text": greeterScript}}})
	m = c.call("textDocument/signatureHelp", pos(uri, 4, 43)) // just inside shout(
	if !strings.Contains(string(m.Result), `"label":"shout(s: string): string"`) {
		t.Fatalf("signature help: %s", m.Result)
	}
	m = c.call("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": uri}})
	if !strings.Contains(string(m.Result), `"name":"Greeter"`) || !strings.Contains(string(m.Result), `"name":"onStart"`) {
		t.Fatalf("symbols: %s", m.Result)
	}
	// References and renames reach the stored script that declares shout
	m = c.call("textDocument/references", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": 4, "character": 37}, "context": map[string]any{"includeDeclaration": true}})
	_ = json.Unmarshal(m.Result, &locs)
	if len(locs) != 3 {
		t.Fatalf("references: %s", m.Result)
	}
	m = c.call("textDocument/prepareRename", pos(uri, 4, 37))
	if m.Error != nil {
		t.Fatalf("prepareRename: %+v", m.Error)
	}
	if m = c.call("textDocument/prepareRename", pos(uri, 0, 30)); m.Error == nil {
		t.Fatal("a host module's name could be renamed")
	}
	if m = c.call("textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": 4, "character": 37}, "newName": "let"}); m.Error == nil {
		t.Fatal("renamed to a keyword")
	}
	m = c.call("textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": 4, "character": 37}, "newName": "yell"})
	var edit WorkspaceEdit
	_ = json.Unmarshal(m.Result, &edit)
	if len(edit.Changes[uri]) != 2 || len(edit.Changes["alo:/script/util"]) != 1 {
		t.Fatalf("rename: %s", m.Result)
	}
	// Formatting replaces the document; formatted text needs nothing
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 4},
		"contentChanges": []map[string]any{{"text": "export const   X=1\n"}}})
	m = c.call("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": uri}})
	if !strings.Contains(string(m.Result), `"newText":"export const X = 1\n"`) {
		t.Fatalf("formatting: %s", m.Result)
	}
	if m = c.call("textDocument/hover", pos("alo:/elsewhere", 0, 0)); m.Error == nil {
		t.Fatal("a URI outside the scripts was read")
	}
	if m = c.call("workspace/symbol", map[string]any{}); m.Error == nil || m.Error.Code != codeMethodNotFound {
		t.Fatalf("an unknown method: %+v", m.Error)
	}
}

func TestTooManyDocuments(t *testing.T) {
	f := newFixture(t)
	c := f.dial()
	c.initialize(f.token("gm", auth.PermWorldEdit))
	name := func(i int) string { return fmt.Sprintf("script/doc-%d", i) }
	for i := range maxDocuments + 1 {
		c.open(name(i), "export const X = 1\n")
	}
	// The first 64 are open; the last isn't (nor stored), so nothing can be read of it
	if m := c.call("textDocument/hover", pos("alo:/"+name(maxDocuments-1), 0, 13)); m.Error != nil || !strings.Contains(string(m.Result), "const X") {
		t.Fatalf("the 64th document: %s %+v", m.Result, m.Error)
	}
	if m := c.call("textDocument/hover", pos("alo:/"+name(maxDocuments), 0, 13)); m.Error == nil {
		t.Fatalf("the 65th document was opened: %s", m.Result)
	}
}

func TestPositions(t *testing.T) {
	tx := newText("let é = \"😀x\"\nnext")
	// é is 2 bytes and 1 UTF-16 unit; 😀 is 4 bytes and 2 units
	if p := tx.position(strings.Index(tx.src, "x\"")); p != (Position{Line: 0, Character: 11}) {
		t.Fatalf("position = %+v", p)
	}
	if off := tx.offset(Position{Line: 0, Character: 11}); tx.src[off] != 'x' {
		t.Fatalf("offset = %d", off)
	}
	if off := tx.offset(Position{Line: 1, Character: 99}); off != len(tx.src) {
		t.Fatalf("past the end of a line = %d", off)
	}
	if start, end := tx.word(4); tx.src[start:end] != "é" && end-start != 2 {
		t.Fatalf("word = %q", tx.src[start:end])
	}
}

func TestSessionsEndWithoutInitializeOrOnExpiry(t *testing.T) {
	f := newFixture(t)
	srv := NewServer(f.library, f.tokens, nil, []*language.HostModule{scripting.GameModule()})
	srv.initTimeout, srv.watch = 100*time.Millisecond, 20*time.Millisecond
	hs := httptest.NewServer(srv)
	defer hs.Close()
	f.url = "ws" + strings.TrimPrefix(hs.URL, "http") + "/lsp"
	waitClosed := func(c *client, what string) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case _, ok := <-c.in:
				if !ok {
					return
				}
			case <-timeout:
				t.Fatalf("%s: the session stayed open", what)
			}
		}
	}
	waitClosed(f.dial(), "no initialize")
	short := auth.NewTokenManager("secret", 2*time.Second) // JWT expiry counts whole seconds
	tok, _ := short.GenerateSessionToken(uuid.New(), "gm@example.com", "gm", []string{auth.PermWorldEdit})
	c := f.dial()
	if m := c.initialize(tok); m.Error != nil {
		t.Fatal(m.Error)
	}
	waitClosed(c, "an expired token")
}

func TestMalformedChangesDontCrash(t *testing.T) {
	f := newFixture(t)
	c := f.dial()
	c.initialize(f.token("gm", auth.PermWorldEdit))
	c.open("script/util", utilScript)
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": "alo:/script/util", "version": 2},
		"contentChanges": []map[string]any{{"range": map[string]any{"start": map[string]any{"line": 1, "character": 0}, "end": map[string]any{"line": 0, "character": 0}}, "text": ""}}})
	if m := c.call("textDocument/hover", pos("alo:/script/util", 1, 20)); m.Error != nil || !strings.Contains(string(m.Result), "shout") {
		t.Fatalf("the session after a bad change: %s %+v", m.Result, m.Error)
	}
}

func TestRenamesThatWouldBreakAHierarchyAreRefused(t *testing.T) {
	f := newFixture(t)
	c := f.dial()
	c.initialize(f.token("gm", auth.PermWorldEdit))
	// greeter's onStart implements the host's Behaviour.onStart, which the host calls by name
	c.open("script/greeter", greeterScript)
	m := c.call("textDocument/prepareRename", pos("alo:/script/greeter", 4, 4))
	if m.Error == nil || !strings.Contains(m.Error.Message, "it implements Behaviour.onStart") {
		t.Fatalf("prepareRename of onStart: %s %+v", m.Result, m.Error)
	}
	m = c.call("textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": "alo:/script/greeter"}, "position": map[string]any{"line": 4, "character": 4}, "newName": "begin"})
	if m.Error == nil {
		t.Fatalf("renamed onStart: %s", m.Result)
	}
}

func TestHostModulesAreReadOnlyDefinitionFiles(t *testing.T) {
	f := newFixture(t)
	c := f.dial()
	c.initialize(f.token("gm", auth.PermWorldEdit))
	c.open("script/greeter", greeterScript)
	// Behaviour's definition is in the game module's definition file
	m := c.call("textDocument/definition", pos("alo:/script/greeter", 2, 45))
	var locs []Location
	_ = json.Unmarshal(m.Result, &locs)
	if len(locs) != 1 || locs[0].URI != "alo:/host/game" {
		t.Fatalf("definition of Behaviour: %s %+v", m.Result, m.Error)
	}
	// The editor reads it, and it answers hover and its outline like a script
	m = c.call("alo/source", map[string]any{"uri": "alo:/host/game"})
	if !strings.Contains(string(m.Result), "export interface Behaviour") || !strings.Contains(string(m.Result), `"readOnly":true`) {
		t.Fatalf("alo/source: %s %+v", m.Result, m.Error)
	}
	line := locs[0].Range.Start.Line
	m = c.call("textDocument/hover", pos("alo:/host/game", line, locs[0].Range.Start.Character+2))
	if !strings.Contains(string(m.Result), "interface Behaviour") {
		t.Fatalf("hover in the definition file: %s", m.Result)
	}
	m = c.call("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": "alo:/host/game"}})
	if !strings.Contains(string(m.Result), `"name":"Context"`) {
		t.Fatalf("outline of the definition file: %s", m.Result)
	}
	// It can't be changed, formatted or have its names renamed
	if m = c.call("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": "alo:/host/game"}}); m.Error == nil {
		t.Fatal("the definition file was formatted")
	}
	if m = c.call("textDocument/prepareRename", pos("alo:/script/greeter", 2, 45)); m.Error == nil {
		t.Fatal("Behaviour could be renamed")
	}
	if m = c.call("alo/source", map[string]any{"uri": "alo:/host/nope"}); m.Error == nil {
		t.Fatal("an unknown host module was read")
	}
}
