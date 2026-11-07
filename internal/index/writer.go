package index

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strings"

	"lls/internal/text"
)

// Builder accumulates modules and symbols in memory and serialises them into
// the index format. It de-duplicates symbols by (name, kind): when the same
// symbol is seen twice (e.g. from a .py and a .pyi file, or from several
// platform specific Go files) the entry carrying documentation and the more
// descriptive signature wins.
//
// Builder is not safe for concurrent use; the build pipeline funnels all
// results through a single goroutine.
type Builder struct {
	modules map[string]*moduleBuild
}

type moduleBuild struct {
	symbols   []Symbol
	byKey     map[symKey]int
	reexports []string
	reSet     map[string]struct{}
	container bool
}

type symKey struct {
	name string
	kind Kind
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder {
	return &Builder{modules: make(map[string]*moduleBuild)}
}

func (b *Builder) module(name string) *moduleBuild {
	m := b.modules[name]
	if m == nil {
		m = &moduleBuild{byKey: make(map[symKey]int)}
		b.modules[name] = m
	}
	return m
}

// EnsureModule makes sure a module exists even if it ends up empty, so that
// import-path completion can still offer it.
func (b *Builder) EnsureModule(name string) { b.module(name) }

// MarkContainer flags a module as a synthetic container module (e.g. the
// members of a class). Container modules are not advertised as sub-modules
// of their parent by AddSubmoduleSymbols.
func (b *Builder) MarkContainer(name string) { b.module(name).container = true }

// HasModule reports whether the module has been registered.
func (b *Builder) HasModule(name string) bool {
	_, ok := b.modules[name]
	return ok
}

// Add inserts or merges a symbol into a module.
func (b *Builder) Add(module string, s Symbol) {
	if s.Name == "" {
		return
	}
	m := b.module(module)
	k := symKey{s.Name, s.Kind}
	if i, ok := m.byKey[k]; ok {
		old := &m.symbols[i]
		if old.Doc == "" && s.Doc != "" {
			old.Doc = s.Doc
		}
		if len(s.Detail) > len(old.Detail) {
			old.Detail = s.Detail
		}
		return
	}
	m.byKey[k] = len(m.symbols)
	m.symbols = append(m.symbols, s)
}

// AddReexport records that target's symbols are visible through module.
func (b *Builder) AddReexport(module, target string) {
	if module == target || target == "" {
		return
	}
	m := b.module(module)
	if m.reSet == nil {
		m.reSet = make(map[string]struct{})
	}
	if _, ok := m.reSet[target]; ok {
		return
	}
	m.reSet[target] = struct{}{}
	m.reexports = append(m.reexports, target)
}

// AddSubmoduleSymbols adds, for every module "a<sep>b<sep>c", a module-kind
// symbol "c" to module "a<sep>b" (creating the parent when needed). This is
// what makes "os." offer "path" in Python.
func (b *Builder) AddSubmoduleSymbols(sep string) {
	if sep == "" {
		return
	}
	names := make([]string, 0, len(b.modules))
	for name, m := range b.modules {
		if !m.container {
			names = append(names, name)
		}
	}
	for _, name := range names {
		for {
			i := strings.LastIndex(name, sep)
			if i <= 0 {
				break
			}
			parent, child := name[:i], name[i+len(sep):]
			b.Add(parent, Symbol{Name: child, Kind: KindModule, Detail: "module " + name})
			name = parent
		}
	}
}

// Counts returns the number of modules and symbols currently held.
func (b *Builder) Counts() (modules, symbols int) {
	for _, m := range b.modules {
		symbols += len(m.symbols)
	}
	return len(b.modules), symbols
}

// ErrTooLarge is returned when the index would exceed the 4 GiB limit imposed
// by 32-bit offsets.
var ErrTooLarge = errors.New("index: file would exceed 4 GiB")

// Write serialises the index to w. meta.Modules and meta.Symbols are filled
// in automatically.
func (b *Builder) Write(w io.Writer, meta Meta) error {
	names := make([]string, 0, len(b.modules))
	for name := range b.modules {
		names = append(names, name)
	}
	slices.Sort(names)

	// Encode everything into a single buffer: index files are small (a few
	// MB for a whole standard library) and this keeps the code simple.
	buf := make([]byte, headerSize, 1<<20)
	nameOffs := make([]uint32, len(names))
	for i, name := range names {
		nameOffs[i] = uint32(len(buf))
		buf = append(buf, name...)
	}

	type entry struct{ dataOff, dataLen, count uint32 }
	entries := make([]entry, len(names))
	total := 0
	for i, name := range names {
		m := b.modules[name]
		slices.SortFunc(m.symbols, func(a, c Symbol) int {
			if r := text.FoldCompare(a.Name, c.Name); r != 0 {
				return r
			}
			return int(a.Kind) - int(c.Kind)
		})
		start := len(buf)
		buf = binary.AppendUvarint(buf, uint64(len(m.reexports)))
		for _, r := range m.reexports {
			buf = appendString(buf, r)
		}
		for _, s := range m.symbols {
			buf = append(buf, byte(s.Kind))
			buf = appendString(buf, s.Name)
			buf = appendString(buf, s.Detail)
			buf = appendString(buf, s.Doc)
		}
		count := uint32(len(m.symbols))
		if m.container {
			count |= containerFlag
		}
		entries[i] = entry{uint32(start), uint32(len(buf) - start), count}
		total += len(m.symbols)
		if len(buf) > math.MaxUint32 {
			return ErrTooLarge
		}
	}

	tableOff := len(buf)
	for i, name := range names {
		buf = binary.LittleEndian.AppendUint32(buf, nameOffs[i])
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(name)))
		buf = binary.LittleEndian.AppendUint32(buf, entries[i].dataOff)
		buf = binary.LittleEndian.AppendUint32(buf, entries[i].dataLen)
		buf = binary.LittleEndian.AppendUint32(buf, entries[i].count)
	}

	meta.Modules, meta.Symbols = len(names), total
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	metaOff := len(buf)
	buf = append(buf, metaJSON...)
	if len(buf) > math.MaxUint32 {
		return ErrTooLarge
	}

	copy(buf[0:4], Magic)
	binary.LittleEndian.PutUint32(buf[4:], Version)
	binary.LittleEndian.PutUint32(buf[8:], uint32(len(names)))
	binary.LittleEndian.PutUint32(buf[12:], 0)
	binary.LittleEndian.PutUint32(buf[16:], uint32(tableOff))
	binary.LittleEndian.PutUint32(buf[20:], uint32(metaOff))
	binary.LittleEndian.PutUint32(buf[24:], uint32(len(metaJSON)))

	bw := bufio.NewWriterSize(w, 1<<16)
	if _, err := bw.Write(buf); err != nil {
		return err
	}
	return bw.Flush()
}

func appendString(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}
