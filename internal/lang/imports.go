package lang

import (
	"bytes"
	"strings"
)

// NameRef points at a symbol imported by name ("from os import path").
type NameRef struct {
	Module   string
	Name     string // original name in the module
	Relative bool
}

// Imports summarises the imports of one document.
type Imports struct {
	Aliases  map[string]string  // local alias -> module ("np" -> "numpy")
	Names    map[string]NameRef // local name -> imported symbol
	Scope    []string           // modules whose symbols are all in scope
	Relative map[string]bool    // modules written relative to the document
}

// ParseImports runs every import rule over the whole document.
// Go's regexp engine is linear-time, so this stays fast even on large files;
// callers additionally cache the result per document version.
func (l *Lang) ParseImports(src []byte) *Imports {
	imp := &Imports{
		Aliases:  make(map[string]string),
		Names:    make(map[string]NameRef),
		Relative: make(map[string]bool),
	}
	seenScope := make(map[string]bool)
	for ri := range l.importRules {
		r := &l.importRules[ri]
		if r.hint != nil && !bytes.Contains(src, r.hint) {
			continue
		}
		for _, m := range r.re.FindAllSubmatchIndex(src, -1) {
			group := func(idx int) string {
				if idx < 0 || m[2*idx] < 0 {
					return ""
				}
				return string(src[m[2*idx]:m[2*idx+1]])
			}
			modText, rel := group(r.modIdx), false
			if modText == "" {
				modText, rel = group(r.relIdx), true
			}
			if modText == "" && !rel {
				continue
			}
			alias := strings.TrimSpace(group(r.aliasIdx))
			names := group(r.namesIdx)

			// "import a, b as c" lists several modules in one statement.
			for _, part := range strings.Split(modText, ",") {
				mod, partAlias := splitAs(part)
				if mod == "" && !rel {
					continue
				}
				if !rel && (strings.HasPrefix(mod, "./") || strings.HasPrefix(mod, "../")) {
					rel = true // path-style relative import ("./util")
				}
				if l.importStrip != nil && !rel {
					mod = l.importStrip.ReplaceAllString(mod, "")
				}
				if rel {
					imp.Relative[mod] = true
				}
				switch {
				case names != "":
					l.addNames(imp, mod, names, rel, seenScope)
				case r.Bind == "scope":
					if !seenScope[mod] {
						seenScope[mod] = true
						imp.Scope = append(imp.Scope, mod)
					}
				default:
					a := alias
					if partAlias != "" {
						a = partAlias
					}
					if a == "" {
						a = l.DefaultAlias(mod, r.Alias)
					}
					if a != "" && a != "_" && a != "." {
						imp.Aliases[a] = mod
					} else if a == "." && !seenScope[mod] { // Go dot import
						seenScope[mod] = true
						imp.Scope = append(imp.Scope, mod)
					}
				}
			}
		}
	}
	return imp
}

func (l *Lang) addNames(imp *Imports, mod, names string, rel bool, seenScope map[string]bool) {
	names = strings.Trim(names, "(){} \t\r\n")
	for _, item := range strings.Split(names, ",") {
		item = strings.TrimSpace(item)
		item = strings.TrimPrefix(item, "type ")
		if i := strings.IndexByte(item, '#'); i >= 0 {
			item = strings.TrimSpace(item[:i])
		}
		name, local := splitAs(item)
		if name == "" {
			continue
		}
		switch name {
		case "*":
			if !seenScope[mod] {
				seenScope[mod] = true
				imp.Scope = append(imp.Scope, mod)
			}
			continue
		case "self": // Rust "use std::io::{self, Read}"
			if local == "" {
				local = l.DefaultAlias(mod, "last")
			}
			imp.Aliases[local] = mod
			continue
		}
		if local == "" {
			local = name
		}
		imp.Names[local] = NameRef{Module: mod, Name: name, Relative: rel}
	}
}

// splitAs splits "x as y" (and "x: y") into ("x", "y"); without an alias
// the second value is empty.
func splitAs(s string) (string, string) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " as "); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+4:])
	}
	if f := strings.Fields(s); len(f) == 3 && f[1] == "as" {
		return f[0], f[2]
	}
	return s, ""
}

// ImportPathPartial reports whether the text before the cursor is inside a
// module path (e.g. `import "net/ht`) and returns the typed part.
func (l *Lang) ImportPathPartial(before []byte) (partial string, ok bool) {
	if l.pathCtx == nil {
		return "", false
	}
	m := l.pathCtx.FindSubmatch(before)
	idx := l.pathCtx.SubexpIndex("partial")
	if m == nil || idx < 0 {
		return "", false
	}
	return string(m[idx]), true
}

// ImportNamePartial reports whether the cursor is in a "from module import x"
// context and returns the module and the typed part.
func (l *Lang) ImportNamePartial(before []byte) (module, partial string, ok bool) {
	if l.nameCtx == nil {
		return "", "", false
	}
	m := l.nameCtx.FindSubmatch(before)
	mi, pi := l.nameCtx.SubexpIndex("module"), l.nameCtx.SubexpIndex("partial")
	if m == nil || mi < 0 || pi < 0 {
		return "", "", false
	}
	return string(m[mi]), string(m[pi]), true
}
