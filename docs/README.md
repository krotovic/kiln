# Kiln documentation

Kiln is a statically typed subset of TypeScript for embedding in hosts. It is used in two forms:

- **Scripts** are whole programs: modules with classes, interfaces, functions and statements, which import each other.
  Scripts compile to WebAssembly: one module per script, which servers run with wazero and the browser with its own
  engine, so a script gives the same results everywhere. What the host answers later (a record a database keeps)
  is a promise an async function `await`s; errors are values of error classes (`Promise<T, E>`), never exceptions.
- **Expressions** are single expressions, the part of the language that computes a value, embedded where content needs
  a small calculation.

Its design goals: **safe** (code reaches only what its host gives it, under budgets of steps, nested calls and
memory), **deterministic** (randomness only from the host's seeded generator), **strictly typed** (no implicit
conversions but joining strings, no `undefined`: every value starts at its type's zero value), **checked early**
(compiled when content is saved), **fast, the same everywhere** (one WebAssembly module on servers and in the
browser) and **reusable** (nothing in it knows about its hosts; hosts embed it with their own names, functions and
modules).

## Documents

| Document | What it covers |
| --- | --- |
| [The language](language-reference.md) | The reference: lexical structure, types and zero values, expressions and operators, conversions, statements, functions, classes, interfaces, modules, built-ins, budgets, errors; the script test cases |
| [Expressions](expressions.md) | Expressions on their own: what they may use, limits, folding; embedding them in Go; the shared expression cases |
| [Script runtime](script-runtime.md) | Embedding scripts in Go and in TypeScript, host modules, and compiling to WebAssembly: memory, the collector, calls, budgets, imports and exports, metadata |
| [Tooling](tooling.md) | What editors and language servers use: analysis, hover, definitions, references, completion, signature help, formatting, rename |
| [Language server](language-server.md) | The LSP server hosts embed: options, authorization, documents and URIs, methods |
