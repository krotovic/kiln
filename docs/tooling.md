# Tooling

What an editor or a language server needs from the language: analysis, completion, signature help, formatting and
renames.

The language's tools (`language`: `analysis.go`, `complete.go`, `format.go`) answer questions about
source that is being written. They share the checker with `CompileScript`, so they see exactly what compiling sees,
and an editor, a build step and the servers running scripts agree on every script.

- **`Analyze(entry, src, opts)`** loads and checks a script like `CompileScript` (or, with a host module's name as the
  entry, that module's definition file on its own), but goes on past errors (a module that doesn't parse is left out,
  and the names imported from it go unchecked rather than unknown), and records every name occurrence as a `Ref`: its
  range, kind (local, parameter, field, method, function, class, interface, type, constant, module, built-in), its type
  or signature as hover shows it, its doc comment, its `Signature` (for calls), and its definition `Location`: in a
  script, or in a host module's definition file (none for the standard library and the built-in members).
  `At(module, offset)` finds the name at a position, `References(def)` every occurrence of what is declared there, and
  `Symbols(module)` the outline: classes with their fields, methods and constructor, interfaces, functions, types and
  constants.
- **Doc comments** are the `//` lines or `/* */` block right above a declaration (no blank line between), each on a
  line of its own, without their markers: `/** A bell. */`. A comment at the end of a line of code belongs to that
  code.
- **Import aliases** count as declared where they are imported: renaming `B` in `import { A as B }` renames the
  module's uses of `B`, and renaming `A` renames `A` in the import.
- **`Complete(module, src, opts, offset)`** puts a probe name in place of the word at the cursor (and, if that doesn't
  parse, ends the statement right after it, for `ctx.| log(…)`) and checks the module; the checker, meeting the probe,
  records what could go there: after a dot, the members the code may use (private ones only inside their class, no
  optional interface methods; arrays', maps' and strings' built-ins); for a name, the variables in scope, the module's
  constants, functions and classes, the standard library and keywords; for a type, the types. In an import's `{ }` it
  offers the module's exports, and in its quotes the module names the `Source` lists (`Lister`) and the host modules.
  Source that still doesn't parse gets the keywords that start declarations and statements.
- **`SignatureAt(module, src, opts, offset)`** finds the innermost `(` still open at the cursor, the function, method
  or constructor before it, and the argument the cursor is in (counting commas); an unfinished call is completed with
  the probe (and a `)`) so the rest of the module checks.
- **`Format(src)`** rewrites a module in one style: two spaces of indentation per open bracket, the line breaks as
  written (at most one blank line in a row), spaces around binary operators and after commas, colons and keywords, none
  inside brackets, around dots, after unary operators (unless the next is one too: `- -x` never becomes `--x`) or in
  `Map<K, V>`. Comments stay where they are; the tokens never change; formatting formatted source changes nothing.
  Source that doesn't lex is refused.
- **`ValidName(name)`** says whether a rename may use a name, and **`RenameConflict(def)`** why a field or method can't
  be renamed on its own: it overrides a base class's member or a subclass overrides it, it implements a member of an
  interface its class names in `implements` (the host's `Behaviour` too, whose methods the host calls by name), or, for
  an interface's member, a class implementing the interface has it, or another interface extends it. Renaming one of
  them would break the others; classes that satisfy an interface without naming it aren't seen.

## Tests

The cases in `testdata/tools/*.txt` (`TestToolCases`; hover with doc comments,
  line and block; definitions across modules, of import aliases and module names, none for host modules; references of
  fields and classes across modules; the outline; names from a module that doesn't parse going unchecked; refs kept
  past an error; completions after a dot (private members only inside the class, no optional interface methods, the
  host's classes, arrays' built-ins), while typing a name, before code on the same line, names in scope, at a
  statement's start, types, an import's names, the host module's names, module names in quotes; signature help in a
  call, an unclosed call, a constructor, the standard library and a built-in method, and none outside a call; a
  trailing comment of the line above not taken as a doc comment; the host's declarations defined in its definition
  file; host modules analysed on their own (refs, outline) and refused when they hold anything but declarations; what
  can be renamed alone: a plain method, but not one that overrides, is overridden, implements an interface's (the
  host's `Behaviour` too), an interface member a class implements, or one another interface extends) and
  `testdata/format/*.txt` (`TestFormatCases`; spacing and indentation, comments and blank lines, optional methods,
  unary operators and arrays, unary operators never joining into longer ones, continuation lines, unlexable source
  refused; each idempotent with its tokens unchanged); `ValidName`. In a tools case, `‸` in a module marks the cursor
  and the sections say what to expect there: `-- hover --` (the detail, then the doc), `-- definition --`
  (`module:line:col` or `none`), `-- completions --` (lines `+ label` that must be offered and `- label` that
  mustn't), `-- signature --` (`label @ active parameter` or `none`), `-- references --`, `-- symbols --` (the
  outline) and `-- diagnostics --`. A format case has `-- in --` and `-- want --` (or `error`); every case also checks
  that formatting again changes nothing and keeps the tokens.
