package ctx

import (
	"bytes"
	"strconv"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxScanDepth = 512 // fast mode; deeper lines go to the slow mode
	maxScanKeys  = 64  // per object in fast mode; bigger objects go to the slow mode
)

// scanner walks one JSON object without decoding values, in one of two modes.
//
// Fast mode is allocation-free and recursive: it answers whether the line is
// valid and repeats no key, comparing raw key bytes, and answers false
// whenever it cannot be sure (keys holding an escape or invalid UTF-8, big or
// deep objects, malformed input).
//
// Slow mode compares keys by their decoded value, as encoding/json does, and
// records where each repeated key needs a prefix. It keeps an explicit stack,
// so nesting depth is limited only by memory, and it never changes a value;
// a key keeps its own escapes.
type scanner struct {
	p       []byte
	i       int
	keys    [][2]int // fast: spans into p of the keys of the open objects
	stack   []frame  // slow: the open containers
	renames []rename // slow: in document order
	esc     bool     // the last string held a backslash
	hiBit   bool     // the last string held a byte >= 0x80
}

// rename says: insert the prefix numbered n at offset at, just inside the
// opening quote of a key. See prefix.
type rename struct{ at, n int }

var scanners = sync.Pool{New: func() any { return new(scanner) }}

// A scanner whose buffers grew beyond these is not pooled.
const (
	maxPooledFrames = 1024
	maxPooledSpans  = 4096 // keys and renames, 16 bytes each
)

// putScanner returns s to the pool and reports whether it did. It drops the
// references the scanner holds (the caller's buffer, and the key maps of frames
// an aborted scan left open) and refuses a scanner whose buffers a huge line
// grew, so one such line does not pin memory.
func putScanner(s *scanner) bool {
	s.p = nil
	clear(s.stack[:cap(s.stack)])
	if cap(s.stack) > maxPooledFrames || cap(s.keys) > maxPooledSpans || cap(s.renames) > maxPooledSpans {
		return false
	}
	scanners.Put(s)
	return true
}

// scan reports whether p is exactly one JSON object, with optional surrounding
// whitespace. In fast mode it also requires that no object repeats a key.
func (s *scanner) scan(p []byte, slow bool) bool {
	s.p, s.i = p, 0
	s.keys, s.renames, s.stack = s.keys[:0], s.renames[:0], s.stack[:0]
	if !s.skipSpace() || s.p[s.i] != '{' {
		return false
	}
	if slow {
		return s.scanSlow() && !s.skipSpace()
	}
	return s.object(0) && !s.skipSpace()
}

func (s *scanner) peek() byte {
	if s.i < len(s.p) {
		return s.p[s.i]
	}
	return 0
}

// skipSpace advances over whitespace and reports whether input remains.
func (s *scanner) skipSpace() bool {
	for s.i < len(s.p) {
		switch s.p[s.i] {
		case ' ', '\t', '\r', '\n':
			s.i++
		default:
			return true
		}
	}
	return false
}

func (s *scanner) expect(c byte) bool {
	if s.skipSpace() && s.p[s.i] == c {
		s.i++
		return true
	}
	return false
}

func (s *scanner) value(depth int) bool {
	if !s.skipSpace() {
		return false
	}
	switch s.p[s.i] {
	case '{':
		return s.object(depth + 1)
	case '[':
		return s.array(depth + 1)
	}
	return s.scalar()
}

func (s *scanner) scalar() bool {
	switch s.p[s.i] {
	case '"':
		_, _, ok := s.str()
		return ok
	case 't':
		return s.literal("true")
	case 'f':
		return s.literal("false")
	case 'n':
		return s.literal("null")
	}
	return s.number()
}

func (s *scanner) object(depth int) bool {
	if depth > maxScanDepth {
		return false
	}
	s.i++ // {
	base := len(s.keys)

	if s.skipSpace() && s.p[s.i] == '}' {
		s.i++
		return true
	}
	for {
		if !s.skipSpace() || s.p[s.i] != '"' {
			return false
		}
		start, end, ok := s.str()
		if !ok || s.esc || (s.hiBit && !utf8.Valid(s.p[start:end])) || len(s.keys)-base >= maxScanKeys {
			return false
		}
		key := s.p[start:end]
		for _, k := range s.keys[base:] {
			if bytes.Equal(key, s.p[k[0]:k[1]]) {
				return false
			}
		}
		s.keys = append(s.keys, [2]int{start, end})

		if !s.expect(':') || !s.value(depth) || !s.skipSpace() {
			return false
		}
		c := s.p[s.i]
		s.i++
		if c == '}' {
			s.keys = s.keys[:base]
			return true
		}
		if c != ',' {
			return false
		}
	}
}

func (s *scanner) array(depth int) bool {
	if depth > maxScanDepth {
		return false
	}
	s.i++ // [
	if s.skipSpace() && s.p[s.i] == ']' {
		s.i++
		return true
	}
	for {
		if !s.value(depth) || !s.skipSpace() {
			return false
		}
		c := s.p[s.i]
		s.i++
		if c == ']' {
			return true
		}
		if c != ',' {
			return false
		}
	}
}

// str scans the string at s.i and returns the span between its quotes.
func (s *scanner) str() (start, end int, ok bool) {
	s.esc, s.hiBit = false, false
	s.i++ // opening quote
	start = s.i
	for s.i < len(s.p) {
		switch c := s.p[s.i]; {
		case c == '"':
			end = s.i
			s.i++
			return start, end, true
		case c == '\\':
			s.esc = true
			if s.i+1 >= len(s.p) {
				return 0, 0, false
			}
			switch s.p[s.i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				s.i += 2
			case 'u':
				if s.i+6 > len(s.p) {
					return 0, 0, false
				}
				for _, h := range s.p[s.i+2 : s.i+6] {
					if !isHex(h) {
						return 0, 0, false
					}
				}
				s.i += 6
			default:
				return 0, 0, false
			}
		case c < 0x20:
			return 0, 0, false
		default:
			if c >= 0x80 {
				s.hiBit = true
			}
			s.i++
		}
	}
	return 0, 0, false
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func (s *scanner) literal(lit string) bool {
	if len(s.p)-s.i >= len(lit) && string(s.p[s.i:s.i+len(lit)]) == lit {
		s.i += len(lit)
		return true
	}
	return false
}

// number scans -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
func (s *scanner) number() bool {
	if s.peek() == '-' {
		s.i++
	}
	switch c := s.peek(); {
	case c == '0':
		s.i++
	case c >= '1' && c <= '9':
		s.digits()
	default:
		return false
	}
	if s.peek() == '.' {
		s.i++
		if !s.digits() {
			return false
		}
	}
	if c := s.peek(); c == 'e' || c == 'E' {
		s.i++
		if c := s.peek(); c == '+' || c == '-' {
			s.i++
		}
		if !s.digits() {
			return false
		}
	}
	return true
}

func (s *scanner) digits() bool {
	start := s.i
	for s.i < len(s.p) && s.p[s.i] >= '0' && s.p[s.i] <= '9' {
		s.i++
	}
	return s.i > start
}

type frameState uint8

const (
	stStart      frameState = iota // just opened
	stNeedKey                      // after a comma in an object
	stNeedValue                    // after a comma in an array
	stAfterValue                   // after a value: a comma or the closing delimiter
)

type frame struct {
	obj   bool
	state frameState
	names keyNames
}

// scanSlow scans the object whose '{' is at s.i, checking the JSON grammar
// and collecting renames, with an explicit stack instead of recursion.
func (s *scanner) scanSlow() bool {
	s.i++
	s.stack = append(s.stack, frame{obj: true})
	for len(s.stack) > 0 {
		f := &s.stack[len(s.stack)-1]
		if !s.skipSpace() {
			return false
		}
		c := s.p[s.i]

		closer := byte(']')
		if f.obj {
			closer = '}'
		}
		switch f.state {
		case stAfterValue:
			s.i++
			switch c {
			case ',':
				f.state = stNeedValue
				if f.obj {
					f.state = stNeedKey
				}
			case closer:
				s.pop()
			default:
				return false
			}
			continue
		case stStart:
			if c == closer {
				s.i++
				s.pop()
				continue
			}
		}

		if f.obj {
			if c != '"' {
				return false
			}
			start, end, ok := s.str()
			if !ok {
				return false
			}
			if n := f.names.add(decodeKey(s.p[start:end])); n > 0 {
				s.renames = append(s.renames, rename{start, n})
			}
			if !s.expect(':') || !s.skipSpace() {
				return false
			}
			c = s.p[s.i]
		}

		f.state = stAfterValue
		if c == '{' || c == '[' {
			s.i++
			s.stack = append(s.stack, frame{obj: c == '{'}) // f is stale from here
			continue
		}
		if !s.scalar() {
			return false
		}
	}
	return true
}

func (s *scanner) pop() {
	s.stack[len(s.stack)-1] = frame{} // drop the key map
	s.stack = s.stack[:len(s.stack)-1]
}

// keyNames tracks the decoded keys of one object in slow mode.
type keyNames struct {
	seen map[string]struct{}
	// next is the number tried next for attr<n>_: one counter per object that
	// only grows, so every failed attempt is charged to a distinct existing key
	// and renaming stays linear.
	next int
}

// add records key and returns the prefix number it needs to be unique among
// the keys of the object: 0 for none, 1 for "attr_", n >= 2 for "attr<n>_".
func (k *keyNames) add(key string) int {
	if k.seen == nil {
		k.seen = map[string]struct{}{}
		k.next = 2
	}
	if _, taken := k.seen[key]; !taken {
		k.seen[key] = struct{}{}
		return 0
	}
	if _, taken := k.seen[prefix(1)+key]; !taken {
		k.seen[prefix(1)+key] = struct{}{}
		return 1
	}
	for {
		n := k.next
		k.next++
		if _, taken := k.seen[prefix(n)+key]; !taken {
			k.seen[prefix(n)+key] = struct{}{}
			return n
		}
	}
}

// prefix returns "attr_" for n == 1 and "attr<n>_" for n >= 2.
func prefix(n int) string {
	if n == 1 {
		return "attr_"
	}
	return "attr" + strconv.Itoa(n) + "_"
}

// decodeKey decodes the inside of a JSON string exactly as encoding/json does:
// invalid UTF-8 and lone surrogates become U+FFFD. raw must be a string the
// scanner accepted, so its escapes are well formed.
func decodeKey(raw []byte) string {
	if bytes.IndexByte(raw, '\\') < 0 && utf8.Valid(raw) {
		return string(raw)
	}
	b := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); {
		c := raw[i]
		switch {
		case c == '\\' && raw[i+1] == 'u':
			r := hex4(raw[i+2 : i+6])
			i += 6
			if utf16.IsSurrogate(r) {
				paired := false
				if i+6 <= len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
					if dec := utf16.DecodeRune(r, hex4(raw[i+2:i+6])); dec != unicode.ReplacementChar {
						r, paired = dec, true
						i += 6
					}
				}
				if !paired {
					r = unicode.ReplacementChar
				}
			}
			b = utf8.AppendRune(b, r)
		case c == '\\':
			b = append(b, unescape(raw[i+1]))
			i += 2
		case c < utf8.RuneSelf:
			b = append(b, c)
			i++
		default:
			r, size := utf8.DecodeRune(raw[i:])
			b = utf8.AppendRune(b, r)
			i += size
		}
	}
	return string(b)
}

func unescape(c byte) byte {
	switch c {
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	}
	return c // " \ /
}

func hex4(h []byte) rune {
	var r rune
	for _, c := range h {
		switch {
		case c >= 'a':
			c -= 'a' - 10
		case c >= 'A':
			c -= 'A' - 10
		default:
			c -= '0'
		}
		r = r<<4 | rune(c)
	}
	return r
}
