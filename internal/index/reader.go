package index

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"unsafe"

	"lls/internal/text"
)

// Module is a decoded module. Its strings alias the mapped index file, so a
// Module must not outlive the Reader that produced it (readers are normally
// kept open for the whole life of the process).
type Module struct {
	Name      string
	Reexports []string
	Symbols   []Symbol // sorted with text.FoldCompare on Name
}

// Reader gives read-only, lazily decoded access to an index file.
// It is safe for concurrent use.
type Reader struct {
	Path  string
	data  []byte
	unmap func() error
	count int
	table []byte

	mu    sync.Mutex
	cache []*Module // decoded modules, indexed by table position
}

// ErrFormat reports a malformed or incompatible index file.
var ErrFormat = errors.New("index: invalid or incompatible file")

// Open maps the index file at path. Only the header is validated; modules
// are decoded on demand.
func Open(path string) (*Reader, error) {
	data, unmap, err := mapFile(path)
	if err != nil {
		return nil, err
	}
	r, err := newReader(data)
	if err != nil {
		if unmap != nil {
			_ = unmap()
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r.Path, r.unmap = path, unmap
	return r, nil
}

// NewReaderBytes wraps an in-memory index (used by tests and tooling).
func NewReaderBytes(data []byte) (*Reader, error) { return newReader(data) }

func newReader(data []byte) (*Reader, error) {
	if len(data) < headerSize || string(data[:4]) != Magic {
		return nil, ErrFormat
	}
	if binary.LittleEndian.Uint32(data[4:]) != Version {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrFormat, binary.LittleEndian.Uint32(data[4:]))
	}
	count := int(binary.LittleEndian.Uint32(data[8:]))
	tableOff := int(binary.LittleEndian.Uint32(data[16:]))
	end := tableOff + count*tableEntrySize
	if tableOff < headerSize || end > len(data) {
		return nil, ErrFormat
	}
	return &Reader{
		data:  data,
		count: count,
		table: data[tableOff:end],
		cache: make([]*Module, count),
	}, nil
}

// Close unmaps the file. Modules obtained from the reader become invalid.
func (r *Reader) Close() error {
	if r.unmap != nil {
		err := r.unmap()
		r.unmap = nil
		return err
	}
	return nil
}

// NumModules returns the number of modules in the index.
func (r *Reader) NumModules() int { return r.count }

// Size returns the size of the index file in bytes.
func (r *Reader) Size() int { return len(r.data) }

// str returns a zero-copy string view of data[off:off+n].
func (r *Reader) str(off, n int) string {
	if n == 0 {
		return ""
	}
	return unsafe.String(&r.data[off], n)
}

func (r *Reader) entry(i int) (nameOff, nameLen, dataOff, dataLen, count int) {
	e := r.table[i*tableEntrySize:]
	return int(binary.LittleEndian.Uint32(e)),
		int(binary.LittleEndian.Uint32(e[4:])),
		int(binary.LittleEndian.Uint32(e[8:])),
		int(binary.LittleEndian.Uint32(e[12:])),
		int(binary.LittleEndian.Uint32(e[16:]) &^ containerFlag)
}

// IsContainer reports whether the i-th module is a container module (class
// members) rather than an importable module.
func (r *Reader) IsContainer(i int) bool {
	return binary.LittleEndian.Uint32(r.table[i*tableEntrySize+16:])&containerFlag != 0
}

// ModuleName returns the name of the i-th module (in sorted order).
func (r *Reader) ModuleName(i int) string {
	off, n, _, _, _ := r.entry(i)
	return r.str(off, n)
}

// ModuleSymbolCount returns the number of symbols of the i-th module without
// decoding it.
func (r *Reader) ModuleSymbolCount(i int) int {
	_, _, _, _, c := r.entry(i)
	return c
}

// Find returns the table position of the module called name.
func (r *Reader) Find(name string) (int, bool) {
	i := sort.Search(r.count, func(i int) bool { return r.ModuleName(i) >= name })
	if i < r.count && r.ModuleName(i) == name {
		return i, true
	}
	return 0, false
}

// PrefixRange returns the half-open range [lo, hi) of modules whose names
// start with prefix (byte-wise).
func (r *Reader) PrefixRange(prefix string) (lo, hi int) {
	lo = sort.Search(r.count, func(i int) bool { return r.ModuleName(i) >= prefix })
	hi = lo + sort.Search(r.count-lo, func(i int) bool {
		n := r.ModuleName(lo + i)
		return len(n) < len(prefix) || n[:len(prefix)] != prefix
	})
	return lo, hi
}

// Lookup returns the decoded module called name.
func (r *Reader) Lookup(name string) (*Module, bool) {
	i, ok := r.Find(name)
	if !ok {
		return nil, false
	}
	m, err := r.ModuleAt(i)
	return m, err == nil
}

// ModuleAt decodes (once) and returns the i-th module.
func (r *Reader) ModuleAt(i int) (*Module, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.cache[i]; m != nil {
		return m, nil
	}
	nameOff, nameLen, dataOff, dataLen, count := r.entry(i)
	if dataOff+dataLen > len(r.data) {
		return nil, ErrFormat
	}
	m := &Module{Name: r.str(nameOff, nameLen), Symbols: make([]Symbol, 0, count)}
	p := dataOff
	end := dataOff + dataLen
	readStr := func() (string, bool) {
		n, w := binary.Uvarint(r.data[p:end])
		if w <= 0 || p+w+int(n) > end {
			return "", false
		}
		s := r.str(p+w, int(n))
		p += w + int(n)
		return s, true
	}
	nre, w := binary.Uvarint(r.data[p:end])
	if w <= 0 {
		return nil, ErrFormat
	}
	p += w
	if nre > 0 {
		m.Reexports = make([]string, 0, nre)
		for j := uint64(0); j < nre; j++ {
			s, ok := readStr()
			if !ok {
				return nil, ErrFormat
			}
			m.Reexports = append(m.Reexports, s)
		}
	}
	for p < end {
		kind := Kind(r.data[p])
		p++
		name, ok1 := readStr()
		detail, ok2 := readStr()
		doc, ok3 := readStr()
		if !ok1 || !ok2 || !ok3 {
			return nil, ErrFormat
		}
		m.Symbols = append(m.Symbols, Symbol{Name: name, Detail: detail, Doc: doc, Kind: kind})
	}
	r.cache[i] = m
	return m, nil
}

// Meta decodes the metadata blob.
func (r *Reader) Meta() (Meta, error) {
	var meta Meta
	off := int(binary.LittleEndian.Uint32(r.data[20:]))
	n := int(binary.LittleEndian.Uint32(r.data[24:]))
	if off+n > len(r.data) {
		return meta, ErrFormat
	}
	err := json.Unmarshal(r.data[off:off+n], &meta)
	return meta, err
}

// PrefixRange returns the half-open range of symbols whose name starts with
// prefix, ignoring ASCII case. It is a pair of binary searches.
func (m *Module) PrefixRange(prefix string) (lo, hi int) {
	syms := m.Symbols
	lo = sort.Search(len(syms), func(i int) bool { return text.FoldComparePrefix(syms[i].Name, prefix) >= 0 })
	hi = lo + sort.Search(len(syms)-lo, func(i int) bool { return text.FoldComparePrefix(syms[lo+i].Name, prefix) > 0 })
	return lo, hi
}

// Get returns the symbol named exactly name (case-sensitive). If several
// kinds share the name, the first one is returned.
func (m *Module) Get(name string) (*Symbol, bool) {
	lo, hi := m.PrefixRange(name)
	for i := lo; i < hi; i++ {
		if m.Symbols[i].Name == name {
			return &m.Symbols[i], true
		}
	}
	return nil, false
}
