// Command lls is the Lightweight Language Server.
//
// It compiles library sources into compact, lazily loaded index files and
// serves completion, hover, signature help and go-to-definition, either as a
// Language Server Protocol server ("lls serve") or straight from the terminal
// ("lls complete", "lls repl"). Run "lls help" for usage.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"lls/internal/build"
	"lls/internal/engine"
	"lls/internal/index"
	"lls/internal/lang"
	"lls/internal/lsp"
)

// version is overridden at link time: -ldflags "-X main.version=1.2.3".
var version = "dev"

const usage = `lls - Lightweight Language Server

Usage:
  lls serve    [--home DIR] [--log FILE] [--trace] [--limit N]   run the LSP server on stdio
  lls build    -l LANG [-r DIR]... [--jsonl FILE]... [-o OUT]    compile an index
  lls complete -l LANG [--file F [--pos L:C] | --stdin | TEXT]   completions at the cursor
  lls hover    -l LANG [--file F --pos L:C | --stdin | TEXT]     describe the symbol at the cursor
  lls check    [-l LANG] FILE... | -l LANG --stdin               report syntax errors
  lls repl     -l LANG                                           interactive completion shell
  lls inspect  FILE.llsi|LANG [MODULE | PREFIX*]                 browse an index
  lls langs                                                      list languages and installed indexes
  lls spec     LANG                                              print a built-in language spec (JSON)
  lls bench    -l LANG [-n N] TEXT                               measure completion latency
  lls version

Indexes live in $LLS_HOME/<lang>/*.llsi (default ~/.local/share/lls).
Positions given with --pos are 1-based "line:column" (column in bytes).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "build":
		err = cmdBuild(args)
	case "complete":
		err = cmdComplete(args)
	case "hover":
		err = cmdHover(args)
	case "check":
		err = cmdCheck(args)
	case "repl":
		err = cmdRepl(args)
	case "inspect":
		err = cmdInspect(args)
	case "langs":
		err = cmdLangs(args)
	case "spec":
		err = cmdSpec(args)
	case "bench":
		err = cmdBench(args)
	case "version", "--version", "-v":
		fmt.Printf("lls %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "lls: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lls:", err)
		os.Exit(1)
	}
}

// multiFlag collects repeated string flags.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func newEngine(home string, logf func(string, ...any)) (*engine.Engine, error) {
	return engine.New(engine.Config{Home: home, Log: logf})
}

func stderrLog(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }

// --- serve -------------------------------------------------------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	home := fs.String("home", "", "data directory (default $LLS_HOME or ~/.local/share/lls)")
	logPath := fs.String("log", "", "write logs to this file instead of stderr")
	trace := fs.Bool("trace", false, "log the duration of every request")
	limit := fs.Int("limit", 100, "maximum completion items per request")
	_ = fs.Bool("stdio", true, "use stdio (the only transport; accepted for editor compatibility)")
	fs.Parse(args)

	logw := io.Writer(os.Stderr)
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		logw = f
	}
	logf := func(format string, a ...any) { fmt.Fprintf(logw, format+"\n", a...) }
	e, err := engine.New(engine.Config{Home: *home, Limit: *limit, Log: logf})
	if err != nil {
		return err
	}
	lsp.Version = version
	logf("lls %s: serving (home %s)", version, e.Home())
	return lsp.Serve(e, os.Stdin, os.Stdout, logf, *trace)
}

// --- build -------------------------------------------------------------------

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	langName := fs.String("l", "", "language (spec name)")
	specPath := fs.String("spec", "", "use this spec file instead of the built-in one")
	var roots, jsonl, excludes multiFlag
	fs.Var(&roots, "r", "source root to scan (repeatable)")
	fs.Var(&excludes, "exclude", "extra directory name glob to skip (repeatable)")
	fs.Var(&jsonl, "jsonl", "introspection JSON Lines file to merge (repeatable)")
	out := fs.String("o", "", "output file (default $LLS_HOME/<lang>/<name>.llsi)")
	name := fs.String("name", "", "index name used for the default output file")
	prefix := fs.String("prefix", "", "prefix prepended to every module found under the roots")
	home := fs.String("home", "", "data directory")
	workers := fs.Int("j", runtime.NumCPU(), "parallel extraction workers")
	quiet := fs.Bool("q", false, "quiet")
	fs.Parse(args)
	roots = append(roots, fs.Args()...)

	var l *lang.Lang
	if *specPath != "" {
		data, err := os.ReadFile(*specPath)
		if err != nil {
			return err
		}
		if l, err = lang.Parse(data); err != nil {
			return err
		}
	} else {
		e, err := newEngine(*home, nil)
		if err != nil {
			return err
		}
		L := e.Language(*langName)
		if L == nil {
			return fmt.Errorf("unknown language %q (see: lls langs)", *langName)
		}
		l = L.Lang
	}
	if len(excludes) > 0 {
		spec := l.Spec // copy, so the shared compiled spec is not mutated
		spec.Source.ExcludeDirs = append(append([]string(nil), spec.Source.ExcludeDirs...), excludes...)
		var err error
		if l, err = lang.Compile(spec); err != nil {
			return err
		}
	}
	if len(roots) == 0 && len(jsonl) == 0 {
		return errors.New("build: give at least one -r DIR or --jsonl FILE")
	}
	if *out == "" {
		n := *name
		if n == "" && len(roots) > 0 {
			n = filepath.Base(filepath.Clean(roots[0]))
		}
		if n == "" || n == "." || n == "/" {
			n = "index"
		}
		h := *home
		if h == "" {
			h = engine.DefaultHome()
		}
		*out = filepath.Join(h, l.Name, n+".llsi")
	}
	var logw io.Writer = os.Stderr
	if *quiet {
		logw = io.Discard
	}
	st, err := build.Run(build.Options{
		Lang: l, Roots: roots, JSONL: jsonl, Out: *out, Prefix: *prefix, Workers: *workers, Log: logw,
	})
	if err != nil {
		return err
	}
	if !*quiet {
		fmt.Fprintf(os.Stderr, "%s: %d files, %d modules, %d symbols, %s in %s\n",
			*out, st.Files, st.Modules, st.Symbols, humanBytes(st.Bytes), st.Duration.Round(time.Millisecond))
	}
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// --- query input shared by complete / hover ----------------------------------

type queryFlags struct {
	lang, file, pos, home string
	stdin                 bool
	offset                int
}

func (q *queryFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&q.lang, "l", "", "language (spec name or LSP id); inferred from --file when omitted")
	fs.StringVar(&q.file, "file", "", "document to load")
	fs.StringVar(&q.pos, "pos", "", "cursor as 1-based line:column (default: end of document)")
	fs.IntVar(&q.offset, "offset", -1, "cursor as a byte offset")
	fs.BoolVar(&q.stdin, "stdin", false, "read the document from stdin")
	fs.StringVar(&q.home, "home", "", "data directory")
}

// open builds the engine and the document described by the flags.
func (q *queryFlags) open(rest []string) (*engine.Engine, *engine.Document, int, error) {
	e, err := newEngine(q.home, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	var content []byte
	uri := "untitled:query"
	switch {
	case q.stdin:
		content, err = io.ReadAll(os.Stdin)
		if q.file != "" {
			uri = engine.PathToURI(q.file)
		}
	case q.file != "":
		content, err = os.ReadFile(q.file)
		uri = engine.PathToURI(q.file)
	default:
		content = []byte(strings.Join(rest, " "))
	}
	if err != nil {
		return nil, nil, 0, err
	}
	d := e.Open(uri, q.lang, 1, content)
	if d == nil {
		return nil, nil, 0, fmt.Errorf("unknown language %q (use -l, see: lls langs)", q.lang)
	}
	off := len(content)
	if q.offset >= 0 {
		off = min(q.offset, len(content))
	} else if q.pos != "" {
		l, c, ok := strings.Cut(q.pos, ":")
		line, err1 := strconv.Atoi(l)
		col, err2 := strconv.Atoi(c)
		if !ok || err1 != nil || err2 != nil || line < 1 || col < 1 {
			return nil, nil, 0, fmt.Errorf("bad --pos %q (want line:column)", q.pos)
		}
		lb := d.LineBytes(line - 1)
		off = d.Offset(line-1, 0) + min(col-1, len(lb))
	}
	return e, d, off, nil
}

// --- complete ------------------------------------------------------------------

func cmdComplete(args []string) error {
	fs := flag.NewFlagSet("complete", flag.ExitOnError)
	var q queryFlags
	q.register(fs)
	n := fs.Int("n", 30, "number of results")
	asJSON := fs.Bool("json", false, "print JSON")
	docs := fs.Bool("doc", false, "print documentation")
	fs.Parse(args)
	e, d, off, err := q.open(fs.Args())
	if err != nil {
		return err
	}
	e.SetLimit(*n)
	start := time.Now()
	res := e.Complete(d, off, true)
	elapsed := time.Since(start)
	if *asJSON {
		type item struct {
			Label  string `json:"label"`
			Kind   string `json:"kind"`
			Detail string `json:"detail,omitempty"`
			Doc    string `json:"doc,omitempty"`
		}
		items := make([]item, 0, len(res.Items))
		for _, it := range res.Items {
			items = append(items, item{it.Label, it.Kind.String(), it.Detail, it.Doc})
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"prefix": res.Prefix, "items": items})
	}
	printCandidates(os.Stdout, res.Items, *docs)
	fmt.Fprintf(os.Stderr, "%d items, prefix %q, %s\n", len(res.Items), res.Prefix, elapsed)
	return nil
}

func printCandidates(w io.Writer, items []engine.Candidate, docs bool) {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	width := 0
	for _, it := range items {
		width = max(width, len(it.Label))
	}
	width = min(width, 40)
	for _, it := range items {
		fmt.Fprintf(bw, "%-10s %-*s  %s\n", it.Kind, width, it.Label, it.Detail)
		if docs && it.Doc != "" {
			fmt.Fprintf(bw, "%10s %s\n", "", it.Doc)
		}
	}
}

// --- hover -----------------------------------------------------------------

func cmdHover(args []string) error {
	fs := flag.NewFlagSet("hover", flag.ExitOnError)
	var q queryFlags
	q.register(fs)
	fs.Parse(args)
	e, d, off, err := q.open(fs.Args())
	if err != nil {
		return err
	}
	if off == len(d.Text) && off > 0 {
		off-- // "TEXT" mode: hover the last identifier
	}
	info, ok := e.Hover(d, off)
	if !ok {
		return errors.New("no symbol found")
	}
	printInfo(os.Stdout, info)
	return nil
}

func printInfo(w io.Writer, info engine.SymbolInfo) {
	fmt.Fprintf(w, "%s  [%s]", info.Detail, info.Kind)
	if info.Module != "" && !strings.HasPrefix(info.Module, "file:") {
		fmt.Fprintf(w, "  (%s)", info.Module)
	}
	fmt.Fprintln(w)
	if info.Doc != "" {
		fmt.Fprintln(w, info.Doc)
	}
}

// --- repl ------------------------------------------------------------------

const replHelp = `Type an expression to complete it (the cursor is at the end), e.g. "os.path.jo".
Import statements ("import numpy as np", "#include <stdio.h>") are added to the context.
Commands: :h EXPR  hover   :ctx  show context   :clear  reset context   :n N  result count   :q  quit`

func cmdRepl(args []string) error {
	fs := flag.NewFlagSet("repl", flag.ExitOnError)
	langName := fs.String("l", "", "language")
	home := fs.String("home", "", "data directory")
	fs.Parse(args)
	e, err := newEngine(*home, stderrLog)
	if err != nil {
		return err
	}
	L := e.Language(*langName)
	if L == nil {
		return fmt.Errorf("unknown language %q (see: lls langs)", *langName)
	}
	e.SetLimit(15)
	fmt.Printf("lls repl (%s, %d index file(s)). %s\n", L.Name, len(L.Indexes()), replHelp)
	var ctx []string
	version := int32(1)
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(L.Name + "> ")
		if !sc.Scan() {
			fmt.Println()
			return sc.Err()
		}
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case trimmed == ":q" || trimmed == ":quit":
			return nil
		case trimmed == ":ctx":
			fmt.Println(strings.Join(ctx, "\n"))
			continue
		case trimmed == ":clear":
			ctx = nil
			continue
		case strings.HasPrefix(trimmed, ":n "):
			if n, err := strconv.Atoi(strings.TrimSpace(trimmed[3:])); err == nil {
				e.SetLimit(n)
			}
			continue
		}
		imp := L.ParseImports([]byte(line))
		if len(imp.Aliases)+len(imp.Names)+len(imp.Scope) > 0 {
			ctx = append(ctx, line)
			fmt.Println("(added to context)")
			continue
		}
		hover := strings.HasPrefix(trimmed, ":h ")
		if hover {
			line = strings.TrimSpace(trimmed[3:])
		}
		content := strings.Join(append(append([]string(nil), ctx...), line), "\n")
		version++
		d := e.Open("untitled:repl", L.Name, version, []byte(content))
		start := time.Now()
		if hover {
			off := len(content)
			if off > 0 {
				off--
			}
			if info, ok := e.Hover(d, off); ok {
				printInfo(os.Stdout, info)
			} else {
				fmt.Println("(nothing found)")
			}
			continue
		}
		res := e.Complete(d, len(content), true)
		printCandidates(os.Stdout, res.Items, false)
		fmt.Printf("-- %d items in %s\n", len(res.Items), time.Since(start))
	}
}

// --- inspect ---------------------------------------------------------------

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	home := fs.String("home", "", "data directory")
	limit := fs.Int("n", 200, "maximum lines to print")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("inspect: give an index file or a language name")
	}
	target := fs.Arg(0)
	var paths []string
	if strings.HasSuffix(target, ".llsi") {
		paths = []string{target}
	} else {
		e, err := newEngine(*home, nil)
		if err != nil {
			return err
		}
		L := e.Language(target)
		if L == nil {
			return fmt.Errorf("unknown language %q", target)
		}
		paths = e.IndexPaths(L)
	}
	query := fs.Arg(1)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	printed := 0
	for _, p := range paths {
		r, err := index.Open(p)
		if err != nil {
			return err
		}
		meta, _ := r.Meta()
		switch {
		case query == "":
			fmt.Fprintf(out, "%s\n  language %s, %d modules, %d symbols, %s, built %s\n  sources: %s\n",
				p, meta.Language, meta.Modules, meta.Symbols, humanBytes(int64(r.Size())),
				meta.Created.Local().Format(time.DateTime), strings.Join(meta.Sources, ", "))
		case strings.HasSuffix(query, "*"):
			lo, hi := r.PrefixRange(strings.TrimSuffix(query, "*"))
			for i := lo; i < hi && printed < *limit; i++ {
				tag := ""
				if r.IsContainer(i) {
					tag = " (container)"
				}
				fmt.Fprintf(out, "%-50s %6d symbols%s\n", r.ModuleName(i), r.ModuleSymbolCount(i), tag)
				printed++
			}
		default:
			m, ok := r.Lookup(query)
			if !ok {
				continue
			}
			fmt.Fprintf(out, "module %s (%s): %d symbols\n", m.Name, p, len(m.Symbols))
			if len(m.Reexports) > 0 {
				fmt.Fprintf(out, "re-exports: %s\n", strings.Join(m.Reexports, ", "))
			}
			for _, s := range m.Symbols {
				if printed >= *limit {
					break
				}
				fmt.Fprintf(out, "  %-10s %-30s %s\n", s.Kind, s.Name, s.Detail)
				printed++
			}
		}
		r.Close()
	}
	return nil
}

// --- check -------------------------------------------------------------------

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	langName := fs.String("l", "", "language (spec name or LSP id); inferred from the file name when omitted")
	stdin := fs.Bool("stdin", false, "read the document from stdin")
	home := fs.String("home", "", "data directory")
	fs.Parse(args)
	files := fs.Args()
	if !*stdin && len(files) == 0 {
		return errors.New("check: give files or --stdin")
	}
	e, err := newEngine(*home, nil)
	if err != nil {
		return err
	}
	type input struct {
		name    string
		content []byte
	}
	var inputs []input
	if *stdin {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		inputs = append(inputs, input{"<stdin>", b})
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		inputs = append(inputs, input{f, b})
	}
	bw := bufio.NewWriter(os.Stdout)
	defer bw.Flush()
	failed := false
	for _, in := range inputs {
		uri := "untitled:stdin"
		if in.name != "<stdin>" {
			uri = engine.PathToURI(in.name)
		}
		d := e.Open(uri, *langName, 1, in.content)
		if d == nil {
			return fmt.Errorf("%s: unknown language (use -l, see: lls langs)", in.name)
		}
		if !d.Lang.HasSyntax() {
			fmt.Fprintf(os.Stderr, "lls: %s: language %s has no syntax rules\n", in.name, d.Lang.Name)
			continue
		}
		for _, dg := range d.Lang.Check(in.content) {
			line, _ := d.Position(dg.Start)
			col := dg.Start - d.Offset(line, 0) + 1
			sev := "error"
			if dg.Severity == lang.SeverityWarning {
				sev = "warning"
			} else {
				failed = true
			}
			fmt.Fprintf(bw, "%s:%d:%d: %s: %s\n", in.name, line+1, col, sev, dg.Message)
		}
		e.Close(uri)
	}
	if failed {
		bw.Flush()
		os.Exit(1)
	}
	return nil
}

// --- langs / spec ------------------------------------------------------------

func cmdLangs(args []string) error {
	fs := flag.NewFlagSet("langs", flag.ExitOnError)
	home := fs.String("home", "", "data directory")
	fs.Parse(args)
	e, err := newEngine(*home, nil)
	if err != nil {
		return err
	}
	fmt.Printf("home: %s\n\n", e.Home())
	for _, L := range e.Languages() {
		fmt.Printf("%-12s ids: %s  ext: %s\n", L.Name, strings.Join(L.LanguageIDs, ","), strings.Join(L.Extensions, " "))
		paths := e.IndexPaths(L)
		sort.Strings(paths)
		if len(paths) == 0 {
			fmt.Printf("%12s (no index: scripts/build-index.sh %s)\n", "", L.Name)
		}
		for _, p := range paths {
			if fi, err := os.Stat(p); err == nil {
				fmt.Printf("%12s %s (%s)\n", "", p, humanBytes(fi.Size()))
			}
		}
	}
	return nil
}

func cmdSpec(args []string) error {
	if len(args) < 1 {
		return errors.New("spec: give a language name")
	}
	data, err := lang.EmbeddedSpec(args[0])
	if err != nil {
		return fmt.Errorf("no built-in spec %q", args[0])
	}
	_, err = os.Stdout.Write(data)
	return err
}

// --- bench -------------------------------------------------------------------

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	var q queryFlags
	q.register(fs)
	n := fs.Int("n", 2000, "iterations")
	fs.Parse(args)

	t0 := time.Now()
	e, d, off, err := q.open(fs.Args())
	if err != nil {
		return err
	}
	res := e.Complete(d, off, true)
	cold := time.Since(t0)

	t1 := time.Now()
	for i := 0; i < *n; i++ {
		res = e.Complete(d, off, true)
	}
	warm := time.Since(t1) / time.Duration(*n)

	// Simulate typing: every iteration is a new document version, as after
	// each keystroke in an editor.
	t2 := time.Now()
	for i := 0; i < *n; i++ {
		d.Replace(d.Version+1, d.Text)
		res = e.Complete(d, off, true)
	}
	typing := time.Since(t2) / time.Duration(*n)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("document: %d bytes, %d results (prefix %q)\n", len(d.Text), len(res.Items), res.Prefix)
	fmt.Printf("cold start (engine + index mmap + first query): %s\n", cold)
	fmt.Printf("warm query:                                     %s\n", warm)
	fmt.Printf("query after edit (new version):                 %s\n", typing)
	fmt.Printf("heap in use: %s\n", humanBytes(int64(ms.HeapInuse)))
	return nil
}
