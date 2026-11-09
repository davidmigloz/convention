package ctx

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
)

// cleanLine reports whether the fast scan accepts p.
func cleanLine(p []byte) bool { return new(scanner).scan(p, false) }

func TestCleanLine(t *testing.T) {
	deep := func(n int) string {
		return strings.Repeat(`{"k":`, n) + "1" + strings.Repeat("}", n)
	}
	manyKeys := func(n int) string {
		var b strings.Builder
		b.WriteString("{")
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"k` + strings.Repeat("x", i) + `":1`)
		}
		b.WriteString("}")
		return b.String()
	}

	clean := []string{
		`{}`,
		"{}\n",
		" \t\r\n{ \"a\" : 1 , \"b\" : { } }\n",
		`{"a":1,"b":{"a":2}}`,
		`{"x":{"k":1},"y":{"k":2}}`,
		`{"l":[{"k":1},{"k":2},[{"k":3},{"k":4}]]}`,
		`{"a":"{}\":\\:,[]","b":"x"}`,
		`{"a":"é\n\/","b":"é"}`,
		`{"n":[0,-0,1.5,-0.5e+10,2E-3,10],"t":true,"f":false,"z":null}`,
		`{"é":1,"ü":2}`,
		`{"a":[],"b":[ ],"c":{}}`,
		deep(100),
		manyKeys(maxScanKeys),
	}
	for _, in := range clean {
		if !cleanLine([]byte(in)) {
			t.Errorf("want clean: %.80q", in)
		}
	}

	notClean := []string{
		// repeats
		`{"a":1,"a":2}`,
		`{"a":{"a":1},"a":2}`,
		`{"o":{"k":1,"k":2}}`,
		`{"l":[{"k":1,"k":2}]}`,
		`{"a":1,"b":2,"c":3,"a":4}`,
		// equal when decoded, spelled differently or not decidable
		`{"a":1,"a":2}`,
		`{"\"":1,"\"":2}`,
		`{"\xff":1,"\xfe":2}`,
		"{\"\xff\":1,\"\xfe\":2}",
		// too big or too deep for the fast path
		manyKeys(maxScanKeys + 1),
		deep(maxScanDepth + 2),
		// malformed or not a single object
		``,
		` `,
		`[]`,
		`1`,
		`"a"`,
		`{`,
		`{"a"`,
		`{"a":`,
		`{"a"}`,
		`{"a":}`,
		`{"a":1,}`,
		`{,"a":1}`,
		`{"a":1 "b":2}`,
		`{a:1}`,
		`{"a":01}`,
		`{"a":1.}`,
		`{"a":-}`,
		`{"a":1e}`,
		`{"a":tru}`,
		`{"a":nul}`,
		`{"a":"x}`,
		`{"a":"\q"}`,
		`{"a":"\u12"}`,
		"{\"a\":\"x\ny\"}",
		`{"a":[1,]}`,
		`{"a":[1 2]}`,
		`{"a":1}{"b":2}`,
		`{"a":1} x`,
		`{"a":1}}`,
	}
	for _, in := range notClean {
		if cleanLine([]byte(in)) {
			t.Errorf("want not clean: %.80q", in)
		}
	}
}

func TestScanFastMode_DoesNotAllocate(t *testing.T) {
	p := []byte(`{"time":"t","level":"INFO","source":{"function":"f","file":"x.go","line":1},"msg":"m","a":[1,{"k":2}],"b":{"k":3}}` + "\n")
	// Not through the pool: under -race, sync.Pool drops items at random.
	s := new(scanner)
	if !s.scan(p, false) { // also grows keys once
		t.Fatal("line not clean")
	}
	if n := testing.AllocsPerRun(100, func() { s.scan(p, false) }); n != 0 {
		t.Fatalf("allocs = %v", n)
	}
}

// maxJSONDepth is encoding/json's nesting limit.
const maxJSONDepth = 10000

// nestingDepth returns the deepest nesting of [ and { outside strings.
func nestingDepth(data []byte) int {
	depth, max := 0, 0
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case inString && c == '\\':
			i++
		case c == '"':
			inString = !inString
		case inString:
		case c == '[' || c == '{':
			depth++
			if depth > max {
				max = depth
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return max
}

// newDecoder decodes numbers as json.Number so an out-of-range number is not
// an error.
func newDecoder(data []byte) *json.Decoder {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec
}

// isObject reports whether data is valid JSON whose top level is an object.
func isObject(data []byte) bool {
	if !json.Valid(data) {
		return false
	}
	tok, err := newDecoder(data).Token()
	return err == nil && tok == json.Delim('{')
}

// repeatsKey is the oracle: it uses encoding/json to find a repeated key in
// any object of data, which must be a valid JSON object.
func repeatsKey(t *testing.T, data []byte) bool {
	t.Helper()
	dec := newDecoder(data)
	if _, err := dec.Token(); err != nil {
		t.Fatalf("oracle: %v", err)
	}
	return repeatsIn(t, dec)
}

// repeatsIn consumes the rest of an object whose '{' was already read.
func repeatsIn(t *testing.T, dec *json.Decoder) bool {
	t.Helper()
	rep := false
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("oracle: %v", err)
		}
		k := kt.(string)
		rep = rep || seen[k]
		seen[k] = true
		rep = repeatsValue(t, dec) || rep
	}
	if _, err := dec.Token(); err != nil { // }
		t.Fatalf("oracle: %v", err)
	}
	return rep
}

func repeatsValue(t *testing.T, dec *json.Decoder) bool {
	t.Helper()
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	switch tok {
	case json.Delim('{'):
		return repeatsIn(t, dec)
	case json.Delim('['):
		rep := false
		for dec.More() {
			rep = repeatsValue(t, dec) || rep
		}
		if _, err := dec.Token(); err != nil { // ]
			t.Fatalf("oracle: %v", err)
		}
		return rep
	}
	return false
}

type rawToken struct {
	gap   string // bytes between the previous token and this one
	raw   string
	isKey bool
}

// rawTokens returns every token of valid JSON with the bytes before it and
// whether it is an object key, plus the bytes after the last token. It uses
// the decoder's offsets, so it is independent of the scanner.
func rawTokens(t *testing.T, data []byte) (tokens []rawToken, tail string) {
	t.Helper()
	dec := newDecoder(data)
	type frame struct{ obj, key bool }
	var stack []frame
	prev := int64(0)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return tokens, string(data[prev:])
		}
		if err != nil {
			t.Fatalf("oracle: %v", err)
		}
		chunk := data[prev:dec.InputOffset()]
		raw := bytes.TrimLeft(chunk, " \t\r\n,:")
		gap := string(chunk[:len(chunk)-len(raw)])
		prev = dec.InputOffset()

		d, isDelim := tok.(json.Delim)
		switch {
		case isDelim && (d == '{' || d == '['):
			tokens = append(tokens, rawToken{gap, string(raw), false})
			stack = append(stack, frame{obj: d == '{', key: d == '{'})
		case isDelim:
			tokens = append(tokens, rawToken{gap, string(raw), false})
			stack = stack[:len(stack)-1]
			if n := len(stack); n > 0 && stack[n-1].obj {
				stack[n-1].key = true
			}
		default:
			top := &stack[len(stack)-1]
			isKey := top.obj && top.key
			tokens = append(tokens, rawToken{gap, string(raw), isKey})
			if top.obj {
				top.key = !isKey
			}
		}
	}
}

// isRenamedKey reports whether got is in with attr_ or attr<n>_ (n >= 2)
// inserted right after the opening quote.
func isRenamedKey(in, got string) bool {
	if !strings.HasSuffix(got, in[1:]) || len(got) <= len(in) {
		return false
	}
	prefix := got[1 : len(got)-len(in)+1]
	if prefix == "attr_" {
		return true
	}
	digits, ok := strings.CutPrefix(prefix, "attr")
	digits, ok2 := strings.CutSuffix(digits, "_")
	n, err := strconv.Atoi(digits)
	return ok && ok2 && err == nil && n >= 2 && strconv.Itoa(n) == digits
}

func FuzzDedupeWriter(f *testing.F) {
	for _, s := range []string{
		`{}`,
		`{"a":1}` + "\n",
		`{"a":1,"a":2}` + "\n",
		`{"a":1,"\u0061":2}`,
		`{"o":{"k":1,"k":2},"k":1,"l":[{"x":1,"x":2}]}`,
		`{"o":{},"k":1,"k":2}`,
		`{"a":1e999,"k":1,"k":2}`,
		`{"a":1,"attr_a":2,"a":3}`,
		`{"a":1,"a":2,"attr_a":3,"attr2_a":4,"a":5}`,
		"{\"\xff\":1,\"\xfe\":2}",
		`{"\ufffd":1,"` + "\xff" + `":2}`,
		`{"k":1,"k":2,"s":"\ud800"}`,
		"{\"k\":1,\"k\":2,\"s\":\"\xff\"}",
		`{"\ud83d\ude00":1,"` + "\U0001F600" + `":2}`,
		`{"\ud800":1,"\ud801":2}`,
		`{"a":"{\"a\":1,\"a\":2}","a":1.50e3}`,
		`{"":1,"":2}`,
		` {"a":1,"a":2} `,
		"\t{ \"a\" : 1 ,\n\"a\" : 2 }\r\n\n",
		`[1,2]`,
		`{"a":1}{"a":2}`,
		`{"k":1,"k":2,"d":` + strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001) + "}\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var out bytes.Buffer
		n, err := newDedupeWriter(&out).Write(data)
		if err != nil || n != len(data) {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if nestingDepth(data) > maxJSONDepth {
			return // encoding/json cannot decode it, so the oracle cannot judge it
		}
		if !isObject(data) {
			if !bytes.Equal(out.Bytes(), data) {
				t.Fatalf("changed input that is not one JSON object:\n in: %q\nout: %q", data, out.Bytes())
			}
			return
		}

		if !isObject(out.Bytes()) || repeatsKey(t, out.Bytes()) {
			t.Fatalf("output is not a repeat-free object:\n in: %q\nout: %q", data, out.Bytes())
		}
		if !repeatsKey(t, data) {
			if !bytes.Equal(out.Bytes(), data) {
				t.Fatalf("changed a line without a repeat:\n in: %q\nout: %q", data, out.Bytes())
			}
			return
		}

		in, inTail := rawTokens(t, data)
		got, gotTail := rawTokens(t, out.Bytes())
		if len(in) != len(got) || inTail != gotTail {
			t.Fatalf("tokens %d -> %d, tail %q -> %q:\n in: %q\nout: %q", len(in), len(got), inTail, gotTail, data, out.Bytes())
		}
		for i := range in {
			same := in[i] == got[i]
			renamed := in[i].isKey && got[i].isKey && in[i].gap == got[i].gap && isRenamedKey(in[i].raw, got[i].raw)
			if !same && !renamed {
				t.Fatalf("token %d changed %+v -> %+v:\n in: %q\nout: %q", i, in[i], got[i], data, out.Bytes())
			}
		}
	})
}

func TestPutScanner(t *testing.T) {
	s := &scanner{}
	for i := 0; i < 5; i++ {
		var f frame
		f.names.add("a")
		s.stack = append(s.stack, f)
	}
	s.stack = s.stack[:2] // frames beyond len are still reachable
	if !putScanner(s) {
		t.Fatal("small scanner was not pooled")
	}
	for i, f := range s.stack[:cap(s.stack)] {
		if f.names.seen != nil {
			t.Fatalf("frame %d keeps its key map", i)
		}
	}

	for name, big := range map[string]*scanner{
		"frames":  {stack: make([]frame, 0, maxPooledFrames+1)},
		"keys":    {keys: make([][2]int, 0, maxPooledSpans+1)},
		"renames": {renames: make([]rename, 0, maxPooledSpans+1)},
	} {
		if putScanner(big) {
			t.Errorf("scanner with oversized %s was pooled", name)
		}
	}
}

func TestScannerPool_BoundedAfterHugeLines(t *testing.T) {
	var many strings.Builder
	many.WriteString("{")
	for i := 0; i < 1_000_000; i++ {
		many.WriteString(`"k":1,`)
	}
	many.WriteString(`"x":1}` + "\n")

	for name, line := range map[string]string{
		"1M repeats":          many.String(),
		"100000 open objects": strings.Repeat(`{"k":`, 100000),
		"small malformed":     `{"a":{"b":{"c":`,
	} {
		if _, err := newDedupeWriter(io.Discard).Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		s := scanners.Get().(*scanner)
		if cap(s.stack) > maxPooledFrames || cap(s.keys) > maxPooledSpans || cap(s.renames) > maxPooledSpans {
			t.Errorf("%s: pooled scanner has caps stack=%d keys=%d renames=%d", name, cap(s.stack), cap(s.keys), cap(s.renames))
		}
		for i, f := range s.stack[:cap(s.stack)] {
			if f.names.seen != nil {
				t.Errorf("%s: frame %d keeps its key map", name, i)
				break
			}
		}
		scanners.Put(s)
	}
}

func TestNestingDepth(t *testing.T) {
	for in, want := range map[string]int{
		`{}`:                       1,
		`{"a":[[1],{"b":2}]}`:      3,
		`{"a":"[[[{{{\"[["}`:       1,
		`{"a":"\\","b":[1]}`:       2,
		strings.Repeat("[", 10001): 10001,
	} {
		if got := nestingDepth([]byte(in)); got != want {
			t.Errorf("%.30q: got %d want %d", in, got, want)
		}
	}
}
