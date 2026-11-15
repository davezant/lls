package lang

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"lls/internal/text"
)

// SyntaxSpec describes the structure of a language well enough to find
// structural errors without a real grammar: how blocks are delimited
// (brackets, indentation or keywords), which literals and comments must be
// skipped, and which tokens open and close what.
//
// It is deliberately shallow. The checker finds unbalanced or mismatched
// brackets, unterminated strings and comments, unclosed keyword blocks
// (Lua "function ... end") and indentation errors (Python), which covers
// the mistakes that make a whole file unparsable while it is being edited.
type SyntaxSpec struct {
	// Blocks is how statement blocks are delimited: "braces" (C, Go, JS,
	// Rust), "indent" (Python: an opener at the end of a line starts an
	// indented block) or "keywords" (Lua: do ... end).
	Blocks string `json:"blocks"`
	// Brackets lists bracket pairs as two-character strings: "()", "[]", "{}".
	Brackets []string `json:"brackets"`
	// Strings lists the literal forms whose contents are skipped.
	Strings []StringRule `json:"strings"`
	// BlockComments are extra [open, close] comment pairs checked before the
	// spec's lineComments (Lua "--[[" ... "]]"). The spec's blockComment is
	// always included.
	BlockComments [][]string `json:"blockComments"`
	// NestedComments makes block comments nest (Rust).
	NestedComments bool `json:"nestedComments"`
	// RegexLiterals enables JavaScript-style /regex/ literal detection.
	RegexLiterals bool `json:"regexLiterals"`
	// Preprocessor enables C preprocessor conditionals: every #else/#elif
	// branch starts from the state before its #if, the state after the
	// first branch is kept at #endif, and "#if 0" branches are skipped.
	Preprocessor bool `json:"preprocessor"`
	// Indent configures indentation checks for Blocks == "indent".
	Indent IndentSpec `json:"indent"`
	// Keywords lists keyword-delimited blocks (Blocks == "keywords").
	Keywords []KeywordBlock `json:"keywords"`
}

// StringRule is one string/char literal form.
type StringRule struct {
	Open      string `json:"open"`
	Close     string `json:"close"`
	Escape    string `json:"escape"`    // escape byte, usually "\\"; "" for raw strings
	Multiline bool   `json:"multiline"` // may span lines
	// Char marks a character literal ('a', '\n') that is only recognised
	// when it is complete, so Rust lifetimes ('a) are not taken as strings.
	Char bool `json:"char"`
	// Lenient ignores an unterminated literal instead of reporting it and
	// treats the opening quote as ordinary text (apostrophes in JSX text).
	Lenient bool `json:"lenient"`
	// NotAfterNumber ignores the opener right after a number, where it is a
	// digit separator (C++14 0xffff'ffff).
	NotAfterNumber bool `json:"notAfterNumber"`
	// Interpolation opens embedded code inside the literal, closed by the
	// matching "}" ("${" in JavaScript template literals).
	Interpolation string `json:"interpolation"`
	// SpaceEscapes are escape letters that skip the following whitespace,
	// newlines included (Lua "\z").
	SpaceEscapes string `json:"spaceEscapes"`
}

// IndentSpec configures indentation-delimited blocks.
type IndentSpec struct {
	// Openers are the characters that, as the last significant character of
	// a logical line, open an indented block (":" for Python).
	Openers string `json:"openers"`
	// Continuation joins a line with the next one ("\\" for Python).
	Continuation string `json:"continuation"`
	// TabWidth used to compare indentation (default 8, like Python).
	TabWidth int `json:"tabWidth"`
}

// KeywordBlock is a keyword-delimited block: any of Open starts it and any
// of Close ends it ({"open": ["function", "if", "do"], "close": ["end"]}).
type KeywordBlock struct {
	Open  []string `json:"open"`
	Close []string `json:"close"`
}

// Severity of a diagnostic, using the LSP numbering.
type Severity int

const (
	SeverityError   Severity = 1
	SeverityWarning Severity = 2
)

// Diagnostic is a problem found by Check. Start and End are byte offsets.
type Diagnostic struct {
	Start, End int
	Severity   Severity
	Message    string
}

// maxDiagnostics caps the number of diagnostics per file: after a real
// structural error the rest of the file is mostly noise.
const maxDiagnostics = 50

// syntax is the compiled form of a SyntaxSpec.
type syntax struct {
	spec      SyntaxSpec
	indent    bool
	openers   map[byte]byte // bracket open -> close
	closers   map[byte]byte // bracket close -> open
	strings   []StringRule  // longest Open first
	comments  [][2]string   // block comments, longest open first
	lineCmts  []string
	kwOpen    map[string]int // keyword -> KeywordBlock index
	kwClose   map[string][]int
	tabWidth  int
	preproc   bool
	interest  [256]bool // bytes that may start a token the checker cares about
	contByte  byte
	indentOpn string
}

func compileSyntax(l *Lang) (*syntax, error) {
	s := l.Spec.Syntax
	if s == nil {
		return nil, nil
	}
	sx := &syntax{
		spec:      *s,
		indent:    s.Blocks == "indent",
		openers:   map[byte]byte{},
		closers:   map[byte]byte{},
		kwOpen:    map[string]int{},
		kwClose:   map[string][]int{},
		tabWidth:  s.Indent.TabWidth,
		lineCmts:  l.LineComments,
		indentOpn: s.Indent.Openers,
		preproc:   s.Preprocessor,
	}
	switch s.Blocks {
	case "", "braces", "indent", "keywords":
	default:
		return nil, fmt.Errorf("lang %s: syntax.blocks must be braces, indent or keywords", l.Name)
	}
	if sx.tabWidth <= 0 {
		sx.tabWidth = 8
	}
	if len(s.Indent.Continuation) > 1 {
		return nil, fmt.Errorf("lang %s: syntax.indent.continuation must be one byte", l.Name)
	} else if len(s.Indent.Continuation) == 1 {
		sx.contByte = s.Indent.Continuation[0]
	}
	for _, p := range s.Brackets {
		if len(p) != 2 {
			return nil, fmt.Errorf("lang %s: syntax bracket %q must be two characters", l.Name, p)
		}
		sx.openers[p[0]] = p[1]
		sx.closers[p[1]] = p[0]
		sx.interest[p[0]], sx.interest[p[1]] = true, true
	}
	for _, r := range s.Strings {
		if r.Open == "" || r.Close == "" {
			return nil, fmt.Errorf("lang %s: syntax string needs open and close", l.Name)
		}
		if r.Interpolation != "" && !strings.HasSuffix(r.Interpolation, "{") {
			return nil, fmt.Errorf("lang %s: syntax string interpolation must end with \"{\"", l.Name)
		}
		if len(r.Escape) > 1 {
			return nil, fmt.Errorf("lang %s: syntax string escape must be one byte", l.Name)
		}
		sx.strings = append(sx.strings, r)
		sx.interest[r.Open[0]] = true
	}
	sort.SliceStable(sx.strings, func(i, j int) bool { return len(sx.strings[i].Open) > len(sx.strings[j].Open) })
	pairs := append([][]string(nil), s.BlockComments...)
	if len(l.BlockComment) == 2 {
		pairs = append(pairs, l.BlockComment)
	}
	for _, p := range pairs {
		if len(p) != 2 || p[0] == "" || p[1] == "" {
			return nil, fmt.Errorf("lang %s: syntax block comment needs [open, close]", l.Name)
		}
		sx.comments = append(sx.comments, [2]string{p[0], p[1]})
		sx.interest[p[0][0]] = true
	}
	sort.SliceStable(sx.comments, func(i, j int) bool { return len(sx.comments[i][0]) > len(sx.comments[j][0]) })
	for _, c := range l.LineComments {
		if c != "" {
			sx.interest[c[0]] = true
		}
	}
	for i, kb := range s.Keywords {
		if len(kb.Open) == 0 || len(kb.Close) == 0 {
			return nil, fmt.Errorf("lang %s: syntax keyword block %d needs open and close", l.Name, i)
		}
		for _, w := range kb.Open {
			sx.kwOpen[w] = i
		}
		for _, w := range kb.Close {
			sx.kwClose[w] = append(sx.kwClose[w], i)
		}
	}
	if s.RegexLiterals {
		sx.interest['/'] = true
	}
	sx.interest['\n'] = true
	return sx, nil
}

// HasSyntax reports whether the language has a syntax section (Check is a
// no-op otherwise).
func (l *Lang) HasSyntax() bool { return l.syntax != nil }

// frame is an open bracket or keyword block.
type frame struct {
	start, end int    // span of the opening token
	open       string // opening token as written
	close      byte   // closing bracket (bracket frames)
	kw         int    // keyword block index, -1 for brackets
	// tmpl is set on a string interpolation frame: closing it resumes the
	// literal that started at strStart.
	tmpl     *StringRule
	strStart int
}

// ppCond is an open preprocessor conditional.
type ppCond struct {
	saved    []frame // stack before the #if
	first    []frame // stack after the first live branch
	hasFirst bool
	skip     bool // inside an "#if 0" branch
}

type checker struct {
	l     *Lang
	sx    *syntax
	src   []byte
	stack []frame
	out   []Diagnostic

	lastSig  byte   // last significant byte on the current line
	lastWord []byte // last identifier (for regex detection)

	// indentation state
	levels      []int
	pendingOpen int  // offset of the opener awaiting an indented block, -1 if none
	continued   bool // previous line ended with a continuation byte

	pp []ppCond
}

// Check finds structural syntax errors in src. It returns nil when the
// language has no syntax section.
func (l *Lang) Check(src []byte) []Diagnostic {
	if l.syntax == nil {
		return nil
	}
	c := checker{l: l, sx: l.syntax, src: src, levels: []int{0}, pendingOpen: -1}
	c.run()
	return c.out
}

func (c *checker) report(start, end int, sev Severity, format string, args ...any) {
	if len(c.out) >= maxDiagnostics {
		return
	}
	if end <= start {
		end = start + 1
	}
	if end > len(c.src) {
		end = len(c.src)
	}
	if start > len(c.src) {
		start = len(c.src)
	}
	c.out = append(c.out, Diagnostic{Start: start, End: end, Severity: sev, Message: fmt.Sprintf(format, args...)})
}

func (c *checker) run() {
	src, sx, id := c.src, c.sx, c.l.Ident
	i := 0
	for i < len(src) {
		if i == 0 || src[i-1] == '\n' {
			if sx.preproc {
				// n == i on an empty line: let the newline be handled below.
				if n, skip := c.directive(i); skip && n > i {
					i = n
					continue
				}
			}
			if sx.indent {
				c.lineStart(i)
			}
		}
		b := src[i]
		if b == ' ' || b == '\t' || b == '\r' {
			i++
			continue
		}
		if b == '\n' {
			c.endLine(i)
			i++
			continue
		}
		if sx.interest[b] {
			if n, ok := c.special(i); ok {
				i = n
				continue
			}
		}
		if id[b] {
			j := id.IdentAfter(src, i)
			c.word(i, j)
			i = j
			continue
		}
		c.lastSig = b
		i++
	}
	c.endLine(len(src))
	c.finish()
}

// special handles comments, strings, regex literals and brackets at i. It
// returns the offset after the token and whether anything was consumed.
func (c *checker) special(i int) (int, bool) {
	src, sx := c.src, c.sx
	rest := src[i:]
	for _, bc := range sx.comments {
		if bytes.HasPrefix(rest, []byte(bc[0])) {
			return c.blockComment(i, bc), true
		}
	}
	for _, lc := range sx.lineCmts {
		if lc != "" && bytes.HasPrefix(rest, []byte(lc)) {
			if j := bytes.IndexByte(rest, '\n'); j >= 0 {
				return i + j, true
			}
			return len(src), true
		}
	}
	for ri := range sx.strings {
		r := &sx.strings[ri]
		if bytes.HasPrefix(rest, []byte(r.Open)) {
			if r.NotAfterNumber && i > 0 && c.l.Ident[src[i-1]] && len(c.lastWord) > 0 &&
				text.IsDigit(c.lastWord[0]) && c.lastWord[len(c.lastWord)-1] == src[i-1] {
				j := c.l.Ident.IdentAfter(src, i+1)
				if j > i+1 {
					c.word(i+1, j)
				}
				return j, true
			}
			if n, ok := c.str(i, r); ok {
				c.lastSig = '"'
				return n, true
			}
			break // not a literal here (lifetime, lenient quote)
		}
	}
	b := src[i]
	if b == '/' && sx.spec.RegexLiterals && c.regexAllowed() {
		if n, ok := c.regex(i); ok {
			c.lastSig = 'r'
			return n, true
		}
	}
	if cl, ok := sx.openers[b]; ok {
		c.stack = append(c.stack, frame{start: i, end: i + 1, open: string(b), close: cl, kw: -1})
		c.lastSig = b
		return i + 1, true
	}
	if _, ok := sx.closers[b]; ok {
		if n := len(c.stack); n > 0 && c.stack[n-1].tmpl != nil && c.stack[n-1].close == b {
			f := c.stack[n-1]
			c.stack = c.stack[:n-1]
			end, _ := c.strBody(f.strStart, i+1, f.tmpl)
			c.lastSig = '"'
			return end, true
		}
		c.closeToken(i, i+1, src[i:i+1], func(f *frame) bool { return f.kw < 0 && f.close == b })
		c.lastSig = b
		return i + 1, true
	}
	return i, false
}

func (c *checker) blockComment(i int, bc [2]string) int {
	src := c.src
	open, close := []byte(bc[0]), []byte(bc[1])
	depth := 1
	j := i + len(open)
	for j < len(src) {
		if c.sx.spec.NestedComments && bytes.HasPrefix(src[j:], open) {
			depth++
			j += len(open)
			continue
		}
		if bytes.HasPrefix(src[j:], close) {
			depth--
			j += len(close)
			if depth == 0 {
				return j
			}
			continue
		}
		j++
	}
	c.report(i, i+len(open), SeverityError, "unterminated comment: missing %q", bc[1])
	return len(src)
}

// str scans a string literal starting at i.
func (c *checker) str(i int, r *StringRule) (int, bool) {
	if r.Char {
		return c.char(i, r)
	}
	return c.strBody(i, i+len(r.Open), r)
}

// strBody scans the body of a literal that started at i from offset j.
func (c *checker) strBody(i, j int, r *StringRule) (int, bool) {
	src := c.src
	close := []byte(r.Close)
	var esc byte
	hasEsc := r.Escape != ""
	if hasEsc {
		esc = r.Escape[0]
	}
	for j < len(src) {
		b := src[j]
		if hasEsc && b == esc {
			j++
			if j < len(src) && r.SpaceEscapes != "" && strings.IndexByte(r.SpaceEscapes, src[j]) >= 0 {
				j++
				for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\r' || src[j] == '\n') {
					j++
				}
				continue
			}
			if j+1 < len(src) && src[j] == '\r' && src[j+1] == '\n' {
				j++
			}
			j++
			continue
		}
		if r.Interpolation != "" && b == r.Interpolation[0] && bytes.HasPrefix(src[j:], []byte(r.Interpolation)) {
			end := j + len(r.Interpolation)
			c.stack = append(c.stack, frame{start: j, end: end, open: r.Interpolation, close: '}', kw: -1, tmpl: r, strStart: i})
			return end, true
		}
		if b == close[0] && bytes.HasPrefix(src[j:], close) {
			return j + len(close), true
		}
		if b == '\n' && !r.Multiline {
			if r.Lenient {
				return i, false
			}
			end := j
			if end > i && src[end-1] == '\r' {
				end--
			}
			c.report(i, end, SeverityError, "unterminated string literal: missing %s", r.Close)
			return j, true // resume at the newline
		}
		j++
	}
	if r.Lenient {
		return i, false
	}
	end := len(src)
	if k := bytes.IndexByte(src[i:], '\n'); k >= 0 {
		end = i + k
	}
	c.report(i, end, SeverityError, "unterminated string literal: missing %s", r.Close)
	return len(src), true
}

// char recognises a complete character literal: one rune or one escape
// sequence (at most 10 bytes) between the delimiters.
func (c *checker) char(i int, r *StringRule) (int, bool) {
	src := c.src
	j := i + len(r.Open)
	if j >= len(src) || src[j] == '\n' {
		return i, false
	}
	if r.Escape != "" && src[j] == r.Escape[0] {
		lim := min(len(src), j+12)
		for k := j + 2; k < lim; k++ {
			if src[k] == '\n' {
				break
			}
			if bytes.HasPrefix(src[k:], []byte(r.Close)) {
				return k + len(r.Close), true
			}
		}
		return i, false
	}
	_, size := utf8.DecodeRune(src[j:])
	k := j + size
	if bytes.HasPrefix(src[k:], []byte(r.Close)) {
		return k + len(r.Close), true
	}
	return i, false
}

// regexAllowed decides whether a '/' starts a regex literal rather than a
// division, from the previous significant token.
func (c *checker) regexAllowed() bool {
	switch c.lastSig {
	case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', ';', '+', '-', '*', '%', '~', '^':
		return true
	}
	if c.l.Ident[c.lastSig] {
		switch string(c.lastWord) {
		case "return", "typeof", "case", "do", "else", "in", "of", "new", "delete",
			"void", "throw", "yield", "await", "instanceof":
			return true
		}
	}
	return false
}

// regex scans a /regex/flags literal on a single line.
func (c *checker) regex(i int) (int, bool) {
	src := c.src
	inClass := false
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				if j == i+1 {
					return i, false // "//" is a comment, handled elsewhere
				}
				return c.l.Ident.IdentAfter(src, j+1), true
			}
		case '\n':
			return i, false
		}
	}
	return i, false
}

// word handles an identifier spanning [i, j): keyword blocks.
func (c *checker) word(i, j int) {
	src, sx := c.src, c.sx
	w := src[i:j]
	c.lastSig = src[j-1]
	c.lastWord = w
	if len(sx.kwOpen) == 0 || (i > 0 && src[i-1] == '.') {
		return
	}
	if kb, ok := sx.kwOpen[string(w)]; ok {
		c.stack = append(c.stack, frame{start: i, end: j, open: string(w), kw: kb})
		return
	}
	if kbs, ok := sx.kwClose[string(w)]; ok {
		c.closeToken(i, j, w, func(f *frame) bool {
			for _, k := range kbs {
				if f.kw == k {
					return true
				}
			}
			return false
		})
	}
}

// closeToken pops the frame closed by the token [i, j). When the innermost
// frame does not match but an outer one does, the frames in between are
// reported as unclosed; when nothing matches the token is reported as
// unexpected and ignored.
func (c *checker) closeToken(i, j int, tok []byte, matches func(*frame) bool) {
	n := len(c.stack)
	if n > 0 && matches(&c.stack[n-1]) {
		c.stack = c.stack[:n-1]
		return
	}
	for k := n - 2; k >= 0; k-- {
		if matches(&c.stack[k]) {
			for m := n - 1; m > k; m-- {
				c.unclosed(&c.stack[m], i)
			}
			c.stack = c.stack[:k]
			return
		}
	}
	if n > 0 {
		top := &c.stack[n-1]
		c.report(i, j, SeverityError, "unexpected %q: %q opened on line %d is still open (expected %s)",
			tok, top.open, c.lineOf(top.start), c.expected(top))
		// A wrong bracket is most likely a typo for the right one: let it
		// close the innermost bracket so the error does not cascade.
		if top.kw < 0 && len(tok) == 1 && c.sx.closers[tok[0]] != 0 {
			c.stack = c.stack[:n-1]
		}
		return
	}
	c.report(i, j, SeverityError, "unexpected %q: nothing to close", tok)
}

func (c *checker) expected(f *frame) string {
	if f.kw < 0 {
		return fmt.Sprintf("%q", string(f.close))
	}
	cl := c.sx.spec.Keywords[f.kw].Close
	if len(cl) == 1 {
		return fmt.Sprintf("%q", cl[0])
	}
	return fmt.Sprintf("one of %q", cl)
}

// unclosed reports a frame that was never closed; at is where the checker
// noticed (another closer or EOF).
func (c *checker) unclosed(f *frame, at int) {
	if at >= len(c.src) {
		c.report(f.start, f.end, SeverityError, "%q was never closed (expected %s)", f.open, c.expected(f))
		return
	}
	c.report(f.start, f.end, SeverityError, "%q was never closed (expected %s before line %d)",
		f.open, c.expected(f), c.lineOf(at))
}

func (c *checker) lineOf(off int) int {
	return bytes.Count(c.src[:min(off, len(c.src))], []byte{'\n'}) + 1
}

// bracketDepth counts open bracket frames (keyword frames do not continue
// lines).
func (c *checker) bracketDepth() int {
	n := 0
	for i := range c.stack {
		if c.stack[i].kw < 0 {
			n++
		}
	}
	return n
}

// endLine is called at every newline outside literals (and at EOF).
func (c *checker) endLine(i int) {
	if c.sx.indent {
		c.continued = c.sx.contByte != 0 && c.lastSig == c.sx.contByte
		if !c.continued && c.bracketDepth() == 0 && c.lastSig != 0 &&
			bytes.IndexByte([]byte(c.sx.indentOpn), c.lastSig) >= 0 {
			c.pendingOpen = i - 1
		}
	}
	c.lastSig = 0
	c.lastWord = nil
}

// lineStart applies the indentation rules at the start of a physical line.
func (c *checker) lineStart(i int) {
	if c.continued || c.bracketDepth() > 0 {
		return
	}
	src := c.src
	j, w := i, 0
	sp, tab := false, false
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		if src[j] == '\t' {
			w += c.sx.tabWidth - w%c.sx.tabWidth
			tab = true
		} else {
			w++
			sp = true
		}
		j++
	}
	if j >= len(src) || src[j] == '\n' || src[j] == '\r' {
		return // blank line
	}
	for _, lc := range c.sx.lineCmts {
		if lc != "" && bytes.HasPrefix(src[j:], []byte(lc)) {
			return // comment-only line
		}
	}
	if sp && tab {
		c.report(i, j, SeverityWarning, "indentation mixes tabs and spaces")
	}
	top := c.levels[len(c.levels)-1]
	if c.pendingOpen >= 0 {
		opener := c.pendingOpen
		c.pendingOpen = -1
		if w > top {
			c.levels = append(c.levels, w)
			return
		}
		c.report(j, c.lineEnd(j), SeverityError, "expected an indented block after line %d", c.lineOf(opener))
	}
	switch {
	case w > top:
		c.report(i, j, SeverityError, "unexpected indent")
	case w < top:
		for len(c.levels) > 1 && c.levels[len(c.levels)-1] > w {
			c.levels = c.levels[:len(c.levels)-1]
		}
		if c.levels[len(c.levels)-1] != w {
			c.report(i, j, SeverityError, "unindent does not match any outer indentation level")
			c.levels = append(c.levels, w)
		}
	}
}

// directive handles a C preprocessor line starting at i. It returns the
// offset to continue from and true when the line must not be scanned (an
// "#if 0" branch, #error text, or any line inside a skipped branch).
func (c *checker) directive(i int) (int, bool) {
	src := c.src
	j := i
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	skipping := len(c.pp) > 0 && c.pp[len(c.pp)-1].skip
	if j >= len(src) || src[j] != '#' {
		if skipping {
			return c.logicalEnd(i), true
		}
		return i, false
	}
	j++
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	k := c.l.Ident.IdentAfter(src, j)
	name := string(src[j:k])
	rest := bytes.TrimSpace(src[k:c.logicalEnd(k)])
	switch name {
	case "if", "ifdef", "ifndef":
		e := ppCond{saved: append([]frame(nil), c.stack...)}
		e.skip = skipping || (name == "if" && len(rest) > 0 && rest[0] == '0' &&
			(len(rest) == 1 || !c.l.Ident[rest[1]]))
		if skipping {
			e.hasFirst = true // nested in a dead branch: never live
		}
		c.pp = append(c.pp, e)
		return c.logicalEnd(i), true
	case "elif", "else":
		if len(c.pp) == 0 {
			break
		}
		e := &c.pp[len(c.pp)-1]
		outerSkip := len(c.pp) > 1 && c.pp[len(c.pp)-2].skip
		if outerSkip {
			return c.logicalEnd(i), true
		}
		if !e.skip && !e.hasFirst {
			e.first, e.hasFirst = append([]frame(nil), c.stack...), true
		}
		e.skip = false
		c.stack = append(c.stack[:0:0], e.saved...)
		return c.logicalEnd(i), true
	case "endif":
		if len(c.pp) == 0 {
			break
		}
		e := c.pp[len(c.pp)-1]
		c.pp = c.pp[:len(c.pp)-1]
		if e.hasFirst && !(len(c.pp) > 0 && c.pp[len(c.pp)-1].skip) {
			c.stack = e.first
		}
		return c.logicalEnd(i), true
	case "error", "warning":
		return c.logicalEnd(i), true
	}
	if skipping {
		return c.logicalEnd(i), true
	}
	return i, false
}

// logicalEnd returns the end of the line at i, following backslash
// continuations (multi-line preprocessor directives).
func (c *checker) logicalEnd(i int) int {
	for {
		e := c.lineEnd(i)
		t := e
		if t > i && c.src[t-1] == '\r' {
			t--
		}
		if e >= len(c.src) || t == i || c.src[t-1] != '\\' {
			return e
		}
		i = e + 1
	}
}

func (c *checker) lineEnd(i int) int {
	if k := bytes.IndexByte(c.src[i:], '\n'); k >= 0 {
		return i + k
	}
	return len(c.src)
}

func (c *checker) finish() {
	if c.pendingOpen >= 0 {
		c.report(c.pendingOpen, c.pendingOpen+1, SeverityError, "expected an indented block after this line")
	}
	for k := len(c.stack) - 1; k >= 0; k-- {
		c.unclosed(&c.stack[k], len(c.src))
	}
	sort.SliceStable(c.out, func(a, b int) bool { return c.out[a].Start < c.out[b].Start })
}
