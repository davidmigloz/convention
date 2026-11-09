package ctx

import (
	"bytes"
	"io"
	"sync"
)

// dedupeWriter guarantees that no JSON object in a log line repeats a key.
// slog does not deduplicate, and log agents such as fluent-bit drop the line
// or corrupt the value when a key repeats. Working on the encoded line, not on
// slog attributes, sees exactly what the handler wrote, whatever LogValuers,
// ReplaceAttr, raw JSON values or invalid UTF-8 keys turned into.
//
// Every line is first checked by an allocation-free scanner (log_scan.go). Only
// a line with a repeat, or one the scanner cannot vouch for, is scanned again
// in slow mode, which compares keys by their decoded value. The line is then
// written with "attr_" inserted into each repeated key and every other byte
// untouched, so values are never re-encoded.
//
// slog.JSONHandler writes each record with one Write call holding one JSON
// object and a trailing newline, under a mutex shared by all derived handlers.
// The writer is stateless, so it is safe for concurrent use, and it does not
// retain p (the handler reuses its buffer after Write returns).
type dedupeWriter struct {
	w io.Writer
}

func newDedupeWriter(w io.Writer) io.Writer { return dedupeWriter{w} }

// A line buffer that grew beyond this is not pooled.
const maxPooledBuffer = 64 << 10

var lineBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func (d dedupeWriter) Write(p []byte) (int, error) {
	s := scanners.Get().(*scanner)
	defer putScanner(s)

	if s.scan(p, false) || !s.scan(p, true) || len(s.renames) == 0 {
		return d.w.Write(p)
	}

	buf := lineBuffers.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledBuffer {
			lineBuffers.Put(buf)
		}
	}()
	prev := 0
	for _, r := range s.renames {
		buf.Write(p[prev:r.at])
		buf.WriteString(prefix(r.n))
		prev = r.at
	}
	buf.Write(p[prev:])

	m, err := d.w.Write(buf.Bytes())
	if err == nil && m < buf.Len() {
		err = io.ErrShortWrite
	}
	if err != nil {
		return inputBytesWritten(s.renames, m), err
	}
	return len(p), nil
}

// inputBytesWritten maps m bytes of spliced output back to the number of
// original input bytes they hold. An inserted prefix that was only partly
// written counts for nothing.
func inputBytesWritten(renames []rename, m int) int {
	inserted := 0
	for _, r := range renames {
		at := r.at + inserted // where this prefix starts in the output
		if m <= at {
			break
		}
		n := len(prefix(r.n))
		if m < at+n {
			return r.at
		}
		inserted += n
	}
	return m - inserted
}
