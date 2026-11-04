# Language server

`lsp` is a Language Server Protocol 3.17 server for the language, which a host embeds, or runs as `kiln lsp`, to give
an editor diagnostics, hover, completion, signature help, definitions, references, renames and formatting. It runs the language's
[tooling](tooling.md) over the host's stored scripts with each session's open documents on top.

## Embedding

```go
srv := lsp.NewServer(lsp.Options{
	Scripts: scripts, // language.Source plus List() and Version(): the stored scripts
	Hosts:   []*language.HostModule{game, ui},
	Authorize: func(raw json.RawMessage) (lsp.Grant, error) {
		var o struct{ Token string }
		_ = json.Unmarshal(raw, &o)
		return checkToken(o.Token) // a Grant whose Check fails once the token is revoked or expired
	},
	ValidModule: func(id string) bool { return strings.HasPrefix(id, "script/") },
	Scheme:      "kiln", // documents are kiln:/script/main
})
http.Handle("/lsp", srv)
```

Every field but `Scripts` has a default: no `Authorize` lets every session in, no `ValidModule` takes any non-empty
ID, `Scheme` and `LanguageID` are `kiln`, `Name` is `kiln-lsp`, and `Upgrader` is a `websocket.Upgrader` that accepts
only same-origin requests (set `CheckOrigin` to allow an editor elsewhere). `Debounce` (200 ms), `Watch` (2 s) and
`InitTimeout` (10 s) set the timings. `ServeConn` serves a WebSocket the host upgraded itself, `ServeStream` a byte
stream (a pipe, or a process's standard input and output), and `Serve` any `Conn` (a transport of the host's: one
JSON-RPC message per read and write). `Notifications` are the host's own notifications by method, handed their params
before `initialize` as after it. `MemoryScripts` are stored scripts kept in memory and replaced whole with `Set`.

## `kiln lsp`

For a host that doesn't embed the Go package, `kiln lsp` (in `cmd/kiln`) serves one session on its standard input and
output, logging to standard error, and exits when it ends:

```sh
kiln lsp -host game=game.d.als -host ui=ui.d.als -scheme alo -language alo-script -name alo-lsp -module 'script/[a-z0-9-]+'
```

- `-host name=file.d.als`, repeated: the host modules, as their definition files (declarations only, as
  [`kiln compile`](compiler.md) takes them). One that doesn't check stops the command with its error.
- `-scheme`, `-language` and `-name` are `Scheme`, `LanguageID` and `Name`; `-module` is a regular expression the whole
  of a script's ID must match (`ValidModule`; any non-empty ID without it).
- **Stored scripts** come from the host in the notification `<scheme>/scripts {scripts: {<ID>: <source>}}`, which
  replaces them whole and republishes the open documents' diagnostics. It may come before `initialize`; until it does
  there are none.
- **Authorization** is the host's: every session is let in, so a host that serves editors it doesn't trust puts the
  command behind its own front, which checks `initialize`'s `initializationOptions` before passing it on and ends the
  process when the session's grant fails (the game's language server does this for game masters' tokens).

## Protocol

- **Transport.** `ServeHTTP` upgrades to a WebSocket; each text frame is one JSON-RPC 2.0 message (no
  `Content-Length` framing), at most 1 MB. On a stream every message comes after its headers (`Content-Length`, which
  is required, and an optional `Content-Type`) and an empty line, as the base protocol has it, at most 64 MB. One
  session per socket or stream.
- **Authorization.** The first request must be `initialize`; its `initializationOptions` go to `Authorize`, whose
  error is the answer, and the socket closes. A request before `initialize` gets `-32002`, and a session that sends no
  `initialize` within `InitTimeout` is closed. A request with the method of one of `Notifications` is refused. The session's `Grant` is checked every `Watch`; once its `Check` fails
  the session is closed. A request that fails inside the server is answered with an error; the session goes on.
- **Documents** are named `<scheme>:/<module ID>`, for the IDs `ValidModule` takes; a host module's definition file
  is `<scheme>:/host/<name>`, read-only: the editor reads it with the custom request `<scheme>/source {uri}`
  (answering `{text, readOnly}`), and changing, formatting or renaming in it is refused. Each session's open documents
  go over the stored scripts (full text sync, at most 64 open). Diagnostics carry the scheme as their source.
- **Methods:** `initialize`, `initialized`, `shutdown`, `exit`; `textDocument/didOpen`, `didChange`, `didClose`;
  `textDocument/publishDiagnostics` (`Debounce` after typing pauses, for every open document, so a change to one
  shows in the open scripts that import it, and again when the stored scripts' `Version` changes); `hover`
  (Markdown: the type or signature in a `LanguageID` code block, and the doc comment), `completion` (trigger
  characters `.`, `"`, `/`), `definition`, `signatureHelp` (`(`, `,`), `documentSymbol`, `references` and `rename`
  (across every stored script and open document; only names scripts declare, never the host's, to a valid name;
  `prepareRename` says where, and both refuse a member that overrides or implements another, as `RenameConflict`
  says), and `formatting` (the whole document). Definitions, hover, references and the outline reach into the host
  modules' definition files too.

## Tests

- `lsp`: sessions over a real WebSocket against a test host module: requests before `initialize` refused,
  `Authorize`'s error answered and the session closed, a failing grant ending the session, a socket without
  `initialize` closed; diagnostics following open documents and the open scripts importing them, with ranges, and
  cleared on close; hover, definition, completion, signature help, symbols, references, prepareRename and rename
  across stored scripts, formatting, a URI outside the scripts refused, unknown methods; at most 64 documents;
  malformed changes not crashing the session; renames that would break a hierarchy refused; host modules'
  definition files read through `<scheme>/source`, answering hover and the outline, and read-only. Positions:
  UTF-16 characters, offsets past a line's end, words.
- `lsp` on streams: the base protocol's framing (headers in any case, a missing or bad `Content-Length`, a message cut
  short, writes after `Close`), a session on pipes with the host's notifications before `initialize` and refused as
  requests, stored scripts that change republishing diagnostics, `exit` and no `initialize` ending it;
  `MemoryScripts`.
- `cmd/kiln`: `kiln lsp`'s flags refused (a malformed or repeated `-host`, a missing or broken definition file, a bad
  `-module`, stray arguments), and a session on its standard streams: stored scripts from `<scheme>/scripts`, hover in
  a host module, a host's definition file through `<scheme>/source`, IDs `-module` refuses, and `exit`.
- `lsp/lsptest` is the client the tests use, exported so hosts can test the server they configure: `Dial`, `Stream`
  and `New` (on any `Conn`), `Call`, `Notify`, `Initialize`, `Open`, `Diagnostics`, `WaitClosed` and `Pos`.
