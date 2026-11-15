# lls — Lightweight Language Server

A tiny, fast, language-agnostic completion server written in Go. It is
built to run well on small machines (Raspberry Pi included): one static
binary, no runtime dependencies, and memory use that scales with the libraries
you actually import.

## How it works

1. **Compile** (`lls build`). The library sources of a language (Python
   stdlib, `$GOROOT/src`, `/usr/include`, ...) are scanned in parallel with
   the rules of a JSON *language spec* (`langs/*.json`). The resulting symbols
   (name, kind, signature, first paragraph of docs) go into one compact binary
   file per library: `$LLS_HOME/<lang>/<name>.llsi`. Introspection scripts
   (`scripts/introspect/`) add modules that have no source, e.g. Python's C
   modules or Node built-ins, as JSON Lines.
2. **Load lazily.** At runtime the index is `mmap`ed. Opening it reads only a
   32-byte header. A module is decoded the first time a buffer imports it, and
   its strings point straight into the mapped file (zero copy).
3. **Analyse buffers.** For each open document, lls works out the imports
   (aliases, imported names, star/`#include` scope) and the local definitions,
   then resolves project-local modules (`from mypkg.util import x`, `./util`)
   by reading those files on demand.
4. **Rank.** Each candidate gets a score: exact prefix, then case-insensitive
   prefix, then fuzzy. A weight for its source is added on top: buffer
   definitions, then imports, scope, builtins, keywords, words from the current
   buffer, other open buffers, and recently closed buffers (the "recent
   context").

Typical request latency is 50–500 µs on a desktop.

## Quick start

```sh
make build                      # -> bin/lls (static, stripped)
make install                    # -> ~/.local/bin/lls
scripts/build-index.sh python   # or: go, c, javascript, rust, lua
scripts/build-all.sh            # every language whose toolchain is installed
lls langs                       # what is installed
```

Terminal usage:

```sh
lls complete -l python 'os.path.jo'
lls complete --file main.c --pos 12:7
printf '#include <string.h>\nstrc' | lls complete -l c --stdin
lls hover -l go 'strings.Builder'
lls check main.py util.lua      # syntax errors as file:line:col (exit 1 on errors)
lls repl -l python              # interactive; import lines extend the context
lls inspect go 'net/http*'      # browse modules / symbols of an index
lls bench -l python 'os.path.jo'
```

To build for a Raspberry Pi, run `scripts/cross-compile.sh`, which writes
`dist/lls-linux-arm64`, `-armv7` and `-armv6`. Copy the binary to the Pi
together with the `.llsi` files, which are portable across architectures, or
build the indexes on the Pi itself.

## Editor setup (`lls serve` speaks LSP over stdio)

* **Neovim**: `vim.lsp.start({ name = "lls", cmd = { "lls", "serve" }, root_dir = vim.fn.getcwd() })`
* **Helix** (`languages.toml`): `[language-server.lls] command = "lls"; args = ["serve"]`, then add `"lls"` to a language's `language-servers`.
* **Emacs eglot**: `(add-to-list 'eglot-server-programs '((python-mode c-mode go-mode) "lls" "serve"))`
* **VS Code**: any generic LSP client extension, command `lls serve`.

Supported requests: completion (+ resolve), hover, signatureHelp,
definition (buffer and project files), documentSymbol, and incremental sync.
Syntax errors are pushed with `textDocument/publishDiagnostics` on every
open/change. Options (`initializationOptions`) are `{"limit": 100}` and
`{"diagnostics": false}` to turn syntax checking off. Server flags are
`--home`, `--log FILE` and `--trace`, which logs the duration of each request.

## Syntax checking

Every spec has a `syntax` section describing the structure of the language,
so checking stays language-agnostic and needs no grammar. The checker is a
single pass that skips comments and literals and tracks open blocks. It reports:

* unbalanced or mismatched brackets (`"(" was never closed`, `unexpected "]"`);
* unterminated strings and block comments;
* unclosed keyword blocks (Lua `function` ... `end`, `repeat` ... `until`);
* indentation errors (Python: missing indented block, unexpected indent,
  dedent to an unknown level, tabs mixed with spaces).

| field | purpose |
|---|---|
| `blocks` | `braces`, `indent` or `keywords` |
| `brackets` | pairs such as `["()", "[]", "{}"]` |
| `strings[]` | `{open, close, escape, multiline, char, lenient, interpolation, spaceEscapes, notAfterNumber}` |
| `blockComments`, `nestedComments` | extra comment pairs (Lua `--[[ ]]`), nesting (Rust) |
| `indent` | `{openers: ":", continuation: "\\", tabWidth: 8}` for `blocks: indent` |
| `keywords[]` | `{open: ["function", "if", "do"], close: ["end"]}` for `blocks: keywords` |
| `regexLiterals`, `preprocessor` | JS `/regex/` literals; C `#if/#else/#endif` branches |

On the Python stdlib, `$GOROOT/src`, Rust crates, Lua and node_modules code it
reports no false positives. In `/usr/include`, 4 of about 5000 headers get
diagnostics, mostly real unbalanced macros or unusual preprocessor layouts.

## Adding a language

Run `lls spec python > ~/.local/share/lls/langs/mylang.json` and edit the
copy. Specs in `$LLS_HOME/langs` override the built-in ones without a
rebuild. The main fields are:

| field | purpose |
|---|---|
| `source.extensions`, `excludeDirs`, `excludeFiles` | which files `lls build` reads |
| `module.mode` | `dir` (Go), `file` (Python/Lua/Rust), `file-ext` (C headers), `package` (node_modules) |
| `symbols[]` | `{kind, pattern (?P<name>), hint, scope: top/container, opens, block, continueUntil, local}` |
| `imports[]` | regex with `module`/`rel`, `alias`, `names` groups; `bind: alias/scope` |
| `reexports[]` | modules whose symbols are visible through a file (`#include`, `export *`) |
| `importPathContext` / `importNameContext` | completion inside `import "..."` / `from x import ...` |
| `autoload`, `keywords`, `memberSeparators`, `localResolve` | builtins, keywords, `.`/`::`/`->`, local module lookup |

Then build with `lls build -l mylang -r /path/to/libs`. The JSONL input
format is one record per line:
`{"m":"module","k":"function","n":"name","d":"signature","doc":"..."}`
(use `"m":"module#Class"` for class members).

## Layout

```
cmd/lls            CLI (serve, build, complete, hover, check, repl, inspect, bench)
internal/index     .llsi format: writer, mmap reader (format doc in format.go)
internal/lang      spec loading, line-based extractor, imports, module naming, syntax checker
internal/build     parallel index compiler
internal/engine    documents, analysis, completion/hover/signature/definition
internal/lsp       JSON-RPC transport + handlers (hand-written JSON on hot paths)
langs/             built-in language specs (embedded in the binary)
scripts/           build-index.sh, build-all.sh, cross-compile.sh, introspect/
```

## Limitations

The syntax checker is structural, not a full parser: it does not catch
invalid expressions or misplaced keywords, only broken structure. There is
no type inference. `obj.` completes from module and class members
when the receiver can be resolved, and otherwise from names used after `.` in
open buffers. Rust support is best effort, because std re-exports are not
followed by name. C++ headers without an extension (`<vector>`) are not
indexed.
