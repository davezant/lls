// Package text contains small, allocation-free helpers shared by every other
// package: ASCII case folding, identifier scanning, fuzzy matching and the
// UTF-8 <-> UTF-16 conversions required by the Language Server Protocol.
//
// Everything here sits on the hot path of completion requests, so the code
// favours simple byte loops over the more general (and slower) helpers found
// in the standard library's unicode packages.
package text

import "unicode/utf8"

// lower folds an ASCII upper-case letter to lower case. Non-ASCII bytes are
// returned unchanged; identifiers in the indexed libraries are overwhelmingly
// ASCII, and byte-wise folding keeps ordering stable and cheap.
func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// FoldCompare compares a and b ignoring ASCII case. It returns -1, 0 or +1.
// Strings that are equal under folding fall back to a byte-wise comparison so
// that the resulting order is total (required for binary search).
func FoldCompare(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ca, cb := lower(a[i]), lower(b[i])
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// FoldComparePrefix compares only the first len(prefix) bytes of s against
// prefix, ignoring ASCII case. A string shorter than the prefix compares as
// the shorter string would. It is the probe used for prefix range searches.
func FoldComparePrefix(s, prefix string) int {
	n := len(prefix)
	if len(s) < n {
		for i := 0; i < len(s); i++ {
			cs, cp := lower(s[i]), lower(prefix[i])
			if cs != cp {
				if cs < cp {
					return -1
				}
				return 1
			}
		}
		return -1
	}
	for i := 0; i < n; i++ {
		cs, cp := lower(s[i]), lower(prefix[i])
		if cs != cp {
			if cs < cp {
				return -1
			}
			return 1
		}
	}
	return 0
}

// HasFoldPrefix reports whether s starts with prefix, ignoring ASCII case.
func HasFoldPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if lower(s[i]) != lower(prefix[i]) {
			return false
		}
	}
	return true
}

// IdentTable is a 256-entry lookup table telling which bytes may appear in an
// identifier. Bytes >= 0x80 are always accepted so that UTF-8 encoded letters
// form part of identifiers without decoding runes.
type IdentTable [256]bool

// NewIdentTable returns the default identifier table ([A-Za-z0-9_] plus all
// non-ASCII bytes) extended with the characters in extra (e.g. "$" for
// JavaScript or "-" for Lisp-like languages).
func NewIdentTable(extra string) *IdentTable {
	var t IdentTable
	for c := 0; c < 256; c++ {
		b := byte(c)
		t[c] = b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b >= 0x80
	}
	for i := 0; i < len(extra); i++ {
		t[extra[i]] = true
	}
	return &t
}

// IsDigit reports whether c is an ASCII digit.
func IsDigit(c byte) bool { return c >= '0' && c <= '9' }

// IdentBefore returns the start offset of the identifier that ends at off.
// If the byte before off is not an identifier byte, off itself is returned.
func (t *IdentTable) IdentBefore(src []byte, off int) int {
	i := off
	for i > 0 && t[src[i-1]] {
		i--
	}
	return i
}

// IdentAfter returns the end offset of the identifier that starts at off.
func (t *IdentTable) IdentAfter(src []byte, off int) int {
	i := off
	for i < len(src) && t[src[i]] {
		i++
	}
	return i
}

// Words calls fn for every identifier in src that is at least minLen bytes
// long and does not start with a digit. member is true when the identifier
// is directly preceded by a '.' (or "->"/"::"), which lets callers build a
// separate set of "member words" used as a fallback for member completion.
//
// The callback receives a slice of src; it must copy it if it is retained.
func (t *IdentTable) Words(src []byte, minLen int, fn func(word []byte, member bool)) {
	n := len(src)
	i := 0
	for i < n {
		if !t[src[i]] {
			i++
			continue
		}
		start := i
		for i < n && t[src[i]] {
			i++
		}
		if i-start < minLen || IsDigit(src[start]) {
			continue
		}
		member := false
		if start > 0 {
			switch src[start-1] {
			case '.':
				member = true
			case '>', ':':
				member = start > 1 && (src[start-2] == '-' || src[start-2] == ':')
			}
		}
		fn(src[start:i], member)
	}
}

// Match scores how well candidate matches the typed prefix. It returns
// ok=false when there is no match at all. Higher scores are better:
//
//	exact case-sensitive prefix     > 3000
//	case-insensitive prefix         > 2000
//	fuzzy subsequence (first char)  > 1000
//
// Within a tier shorter candidates score slightly higher, so "len" beats
// "length" for the prefix "le". Fuzzy matching only applies to prefixes of at
// least two bytes to avoid flooding the list with noise.
func Match(candidate, prefix string) (score int, ok bool) {
	if prefix == "" {
		return 2000 - min(len(candidate), 64), true
	}
	if len(candidate) < len(prefix) {
		return 0, false
	}
	lenBonus := 64 - min(len(candidate)-len(prefix), 64)
	if candidate[:len(prefix)] == prefix {
		return 3000 + lenBonus, true
	}
	if HasFoldPrefix(candidate, prefix) {
		return 2000 + lenBonus, true
	}
	if len(prefix) < 2 || lower(candidate[0]) != lower(prefix[0]) {
		return 0, false
	}
	// Subsequence match; penalise gaps between matched characters and reward
	// matches that land on word boundaries (camelCase humps or after '_').
	j := 1
	gaps := 0
	bonus := 0
	last := 0
	for i := 1; i < len(candidate) && j < len(prefix); i++ {
		c := candidate[i]
		if lower(c) == lower(prefix[j]) {
			if i != last+1 {
				gaps++
			}
			if candidate[i-1] == '_' || (c >= 'A' && c <= 'Z' && !(candidate[i-1] >= 'A' && candidate[i-1] <= 'Z')) {
				bonus += 8
			}
			last = i
			j++
		}
	}
	if j < len(prefix) {
		return 0, false
	}
	return 1000 + bonus - gaps*10 + lenBonus/4, true
}

// UTF16Len returns the length of s in UTF-16 code units, which is the unit
// LSP uses for character offsets.
func UTF16Len(s []byte) int {
	n := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			n++
			i++
			continue
		}
		r, size := utf8.DecodeRune(s[i:])
		i += size
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// UTF16ToByte converts a UTF-16 column within line to a byte offset, clamping
// to the line length (clients may send positions past the end of a line).
func UTF16ToByte(line []byte, col int) int {
	units := 0
	i := 0
	for i < len(line) && units < col {
		c := line[i]
		if c == '\n' {
			break
		}
		if c < utf8.RuneSelf {
			units++
			i++
			continue
		}
		r, size := utf8.DecodeRune(line[i:])
		i += size
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
	}
	return i
}
