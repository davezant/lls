package lang

import (
	"bytes"
	"strings"

	"lls/internal/index"
)

// Def is a definition found by the extractor.
type Def struct {
	Name      string
	Detail    string
	Doc       string
	Kind      index.Kind
	Container string // enclosing container (class/receiver), "" at top level
	Module    string // module declared inside the file (Module.Declared), usually ""
	Line      int    // zero-based line number
}

// Reexport is a module re-exported by a file (C include, "export * from").
type Reexport struct {
	Module   string
	Relative bool // written relative to the current file
}

// FileInfo is the result of extracting one file.
type FileInfo struct {
	Defs      []Def
	Reexports []Reexport
}

// ExtractOptions tunes extraction.
type ExtractOptions struct {
	// All disables the visibility filter (used for the user's own buffers,
	// where private helpers are just as relevant as public API).
	All bool
	// Module is the module the file belongs to (for visibility exemptions).
	Module string
	// NoDocs skips documentation gathering (faster for live buffers where
	// only names, kinds and lines are needed).
	NoDocs bool
}

// extractor holds the per-file scanning state.
type extractor struct {
	l       *Lang
	opts    ExtractOptions
	lines   [][]byte
	comment []string // doc comment lines collected above the current line
	inBlock bool     // inside a block comment
	out     FileInfo
}

// Extract scans src line by line and returns its definitions and re-exports.
//
// The scanner is a single pass with a tiny amount of state: the doc comment
// collected above the current line, the active block (Go "const (") and the
// active indentation container (Python class). Rules are tried in order and
// the first match wins; a literal hint lets most lines skip regex matching.
func (l *Lang) Extract(src []byte, opts ExtractOptions) *FileInfo {
	e := extractor{l: l, opts: opts, lines: splitLines(src)}
	e.run()
	return &e.out
}

func splitLines(src []byte) [][]byte {
	n := bytes.Count(src, []byte{'\n'}) + 1
	lines := make([][]byte, 0, n)
	for len(src) > 0 {
		i := bytes.IndexByte(src, '\n')
		if i < 0 {
			lines = append(lines, bytes.TrimSuffix(src, []byte{'\r'}))
			break
		}
		lines = append(lines, bytes.TrimSuffix(src[:i], []byte{'\r'}))
		src = src[i+1:]
	}
	return lines
}

// indentOf returns the indentation width of line (tabs count as 4).
func indentOf(line []byte) int {
	w := 0
	for _, c := range line {
		switch c {
		case ' ':
			w++
		case '\t':
			w += 4
		default:
			return w
		}
	}
	return w
}

func (e *extractor) run() {
	l := e.l
	var (
		curBlock  *block
		contName  string
		contInd   = -1
		declMod   string
		hasBlocks = len(l.blocks) > 0
	)
	var blockOpen, blockClose string
	if len(l.BlockComment) == 2 {
		blockOpen, blockClose = l.BlockComment[0], l.BlockComment[1]
	}

	for i := 0; i < len(e.lines); i++ {
		line := e.lines[i]
		trimmed := bytes.TrimSpace(line)

		// Block comments: collect their text as documentation.
		if e.inBlock {
			if j := bytes.Index(trimmed, []byte(blockClose)); j >= 0 {
				e.addComment(trimmed[:j])
				e.inBlock = false
			} else {
				e.addComment(trimmed)
			}
			continue
		}
		if len(trimmed) == 0 {
			e.comment = e.comment[:0]
			continue
		}
		if blockOpen != "" && bytes.HasPrefix(trimmed, []byte(blockOpen)) {
			e.comment = e.comment[:0]
			rest := trimmed[len(blockOpen):]
			if j := bytes.Index(rest, []byte(blockClose)); j >= 0 {
				e.addComment(rest[:j])
				if len(bytes.TrimSpace(rest[j+len(blockClose):])) == 0 {
					continue
				}
			} else {
				e.addComment(rest)
				e.inBlock = true
				continue
			}
		}
		if p := l.lineCommentPrefix(trimmed); p != "" {
			e.addComment(trimmed[len(p):])
			continue
		}

		indent := indentOf(line)
		if contInd >= 0 && indent <= contInd {
			contName, contInd = "", -1
		}

		// Named blocks (e.g. Go "const (" ... ")").
		if curBlock != nil {
			if curBlock.end.Match(line) {
				curBlock = nil
				e.comment = e.comment[:0]
				continue
			}
		} else if hasBlocks {
			opened := false
			for bi := range l.blocks {
				if l.blocks[bi].start.Match(line) {
					curBlock = &l.blocks[bi]
					opened = true
					break
				}
			}
			if opened {
				e.comment = e.comment[:0]
				continue
			}
		}

		if l.declared != nil {
			if m := l.declared.FindSubmatch(line); m != nil {
				if idx := l.declared.SubexpIndex("module"); idx > 0 && m[idx] != nil {
					declMod = string(m[idx])
					if l.importStrip != nil {
						declMod = l.importStrip.ReplaceAllString(declMod, "")
					}
				}
				e.comment = e.comment[:0]
				continue
			}
		}

		for _, re := range l.reexports {
			m := re.FindSubmatch(line)
			if m == nil {
				continue
			}
			if idx := re.SubexpIndex("module"); idx > 0 && m[idx] != nil {
				e.out.Reexports = append(e.out.Reexports, Reexport{Module: string(m[idx])})
			} else if idx := re.SubexpIndex("rel"); idx > 0 && m[idx] != nil {
				e.out.Reexports = append(e.out.Reexports, Reexport{Module: string(m[idx]), Relative: true})
			}
		}

		for ri := range l.symRules {
			r := &l.symRules[ri]
			if curBlock != nil {
				if r.blockName != curBlock.name {
					continue
				}
			} else if r.blockName != "" {
				continue
			}
			inCont := contInd >= 0
			if (r.Scope == "top" && inCont) || (r.Scope == "container" && !inCont) {
				continue
			}
			if r.Local && !e.opts.All {
				continue
			}
			if r.hint != nil && !bytes.Contains(line, r.hint) {
				continue
			}
			subject, last := line, i
			m := r.re.FindSubmatchIndex(subject)
			if m == nil && r.ContinueUntil != "" && !endsWithAny(trimmed, r.ContinueUntil) {
				subject, last = e.joinLines(i, r.ContinueUntil)
				m = r.re.FindSubmatchIndex(subject)
			}
			if m == nil || m[2*r.nameIdx] < 0 {
				continue
			}
			name := string(subject[m[2*r.nameIdx]:m[2*r.nameIdx+1]])
			if l.keywords[name] {
				continue
			}
			container := ""
			if r.contIdx > 0 && m[2*r.contIdx] >= 0 {
				container = string(subject[m[2*r.contIdx]:m[2*r.contIdx+1]])
			} else if r.Scope == "container" {
				container = contName
			}
			if r.Opens {
				contName, contInd = name, indent
			}
			if r.emit && (e.opts.All || (l.Visible(name, e.opts.Module) &&
				(container == "" || l.Visible(container, e.opts.Module)))) {
				d := Def{
					Name:      name,
					Kind:      r.kind,
					Container: container,
					Module:    declMod,
					Line:      i,
					Detail:    l.cleanDetail(subject[m[0]:m[1]]),
				}
				if !e.opts.NoDocs {
					d.Doc = e.doc(last)
				}
				e.out.Defs = append(e.out.Defs, d)
			}
			break
		}
		e.comment = e.comment[:0]
	}
}

func endsWithAny(b []byte, chars string) bool {
	return len(b) > 0 && strings.IndexByte(chars, b[len(b)-1]) >= 0
}

// joinLines joins line i with up to 8 following lines until one ends with a
// terminator character, returning the joined text and the last line used.
func (e *extractor) joinLines(i int, term string) ([]byte, int) {
	var buf []byte
	buf = append(buf, bytes.TrimRight(e.lines[i], " \t")...)
	last := i
	for j := i + 1; j < len(e.lines) && j <= i+8; j++ {
		t := bytes.TrimSpace(e.lines[j])
		buf = append(buf, ' ')
		buf = append(buf, t...)
		last = j
		if endsWithAny(t, term) {
			break
		}
	}
	return buf, last
}

func (l *Lang) lineCommentPrefix(trimmed []byte) string {
	for _, p := range l.LineComments {
		if bytes.HasPrefix(trimmed, []byte(p)) {
			return p
		}
	}
	return ""
}

func (e *extractor) addComment(b []byte) {
	b = bytes.TrimSpace(b)
	b = bytes.TrimLeft(b, "*!/")
	e.comment = append(e.comment, string(bytes.TrimSpace(b)))
}

// doc returns the documentation for a definition ending at line last: the
// comment block collected above it, or a docstring right after it.
func (e *extractor) doc(last int) string {
	if len(e.comment) > 0 {
		return e.l.clip(firstParagraph(e.comment))
	}
	if e.l.Docstrings {
		return e.l.clip(e.docstring(last))
	}
	return ""
}

// firstParagraph joins lines up to the first empty one.
func firstParagraph(lines []string) string {
	var sb strings.Builder
	for _, s := range lines {
		if s == "" {
			if sb.Len() > 0 {
				break
			}
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(s)
	}
	return sb.String()
}

// docstring reads a Python-style triple-quoted string following line last.
func (e *extractor) docstring(last int) string {
	j := last + 1
	for j < len(e.lines) && len(bytes.TrimSpace(e.lines[j])) == 0 {
		j++
	}
	if j >= len(e.lines) {
		return ""
	}
	t := bytes.TrimSpace(e.lines[j])
	t = bytes.TrimLeft(t, "rRuUbB")
	var q []byte
	switch {
	case bytes.HasPrefix(t, []byte(`"""`)):
		q = []byte(`"""`)
	case bytes.HasPrefix(t, []byte(`'''`)):
		q = []byte(`'''`)
	default:
		return ""
	}
	t = t[3:]
	var lines []string
	for {
		if k := bytes.Index(t, q); k >= 0 {
			lines = append(lines, string(bytes.TrimSpace(t[:k])))
			break
		}
		lines = append(lines, string(bytes.TrimSpace(t)))
		j++
		if j >= len(e.lines) || j > last+40 {
			break
		}
		t = e.lines[j]
	}
	return firstParagraph(lines)
}

// clip truncates documentation at a word boundary.
func (l *Lang) clip(s string) string {
	if len(s) <= l.maxDoc {
		return s
	}
	cut := strings.LastIndexByte(s[:l.maxDoc], ' ')
	if cut < l.maxDoc/2 {
		cut = l.maxDoc
	}
	return s[:cut] + "…"
}

// cleanDetail normalises a matched definition into a one-line signature.
func (l *Lang) cleanDetail(b []byte) string {
	s := string(b)
	for _, re := range l.detailStrip {
		s = re.ReplaceAllString(s, "")
	}
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(s, " {;:=")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}
