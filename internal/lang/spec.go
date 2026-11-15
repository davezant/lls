// Package lang turns a declarative language specification (a JSON file in
// the langs/ directory) into everything lls needs to support a language:
//
//   - a line-oriented symbol extractor used both when compiling library
//     indexes and when analysing the buffers open in the editor;
//   - an import analyser that maps aliases and imported names to modules;
//   - the rules that derive a module name from a file path.
//
// The extractor is intentionally regex based rather than a real parser: it
// is language agnostic, needs no per-language code, compiles a full standard
// library in seconds even on a Raspberry Pi and degrades gracefully on
// syntax errors (which is the normal state of a file being edited).
// See docs/LANGSPEC.md for the full reference of the JSON format.
package lang

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"lls/internal/index"
	"lls/internal/text"
	"lls/langs"
)

// Spec is the JSON representation of a language specification.
type Spec struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	LanguageIDs []string `json:"languageIds"` // LSP languageId values
	Extensions  []string `json:"extensions"`  // editor file extensions (".py")

	Source SourceSpec `json:"source"`
	Module ModuleSpec `json:"module"`

	// Visibility filters which names are indexed from libraries:
	// "all", "no-underscore" (skip names starting with "_") or "uppercase"
	// (Go-style exported names). VisibilityExempt lists modules exempt from
	// the filter (e.g. Go's "builtin").
	Visibility       string   `json:"visibility"`
	VisibilityExempt []string `json:"visibilityExempt"`

	LineComments []string `json:"lineComments"` // longest first, e.g. ["///", "//"]
	BlockComment []string `json:"blockComment"` // [open, close], e.g. ["/*", "*/"]
	Docstrings   bool     `json:"docstrings"`   // Python-style """docstrings"""
	MaxDoc       int      `json:"maxDoc"`       // max documentation bytes (default 300)
	DetailStrip  []string `json:"detailStrip"`  // regexes removed from signatures

	IdentExtra       string   `json:"identExtra"`       // extra identifier bytes ("$")
	MemberSeparators []string `json:"memberSeparators"` // ".", "::", "->"
	Keywords         []string `json:"keywords"`
	Autoload         []string `json:"autoload"` // modules always in scope (builtins)

	Blocks    map[string]BlockSpec `json:"blocks"`
	Symbols   []SymbolRule         `json:"symbols"`
	Imports   []ImportRule         `json:"imports"`
	Reexports []string             `json:"reexports"` // regexes with "module" or "rel" group

	// ImportPathContext matches the text before the cursor when the user is
	// typing a module path; the "partial" group is the typed part.
	ImportPathContext string `json:"importPathContext"`
	// ImportNameContext matches "from module import partial" style contexts
	// with "module" and "partial" groups.
	ImportNameContext string `json:"importNameContext"`
	// LocalResolve lists path templates used to find the source of a module
	// that is not in any index (project-local code). Tokens: {module} is the
	// module as written, {path} the module with the module separator
	// replaced by "/".
	LocalResolve []string `json:"localResolve"`

	// Syntax enables structural syntax checking (see syntax.go).
	Syntax *SyntaxSpec `json:"syntax"`
}

// SourceSpec controls which files are read when compiling an index.
type SourceSpec struct {
	Extensions   []string `json:"extensions"`   // ".py", ".pyi", ".d.ts", ...
	ExcludeDirs  []string `json:"excludeDirs"`  // glob patterns on directory names
	ExcludeFiles []string `json:"excludeFiles"` // glob patterns on file names
}

// ModuleSpec controls how module names are derived from file paths.
type ModuleSpec struct {
	// Mode is one of:
	//   "dir"      module = directory of the file ("net/http")
	//   "file"     module = path without extension, index files map to their
	//              directory, "/" replaced by Separator ("os.path")
	//   "file-ext" module = path relative to the root, extension kept ("stdio.h")
	//   "package"  module = first path segment, or two for "@scope/pkg"
	Mode         string   `json:"mode"`
	Separator    string   `json:"separator"`    // "/" by default
	IndexNames   []string `json:"indexNames"`   // "__init__", "index", "mod"
	DropSegments []string `json:"dropSegments"` // path segments removed ("src")
	Submodules   bool     `json:"submodules"`   // expose child modules as members
	Private      bool     `json:"private"`      // skip modules with a "_"-prefixed segment
	Declared     string   `json:"declared"`     // regex switching module mid-file ("module" group)
	AliasSkip    string   `json:"aliasSkip"`    // segments skipped for default aliases ("^v\\d+$")
	ImportStrip  string   `json:"importStrip"`  // regex removed from imported paths ("^node:")
}

// BlockSpec delimits a block such as Go's "const ( ... )".
type BlockSpec struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// SymbolRule extracts one kind of definition. The pattern must contain a
// "name" group; an optional "container" group places the symbol inside a
// container module (e.g. a Go method receiver).
type SymbolRule struct {
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	// Hint is a literal that must occur in the line for the rule to be tried.
	// It lets most lines skip the regex engine entirely.
	Hint string `json:"hint"`
	// Block restricts the rule to lines inside the named block.
	Block string `json:"block"`
	// Scope is "" (anywhere), "top" (outside containers) or "container"
	// (only inside an indentation container opened by an Opens rule).
	Scope string `json:"scope"`
	// Opens makes the matched symbol open an indentation-delimited container
	// (Python classes, Rust impl blocks, JS classes).
	Opens bool `json:"opens"`
	// Emit=false records the container without emitting a symbol.
	Emit *bool `json:"emit"`
	// ContinueUntil joins following lines (max 8) until a line ends with one
	// of these characters before matching, for multi-line signatures.
	ContinueUntil string `json:"continueUntil"`
	// Local restricts the rule to the user's own buffers (ExtractOptions.All),
	// e.g. non-exported helpers that are irrelevant inside library indexes.
	Local bool `json:"local"`
}

// ImportRule recognises one import form. Groups: "module" (absolute module),
// "rel" (relative module), "alias" and "names" (comma separated list,
// "x as y" supported, "*" imports everything).
type ImportRule struct {
	Pattern string `json:"pattern"`
	Hint    string `json:"hint"`
	// Bind is "alias" (default: the module is reachable through an alias) or
	// "scope" (all of the module's symbols become visible, e.g. C #include).
	Bind string `json:"bind"`
	// Alias is the default alias when the rule has no alias group:
	// "module" (the full module, Python) or "last" (last segment, Go).
	Alias string `json:"alias"`
}

// Lang is a compiled, ready to use language specification.
type Lang struct {
	Spec
	Ident *text.IdentTable

	symRules    []symRule
	blocks      []block
	importRules []importRule
	reexports   []*regexp.Regexp
	pathCtx     *regexp.Regexp
	nameCtx     *regexp.Regexp
	detailStrip []*regexp.Regexp
	declared    *regexp.Regexp
	aliasSkip   *regexp.Regexp
	importStrip *regexp.Regexp
	srcExts     []string
	exempt      map[string]bool
	keywords    map[string]bool
	maxDoc      int
	syntax      *syntax
}

type symRule struct {
	SymbolRule
	re        *regexp.Regexp
	hint      []byte
	kind      index.Kind
	emit      bool
	nameIdx   int
	contIdx   int
	blockName string
}

type block struct {
	name       string
	start, end *regexp.Regexp
}

type importRule struct {
	ImportRule
	re                                 *regexp.Regexp
	hint                               []byte
	modIdx, relIdx, aliasIdx, namesIdx int
}

// Parse compiles a JSON language specification.
func Parse(data []byte) (*Lang, error) {
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return Compile(s)
}

// Compile validates a Spec and compiles all of its regular expressions.
func Compile(s Spec) (*Lang, error) {
	if s.Name == "" {
		return nil, fmt.Errorf("lang: spec without name")
	}
	if s.Module.Separator == "" {
		s.Module.Separator = "/"
	}
	l := &Lang{Spec: s, Ident: text.NewIdentTable(s.IdentExtra), maxDoc: s.MaxDoc}
	if l.maxDoc <= 0 {
		l.maxDoc = 300
	}
	re := func(where, p string, multiline bool) (*regexp.Regexp, error) {
		if p == "" {
			return nil, nil
		}
		if multiline {
			p = "(?m)" + p
		}
		r, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("lang %s: %s: %w", s.Name, where, err)
		}
		return r, nil
	}
	var err error
	for name, b := range s.Blocks {
		var cb block
		cb.name = name
		if cb.start, err = re("block "+name, b.Start, false); err != nil {
			return nil, err
		}
		if cb.end, err = re("block "+name, b.End, false); err != nil {
			return nil, err
		}
		if cb.start == nil || cb.end == nil {
			return nil, fmt.Errorf("lang %s: block %s needs start and end", s.Name, name)
		}
		l.blocks = append(l.blocks, cb)
	}
	sort.Slice(l.blocks, func(i, j int) bool { return l.blocks[i].name < l.blocks[j].name })
	for i, r := range s.Symbols {
		k, ok := index.ParseKind(r.Kind)
		if !ok {
			return nil, fmt.Errorf("lang %s: symbol rule %d: unknown kind %q", s.Name, i, r.Kind)
		}
		cr := symRule{SymbolRule: r, kind: k, emit: r.Emit == nil || *r.Emit, blockName: r.Block}
		if cr.re, err = re(fmt.Sprintf("symbol rule %d", i), r.Pattern, false); err != nil {
			return nil, err
		}
		if cr.re == nil {
			return nil, fmt.Errorf("lang %s: symbol rule %d: empty pattern", s.Name, i)
		}
		cr.nameIdx = cr.re.SubexpIndex("name")
		cr.contIdx = cr.re.SubexpIndex("container")
		if cr.nameIdx < 0 {
			return nil, fmt.Errorf("lang %s: symbol rule %d: pattern needs a (?P<name>...) group", s.Name, i)
		}
		if r.Hint != "" {
			cr.hint = []byte(r.Hint)
		}
		l.symRules = append(l.symRules, cr)
	}
	for i, r := range s.Imports {
		ir := importRule{ImportRule: r}
		if ir.re, err = re(fmt.Sprintf("import rule %d", i), r.Pattern, true); err != nil {
			return nil, err
		}
		if ir.re == nil {
			return nil, fmt.Errorf("lang %s: import rule %d: empty pattern", s.Name, i)
		}
		ir.modIdx = ir.re.SubexpIndex("module")
		ir.relIdx = ir.re.SubexpIndex("rel")
		ir.aliasIdx = ir.re.SubexpIndex("alias")
		ir.namesIdx = ir.re.SubexpIndex("names")
		if r.Hint != "" {
			ir.hint = []byte(r.Hint)
		}
		l.importRules = append(l.importRules, ir)
	}
	for i, p := range s.Reexports {
		r, err := re(fmt.Sprintf("reexport %d", i), p, false)
		if err != nil {
			return nil, err
		}
		l.reexports = append(l.reexports, r)
	}
	for i, p := range s.DetailStrip {
		r, err := re(fmt.Sprintf("detailStrip %d", i), p, false)
		if err != nil {
			return nil, err
		}
		l.detailStrip = append(l.detailStrip, r)
	}
	if l.pathCtx, err = re("importPathContext", s.ImportPathContext, false); err != nil {
		return nil, err
	}
	if l.nameCtx, err = re("importNameContext", s.ImportNameContext, false); err != nil {
		return nil, err
	}
	if l.declared, err = re("module.declared", s.Module.Declared, false); err != nil {
		return nil, err
	}
	if l.aliasSkip, err = re("module.aliasSkip", s.Module.AliasSkip, false); err != nil {
		return nil, err
	}
	if l.importStrip, err = re("module.importStrip", s.Module.ImportStrip, false); err != nil {
		return nil, err
	}
	l.srcExts = append([]string(nil), s.Source.Extensions...)
	sort.Slice(l.srcExts, func(i, j int) bool { return len(l.srcExts[i]) > len(l.srcExts[j]) })
	l.exempt = make(map[string]bool, len(s.VisibilityExempt))
	for _, m := range s.VisibilityExempt {
		l.exempt[m] = true
	}
	l.keywords = make(map[string]bool, len(s.Keywords))
	for _, k := range s.Keywords {
		l.keywords[k] = true
	}
	if l.syntax, err = compileSyntax(l); err != nil {
		return nil, err
	}
	return l, nil
}

// IsKeyword reports whether name is a reserved word of the language.
func (l *Lang) IsKeyword(name string) bool { return l.keywords[name] }

// Visible reports whether a library symbol called name in module should be
// indexed, according to the spec's visibility policy.
func (l *Lang) Visible(name, module string) bool {
	if name == "" || l.exempt[module] {
		return name != ""
	}
	switch l.Visibility {
	case "no-underscore":
		return name[0] != '_'
	case "uppercase":
		return name[0] >= 'A' && name[0] <= 'Z'
	}
	return true
}

// LoadAll loads the specs embedded in the binary and then the "*.json"
// files found in dirs; a spec in a directory overrides an embedded one with
// the same name, so users can customise languages without recompiling.
func LoadAll(dirs ...string) (map[string]*Lang, error) {
	out := make(map[string]*Lang)
	err := fs.WalkDir(langs.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		data, err := fs.ReadFile(langs.FS, p)
		if err != nil {
			return err
		}
		l, err := Parse(data)
		if err != nil {
			return fmt.Errorf("embedded %s: %w", p, err)
		}
		out[l.Name] = l
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, p := range matches {
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			l, err := Parse(data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			out[l.Name] = l
		}
	}
	return out, nil
}

// EmbeddedSpec returns the raw JSON of an embedded spec (for "lls spec").
func EmbeddedSpec(name string) ([]byte, error) {
	return fs.ReadFile(langs.FS, name+".json")
}
