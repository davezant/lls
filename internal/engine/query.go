package engine

import (
	"strings"

	"lls/internal/index"
	"lls/internal/lang"
)

// SymbolInfo describes a resolved symbol (hover, signature help).
type SymbolInfo struct {
	Name   string
	Detail string
	Doc    string
	Kind   index.Kind
	Module string // module it was found in ("" for buffer definitions)
}

// Location is a definition site.
type Location struct {
	Path string
	Line int // zero-based
}

// identAt returns the identifier under off (start, end) and its qualifier.
func (e *Engine) identAt(d *Document, off int) (name, qual string, start int) {
	L := d.Lang
	if off > len(d.Text) {
		off = len(d.Text)
	}
	start = L.Ident.IdentBefore(d.Text, off)
	end := L.Ident.IdentAfter(d.Text, off)
	if start == end {
		return "", "", off
	}
	name = string(d.Text[start:end])
	if q, ok := e.qualifierBefore(L, d.Text, start); ok && q != "?" {
		qual = q
	}
	return name, qual, start
}

// Hover describes the symbol under the cursor.
func (e *Engine) Hover(d *Document, off int) (SymbolInfo, bool) {
	name, qual, _ := e.identAt(d, off)
	if name == "" {
		return SymbolInfo{}, false
	}
	return e.lookup(d, e.analyze(d, false), qual, name, off)
}

// lookup resolves qual.name (or a bare name when qual is empty).
func (e *Engine) lookup(d *Document, an *analysis, qual, name string, off int) (SymbolInfo, bool) {
	L := d.Lang
	if qual != "" {
		for _, mod := range e.resolveQualifier(d, an, qual) {
			if s := e.findSymbol(L, mod, name); s != nil {
				return SymbolInfo{Name: s.Name, Detail: s.Detail, Doc: s.Doc, Kind: s.Kind, Module: mod}, true
			}
		}
		cont := lastSegment(L, qual)
		if qual == "self" || qual == "this" || qual == "Self" {
			cont = enclosingContainer(an.defs, d, off)
		}
		for i := range an.defs {
			def := &an.defs[i]
			if def.Name == name && def.Container == cont {
				return defInfo(def), true
			}
		}
		return SymbolInfo{}, false
	}
	for i := range an.defs {
		def := &an.defs[i]
		if def.Name == name && def.Container == "" {
			return defInfo(def), true
		}
	}
	if ref, ok := an.names[name]; ok {
		if s := e.findSymbol(L, ref.module, ref.name); s != nil {
			return SymbolInfo{Name: name, Detail: s.Detail, Doc: s.Doc, Kind: s.Kind, Module: ref.module}, true
		}
		if L.HasModule(ref.module + L.Module.Separator + ref.name) {
			return SymbolInfo{Name: name, Detail: "module " + ref.module + L.Module.Separator + ref.name, Kind: index.KindModule}, true
		}
	}
	if mod, ok := an.aliases[name]; ok {
		return SymbolInfo{Name: name, Detail: "module " + mod, Kind: index.KindModule, Module: mod}, true
	}
	for _, m := range an.mods {
		if s, ok := m.Get(name); ok {
			return SymbolInfo{Name: s.Name, Detail: s.Detail, Doc: s.Doc, Kind: s.Kind, Module: m.Name}, true
		}
	}
	for i := range an.defs { // a method name used without qualifier
		def := &an.defs[i]
		if def.Name == name {
			return defInfo(def), true
		}
	}
	return SymbolInfo{}, false
}

func defInfo(def *lang.Def) SymbolInfo {
	return SymbolInfo{Name: def.Name, Detail: def.Detail, Doc: def.Doc, Kind: def.Kind}
}

// Signature is the result of signature help.
type Signature struct {
	Label  string
	Doc    string
	Params []string // parameter labels (substrings of Label)
	Active int      // index of the parameter under the cursor
}

// SignatureHelp finds the call surrounding off and describes its callee.
func (e *Engine) SignatureHelp(d *Document, off int) (Signature, bool) {
	if off > len(d.Text) {
		off = len(d.Text)
	}
	src := d.Text
	depth, commas := 0, 0
	open := -1
	limit := max(0, off-4096)
scan:
	for i := off - 1; i >= limit; i-- {
		switch src[i] {
		case ')', ']', '}':
			depth++
		case '[', '{':
			if depth == 0 {
				return Signature{}, false
			}
			depth--
		case '(':
			if depth == 0 {
				open = i
				break scan
			}
			depth--
		case ',':
			if depth == 0 {
				commas++
			}
		case ';':
			if depth == 0 {
				return Signature{}, false
			}
		}
	}
	if open < 0 {
		return Signature{}, false
	}
	end := open
	for end > 0 && (src[end-1] == ' ' || src[end-1] == '\t') {
		end--
	}
	name, qual, _ := e.identAt(d, end)
	if name == "" {
		return Signature{}, false
	}
	info, ok := e.lookup(d, e.analyze(d, false), qual, name, off)
	if !ok || !strings.Contains(info.Detail, "(") {
		return Signature{}, false
	}
	sig := Signature{Label: info.Detail, Doc: info.Doc, Params: splitParams(info.Detail)}
	sig.Active = min(commas, max(0, len(sig.Params)-1))
	return sig, true
}

// splitParams returns the top-level comma separated parameters of the first
// parenthesised list in a signature.
func splitParams(sig string) []string {
	i := strings.IndexByte(sig, '(')
	if i < 0 {
		return nil
	}
	depth := 0
	start := i + 1
	var params []string
	for j := i + 1; j < len(sig); j++ {
		switch sig[j] {
		case '(', '[', '{', '<':
			depth++
		case ']', '}', '>':
			depth--
		case ')':
			if depth == 0 {
				if p := strings.TrimSpace(sig[start:j]); p != "" {
					params = append(params, p)
				}
				return params
			}
			depth--
		case ',':
			if depth == 0 {
				params = append(params, strings.TrimSpace(sig[start:j]))
				start = j + 1
			}
		}
	}
	return params
}

// Definition finds where the symbol under the cursor is defined: in the
// buffer itself or in a project-local module (library indexes do not store
// locations, which keeps them small).
func (e *Engine) Definition(d *Document, off int) (Location, bool) {
	name, qual, _ := e.identAt(d, off)
	if name == "" {
		return Location{}, false
	}
	an := e.analyze(d, false)
	L := d.Lang
	if qual == "" {
		for i := range an.defs {
			if an.defs[i].Name == name && an.defs[i].Container == "" {
				return Location{Path: d.Path, Line: an.defs[i].Line}, true
			}
		}
		if ref, ok := an.names[name]; ok {
			if loc, ok := e.localLocation(ref.module, ref.name); ok {
				return Location{Path: loc.path, Line: loc.line}, true
			}
		}
		for _, m := range an.mods {
			if loc, ok := e.localLocation(m.Name, name); ok {
				return Location{Path: loc.path, Line: loc.line}, true
			}
		}
		for i := range an.defs {
			if an.defs[i].Name == name {
				return Location{Path: d.Path, Line: an.defs[i].Line}, true
			}
		}
		return Location{}, false
	}
	for _, mod := range e.resolveQualifier(d, an, qual) {
		if loc, ok := e.localLocation(mod, name); ok {
			return Location{Path: loc.path, Line: loc.line}, true
		}
	}
	cont := lastSegment(L, qual)
	if qual == "self" || qual == "this" || qual == "Self" {
		cont = enclosingContainer(an.defs, d, off)
	}
	for i := range an.defs {
		if an.defs[i].Name == name && an.defs[i].Container == cont {
			return Location{Path: d.Path, Line: an.defs[i].Line}, true
		}
	}
	return Location{}, false
}

// Symbols returns the definitions of the buffer (document outline).
func (e *Engine) Symbols(d *Document) []lang.Def {
	return e.analyze(d, false).defs
}
