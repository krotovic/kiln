package lsp_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krotovic/kiln/language"
	"github.com/krotovic/kiln/lsp"
	"github.com/krotovic/kiln/lsp/lsptest"
)

const (
	codeInvalidRequest       = -32600
	codeMethodNotFound       = -32601
	codeServerNotInitialized = -32002
	maxDocuments             = 64
)

const utilScript = `// Shouts a word.
export function shout(s: string): string { return s.toUpperCase() + "!" }
`

const greeterScript = `import { Behaviour, Context, log } from "host"
import { shout } from "script/util"
export default class Greeter implements Behaviour {
  greeting = "hi"
  onStart(ctx: Context): void { log(shout(this.greeting) + ctx.zone) }
}
`

// The host module scripts are checked against
var host = &language.HostModule{Name: "host", Source: `// The test's host.
export interface Behaviour {
  // onStart runs once the object is made.
  onStart?(ctx: Context): void
  // onUpdate runs every tick; dt is the seconds since the last one.
  onUpdate?(ctx: Context, dt: number): void
}

export native class Thing {
  readonly name: string
}

export native class Context {
  readonly object: Thing
  readonly zone: string
  // hour is the hour of the day, 0 to 24
  hour(): number
}

export native function log(message: string): void
`, Natives: map[string]language.NativeFunc{
	"Context.object": none, "Context.zone": none, "Context.hour": none, "Thing.name": none, "log": none,
}}

// none is every native's code: sessions only analyse scripts, never run them.
func none(*language.Call, language.Value, []language.Value) (language.Value, error) {
	return language.Value{}, nil
}

func TestTheHostModuleChecks(t *testing.T) {
	if err := host.Check(); err != nil {
		t.Fatal(err)
	}
}

// scripts are the stored scripts: a map, its version bumped by hand.
type scripts struct {
	src     map[string]string
	version atomic.Uint64
}

func (s *scripts) Load(id string) (string, error) {
	if src, ok := s.src[id]; ok {
		return src, nil
	}
	return "", fmt.Errorf("no script %s", id)
}

func (s *scripts) List() []string {
	var out []string
	for id := range s.src {
		out = append(out, id)
	}
	return out
}

func (s *scripts) Version() uint64 { return s.version.Load() }

// grant is a session's, failing once revoked.
type grant struct{ revoked *atomic.Bool }

func (g grant) Check() error {
	if g.revoked.Load() {
		return errors.New("revoked")
	}
	return nil
}

type fixture struct {
	t       *testing.T
	url     string
	revoked atomic.Bool
}

func newFixture(t *testing.T, change func(*lsp.Options)) *fixture {
	f := &fixture{t: t}
	opts := lsp.Options{
		Scripts: &scripts{src: map[string]string{"script/util": utilScript, "script/greeter": greeterScript}},
		Hosts:   []*language.HostModule{host},
		Authorize: func(raw json.RawMessage) (lsp.Grant, error) {
			var o struct{ Token string }
			_ = json.Unmarshal(raw, &o)
			if o.Token == "nope" {
				return nil, errors.New("not you")
			}
			return grant{&f.revoked}, nil
		},
		ValidModule: func(id string) bool { return strings.HasPrefix(id, "script/") },
		Scheme:      "alo",
		Debounce:    10 * time.Millisecond,
		Watch:       20 * time.Millisecond,
	}
	if change != nil {
		change(&opts)
	}
	hs := httptest.NewServer(lsp.NewServer(opts))
	t.Cleanup(hs.Close)
	f.url = "ws" + strings.TrimPrefix(hs.URL, "http") + "/lsp"
	return f
}

func (f *fixture) dial() *lsptest.Client { return lsptest.Dial(f.t, f.url) }

func TestSessionsAreAuthorizedByTheHost(t *testing.T) {
	f := newFixture(t, nil)
	c := f.dial()
	if m := c.Call("textDocument/hover", lsptest.Pos("alo:/script/util", 0, 0)); m.Error == nil || m.Error.Code != codeServerNotInitialized {
		t.Fatalf("a request before initialize: %+v", m)
	}
	if m := c.Initialize(map[string]any{"token": "nope"}); m.Error == nil || m.Error.Message != "not you" {
		t.Fatalf("an unauthorized session: %+v", m)
	}
	c.WaitClosed("an unauthorized session")
	c = f.dial()
	m := c.Initialize(map[string]any{"token": "me"})
	if m.Error != nil || !strings.Contains(string(m.Result), `"hoverProvider":true`) || !strings.Contains(string(m.Result), `"kiln-lsp"`) {
		t.Fatalf("an authorized session: %s %+v", m.Result, m.Error)
	}
	// A grant failing later ends the session
	f.revoked.Store(true)
	c.WaitClosed("a revoked grant")
}

func TestRequestsNeedANumberOrAStringID(t *testing.T) {
	f := newFixture(t, nil)
	c := f.dial()
	// An initialize with a null id doesn't initialize, even with a good token
	c.Send(map[string]any{"jsonrpc": "2.0", "id": nil, "method": "initialize",
		"params": map[string]any{"initializationOptions": map[string]any{"token": "me"}}})
	select {
	case m := <-c.In:
		if m.Error == nil || m.Error.Code != codeInvalidRequest || string(m.ID) != "null" {
			t.Fatalf("initialize with a null id: %+v", m)
		}
	case <-time.After(c.Timeout):
		t.Fatal("no answer to initialize with a null id")
	}
	if m := c.Call("textDocument/hover", lsptest.Pos("alo:/script/util", 0, 0)); m.Error == nil || m.Error.Code != codeServerNotInitialized {
		t.Fatalf("a request after it: %+v", m)
	}
	// Nor does an id of another type, once initialized
	if m := c.Initialize(map[string]any{"token": "me"}); m.Error != nil {
		t.Fatalf("initialize: %+v", m.Error)
	}
	c.Send(map[string]any{"jsonrpc": "2.0", "id": map[string]any{"x": 1}, "method": "textDocument/hover",
		"params": lsptest.Pos("alo:/script/util", 0, 0)})
	select {
	case m := <-c.In:
		if m.Error == nil || m.Error.Code != codeInvalidRequest {
			t.Fatalf("an object id: %+v", m)
		}
	case <-time.After(c.Timeout):
		t.Fatal("no answer to a request with an object id")
	}
}

func TestSessionsEndWithoutInitialize(t *testing.T) {
	f := newFixture(t, func(o *lsp.Options) { o.InitTimeout = 100 * time.Millisecond })
	f.dial().WaitClosed("no initialize")
}

func TestDiagnosticsFollowOpenDocumentsAndWhatImportsThem(t *testing.T) {
	f := newFixture(t, nil)
	c := f.dial()
	c.Initialize(nil)
	c.Open("alo:/script/greeter", "alo", greeterScript)
	if d := c.Diagnostics("alo:/script/greeter"); len(d) != 0 {
		t.Fatalf("a good script has diagnostics: %+v", d)
	}
	// Breaking the import shows in the open script that uses it
	c.Open("alo:/script/util", "alo", "export function shout(n: number): number { return n }\n")
	got := map[string][]lsp.Diagnostic{}
	for len(got) < 2 {
		m := <-c.In
		if m.Method == "textDocument/publishDiagnostics" {
			var p lsp.PublishDiagnosticsParams
			_ = json.Unmarshal(m.Params, &p)
			got[p.URI] = p.Diagnostics
		}
	}
	d := got["alo:/script/greeter"]
	if len(d) != 1 || !strings.Contains(d[0].Message, "argument 1 of shout") || d[0].Range.Start != (lsp.Position{Line: 4, Character: 47}) {
		t.Fatalf("greeter's diagnostics: %+v", d)
	}
	// A type error as one types, with its range
	c.Notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": "alo:/script/util", "version": 2},
		"contentChanges": []map[string]any{{"text": "export function shout(s: string): string { return nope }\n"}}})
	for {
		d := c.Diagnostics("alo:/script/util")
		if len(d) == 1 {
			if d[0].Message != "unknown name nope" || d[0].Range != (lsp.Range{Start: lsp.Position{Line: 0, Character: 50}, End: lsp.Position{Line: 0, Character: 54}}) {
				t.Fatalf("util's diagnostics: %+v", d)
			}
			break
		}
	}
	c.Notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": "alo:/script/util"}})
	if d := c.Diagnostics("alo:/script/util"); len(d) != 0 {
		t.Fatalf("a closed document keeps diagnostics: %+v", d)
	}
}

func TestLanguageFeatures(t *testing.T) {
	f := newFixture(t, nil)
	c := f.dial()
	c.Initialize(nil)
	c.Open("alo:/script/greeter", "alo", greeterScript)
	uri := "alo:/script/greeter"

	m := c.Call("textDocument/hover", lsptest.Pos(uri, 4, 37))
	if !strings.Contains(string(m.Result), "function shout(s: string): string") || !strings.Contains(string(m.Result), "Shouts a word.") {
		t.Fatalf("hover: %s", m.Result)
	}
	m = c.Call("textDocument/definition", lsptest.Pos(uri, 4, 37))
	var locs []lsp.Location
	_ = json.Unmarshal(m.Result, &locs)
	if len(locs) != 1 || locs[0].URI != "alo:/script/util" || locs[0].Range.Start != (lsp.Position{Line: 1, Character: 16}) {
		t.Fatalf("definition: %s", m.Result)
	}
	// ctx. offers the Context's members
	c.Notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 2},
		"contentChanges": []map[string]any{{"text": strings.Replace(greeterScript, "ctx.zone", "ctx.", 1)}}})
	m = c.Call("textDocument/completion", lsptest.Pos(uri, 4, 63))
	for _, want := range []string{`"label":"zone"`, `"label":"object"`, `"label":"hour"`} {
		if !strings.Contains(string(m.Result), want) {
			t.Fatalf("completion lacks %s: %s", want, m.Result)
		}
	}
	c.Notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 3},
		"contentChanges": []map[string]any{{"text": greeterScript}}})
	m = c.Call("textDocument/signatureHelp", lsptest.Pos(uri, 4, 43)) // just inside shout(
	if !strings.Contains(string(m.Result), `"label":"shout(s: string): string"`) {
		t.Fatalf("signature help: %s", m.Result)
	}
	m = c.Call("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": uri}})
	if !strings.Contains(string(m.Result), `"name":"Greeter"`) || !strings.Contains(string(m.Result), `"name":"onStart"`) {
		t.Fatalf("symbols: %s", m.Result)
	}
	// References and renames reach the stored script that declares shout
	m = c.Call("textDocument/references", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": 4, "character": 37}, "context": map[string]any{"includeDeclaration": true}})
	_ = json.Unmarshal(m.Result, &locs)
	if len(locs) != 3 {
		t.Fatalf("references: %s", m.Result)
	}
	m = c.Call("textDocument/prepareRename", lsptest.Pos(uri, 4, 37))
	if m.Error != nil {
		t.Fatalf("prepareRename: %+v", m.Error)
	}
	if m = c.Call("textDocument/prepareRename", lsptest.Pos(uri, 0, 30)); m.Error == nil {
		t.Fatal("a host module's name could be renamed")
	}
	if m = c.Call("textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": 4, "character": 37}, "newName": "let"}); m.Error == nil {
		t.Fatal("renamed to a keyword")
	}
	m = c.Call("textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": 4, "character": 37}, "newName": "yell"})
	var edit lsp.WorkspaceEdit
	_ = json.Unmarshal(m.Result, &edit)
	if len(edit.Changes[uri]) != 2 || len(edit.Changes["alo:/script/util"]) != 1 {
		t.Fatalf("rename: %s", m.Result)
	}
	// Formatting replaces the document; formatted text needs nothing
	c.Notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 4},
		"contentChanges": []map[string]any{{"text": "export const   X=1\n"}}})
	m = c.Call("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": uri}})
	if !strings.Contains(string(m.Result), `"newText":"export const X = 1\n"`) {
		t.Fatalf("formatting: %s", m.Result)
	}
	if m = c.Call("textDocument/hover", lsptest.Pos("alo:/elsewhere", 0, 0)); m.Error == nil {
		t.Fatal("a URI outside the scripts was read")
	}
	if m = c.Call("workspace/symbol", map[string]any{}); m.Error == nil || m.Error.Code != codeMethodNotFound {
		t.Fatalf("an unknown method: %+v", m.Error)
	}
}

func TestTooManyDocuments(t *testing.T) {
	f := newFixture(t, nil)
	c := f.dial()
	c.Initialize(nil)
	name := func(i int) string { return fmt.Sprintf("script/doc-%d", i) }
	for i := range maxDocuments + 1 {
		c.Open("alo:/"+name(i), "alo", "export const X = 1\n")
	}
	// The first 64 are open; the last isn't (nor stored), so nothing can be read of it
	if m := c.Call("textDocument/hover", lsptest.Pos("alo:/"+name(maxDocuments-1), 0, 13)); m.Error != nil || !strings.Contains(string(m.Result), "const X") {
		t.Fatalf("the 64th document: %s %+v", m.Result, m.Error)
	}
	if m := c.Call("textDocument/hover", lsptest.Pos("alo:/"+name(maxDocuments), 0, 13)); m.Error == nil {
		t.Fatalf("the 65th document was opened: %s", m.Result)
	}
}

func TestMalformedChangesDontCrash(t *testing.T) {
	f := newFixture(t, nil)
	c := f.dial()
	c.Initialize(nil)
	c.Open("alo:/script/util", "alo", utilScript)
	c.Notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": "alo:/script/util", "version": 2},
		"contentChanges": []map[string]any{{"range": map[string]any{"start": map[string]any{"line": 1, "character": 0}, "end": map[string]any{"line": 0, "character": 0}}, "text": ""}}})
	if m := c.Call("textDocument/hover", lsptest.Pos("alo:/script/util", 1, 20)); m.Error != nil || !strings.Contains(string(m.Result), "shout") {
		t.Fatalf("the session after a bad change: %s %+v", m.Result, m.Error)
	}
}

func TestRenamesThatWouldBreakAHierarchyAreRefused(t *testing.T) {
	f := newFixture(t, nil)
	c := f.dial()
	c.Initialize(nil)
	// greeter's onStart implements the host's Behaviour.onStart, which the host calls by name
	c.Open("alo:/script/greeter", "alo", greeterScript)
	m := c.Call("textDocument/prepareRename", lsptest.Pos("alo:/script/greeter", 4, 4))
	if m.Error == nil || !strings.Contains(m.Error.Message, "it implements Behaviour.onStart") {
		t.Fatalf("prepareRename of onStart: %s %+v", m.Result, m.Error)
	}
	m = c.Call("textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": "alo:/script/greeter"}, "position": map[string]any{"line": 4, "character": 4}, "newName": "begin"})
	if m.Error == nil {
		t.Fatalf("renamed onStart: %s", m.Result)
	}
}

func TestHostModulesAreReadOnlyDefinitionFiles(t *testing.T) {
	f := newFixture(t, nil)
	c := f.dial()
	c.Initialize(nil)
	c.Open("alo:/script/greeter", "alo", greeterScript)
	// Behaviour's definition is in the host module's definition file
	m := c.Call("textDocument/definition", lsptest.Pos("alo:/script/greeter", 2, 45))
	var locs []lsp.Location
	_ = json.Unmarshal(m.Result, &locs)
	if len(locs) != 1 || locs[0].URI != "alo:/host/host" {
		t.Fatalf("definition of Behaviour: %s %+v", m.Result, m.Error)
	}
	// The editor reads it, and it answers hover and its outline like a script
	m = c.Call("alo/source", map[string]any{"uri": "alo:/host/host"})
	if !strings.Contains(string(m.Result), "export interface Behaviour") || !strings.Contains(string(m.Result), `"readOnly":true`) {
		t.Fatalf("alo/source: %s %+v", m.Result, m.Error)
	}
	line := locs[0].Range.Start.Line
	m = c.Call("textDocument/hover", lsptest.Pos("alo:/host/host", line, locs[0].Range.Start.Character+2))
	if !strings.Contains(string(m.Result), "interface Behaviour") {
		t.Fatalf("hover in the definition file: %s", m.Result)
	}
	m = c.Call("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": "alo:/host/host"}})
	if !strings.Contains(string(m.Result), `"name":"Context"`) {
		t.Fatalf("outline of the definition file: %s", m.Result)
	}
	// It can't be changed, formatted or have its names renamed
	if m = c.Call("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": "alo:/host/host"}}); m.Error == nil {
		t.Fatal("the definition file was formatted")
	}
	if m = c.Call("textDocument/prepareRename", lsptest.Pos("alo:/script/greeter", 2, 45)); m.Error == nil {
		t.Fatal("Behaviour could be renamed")
	}
	if m = c.Call("alo/source", map[string]any{"uri": "alo:/host/nope"}); m.Error == nil {
		t.Fatal("an unknown host module was read")
	}
}
