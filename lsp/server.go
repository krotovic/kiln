// Package lsp is a language server for the language: an editor talks the Language Server Protocol to it over a
// WebSocket or a byte stream (a process's standard input and output). It reads the stored scripts from its host and overlays each session's open documents on them, and
// answers with the language package's analysis (diagnostics, hover, completion, definitions, signatures, symbols,
// references, renames, formatting). What a host knows (who may connect, which IDs name scripts, its host modules)
// comes in through Options.
package lsp

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/krotovic/kiln/language"
)

// Scripts are the stored scripts the server reads.
type Scripts interface {
	language.Source
	List() []string
	// Version changes whenever the stored scripts do, so sessions republish their diagnostics.
	Version() uint64
}

// Grant is what Authorize lets a session do. Check runs every Options.Watch and ends the session once it fails (a
// revoked or expired token, say).
type Grant interface {
	Check() error
}

const (
	hostPrefix = "host/"
	// maxDocuments is how many documents one session may have open
	maxDocuments = 64
	// maxMessage bounds one message, a whole document included
	maxMessage = 1 << 20
)

// Options is what a host tells the server. Only Scripts is needed.
type Options struct {
	Scripts Scripts
	// Hosts are the host modules scripts are checked against; their definition files open read-only at
	// <scheme>:/host/<name>.
	Hosts []*language.HostModule
	// Authorize checks initialize's initializationOptions; an error ends the session with its message. Nil
	// lets every session in.
	Authorize func(initializationOptions json.RawMessage) (Grant, error)
	// ValidModule reports whether an ID names a script (nil: any ID but empty).
	ValidModule func(id string) bool
	// Scheme starts every document's URI, <scheme>:/<module ID>, and names the server's own method,
	// <scheme>/source (default "kiln").
	Scheme string
	// LanguageID names the language in Markdown code blocks, as the editor registers it (default "kiln").
	LanguageID string
	// Name is the serverInfo name initialize answers (default "kiln-lsp").
	Name string
	// Upgrader upgrades HTTP requests to WebSockets in ServeHTTP (default: a websocket.Upgrader with its defaults,
	// which accepts only same-origin requests).
	Upgrader *websocket.Upgrader
	// Debounce is how long diagnostics wait for typing to pause (default 200 ms).
	Debounce time.Duration
	// Watch is how often sessions check for changed scripts and their grant (default 2 s).
	Watch time.Duration
	// InitTimeout is how long a session may wait before initialize (default 10 s).
	InitTimeout time.Duration
	// Notifications are the host's own notifications, by method: the server hands their params to them, before
	// initialize as after it, and answers nothing. A request with one of these methods is refused.
	Notifications map[string]func(params json.RawMessage)
}

// Server answers language server sessions, one per WebSocket.
type Server struct {
	opts      Options
	uriPrefix string
}

// NewServer makes a server with opts.
func NewServer(opts Options) *Server {
	if opts.Authorize == nil {
		opts.Authorize = func(json.RawMessage) (Grant, error) { return openGrant{}, nil }
	}
	if opts.ValidModule == nil {
		opts.ValidModule = func(id string) bool { return id != "" }
	}
	if opts.Scheme == "" {
		opts.Scheme = "kiln"
	}
	if opts.LanguageID == "" {
		opts.LanguageID = "kiln"
	}
	if opts.Name == "" {
		opts.Name = "kiln-lsp"
	}
	if opts.Upgrader == nil {
		opts.Upgrader = &websocket.Upgrader{}
	}
	if opts.Debounce == 0 {
		opts.Debounce = 200 * time.Millisecond
	}
	if opts.Watch == 0 {
		opts.Watch = 2 * time.Second
	}
	if opts.InitTimeout == 0 {
		opts.InitTimeout = 10 * time.Second
	}
	return &Server{opts: opts, uriPrefix: opts.Scheme + ":/"}
}

type openGrant struct{}

func (openGrant) Check() error { return nil }

// ServeHTTP upgrades the request to a WebSocket and serves one session on it until it closes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.opts.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.ServeConn(conn)
}

// ServeConn serves one session on a WebSocket until it closes.
func (s *Server) ServeConn(conn *websocket.Conn) {
	conn.SetReadLimit(maxMessage)
	s.Serve(wsConn{conn})
}

// ServeStream serves one session on a byte stream, framed as the Language Server Protocol's base protocol (each
// message after its Content-Length header), until in ends or the session does.
func (s *Server) ServeStream(in io.Reader, out io.Writer) {
	s.Serve(NewStreamConn(in, out))
}

// Serve serves one session on conn until it closes or the session ends, which closes it.
func (s *Server) Serve(conn Conn) {
	ss := &session{srv: s, conn: conn, docs: map[string]*document{}, done: make(chan struct{})}
	ss.run()
}

// document is an open document: its module and text.
type document struct {
	text    string
	version int
}

type session struct {
	srv     *Server
	conn    Conn
	writeMu sync.Mutex

	mu          sync.Mutex // guards what follows
	docs        map[string]*document
	grant       Grant // set by initialize
	timer       *time.Timer
	seenVersion uint64
	closed      bool
	done        chan struct{}
}

func (ss *session) run() {
	defer ss.close()
	go ss.watch()
	// A session that never initializes isn't held open
	initTimer := time.AfterFunc(ss.srv.opts.InitTimeout, func() {
		ss.mu.Lock()
		initialized := ss.grant != nil
		ss.mu.Unlock()
		if !initialized {
			ss.close()
		}
	})
	defer initTimer.Stop()
	// Messages are read apart, so a session that ends (its grant failing, or no initialize) returns at once even
	// on a transport whose Close can't interrupt a read
	in := make(chan []byte)
	go func() {
		defer close(in)
		for {
			data, err := ss.conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case in <- data:
			case <-ss.done:
				return
			}
		}
	}()
	for {
		var data []byte
		select {
		case <-ss.done:
			return
		case d, ok := <-in:
			if !ok {
				return
			}
			data = d
		}
		var msg message
		if err := json.Unmarshal(data, &msg); err != nil {
			ss.replyError(nil, codeParseError, "not JSON-RPC: "+err.Error())
			continue
		}
		if msg.Method == "" {
			continue // a response to something the server never asks
		}
		if !ss.safely(&msg) {
			return
		}
	}
}

// safely handles a message, turning a crash on a malformed request into an error answer rather than taking the
// server down.
func (ss *session) safely(msg *message) (keep bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[LSP] %s failed: %v", msg.Method, r)
			if len(msg.ID) > 0 {
				ss.replyError(msg.ID, codeInternalError, fmt.Sprintf("%s failed", msg.Method))
			}
			keep = true
		}
	}()
	return ss.handle(msg)
}

func (ss *session) close() {
	ss.mu.Lock()
	if !ss.closed {
		ss.closed = true
		close(ss.done)
		if ss.timer != nil {
			ss.timer.Stop()
		}
	}
	ss.mu.Unlock()
	_ = ss.conn.Close()
}

// watch republishes diagnostics when the stored scripts change, and ends the session when its grant fails.
func (ss *session) watch() {
	ticker := time.NewTicker(ss.srv.opts.Watch)
	defer ticker.Stop()
	for {
		select {
		case <-ss.done:
			return
		case <-ticker.C:
		}
		ss.mu.Lock()
		grant := ss.grant
		changed := grant != nil && ss.srv.opts.Scripts.Version() != ss.seenVersion
		ss.mu.Unlock()
		if grant != nil {
			if err := grant.Check(); err != nil {
				log.Printf("[LSP] Ending a session: %v", err)
				ss.close()
				return
			}
		}
		if changed {
			ss.schedule()
		}
	}
}

// handle answers one request or notification; false ends the session.
func (ss *session) handle(msg *message) bool {
	isRequest := len(msg.ID) > 0
	if notify, ok := ss.srv.opts.Notifications[msg.Method]; ok {
		if isRequest {
			ss.replyError(msg.ID, codeInvalidRequest, msg.Method+" is a notification")
		} else {
			notify(msg.Params)
		}
		return true
	}
	ss.mu.Lock()
	initialized := ss.grant != nil
	ss.mu.Unlock()
	if !initialized {
		switch {
		case msg.Method == "initialize" && isRequest:
			return ss.initialize(msg)
		case msg.Method == "exit":
			return false
		case isRequest:
			ss.replyError(msg.ID, codeServerNotInitialized, "initialize first")
		}
		return true
	}
	var result any
	var err error
	switch msg.Method {
	case "initialized", "$/cancelRequest", "$/setTrace", "textDocument/didSave":
		return true
	case "shutdown":
		result = nil
	case "exit":
		return false
	case "textDocument/didOpen":
		err = ss.didOpen(msg.Params)
	case "textDocument/didChange":
		err = ss.didChange(msg.Params)
	case "textDocument/didClose":
		err = ss.didClose(msg.Params)
	case "textDocument/hover":
		result, err = ss.hover(msg.Params)
	case "textDocument/completion":
		result, err = ss.completion(msg.Params)
	case "textDocument/definition":
		result, err = ss.definition(msg.Params)
	case "textDocument/signatureHelp":
		result, err = ss.signatureHelp(msg.Params)
	case "textDocument/documentSymbol":
		result, err = ss.documentSymbol(msg.Params)
	case "textDocument/references":
		result, err = ss.references(msg.Params)
	case "textDocument/prepareRename":
		result, err = ss.prepareRename(msg.Params)
	case "textDocument/rename":
		result, err = ss.rename(msg.Params)
	case "textDocument/formatting":
		result, err = ss.formatting(msg.Params)
	case ss.srv.opts.Scheme + "/source":
		result, err = ss.sourceOf(msg.Params)
	default:
		if isRequest {
			ss.replyError(msg.ID, codeMethodNotFound, "unknown method "+msg.Method)
		}
		return true
	}
	if !isRequest {
		if err != nil {
			log.Printf("[LSP] %s: %v", msg.Method, err)
		}
		return true
	}
	if err != nil {
		code := codeRequestFailed
		if e, ok := err.(*rpcError); ok {
			code = e.Code
		}
		ss.replyError(msg.ID, code, err.Error())
		return true
	}
	ss.reply(msg.ID, result)
	return true
}

func (ss *session) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("[LSP] Encoding a message: %v", err)
		return
	}
	ss.writeMu.Lock()
	defer ss.writeMu.Unlock()
	_ = ss.conn.WriteMessage(data)
}

func (ss *session) reply(id json.RawMessage, result any) {
	ss.send(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (ss *session) replyError(id json.RawMessage, code int, msg string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	ss.send(errorResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (ss *session) notify(method string, params any) {
	ss.send(notification{JSONRPC: "2.0", Method: method, Params: params})
}

func invalidParams(err error) error {
	return &rpcError{Code: codeInvalidParams, Message: "bad params: " + err.Error()}
}

// initialize asks the host to authorize the session and answers what the server can do. A session that fails it
// ends.
func (ss *session) initialize(msg *message) bool {
	var p InitializeParams
	_ = json.Unmarshal(msg.Params, &p)
	grant, err := ss.srv.opts.Authorize(p.InitializationOptions)
	if err == nil && grant == nil {
		err = fmt.Errorf("the session wasn't authorized")
	}
	if err != nil {
		ss.replyError(msg.ID, codeInvalidRequest, err.Error())
		return false
	}
	ss.mu.Lock()
	ss.grant = grant
	ss.mu.Unlock()
	ss.reply(msg.ID, map[string]any{
		"capabilities": map[string]any{
			"textDocumentSync":           map[string]any{"openClose": true, "change": 1},
			"hoverProvider":              true,
			"completionProvider":         map[string]any{"triggerCharacters": []string{".", `"`, "/"}},
			"definitionProvider":         true,
			"signatureHelpProvider":      map[string]any{"triggerCharacters": []string{"(", ","}},
			"documentSymbolProvider":     true,
			"referencesProvider":         true,
			"renameProvider":             map[string]any{"prepareProvider": true},
			"documentFormattingProvider": true,
		},
		"serverInfo": map[string]any{"name": ss.srv.opts.Name},
	})
	return true
}

// moduleOf turns a document URI into its module ID: a script's, or a host module's name.
func (ss *session) moduleOf(uri string) (string, error) {
	id, ok := strings.CutPrefix(uri, ss.srv.uriPrefix)
	if name, isHost := strings.CutPrefix(id, hostPrefix); ok && isHost && ss.host(name) != nil {
		return name, nil
	}
	if !ok || strings.HasPrefix(id, hostPrefix) || !ss.srv.opts.ValidModule(id) {
		return "", &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("%q is not a script (%s<module>)", uri, ss.srv.uriPrefix)}
	}
	return id, nil
}

// host finds a host module by name.
func (ss *session) host(name string) *language.HostModule {
	for _, h := range ss.srv.opts.Hosts {
		if h.Name == name {
			return h
		}
	}
	return nil
}

func (ss *session) uriOf(module string) string {
	if ss.host(module) != nil {
		return ss.srv.uriPrefix + hostPrefix + module
	}
	return ss.srv.uriPrefix + module
}

// readOnly refuses to change a host module's definition file.
func (ss *session) readOnly(module string) error {
	if ss.host(module) != nil {
		return &rpcError{Code: codeRequestFailed, Message: module + " is the host's definition file: it can't be changed"}
	}
	return nil
}

// sourceOf answers <scheme>/source {uri}: a document's text as the server reads it (how the editor opens a host
// module's definition file, which isn't an asset).
func (ss *session) sourceOf(raw json.RawMessage) (any, error) {
	var p TextDocumentIdentifier
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams(err)
	}
	id, err := ss.moduleOf(p.URI)
	if err != nil {
		return nil, err
	}
	t, err := ss.textOf(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": t.src, "readOnly": ss.host(id) != nil}, nil
}

// source is what the session's analyses read: its open documents over the stored scripts.
type source struct{ ss *session }

func (s source) Load(id string) (string, error) {
	s.ss.mu.Lock()
	d := s.ss.docs[id]
	s.ss.mu.Unlock()
	if d != nil {
		return d.text, nil
	}
	return s.ss.srv.opts.Scripts.Load(id)
}

func (s source) List() []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range s.ss.srv.opts.Scripts.List() {
		seen[id] = true
		out = append(out, id)
	}
	s.ss.mu.Lock()
	for id := range s.ss.docs {
		if !seen[id] {
			out = append(out, id)
		}
	}
	s.ss.mu.Unlock()
	sort.Strings(out)
	return out
}

func (ss *session) options() language.ScriptOptions {
	return language.ScriptOptions{Hosts: ss.srv.opts.Hosts}
}

// textOf is a module's text: the open document's, the stored script's, or a host module's definition file.
func (ss *session) textOf(module string) (*text, error) {
	if h := ss.host(module); h != nil {
		return newText(h.Source), nil
	}
	src, err := source{ss}.Load(module)
	if err != nil {
		return nil, &rpcError{Code: codeRequestFailed, Message: err.Error()}
	}
	return newText(src), nil
}

func (ss *session) didOpen(raw json.RawMessage) error {
	var p DidOpenParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return invalidParams(err)
	}
	id, err := ss.moduleOf(p.TextDocument.URI)
	if err != nil {
		return err
	}
	if err := ss.readOnly(id); err != nil {
		return err
	}
	ss.mu.Lock()
	if _, open := ss.docs[id]; !open && len(ss.docs) >= maxDocuments {
		ss.mu.Unlock()
		return fmt.Errorf("at most %d documents may be open", maxDocuments)
	}
	ss.docs[id] = &document{text: p.TextDocument.Text, version: p.TextDocument.Version}
	ss.mu.Unlock()
	ss.schedule()
	return nil
}

func (ss *session) didChange(raw json.RawMessage) error {
	var p DidChangeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return invalidParams(err)
	}
	id, err := ss.moduleOf(p.TextDocument.URI)
	if err != nil {
		return err
	}
	if err := ss.readOnly(id); err != nil {
		return err
	}
	ss.mu.Lock()
	d := ss.docs[id]
	if d != nil {
		for _, c := range p.ContentChanges {
			if c.Range == nil {
				d.text = c.Text
				continue
			}
			t := newText(d.text) // a ranged change, though the server asked for whole documents
			start, end := t.offset(c.Range.Start), t.offset(c.Range.End)
			if start > end {
				ss.mu.Unlock()
				return invalidParams(fmt.Errorf("a change's range ends before it starts"))
			}
			d.text = d.text[:start] + c.Text + d.text[end:]
		}
		d.version = p.TextDocument.Version
	}
	ss.mu.Unlock()
	if d == nil {
		return fmt.Errorf("%s isn't open", id)
	}
	ss.schedule()
	return nil
}

func (ss *session) didClose(raw json.RawMessage) error {
	var p DidCloseParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return invalidParams(err)
	}
	id, err := ss.moduleOf(p.TextDocument.URI)
	if err != nil {
		return err
	}
	ss.mu.Lock()
	delete(ss.docs, id)
	ss.mu.Unlock()
	ss.notify("textDocument/publishDiagnostics", PublishDiagnosticsParams{URI: ss.uriOf(id), Diagnostics: []Diagnostic{}})
	ss.schedule() // what imports it now reads the stored script
	return nil
}

// schedule publishes the open documents' diagnostics once typing pauses.
func (ss *session) schedule() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return
	}
	if ss.timer != nil {
		ss.timer.Stop()
	}
	ss.timer = time.AfterFunc(ss.srv.opts.Debounce, ss.publish)
}

// publish checks every open document (each with what it imports, open ones as they are now) and sends its
// diagnostics: a change to one module shows in the open modules that import it too.
func (ss *session) publish() {
	ss.mu.Lock()
	ss.seenVersion = ss.srv.opts.Scripts.Version()
	docs := make(map[string]document, len(ss.docs))
	for id, d := range ss.docs {
		docs[id] = *d
	}
	ss.mu.Unlock()
	for _, id := range sortedKeys(docs) {
		d := docs[id]
		a := language.Analyze(id, source{ss}, ss.options())
		t := newText(d.text)
		out := []Diagnostic{}
		for _, diag := range a.Diagnostics {
			if diag.Module != id {
				continue
			}
			start, end := t.word(t.lineCol(diag.Line, diag.Col))
			out = append(out, Diagnostic{Range: t.rangeOf(start, end), Severity: 1, Source: ss.srv.opts.Scheme, Message: diag.Msg})
		}
		version := d.version
		ss.notify("textDocument/publishDiagnostics", PublishDiagnosticsParams{URI: ss.uriOf(id), Version: &version, Diagnostics: out})
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// at reads a position request: its module, text and byte offset.
func (ss *session) at(raw json.RawMessage, p *TextDocumentPositionParams) (string, *text, int, error) {
	if err := json.Unmarshal(raw, p); err != nil {
		return "", nil, 0, invalidParams(err)
	}
	id, err := ss.moduleOf(p.TextDocument.URI)
	if err != nil {
		return "", nil, 0, err
	}
	t, err := ss.textOf(id)
	if err != nil {
		return "", nil, 0, err
	}
	return id, t, t.offset(p.Position), nil
}

// refAt is the name at a position, from an analysis of its module.
func (ss *session) refAt(raw json.RawMessage, p *TextDocumentPositionParams) (language.Ref, *text, bool, error) {
	id, t, off, err := ss.at(raw, p)
	if err != nil {
		return language.Ref{}, nil, false, err
	}
	a := language.Analyze(id, source{ss}, ss.options())
	ref, ok := a.At(id, off)
	return ref, t, ok, nil
}

func (ss *session) markdown(detail, doc string) *MarkupContent {
	value := "```" + ss.srv.opts.LanguageID + "\n" + detail + "\n```"
	if doc != "" {
		value += "\n\n" + doc
	}
	return &MarkupContent{Kind: "markdown", Value: value}
}

func (ss *session) hover(raw json.RawMessage) (any, error) {
	var p TextDocumentPositionParams
	ref, t, ok, err := ss.refAt(raw, &p)
	if err != nil || !ok || ref.Detail == "" {
		return nil, err
	}
	r := t.rangeOf(ref.Start, ref.End)
	return Hover{Contents: *ss.markdown(ref.Detail, ref.Doc), Range: &r}, nil
}

// location turns a language location into an LSP one, reading its module's text.
func (ss *session) location(l language.Location) (Location, bool) {
	t, err := ss.textOf(l.Module)
	if err != nil {
		return Location{}, false
	}
	return Location{URI: ss.uriOf(l.Module), Range: t.rangeOf(l.Start, l.End)}, true
}

func (ss *session) definition(raw json.RawMessage) (any, error) {
	var p TextDocumentPositionParams
	ref, _, ok, err := ss.refAt(raw, &p)
	if err != nil || !ok || ref.Def == nil {
		return nil, err
	}
	l, ok := ss.location(*ref.Def)
	if !ok {
		return nil, nil
	}
	return []Location{l}, nil
}

var completionKinds = map[language.RefKind]int{
	language.RefLocal: completionVariable, language.RefParameter: completionVariable, language.RefField: completionField,
	language.RefMethod: completionMethod, language.RefFunction: completionFunction, language.RefClass: completionClass,
	language.RefInterface: completionInterface, language.RefType: completionTypeParam, language.RefConstant: completionConstant,
	language.RefModule: completionModule, language.RefBuiltin: completionProperty, language.RefKeyword: completionKeyword,
}

func (ss *session) completion(raw json.RawMessage) (any, error) {
	var p TextDocumentPositionParams
	id, _, off, err := ss.at(raw, &p)
	if err != nil {
		return nil, err
	}
	items := []CompletionItem{}
	for i, c := range language.Complete(id, source{ss}, ss.options(), off) {
		kind := completionKinds[c.Kind]
		if c.Kind == language.RefBuiltin && strings.Contains(c.Detail, "(") {
			kind = completionFunction
		}
		item := CompletionItem{Label: c.Label, Kind: kind, Detail: c.Detail, SortText: fmt.Sprintf("%04d", i)}
		if c.Doc != "" {
			item.Documentation = &MarkupContent{Kind: "markdown", Value: c.Doc}
		}
		items = append(items, item)
	}
	return CompletionList{Items: items}, nil
}

func (ss *session) signatureHelp(raw json.RawMessage) (any, error) {
	var p TextDocumentPositionParams
	id, _, off, err := ss.at(raw, &p)
	if err != nil {
		return nil, err
	}
	sh, ok := language.SignatureAt(id, source{ss}, ss.options(), off)
	if !ok {
		return nil, nil
	}
	info := SignatureInformation{Label: sh.Signature.Label, Parameters: []ParameterInformation{}}
	if sh.Signature.Doc != "" {
		info.Documentation = &MarkupContent{Kind: "markdown", Value: sh.Signature.Doc}
	}
	for _, param := range sh.Signature.Params {
		info.Parameters = append(info.Parameters, ParameterInformation{Label: param.Name + ": " + param.Type})
	}
	return SignatureHelp{Signatures: []SignatureInformation{info}, ActiveParameter: sh.Active}, nil
}

var symbolKinds = map[language.RefKind]int{
	language.RefClass: symbolClass, language.RefMethod: symbolMethod, language.RefField: symbolField,
	language.RefInterface: symbolInterface, language.RefFunction: symbolFunction, language.RefConstant: symbolConstant,
	language.RefType: symbolTypeParam,
}

func (ss *session) documentSymbol(raw json.RawMessage) (any, error) {
	var p struct {
		TextDocument TextDocumentIdentifier `json:"textDocument"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams(err)
	}
	id, err := ss.moduleOf(p.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	t, err := ss.textOf(id)
	if err != nil {
		return nil, err
	}
	a := language.Analyze(id, source{ss}, ss.options())
	var convert func(syms []language.Symbol) []DocumentSymbol
	convert = func(syms []language.Symbol) []DocumentSymbol {
		out := []DocumentSymbol{}
		for _, s := range syms {
			out = append(out, DocumentSymbol{Name: s.Name, Detail: s.Detail, Kind: symbolKinds[s.Kind],
				Range: t.rangeOf(s.Range.Start, s.Range.End), SelectionRange: t.rangeOf(s.NameAt.Start, s.NameAt.End),
				Children: convert(s.Children)})
		}
		return out
	}
	return convert(a.Symbols(id)), nil
}

// everywhere lists every occurrence of what is declared at def, in every script: the stored ones and the open
// documents, each checked with what it imports. conflict says why it can't be renamed on its own ("" if it can).
func (ss *session) everywhere(def language.Location) (out []language.Ref, conflict string) {
	seen := map[language.Location]bool{}
	src := source{ss}
	ids := src.List()
	for _, h := range ss.srv.opts.Hosts {
		ids = append(ids, h.Name) // the definition files name what they declare too
	}
	for _, id := range ids {
		a := language.Analyze(id, src, ss.options())
		if conflict == "" {
			conflict = a.RenameConflict(def)
		}
		for _, r := range a.References(def) {
			if !seen[r.Location] {
				seen[r.Location] = true
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Module != out[j].Module {
			return out[i].Module < out[j].Module
		}
		return out[i].Start < out[j].Start
	})
	return out, conflict
}

func (ss *session) references(raw json.RawMessage) (any, error) {
	var p ReferenceParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams(err)
	}
	ref, _, ok, err := ss.refAt(raw, &p.TextDocumentPositionParams)
	if err != nil || !ok || ref.Def == nil {
		return []Location{}, err
	}
	out := []Location{}
	refs, _ := ss.everywhere(*ref.Def)
	for _, r := range refs {
		if r.Decl && !p.Context.IncludeDeclaration {
			continue
		}
		if l, ok := ss.location(r.Location); ok {
			out = append(out, l)
		}
	}
	return out, nil
}

// renamable reports whether a name can be renamed: something a script declares (not a host module's).
func (ss *session) renamable(ref language.Ref) bool {
	switch ref.Kind {
	case language.RefModule, language.RefBuiltin, language.RefKeyword:
		return false
	}
	return ref.Def != nil && ss.host(ref.Def.Module) == nil && ss.srv.opts.ValidModule(ref.Def.Module)
}

func (ss *session) prepareRename(raw json.RawMessage) (any, error) {
	var p TextDocumentPositionParams
	ref, t, ok, err := ss.refAt(raw, &p)
	if err != nil {
		return nil, err
	}
	if !ok || !ss.renamable(ref) {
		return nil, &rpcError{Code: codeRequestFailed, Message: "only names that scripts declare can be renamed"}
	}
	if _, conflict := ss.everywhere(*ref.Def); conflict != "" {
		return nil, &rpcError{Code: codeRequestFailed, Message: "can't rename " + ref.Name + " on its own: " + conflict}
	}
	return t.rangeOf(ref.Start, ref.End), nil
}

func (ss *session) rename(raw json.RawMessage) (any, error) {
	var p RenameParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams(err)
	}
	if !language.ValidName(p.NewName) {
		return nil, &rpcError{Code: codeRequestFailed, Message: strconv.Quote(p.NewName) + " can't be a name"}
	}
	ref, _, ok, err := ss.refAt(raw, &p.TextDocumentPositionParams)
	if err != nil {
		return nil, err
	}
	if !ok || !ss.renamable(ref) {
		return nil, &rpcError{Code: codeRequestFailed, Message: "only names that scripts declare can be renamed"}
	}
	refs, conflict := ss.everywhere(*ref.Def)
	if conflict != "" {
		return nil, &rpcError{Code: codeRequestFailed, Message: "can't rename " + ref.Name + " on its own: " + conflict}
	}
	edit := WorkspaceEdit{Changes: map[string][]TextEdit{}}
	texts := map[string]*text{}
	for _, r := range refs {
		t := texts[r.Module]
		if t == nil {
			if t, err = ss.textOf(r.Module); err != nil {
				return nil, err
			}
			texts[r.Module] = t
		}
		uri := ss.uriOf(r.Module)
		edit.Changes[uri] = append(edit.Changes[uri], TextEdit{Range: t.rangeOf(r.Start, r.End), NewText: p.NewName})
	}
	return edit, nil
}

func (ss *session) formatting(raw json.RawMessage) (any, error) {
	var p DocumentFormattingParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams(err)
	}
	id, err := ss.moduleOf(p.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	if err := ss.readOnly(id); err != nil {
		return nil, err
	}
	t, err := ss.textOf(id)
	if err != nil {
		return nil, err
	}
	formatted, err := language.Format(t.src)
	if err != nil {
		return nil, &rpcError{Code: codeRequestFailed, Message: "can't format: " + err.Error()}
	}
	if formatted == t.src {
		return []TextEdit{}, nil
	}
	return []TextEdit{{Range: t.rangeOf(0, len(t.src)), NewText: formatted}}, nil
}
