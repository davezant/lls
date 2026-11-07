// Package index implements the compact on-disk symbol index (".llsi" files)
// that lls compiles from library sources and loads lazily at runtime.
//
// # Design goals
//
//   - Startup cost must be O(1): opening an index only maps the file and reads
//     a 32-byte header. Nothing is parsed until a module is actually used.
//   - Module lookups are a binary search over a fixed-width table that lives
//     inside the mapped file, so no per-module allocation is needed to find one.
//   - A module's symbols are decoded on first use only, and strings point
//     straight into the mapped file (zero copy). On a Raspberry Pi this keeps
//     the resident memory proportional to the libraries you actually import.
//
// # File layout (all integers little-endian)
//
//	offset  size  field
//	0       4     magic "LLSI"
//	4       4     format version (currently 1)
//	8       4     module count
//	12      4     flags (reserved, 0)
//	16      4     module table offset
//	20      4     metadata offset (JSON blob)
//	24      4     metadata length
//	28      4     reserved
//	32      ...   module name blob, module data blocks, module table, metadata
//
// The module table holds one 20-byte entry per module, sorted byte-wise by
// module name:
//
//	u32 name offset, u32 name length, u32 data offset, u32 data length, u32 symbol count
//
// The top bit of the symbol count flags a container module (the members of a
// class or type) as opposed to an importable module.
//
// A module data block is:
//
//	uvarint re-export count, then for each: uvarint length + bytes
//	then, for each symbol (sorted with text.FoldCompare on the name):
//	  u8 kind, uvarint len + name, uvarint len + detail, uvarint len + doc
//
// Re-exports name other modules whose symbols are visible through this one
// (C "#include", Python "from .x import *", JavaScript "export * from").
package index

import "time"

// Magic identifies an lls index file.
const Magic = "LLSI"

// Version is the current on-disk format version.
const Version = 1

const (
	headerSize     = 32
	tableEntrySize = 20
	containerFlag  = 1 << 31
)

// Kind classifies a symbol. The numeric values are identical to the LSP
// CompletionItemKind enumeration so they can be sent to clients unchanged.
type Kind uint8

// Symbol kinds (values match LSP CompletionItemKind).
const (
	KindText          Kind = 1
	KindMethod        Kind = 2
	KindFunction      Kind = 3
	KindConstructor   Kind = 4
	KindField         Kind = 5
	KindVariable      Kind = 6
	KindClass         Kind = 7
	KindInterface     Kind = 8
	KindModule        Kind = 9
	KindProperty      Kind = 10
	KindUnit          Kind = 11
	KindValue         Kind = 12
	KindEnum          Kind = 13
	KindKeyword       Kind = 14
	KindSnippet       Kind = 15
	KindEnumMember    Kind = 20
	KindConstant      Kind = 21
	KindStruct        Kind = 22
	KindEvent         Kind = 23
	KindOperator      Kind = 24
	KindTypeParameter Kind = 25
)

var kindNames = map[string]Kind{
	"text": KindText, "method": KindMethod, "function": KindFunction,
	"constructor": KindConstructor, "field": KindField, "variable": KindVariable,
	"class": KindClass, "interface": KindInterface, "module": KindModule,
	"property": KindProperty, "unit": KindUnit, "value": KindValue,
	"enum": KindEnum, "keyword": KindKeyword, "snippet": KindSnippet,
	"enummember": KindEnumMember, "constant": KindConstant, "struct": KindStruct,
	"event": KindEvent, "operator": KindOperator, "typeparameter": KindTypeParameter,
	// Friendly aliases accepted in language specs and JSONL input.
	"type": KindClass, "const": KindConstant, "var": KindVariable,
	"func": KindFunction, "fn": KindFunction, "macro": KindConstant,
	"trait": KindInterface, "package": KindModule,
}

// ParseKind converts a kind name (case-sensitive, lower case) to a Kind.
func ParseKind(s string) (Kind, bool) {
	k, ok := kindNames[s]
	return k, ok
}

// String returns a short human readable name of the kind.
func (k Kind) String() string {
	switch k {
	case KindText:
		return "text"
	case KindMethod:
		return "method"
	case KindFunction:
		return "function"
	case KindConstructor:
		return "constructor"
	case KindField:
		return "field"
	case KindVariable:
		return "variable"
	case KindClass:
		return "class"
	case KindInterface:
		return "interface"
	case KindModule:
		return "module"
	case KindProperty:
		return "property"
	case KindEnum:
		return "enum"
	case KindKeyword:
		return "keyword"
	case KindSnippet:
		return "snippet"
	case KindEnumMember:
		return "enummember"
	case KindConstant:
		return "constant"
	case KindStruct:
		return "struct"
	case KindTypeParameter:
		return "typeparam"
	}
	return "symbol"
}

// Symbol is one completion entry inside a module.
type Symbol struct {
	Name   string // identifier as written in source
	Detail string // one-line signature, e.g. "def join(a, *p)"
	Doc    string // short documentation (first paragraph), may be empty
	Kind   Kind
}

// Meta is free-form build information stored at the end of the file. It is
// only read by "lls inspect"; the runtime never needs it.
type Meta struct {
	Language string    `json:"language"`
	Created  time.Time `json:"created"`
	Sources  []string  `json:"sources"`
	Modules  int       `json:"modules"`
	Symbols  int       `json:"symbols"`
	Builder  string    `json:"builder"`
}
