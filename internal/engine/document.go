package engine

import (
	"bytes"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"lls/internal/index"
	"lls/internal/lang"
	"lls/internal/text"
)

// Document is a buffer open in the editor.
type Document struct {
	URI     string
	Path    string
	Lang    *Language
	Version int32
	Text    []byte

	lineStarts []int // offsets of line starts; nil after an edit
	an         *analysis
	words      *wordStats // cached word statistics (for use by other docs)
}

// analysis is everything derived from a document version that is expensive
// enough to cache: definitions and resolved imports.
type analysis struct {
	version int32
	at      time.Time
	defs    []lang.Def
	aliases map[string]string  // local alias -> canonical module name
	names   map[string]nameRef // local name -> canonical module + name
	scope   []string           // canonical modules whose symbols are in scope
	mods    []*index.Module    // expanded scope + autoload modules
}

type nameRef struct {
	module string
	name   string
}

// wordStats are the identifiers of a document, used for "recent context"
// completion across buffers.
type wordStats struct {
	lang    *Language
	version int32
	words   map[string]uint32
	members map[string]uint32
}

// URIToPath converts a file:// URI to a local path.
func URIToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' { // Windows "/C:/x"
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// PathToURI converts a local path to a file:// URI.
func PathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.ToSlash(abs)
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	return (&url.URL{Scheme: "file", Path: abs}).String()
}

// Open registers a document. It returns nil when no language matches.
func (e *Engine) Open(uri, languageID string, version int32, content []byte) *Document {
	path := URIToPath(uri)
	L := e.LanguageFor(languageID, path)
	if L == nil {
		return nil
	}
	d := &Document{URI: uri, Path: path, Lang: L, Version: version, Text: content}
	e.docs[uri] = d
	e.byPath[path] = d
	return d
}

// Get returns an open document.
func (e *Engine) Get(uri string) *Document { return e.docs[uri] }

// Close forgets a document, keeping its words as recent context.
func (e *Engine) Close(uri string) {
	d := e.docs[uri]
	if d == nil {
		return
	}
	ws := d.wordStats()
	e.recent = append(e.recent, ws)
	if len(e.recent) > 8 {
		e.recent = e.recent[1:]
	}
	delete(e.docs, uri)
	delete(e.byPath, d.Path)
}

// Replace sets the full text of a document.
func (d *Document) Replace(version int32, content []byte) {
	d.Text = content
	d.Version = version
	d.lineStarts = nil
}

// Edit replaces the byte range [start, end) with s.
func (d *Document) Edit(version int32, start, end int, s string) {
	if start < 0 {
		start = 0
	}
	if end > len(d.Text) {
		end = len(d.Text)
	}
	if start > end {
		start = end
	}
	grow := len(s) - (end - start)
	if grow <= 0 {
		copy(d.Text[start:], s)
		copy(d.Text[start+len(s):], d.Text[end:])
		d.Text = d.Text[:len(d.Text)+grow]
	} else {
		old := len(d.Text)
		d.Text = append(d.Text, make([]byte, grow)...)
		copy(d.Text[end+grow:], d.Text[end:old])
		copy(d.Text[start:], s)
	}
	d.Version = version
	d.lineStarts = nil
}

func (d *Document) lines() []int {
	if d.lineStarts == nil {
		starts := make([]int, 1, bytes.Count(d.Text, []byte{'\n'})+1)
		for i, c := range d.Text {
			if c == '\n' {
				starts = append(starts, i+1)
			}
		}
		d.lineStarts = starts
	}
	return d.lineStarts
}

// LineCount returns the number of lines.
func (d *Document) LineCount() int { return len(d.lines()) }

// LineBytes returns line n without its terminator.
func (d *Document) LineBytes(n int) []byte {
	ls := d.lines()
	if n < 0 || n >= len(ls) {
		return nil
	}
	end := len(d.Text)
	if n+1 < len(ls) {
		end = ls[n+1] - 1
	}
	return bytes.TrimSuffix(d.Text[ls[n]:end], []byte{'\r'})
}

// Offset converts an LSP position (zero-based line, UTF-16 column) to a byte
// offset, clamping out-of-range positions.
func (d *Document) Offset(line, col int) int {
	ls := d.lines()
	if line < 0 {
		return 0
	}
	if line >= len(ls) {
		return len(d.Text)
	}
	start := ls[line]
	return start + text.UTF16ToByte(d.Text[start:], col)
}

// Position converts a byte offset to an LSP position.
func (d *Document) Position(off int) (line, col int) {
	ls := d.lines()
	lo, hi := 0, len(ls)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if ls[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, text.UTF16Len(d.Text[ls[lo]:off])
}

// lineBefore returns the text of the current line up to off.
func (d *Document) lineBefore(off int) []byte {
	start := bytes.LastIndexByte(d.Text[:off], '\n') + 1
	return d.Text[start:off]
}

// wordStats returns (cached per version) the identifiers of the document.
func (d *Document) wordStats() *wordStats {
	if d.words != nil && d.words.version == d.Version {
		return d.words
	}
	ws := &wordStats{lang: d.Lang, version: d.Version, words: make(map[string]uint32), members: make(map[string]uint32)}
	d.Lang.Ident.Words(d.Text, 3, func(w []byte, member bool) {
		if member {
			ws.members[string(w)]++
		} else {
			ws.words[string(w)]++
		}
	})
	d.words = ws
	return ws
}

// staleOK is how long a document analysis may be reused across edits for
// completion. Imports and definitions rarely change between two keystrokes,
// and skipping re-analysis keeps per-keystroke latency flat on large files.
// Hover, signature help and definition always use a fresh analysis.
const staleOK = 300 * time.Millisecond

// analyze returns the document's analysis, recomputing it when the version
// changed (or, with allowStale, when it is older than staleOK).
func (e *Engine) analyze(d *Document, allowStale bool) *analysis {
	if a := d.an; a != nil {
		if a.version == d.Version || (allowStale && time.Since(a.at) < staleOK) {
			return a
		}
	}
	L := d.Lang
	info := L.Extract(d.Text, lang.ExtractOptions{All: true})
	imp := L.ParseImports(d.Text)
	a := &analysis{
		version: d.Version,
		at:      time.Now(),
		defs:    info.Defs,
		aliases: make(map[string]string, len(imp.Aliases)),
		names:   make(map[string]nameRef, len(imp.Names)),
	}
	for alias, mod := range imp.Aliases {
		a.aliases[alias] = e.canonical(d, mod, imp.Relative[mod])
	}
	for local, ref := range imp.Names {
		a.names[local] = nameRef{module: e.canonical(d, ref.Module, ref.Relative), name: ref.Name}
	}
	for _, mod := range imp.Scope {
		a.scope = append(a.scope, e.canonical(d, mod, imp.Relative[mod]))
	}
	a.mods = e.expandScope(L, append(append([]string(nil), a.scope...), L.Autoload...))
	d.an = a
	return a
}

// canonical maps a module as written in an import to the name under which
// it can be looked up: the module itself if an index has it, otherwise the
// overlay name of a project-local module, otherwise the name unchanged.
func (e *Engine) canonical(d *Document, mod string, relative bool) string {
	if !relative && d.Lang.HasModule(mod) {
		return mod
	}
	if key, ok := e.localModule(d, mod, relative); ok {
		return key
	}
	return mod
}

// expandScope resolves scope modules and follows their re-exports (C header
// includes, "from x import *") up to a few levels deep.
func (e *Engine) expandScope(L *Language, names []string) []*index.Module {
	const maxDepth, maxModules = 3, 96
	seen := make(map[string]bool)
	var out []*index.Module
	type item struct {
		name  string
		depth int
	}
	queue := make([]item, 0, len(names))
	for _, n := range names {
		queue = append(queue, item{n, 0})
	}
	for len(queue) > 0 && len(out) < maxModules {
		it := queue[0]
		queue = queue[1:]
		if seen[it.name] {
			continue
		}
		seen[it.name] = true
		for _, m := range L.Modules(it.name) {
			out = append(out, m)
			if it.depth < maxDepth {
				for _, re := range m.Reexports {
					queue = append(queue, item{re, it.depth + 1})
				}
			}
		}
	}
	return out
}
