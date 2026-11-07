// Package build compiles library sources into an lls index file.
//
// The pipeline is:
//
//	walk roots ──► file paths ──► N extractor workers ──► 1 aggregator ──► index.Builder ──► .llsi
//
// Walking is sequential (it is I/O bound and cheap), extraction runs on all
// CPU cores and a single goroutine owns the index.Builder, so no locking is
// needed around the symbol tables. Besides source trees, the builder accepts
// JSON Lines files produced by introspection scripts (see scripts/introspect),
// which is how modules implemented in C (Python's builtins, Lua's string
// library, Node's fs...) get indexed.
package build

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"lls/internal/index"
	"lls/internal/lang"
)

// Options configures a build.
type Options struct {
	Lang    *lang.Lang
	Roots   []string // source directories to scan
	JSONL   []string // introspection outputs to merge
	Out     string   // output .llsi path
	Prefix  string   // prepended to every module derived from a root
	Workers int      // extraction goroutines (default: NumCPU)
	MaxSize int64    // skip source files larger than this (default 2 MiB)
	Log     io.Writer
}

// Stats summarises a finished build.
type Stats struct {
	Files    int
	Skipped  int
	Modules  int
	Symbols  int
	Bytes    int64
	Duration time.Duration
}

type job struct {
	path, rel string
}

type result struct {
	module  string
	isIndex bool
	info    *lang.FileInfo
}

// Run executes the build and writes the index atomically (temp file + rename).
func Run(opts Options) (Stats, error) {
	start := time.Now()
	var st Stats
	if opts.Lang == nil {
		return st, fmt.Errorf("build: no language")
	}
	if opts.Workers <= 0 {
		opts.Workers = runtime.NumCPU()
	}
	if opts.MaxSize <= 0 {
		opts.MaxSize = 2 << 20
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	l := opts.Lang
	b := index.NewBuilder()

	for _, p := range opts.JSONL {
		n, err := loadJSONL(b, p)
		if err != nil {
			return st, fmt.Errorf("%s: %w", p, err)
		}
		fmt.Fprintf(opts.Log, "jsonl %s: %d symbols\n", p, n)
	}

	jobs := make(chan job, 256)
	results := make(chan result, 256)
	var wg sync.WaitGroup
	for i := 0; i < opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				src, err := os.ReadFile(j.path)
				if err != nil {
					continue
				}
				mod, isIndex, ok := l.ModuleFor(j.rel)
				if !ok {
					continue
				}
				if opts.Prefix != "" {
					mod = joinModule(l, opts.Prefix, mod)
				}
				info := l.Extract(src, lang.ExtractOptions{Module: mod})
				results <- result{module: mod, isIndex: isIndex, info: info}
			}
		}()
	}

	walkErr := make(chan error, 1)
	go func() {
		defer close(jobs)
		for _, root := range opts.Roots {
			if err := walk(l, root, opts.MaxSize, jobs, &st); err != nil {
				walkErr <- err
				return
			}
		}
		walkErr <- nil
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		st.Files++
		b.EnsureModule(r.module)
		for _, d := range r.info.Defs {
			mod := r.module
			if d.Module != "" {
				mod = d.Module
			}
			if d.Container != "" {
				cm := lang.ContainerModule(mod, d.Container)
				b.MarkContainer(cm)
				mod = cm
			}
			b.Add(mod, index.Symbol{Name: d.Name, Detail: d.Detail, Doc: d.Doc, Kind: d.Kind})
		}
		for _, re := range r.info.Reexports {
			target := re.Module
			if re.Relative {
				target = l.ResolveRelative(r.module, r.isIndex, re.Module)
			}
			b.AddReexport(r.module, target)
		}
	}
	if err := <-walkErr; err != nil {
		return st, err
	}
	if l.Module.Submodules {
		b.AddSubmoduleSymbols(l.Module.Separator)
	}
	st.Modules, st.Symbols = b.Counts()

	if err := os.MkdirAll(filepath.Dir(opts.Out), 0o755); err != nil {
		return st, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(opts.Out), ".lls-build-*")
	if err != nil {
		return st, err
	}
	defer os.Remove(tmp.Name())
	sources := append(append([]string(nil), opts.Roots...), opts.JSONL...)
	meta := index.Meta{Language: l.Name, Created: time.Now().UTC(), Sources: sources, Builder: "lls"}
	if err := b.Write(tmp, meta); err != nil {
		tmp.Close()
		return st, err
	}
	if err := tmp.Close(); err != nil {
		return st, err
	}
	if err := os.Rename(tmp.Name(), opts.Out); err != nil {
		return st, err
	}
	if fi, err := os.Stat(opts.Out); err == nil {
		st.Bytes = fi.Size()
	}
	st.Duration = time.Since(start)
	return st, nil
}

func joinModule(l *lang.Lang, prefix, mod string) string {
	if mod == "" {
		return prefix
	}
	return prefix + l.Module.Separator + mod
}

// walk feeds every eligible source file under root into jobs.
func walk(l *lang.Lang, root string, maxSize int64, jobs chan<- job, st *Stats) error {
	root = filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved // WalkDir does not descend into a symlinked root
	} else {
		return fmt.Errorf("build: %w", err)
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && matchAny(l.Source.ExcludeDirs, name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !l.IsSource(name) || matchAny(l.Source.ExcludeFiles, name) {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.Size() > maxSize {
			st.Skipped++
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		jobs <- job{path: p, rel: filepath.ToSlash(rel)}
		return nil
	})
}

func matchAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// jsonlRecord is one line of an introspection file.
//
//	{"m":"os.path","k":"function","n":"join","d":"join(a, *p)","doc":"Join two or more pathname components"}
//
// A record with only "m" (and optionally "r", a list of re-exported modules)
// declares a module.
type jsonlRecord struct {
	Module string   `json:"m"`
	Kind   string   `json:"k"`
	Name   string   `json:"n"`
	Detail string   `json:"d"`
	Doc    string   `json:"doc"`
	Re     []string `json:"r"`
}

func loadJSONL(b *index.Builder, p string) (int, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<24)
	n := 0
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		var r jsonlRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return n, fmt.Errorf("line %d: %w", line, err)
		}
		mod := r.Module
		if i := strings.IndexByte(mod, '#'); i >= 0 {
			// "#" marks a container: "collections#OrderedDict" holds the
			// members of class OrderedDict of module collections.
			mod = lang.ContainerModule(mod[:i], mod[i+1:])
			b.MarkContainer(mod)
		}
		b.EnsureModule(mod)
		for _, re := range r.Re {
			b.AddReexport(mod, re)
		}
		if r.Name == "" {
			continue
		}
		k, ok := index.ParseKind(strings.ToLower(r.Kind))
		if !ok {
			k = index.KindVariable
		}
		b.Add(mod, index.Symbol{Name: r.Name, Detail: r.Detail, Doc: r.Doc, Kind: k})
		n++
	}
	return n, sc.Err()
}
