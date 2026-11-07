package engine

import (
	"slices"
	"strings"

	"lls/internal/index"
	"lls/internal/lang"
	"lls/internal/text"
)

// Candidate is one completion suggestion.
type Candidate struct {
	Label  string
	Detail string
	Doc    string
	Kind   index.Kind
	Score  int
	Source string // where it came from: "local", "import", "scope", "builtin", "keyword", "word", "module"
}

// Completion is the result of a completion request.
type Completion struct {
	Items []Candidate
	// Start is the byte offset where the replaced text begins (the cursor
	// minus the typed prefix). Prefix is the typed text itself.
	Start  int
	Prefix string
	// Incomplete tells clients to re-query as the user keeps typing (true
	// whenever the list was truncated).
	Incomplete bool
}

// Source weights: added to the match score so that, for equally good
// matches, closer context wins.
const (
	wLocalDef = 400
	wImport   = 350
	wScope    = 300
	wMember   = 300
	wBuiltin  = 200
	wKeyword  = 150
	wWord     = 100
	wOtherDoc = 60
	wRecent   = 30
)

type collector struct {
	prefix string
	items  []Candidate
	seen   map[string]int
}

func newCollector(prefix string) *collector {
	return &collector{prefix: prefix, items: make([]Candidate, 0, 64), seen: make(map[string]int, 64)}
}

func (c *collector) add(label string, kind index.Kind, detail, doc string, weight int, source string) {
	score, ok := text.Match(label, c.prefix)
	if !ok {
		return
	}
	score += weight
	if i, ok := c.seen[label]; ok {
		it := &c.items[i]
		if score > it.Score {
			it.Score, it.Source = score, source
			if kind != index.KindText {
				it.Kind = kind
			}
		}
		if it.Detail == "" && detail != "" {
			it.Detail, it.Doc, it.Kind = detail, doc, kind
		}
		return
	}
	c.seen[label] = len(c.items)
	c.items = append(c.items, Candidate{Label: label, Detail: detail, Doc: doc, Kind: kind, Score: score, Source: source})
}

// addModule adds the symbols of m matching the prefix. Exact prefix matches
// come from a binary-searched range; fuzzy matches scan only the symbols
// sharing the first letter.
func (c *collector) addModule(m *index.Module, weight int, source string) {
	syms := m.Symbols
	if c.prefix == "" {
		for i := range syms {
			s := &syms[i]
			c.add(s.Name, s.Kind, s.Detail, s.Doc, weight, source)
		}
		return
	}
	var lo, hi int
	if len(c.prefix) >= 2 {
		lo, hi = m.PrefixRange(c.prefix[:1])
	} else {
		lo, hi = m.PrefixRange(c.prefix)
	}
	for i := lo; i < hi; i++ {
		s := &syms[i]
		c.add(s.Name, s.Kind, s.Detail, s.Doc, weight, source)
	}
}

func (c *collector) addWords(words map[string]uint32, weight int, skip string) {
	for w, n := range words {
		if w == skip && n <= 1 {
			continue
		}
		c.add(w, index.KindText, "", "", weight+int(min(n, 50)), "word")
	}
}

func (c *collector) finish(limit int) ([]Candidate, bool) {
	slices.SortFunc(c.items, func(a, b Candidate) int {
		if a.Score != b.Score {
			return b.Score - a.Score
		}
		return strings.Compare(a.Label, b.Label)
	})
	if len(c.items) > limit {
		return c.items[:limit], true
	}
	return c.items, false
}

// Complete computes completions at byte offset off of document d.
//
// explicit reports that the user asked for completion (e.g. Ctrl+Space)
// rather than it being fired by a trigger character; with no typed prefix
// outside of a member or import context, only explicit requests list the
// whole scope (otherwise typing "x:" in Python would pop up every symbol).
func (e *Engine) Complete(d *Document, off int, explicit bool) Completion {
	if off > len(d.Text) {
		off = len(d.Text)
	}
	L := d.Lang
	before := d.lineBefore(off)

	// 1. Module paths: `import "net/ht|`, `from os.pa|`, `#include <sys/|`.
	if partial, ok := L.ImportPathPartial(before); ok {
		return e.completeModulePath(L, partial, off)
	}
	an := e.analyze(d, true)

	// 2. Names inside "from module import |".
	if mod, partial, ok := L.ImportNamePartial(before); ok {
		c := newCollector(partial)
		for _, m := range L.Modules(e.canonical(d, mod, strings.HasPrefix(mod, "."))) {
			c.addModule(m, wImport, "import")
		}
		items, trunc := c.finish(e.cfg.Limit)
		return Completion{Items: items, Start: off - len(partial), Prefix: partial, Incomplete: trunc}
	}

	start := L.Ident.IdentBefore(d.Text, off)
	prefix := string(d.Text[start:off])
	if prefix != "" && text.IsDigit(prefix[0]) {
		return Completion{Start: off, Prefix: ""} // a number literal
	}
	c := newCollector(prefix)

	// 3. Member access: `os.path.jo|`, `std::fs::|`, `obj.|`.
	if qual, ok := e.qualifierBefore(L, d.Text, start); ok {
		if text.IsDigit(qual[0]) {
			return Completion{Start: off} // "3." is a float literal, not a member
		}
		e.completeMembers(c, d, an, qual, off)
		items, trunc := c.finish(e.cfg.Limit)
		return Completion{Items: items, Start: start, Prefix: prefix, Incomplete: trunc || prefix == ""}
	}

	// 4. Plain identifier: everything in scope plus words from context.
	if prefix == "" && !explicit {
		return Completion{Start: off}
	}
	for i := range an.defs {
		def := &an.defs[i]
		if def.Container == "" {
			c.add(def.Name, def.Kind, def.Detail, def.Doc, wLocalDef, "local")
		}
	}
	for alias, mod := range an.aliases {
		c.add(alias, index.KindModule, "module "+mod, "", wImport, "import")
	}
	for local, ref := range an.names {
		if !text.HasFoldPrefix(local, prefix[:min(1, len(prefix))]) {
			continue
		}
		if sym := e.findSymbol(L, ref.module, ref.name); sym != nil {
			c.add(local, sym.Kind, sym.Detail, sym.Doc, wImport, "import")
		} else {
			c.add(local, index.KindVariable, "from "+ref.module, "", wImport, "import")
		}
	}
	for _, m := range an.mods {
		w, src := wScope, "scope"
		if slices.Contains(L.Autoload, m.Name) {
			w, src = wBuiltin, "builtin"
		}
		c.addModule(m, w, src)
	}
	for _, kw := range L.Keywords {
		c.add(kw, index.KindKeyword, "", "", wKeyword, "keyword")
	}
	e.addContextWords(c, d, off, prefix, false)

	items, trunc := c.finish(e.cfg.Limit)
	return Completion{Items: items, Start: start, Prefix: prefix, Incomplete: trunc || len(prefix) < 2}
}

// addContextWords adds identifiers from the current buffer (scanned live, so
// they are never stale), other open buffers and recently closed buffers.
func (e *Engine) addContextWords(c *collector, d *Document, off int, prefix string, members bool) {
	L := d.Lang
	// Live scan of the current buffer, skipping the word under the cursor.
	cur := make(map[string]uint32)
	pos := 0
	L.Ident.Words(d.Text, 3, func(w []byte, member bool) {
		if member != members {
			return
		}
		if len(prefix) > 0 && (len(w) < len(prefix) || !sameFirst(w[0], prefix[0])) {
			return
		}
		// Words returns sub-slices of d.Text: compute the offset cheaply.
		pos = cap(d.Text) - cap(w)
		if pos <= off && off <= pos+len(w) {
			return // the identifier being typed
		}
		cur[string(w)]++
	})
	c.addWords(cur, wWord, "")
	for _, od := range e.docs {
		if od == d || od.Lang != L {
			continue
		}
		ws := od.wordStats()
		if members {
			c.addWords(ws.members, wOtherDoc, "")
		} else {
			c.addWords(ws.words, wOtherDoc, "")
		}
	}
	for _, ws := range e.recent {
		if ws.lang != L {
			continue
		}
		if members {
			c.addWords(ws.members, wRecent, "")
		} else {
			c.addWords(ws.words, wRecent, "")
		}
	}
}

func sameFirst(a, b byte) bool {
	if a >= 'A' && a <= 'Z' {
		a += 32
	}
	if b >= 'A' && b <= 'Z' {
		b += 32
	}
	return a == b
}

// completeMembers fills c with the members reachable through qual.
func (e *Engine) completeMembers(c *collector, d *Document, an *analysis, qual string, off int) {
	L := d.Lang
	if qual != "?" {
		for _, name := range e.resolveQualifier(d, an, qual) {
			for _, m := range L.Modules(name) {
				c.addModule(m, wMember, "member")
			}
		}
		// Containers defined in this very buffer: `MyClass.` or `self.`.
		cont := lastSegment(L, qual)
		if qual == "self" || qual == "this" || qual == "Self" {
			cont = enclosingContainer(an.defs, d, off)
		}
		if cont != "" {
			for i := range an.defs {
				def := &an.defs[i]
				if def.Container == cont {
					c.add(def.Name, def.Kind, def.Detail, def.Doc, wLocalDef, "local")
				}
			}
		}
	}
	if len(c.items) == 0 {
		// Unknown receiver type (no type inference): fall back to names that
		// have been used after a '.' anywhere in the open buffers.
		e.addContextWords(c, d, off, c.prefix, true)
	}
}

// enclosingContainer returns the container of the closest definition above
// off that belongs to one (the class whose method the cursor is in).
func enclosingContainer(defs []lang.Def, d *Document, off int) string {
	line, _ := d.Position(off)
	best := ""
	for i := range defs {
		if defs[i].Line > line {
			break
		}
		if defs[i].Container != "" {
			best = defs[i].Container
		} else if defs[i].Kind == index.KindClass || defs[i].Kind == index.KindStruct {
			best = defs[i].Name
		}
	}
	return best
}

func (e *Engine) completeModulePath(L *Language, partial string, off int) Completion {
	c := newCollector(partial)
	sep := L.Module.Separator
	seen := make(map[string]bool)
	for _, r := range L.Indexes() {
		lo, hi := r.PrefixRange(partial)
		for i := lo; i < hi; i++ {
			if r.IsContainer(i) {
				continue
			}
			name := r.ModuleName(i)
			// Offer one level at a time, like file path completion:
			// "net/" -> "net/http", "net/url", ...
			label := name
			if j := strings.Index(name[len(partial):], sep); j > 0 {
				label = name[:len(partial)+j]
			}
			if seen[label] {
				continue
			}
			seen[label] = true
			detail := "module"
			if label != name {
				detail = "package"
			}
			c.add(label, index.KindModule, detail, "", 0, "module")
		}
	}
	items, trunc := c.finish(e.cfg.Limit)
	return Completion{Items: items, Start: off - len(partial), Prefix: partial, Incomplete: trunc}
}

// qualifierBefore returns the member-access expression ending right before
// start ("os.path" for "os.path.jo|"). ok=false when start is not preceded
// by a member separator. The special qualifier "?" means "a member of
// something we cannot name", such as a call result: foo().bar|.
func (e *Engine) qualifierBefore(L *Language, src []byte, start int) (string, bool) {
	n := L.sepBefore(src, start)
	if n == 0 {
		return "", false
	}
	end := start - n
	i := L.Ident.IdentBefore(src, end)
	if i == end {
		return "?", true // nothing nameable before the separator: ").", "]."
	}
	for {
		s := L.sepBefore(src, i)
		if s == 0 {
			break
		}
		k := L.Ident.IdentBefore(src, i-s)
		if k == i-s {
			return "?", true // the chain starts with a call or index: "f().x."
		}
		i = k
	}
	return string(src[i:end]), true
}

// sepBefore returns the length of the member separator ending at off, or 0.
func (L *Language) sepBefore(src []byte, off int) int {
	for _, s := range L.seps {
		if off >= len(s) && string(src[off-len(s):off]) == s {
			// "::" must not be mistaken for ":" + ":" and a ":" separator
			// (Lua methods) must not fire inside "::".
			if s == ":" && off >= 2 && src[off-2] == ':' {
				continue
			}
			return len(s)
		}
	}
	return 0
}

// splitQualifier splits "a.b::c" into its identifier segments and returns
// the end offset of each segment inside q.
func (L *Language) splitQualifier(q string) (segs []string, ends []int) {
	b := []byte(q)
	i := 0
	for i < len(b) {
		j := L.Ident.IdentAfter(b, i)
		if j > i {
			segs = append(segs, q[i:j])
			ends = append(ends, j)
			i = j
			continue
		}
		i++
	}
	return segs, ends
}

func lastSegment(L *Language, q string) string {
	segs, _ := L.splitQualifier(q)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}

// resolveQualifier maps a qualifier expression to the names of the modules
// whose members it exposes. Resolution order:
//
//  1. an import alias            ("np" -> "numpy")
//  2. a name imported from a module ("OrderedDict" -> "collections.OrderedDict")
//  3. the qualifier as a module  ("os.path", Lua "string")
//  4. head resolved recursively + tail as sub-module or container
//     ("np.linalg" -> "numpy.linalg", "http.Client" -> "net/http.Client")
//  5. a module whose last segment equals the qualifier ("http" -> "net/http")
func (e *Engine) resolveQualifier(d *Document, an *analysis, qual string) []string {
	return e.resolve(d.Lang, an, qual, 0)
}

func (e *Engine) resolve(L *Language, an *analysis, qual string, depth int) []string {
	if depth > 4 {
		return nil
	}
	var out []string
	addIf := func(name string) {
		if L.HasModule(name) && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	if an != nil {
		if mod, ok := an.aliases[qual]; ok {
			addIf(mod)
		}
		if ref, ok := an.names[qual]; ok {
			addIf(lang.ContainerModule(ref.module, ref.name))
			addIf(ref.module + L.Module.Separator + ref.name)
		}
		if len(out) > 0 {
			return out
		}
	}
	addIf(qual)
	if norm := L.normalizeQualifier(qual); norm != qual {
		addIf(norm)
	}
	if len(out) > 0 {
		return out
	}
	segs, ends := L.splitQualifier(qual)
	if len(segs) > 1 {
		// Find the longest head that resolves, then append the tail.
		for k := len(segs) - 1; k >= 1; k-- {
			for _, base := range e.resolve(L, an, qual[:ends[k-1]], depth+1) {
				tail := segs[k:]
				addIf(base + L.Module.Separator + strings.Join(tail, L.Module.Separator))
				addIf(lang.ContainerModule(base, strings.Join(tail, ".")))
			}
			if len(out) > 0 {
				return out
			}
		}
		return out
	}
	// Scope-imported containers: a class from a C header or a star import.
	if an != nil {
		for _, m := range an.mods {
			addIf(lang.ContainerModule(m.Name, qual))
		}
		if len(out) > 0 {
			return out
		}
	}
	for _, name := range L.ModulesByLastSegment(qual) {
		addIf(name)
		if len(out) >= 4 {
			break
		}
	}
	return out
}

// normalizeQualifier rewrites a qualifier written with member separators
// using the module separator, e.g. Rust "std.io" -> "std::io" or Go-style
// paths. For most languages it is the identity.
func (L *Language) normalizeQualifier(q string) string {
	segs, _ := L.splitQualifier(q)
	if len(segs) < 2 {
		return q
	}
	return strings.Join(segs, L.Module.Separator)
}

// findSymbol looks name up in the module(s) called module.
func (e *Engine) findSymbol(L *Language, module, name string) *index.Symbol {
	for _, m := range L.Modules(module) {
		if s, ok := m.Get(name); ok {
			return s
		}
	}
	return nil
}
