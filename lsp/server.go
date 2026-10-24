// Package lsp is the language server for scripts: the editor's code editor talks the Language Server Protocol to
// it over a WebSocket while the world is in build mode. It reads the stored scripts from the asset service and
// overlays each session's open documents on them, and answers with the language package's analysis
// (diagnostics, hover, completion, definitions, signatures, symbols, references, renames, formatting).
package lsp

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"adventurers-life-online/server/internal/engine/language"
	"adventurers-life-online/server/internal/game/zones"
	"adventurers-life-online/server/pkg/auth"
	"adventurers-life-online/server/pkg/transport"
	"github.com/gorilla/websocket"
)

// Scripts are the stored scripts a server reads: the asset service's, through a scripting.Library.
type Scripts interface {
	language.Source
	List() []string
	Version() uint64
}

const (
	// uriPrefix starts every document's URI: alo:/script/<slug>, and alo:/host/<name> for a host module's
	// definition file (read-only)
	uriPrefix  = "alo:/"
	hostPrefix = "host/"
	// maxDocuments is how many documents one session may have open
	maxDocuments = 64
	// maxMessage bounds one message, a whole document included
	maxMessage = 1 << 20
	// languageID names the language in Markdown code blocks, as the editor registers it
	languageID = "alo-script"
)

// Server answers language server sessions, one per WebSocket.
type Server struct {
	scripts Scripts
	tokens  *auth.TokenManager
	// revoked reports whether a session token was revoked since it was issued (nil: none are)
	revoked  func(*auth.SessionClaims) bool
	hosts    []*language.HostModule
	debounce time.Duration // how long diagnostics wait for typing to pause
	watch    time.Duration // how often sessions check for changed scripts and revoked or expired tokens
	// initTimeout is how long a socket may wait before initialize
	initTimeout time.Duration
}

// NewServer makes a server over the stored scripts, checking scripts against hosts.
func NewServer(scripts Scripts, tokens *auth.TokenManager, revoked func(*auth.SessionClaims) bool, hosts []*language.HostModule) *Server {
	if revoked == nil {
		revoked = func(*auth.SessionClaims) bool { return false }
	}
	return &Server{scripts: scripts, tokens: tokens, revoked: revoked, hosts: hosts, debounce: 200 * time.Millisecond,
		watch: 2 * time.Second, initTimeout: 10 * time.Second}
}

// ServeHTTP upgrades /lsp to a WebSocket and serves one session on it until it closes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := transport.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxMessage)
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
	conn    *websocket.Conn
	writeMu sync.Mutex

	mu          sync.Mutex // guards what follows
	docs        map[string]*document
	claims      *auth.SessionClaims
	timer       *time.Timer
	seenVersion uint64
	closed      bool
	done        chan struct{}
}

func (ss *session) run() {
	defer ss.close()
	go ss.watch()
	// A socket that never initializes doesn't hold a session open
	_ = ss.conn.SetReadDeadline(time.Now().Add(ss.srv.initTimeout))
	for {
		_, data, err := ss.conn.ReadMessage()
		if err != nil {
			return
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

// watch republishes diagnostics when the stored scripts change, and ends the session when its token is
// revoked.
func (ss *session) watch() {
	ticker := time.NewTicker(ss.srv.watch)
	defer ticker.Stop()
	for {
		select {
		case <-ss.done:
			return
		case <-ticker.C:
		}
		ss.mu.Lock()
		claims := ss.claims
		changed := claims != nil && ss.srv.scripts.Version() != ss.seenVersion
		ss.mu.Unlock()
		if claims != nil && ss.srv.revoked(claims) {
			log.Printf("[LSP] Ending %s's session: their token was revoked", claims.Email)
			_ = ss.conn.Close()
			return
		}
		if claims != nil && claims.ExpiresAt != nil && time.Now().After(claims.ExpiresAt.Time) {
			log.Printf("[LSP] Ending %s's session: their token expired", claims.Email)
			_ = ss.conn.Close()
			return
		}
		if changed {
			ss.schedule()
		}
	}
}

// handle answers one request or notification; false ends the session.
func (ss *session) handle(msg *message) bool {
	isRequest := len(msg.ID) > 0
	ss.mu.Lock()
	initialized := ss.claims != nil
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
	case "alo/source":
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
	_ = ss.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = ss.conn.WriteMessage(websocket.TextMessage, data)
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

// initialize checks the session token (a game master's, with world.edit, not revoked) and answers what the
// server can do. A session that fails it ends.
func (ss *session) initialize(msg *message) bool {
	var p InitializeParams
	_ = json.Unmarshal(msg.Params, &p)
	claims, err := ss.srv.tokens.ValidateToken(p.InitializationOptions.Token)
	if err != nil || claims.Issuer != auth.SessionTokenIssuer || ss.srv.revoked(claims) {
		ss.replyError(msg.ID, codeInvalidRequest, "a session token is needed (initializationOptions.token)")
		return false
	}
	if !claims.Can(auth.PermWorldEdit) {
		ss.replyError(msg.ID, codeInvalidRequest, "editing scripts needs the "+auth.PermWorldEdit+" permission")
		return false
	}
	ss.mu.Lock()
	ss.claims = claims
	ss.mu.Unlock()
	_ = ss.conn.SetReadDeadline(time.Time{})
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
		"serverInfo": map[string]any{"name": "alo-lsp"},
	})
	return true
}

// moduleOf turns a document URI into its module ID: a script's, or a host module's name.
func (ss *session) moduleOf(uri string) (string, error) {
	id, ok := strings.CutPrefix(uri, uriPrefix)
	if name, isHost := strings.CutPrefix(id, hostPrefix); ok && isHost && ss.host(name) != nil {
		return name, nil
	}
	if !ok || !zones.ValidScriptID(id) {
		return "", &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("%q is not a script (alo:/script/<name>)", uri)}
	}
	return id, nil
}

// host finds a host module by name.
func (ss *session) host(name string) *language.HostModule {
	for _, h := range ss.srv.hosts {
		if h.Name == name {
			return h
		}
	}
	return nil
}

func (ss *session) uriOf(module string) string {
	if ss.host(module) != nil {
		return uriPrefix + hostPrefix + module
	}
	return uriPrefix + module
}

// readOnly refuses to change a host module's definition file.
func (ss *session) readOnly(module string) error {
	if ss.host(module) != nil {
		return &rpcError{Code: codeRequestFailed, Message: module + " is the host's definition file: it can't be changed"}
	}
	return nil
}

// sourceOf answers alo/source {uri}: a document's text as the server reads it (how the editor opens a host
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
	return s.ss.srv.scripts.Load(id)
}

func (s source) List() []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range s.ss.srv.scripts.List() {
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
	return language.ScriptOptions{Hosts: ss.srv.hosts}
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
	ss.timer = time.AfterFunc(ss.srv.debounce, ss.publish)
}

// publish checks every open document (each with what it imports, open ones as they are now) and sends its
// diagnostics: a change to one module shows in the open modules that import it too.
func (ss *session) publish() {
	ss.mu.Lock()
	ss.seenVersion = ss.srv.scripts.Version()
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
			out = append(out, Diagnostic{Range: t.rangeOf(start, end), Severity: 1, Source: "alo", Message: diag.Msg})
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

func markdown(detail, doc string) *MarkupContent {
	value := "```" + languageID + "\n" + detail + "\n```"
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
	return Hover{Contents: *markdown(ref.Detail, ref.Doc), Range: &r}, nil
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
	for _, h := range ss.srv.hosts {
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
func renamable(ref language.Ref) bool {
	switch ref.Kind {
	case language.RefModule, language.RefBuiltin, language.RefKeyword:
		return false
	}
	return ref.Def != nil && zones.ValidScriptID(ref.Def.Module)
}

func (ss *session) prepareRename(raw json.RawMessage) (any, error) {
	var p TextDocumentPositionParams
	ref, t, ok, err := ss.refAt(raw, &p)
	if err != nil {
		return nil, err
	}
	if !ok || !renamable(ref) {
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
	if !ok || !renamable(ref) {
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
