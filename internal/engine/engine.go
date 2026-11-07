// Package engine is the language-independent brain of lls. It owns the open
// documents, the loaded indexes and the project-local modules, and answers
// completion, hover, signature-help and go-to-definition queries. Both the
// LSP server and the terminal commands are thin layers over this package.
//
// # Concurrency
//
// An Engine is NOT safe for concurrent use. The LSP server processes
// messages sequentially on one goroutine; this removes every lock from the
// hot path (and a single core is plenty: typical requests take microseconds).
//
// # Laziness
//
// Nothing is loaded up front. A language's indexes are mapped the first time
// a document of that language is opened, a module is decoded the first time
// it is referenced, and a project-local module is read the first time an
// import of it is seen.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lls/internal/index"
	"lls/internal/lang"
)

// Config configures an Engine.
type Config struct {
	// Home is the lls data directory. Indexes live in Home/<language>/*.llsi
	// and user language specs in Home/langs/*.json. See DefaultHome.
	Home string
	// SpecDirs lists extra directories with language specs.
	SpecDirs []string
	// Indexes maps a language name to extra index files to load.
	Indexes map[string][]string
	// Roots are workspace roots used to resolve project-local imports.
	Roots []string
	// Limit caps the number of completion items returned (default 100).
	Limit int
	// Log receives diagnostic messages (may be nil).
	Log func(format string, args ...any)
}

// DefaultHome returns $LLS_HOME, or $XDG_DATA_HOME/lls, or ~/.local/share/lls.
func DefaultHome() string {
	if h := os.Getenv("LLS_HOME"); h != "" {
		return h
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "lls")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "lls")
	}
	return ".lls"
}

// Engine answers language queries. Create one with New.
type Engine struct {
	cfg    Config
	langs  map[string]*Language
	byID   map[string]*Language
	byExt  map[string]*Language
	docs   map[string]*Document
	byPath map[string]*Document
	recent []*wordStats // word statistics of recently closed documents
	local  map[string]*localFile
	owner  map[string]ownerRef // overlay module name -> local file owning it
}

type ownerRef struct {
	file      *localFile
	container string
}

// Language is a compiled language spec plus its (lazily opened) indexes.
type Language struct {
	*lang.Lang
	indexes []*index.Reader
	loaded  bool
	cache   map[string][]*index.Module // module name -> modules (nil = absent)
	overlay map[string]*index.Module   // project-local modules
	lastSeg map[string][]string        // last module segment -> module names
	seps    []string                   // member separators, longest first
}

// New creates an engine and loads (but does not compile indexes for) all
// language specs.
func New(cfg Config) (*Engine, error) {
	if cfg.Home == "" {
		cfg.Home = DefaultHome()
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 100
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	dirs := append([]string{filepath.Join(cfg.Home, "langs")}, cfg.SpecDirs...)
	specs, err := lang.LoadAll(dirs...)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg:    cfg,
		langs:  make(map[string]*Language),
		byID:   make(map[string]*Language),
		byExt:  make(map[string]*Language),
		docs:   make(map[string]*Document),
		byPath: make(map[string]*Document),
		local:  make(map[string]*localFile),
		owner:  make(map[string]ownerRef),
	}
	names := make([]string, 0, len(specs))
	for n := range specs {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic precedence for shared extensions
	for _, n := range names {
		l := specs[n]
		L := &Language{Lang: l, cache: make(map[string][]*index.Module), overlay: make(map[string]*index.Module)}
		L.seps = append([]string(nil), l.MemberSeparators...)
		sort.Slice(L.seps, func(i, j int) bool { return len(L.seps[i]) > len(L.seps[j]) })
		e.langs[n] = L
		e.byID[n] = L
		for _, id := range l.LanguageIDs {
			e.byID[id] = L
		}
		for _, ext := range l.Extensions {
			if _, taken := e.byExt[ext]; !taken {
				e.byExt[ext] = L
			}
		}
	}
	return e, nil
}

// Home returns the data directory in use.
func (e *Engine) Home() string { return e.cfg.Home }

// SetRoots replaces the workspace roots.
func (e *Engine) SetRoots(roots []string) { e.cfg.Roots = roots }

// SetLimit changes the completion item limit.
func (e *Engine) SetLimit(n int) {
	if n > 0 {
		e.cfg.Limit = n
	}
}

// Languages returns all known languages sorted by name.
func (e *Engine) Languages() []*Language {
	out := make([]*Language, 0, len(e.langs))
	for _, L := range e.langs {
		out = append(out, L)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Language returns a language by spec name or LSP language id.
func (e *Engine) Language(name string) *Language {
	L := e.byID[name]
	if L != nil {
		e.ensureLoaded(L)
	}
	return L
}

// LanguageFor picks the language of a document from its LSP language id,
// falling back to the file extension.
func (e *Engine) LanguageFor(languageID, path string) *Language {
	L := e.byID[languageID]
	if L == nil {
		ext := filepath.Ext(path)
		if strings.HasSuffix(path, ".d.ts") {
			ext = ".ts"
		}
		L = e.byExt[ext]
	}
	if L != nil {
		e.ensureLoaded(L)
	}
	return L
}

// IndexPaths lists the index files that belong to a language.
func (e *Engine) IndexPaths(L *Language) []string {
	paths, _ := filepath.Glob(filepath.Join(e.cfg.Home, L.Name, "*.llsi"))
	return append(paths, e.cfg.Indexes[L.Name]...)
}

// ensureLoaded maps the language's index files on first use.
func (e *Engine) ensureLoaded(L *Language) {
	if L.loaded {
		return
	}
	L.loaded = true
	start := time.Now()
	for _, p := range e.IndexPaths(L) {
		r, err := index.Open(p)
		if err != nil {
			e.cfg.Log("lls: cannot open index %s: %v", p, err)
			continue
		}
		L.indexes = append(L.indexes, r)
	}
	if len(L.indexes) == 0 {
		e.cfg.Log("lls: no index for %s in %s (run: lls build -l %s ...); using buffer words only",
			L.Name, filepath.Join(e.cfg.Home, L.Name), L.Name)
	} else {
		e.cfg.Log("lls: %s: %d index file(s) mapped in %s", L.Name, len(L.indexes), time.Since(start))
	}
}

// Indexes returns the opened index readers of a language.
func (L *Language) Indexes() []*index.Reader { return L.indexes }

// Modules returns every module called name (from local overlays and every
// index). The result of index lookups is memoised, including misses.
func (L *Language) Modules(name string) []*index.Module {
	if m, ok := L.overlay[name]; ok {
		return []*index.Module{m}
	}
	if mods, ok := L.cache[name]; ok {
		return mods
	}
	var mods []*index.Module
	for _, r := range L.indexes {
		if m, ok := r.Lookup(name); ok {
			mods = append(mods, m)
		}
	}
	L.cache[name] = mods
	return mods
}

// HasModule reports whether a module with that name exists anywhere.
func (L *Language) HasModule(name string) bool { return len(L.Modules(name)) > 0 }

// ModulesByLastSegment returns modules whose last path segment is seg. The
// table is built on first use; it powers completion on a bare package name
// without an import (handy in the terminal and for not-yet-imported code).
func (L *Language) ModulesByLastSegment(seg string) []string {
	if L.lastSeg == nil {
		L.lastSeg = make(map[string][]string)
		sep := L.Module.Separator
		for _, r := range L.indexes {
			for i := 0; i < r.NumModules(); i++ {
				name := r.ModuleName(i)
				last := name
				if r.IsContainer(i) {
					if j := strings.LastIndexByte(name, '.'); j >= 0 {
						last = name[j+1:]
					}
				} else if j := strings.LastIndex(name, sep); j >= 0 {
					last = name[j+len(sep):]
				}
				L.lastSeg[last] = append(L.lastSeg[last], name)
			}
		}
	}
	return L.lastSeg[seg]
}

// --- project-local modules -------------------------------------------------

// localFile is a project source file loaded because a document imports it.
type localFile struct {
	key     string // module name it is registered under
	paths   []string
	stamp   string // concatenated mtimes/sizes (or buffer versions)
	checked time.Time
	lines   map[string]localLoc // "Name" or "Container.Name" -> location
	modules []string            // overlay names owned by this file
}

type localLoc struct {
	path string
	line int
}

// localModule tries to resolve module (as written in a document) to project
// sources using the spec's LocalResolve templates. On success the module is
// registered in the language overlay and its overlay name is returned.
func (e *Engine) localModule(d *Document, module string, relative bool) (string, bool) {
	L := d.Lang
	if len(L.LocalResolve) == 0 {
		return "", false
	}
	key := module
	if relative {
		key = "file:" + filepath.Dir(d.Path) + "|" + module
	}
	lf, ok := e.local[key]
	if ok && len(lf.paths) > 0 {
		if time.Since(lf.checked) < 2*time.Second || lf.stamp == e.stampOf(lf.paths) {
			lf.checked = time.Now()
			return key, true
		}
		e.loadLocal(L, lf, lf.paths) // changed on disk or in the editor
		return key, true
	}
	if ok && time.Since(lf.checked) < 10*time.Second {
		return "", false // recent miss: do not hit the file system again
	}
	paths := e.findLocal(d, module, relative)
	if lf == nil {
		lf = &localFile{key: key}
		e.local[key] = lf
	}
	if len(paths) == 0 {
		lf.checked = time.Now()
		return "", false
	}
	e.loadLocal(L, lf, paths)
	return key, true
}

// findLocal expands LocalResolve templates relative to the document's
// directory and the workspace roots.
func (e *Engine) findLocal(d *Document, module string, relative bool) []string {
	L := d.Lang
	sep := L.Module.Separator
	mod := module
	var bases []string
	docDir := filepath.Dir(d.Path)
	if relative && sep != "/" && strings.HasPrefix(mod, ".") {
		// Python-style ".pkg.mod": each extra dot climbs one directory.
		dots := len(mod) - len(strings.TrimLeft(mod, "."))
		dir := docDir
		for i := 1; i < dots; i++ {
			dir = filepath.Dir(dir)
		}
		mod = mod[dots:]
		bases = []string{dir}
	} else if relative {
		bases = []string{docDir}
	} else {
		bases = append([]string{docDir}, e.cfg.Roots...)
	}
	pathForm := mod
	if sep != "/" {
		pathForm = strings.ReplaceAll(mod, sep, "/")
	}
	for _, base := range bases {
		for _, t := range L.LocalResolve {
			rel := strings.NewReplacer("{module}", mod, "{path}", pathForm).Replace(t)
			if rel == "" {
				continue
			}
			p := filepath.Join(base, filepath.FromSlash(rel))
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			if !fi.IsDir() {
				return []string{p}
			}
			// A directory module (Go-like): every source file inside it.
			ents, _ := os.ReadDir(p)
			var files []string
			for _, en := range ents {
				if !en.IsDir() && L.IsSource(en.Name()) && len(files) < 64 {
					files = append(files, filepath.Join(p, en.Name()))
				}
			}
			if len(files) > 0 {
				return files
			}
		}
	}
	return nil
}

func (e *Engine) stampOf(paths []string) string {
	var sb strings.Builder
	for _, p := range paths {
		if d := e.byPath[p]; d != nil {
			fmt.Fprintf(&sb, "v%d;", d.Version)
			continue
		}
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&sb, "%d:%d;", fi.ModTime().UnixNano(), fi.Size())
		}
	}
	return sb.String()
}

// loadLocal (re)extracts a local module and registers it in the overlay,
// preferring the editor buffer over the file on disk when it is open.
func (e *Engine) loadLocal(L *Language, lf *localFile, paths []string) {
	for _, name := range lf.modules {
		delete(L.overlay, name)
		delete(e.owner, name)
	}
	lf.paths, lf.modules = paths, nil
	lf.stamp = e.stampOf(paths)
	lf.checked = time.Now()
	lf.lines = make(map[string]localLoc)
	b := index.NewBuilder()
	b.EnsureModule(lf.key)
	for _, p := range paths {
		var src []byte
		if d := e.byPath[p]; d != nil {
			src = d.Text
		} else {
			var err error
			if src, err = os.ReadFile(p); err != nil {
				continue
			}
		}
		info := L.Extract(src, lang.ExtractOptions{Module: lf.key})
		for _, def := range info.Defs {
			mod, key := lf.key, def.Name
			if def.Container != "" {
				mod = lang.ContainerModule(lf.key, def.Container)
				key = def.Container + "." + def.Name
			}
			b.Add(mod, index.Symbol{Name: def.Name, Detail: def.Detail, Doc: def.Doc, Kind: def.Kind})
			if _, dup := lf.lines[key]; !dup {
				lf.lines[key] = localLoc{path: p, line: def.Line}
			}
		}
	}
	// Serialise through the regular index format so local modules behave
	// exactly like library modules (sorted, prefix-searchable).
	var buf strings.Builder
	_ = b.Write(&buf, index.Meta{Language: L.Name})
	r, err := index.NewReaderBytes([]byte(buf.String()))
	if err != nil {
		return
	}
	for i := 0; i < r.NumModules(); i++ {
		if m, err := r.ModuleAt(i); err == nil {
			L.overlay[m.Name] = m
			lf.modules = append(lf.modules, m.Name)
			e.owner[m.Name] = ownerRef{file: lf, container: strings.TrimPrefix(strings.TrimPrefix(m.Name, lf.key), ".")}
		}
	}
}

// localLocation returns where symbol name of overlay module is defined.
func (e *Engine) localLocation(module, name string) (localLoc, bool) {
	ref, ok := e.owner[module]
	if !ok || ref.file.lines == nil {
		return localLoc{}, false
	}
	key := name
	if ref.container != "" {
		key = ref.container + "." + name
	}
	loc, ok := ref.file.lines[key]
	return loc, ok
}
