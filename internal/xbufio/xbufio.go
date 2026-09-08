// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package xbufio

import (
	"bufio"
	"fmt"
	"io"

	"github.com/ironcore-dev/kbake/internal/xio"
)

type ScanWriter struct {
	closed bool
	buf    []byte
	split  bufio.SplitFunc

	dst io.Writer
}

// NewScanWriter returns an io.Writer that tokenizes its input with
// bufio.ScanLines and forwards each *token* to dst — verbatim, without any
// delimiter the split func consumed. A Split func (see Split) that wants
// downstream consumers to see line terminators must include them in the
// tokens it returns (the git-noise filter in kbake/internal/source does).
// Close flushes a trailing partial token (with atEOF=true).
func NewScanWriter(dst io.Writer) *ScanWriter {
	return &ScanWriter{
		dst:   dst,
		split: bufio.ScanLines,
	}
}

func (w *ScanWriter) Split(split bufio.SplitFunc) {
	w.split = split
}

func (w *ScanWriter) Write(p []byte) (n int, err error) {
	if w.closed {
		return 0, fmt.Errorf("ScanWriter already closed")
	}

	for {
		advance, token, _ := w.split(p, false)
		if advance == 0 {
			w.buf = append(w.buf, p...)
			n += len(p)
			return n, nil
		}

		p = p[advance:]
		if len(w.buf) > 0 {
			w.buf = append(w.buf, token...)
			if _, err := w.dst.Write(w.buf); err != nil {
				return n, err
			}

			w.buf = w.buf[:0]
			n += advance
			continue
		}

		if _, err := w.dst.Write(token); err != nil {
			return n, err
		}
		n += advance
	}
}

// Flush emits any pending partial token (the split func is called with
// atEOF=true) without closing the writer — later Writes keep working.
func (w *ScanWriter) Flush() error {
	_, token, err := w.split(w.buf, true)
	if err != nil {
		return err
	}
	w.buf = w.buf[:0]
	// A SplitFunc (e.g. bufio.ScanLines on an empty buffer, or one that filtered
	// the final token to nil) returns a nil token to mean "nothing to emit";
	// writing nil would surface as a spurious empty line downstream.
	if len(token) > 0 {
		_, err = w.dst.Write(token)
	}
	return err
}

func (w *ScanWriter) Close() error {
	if w.closed {
		return fmt.Errorf("ScanWriter already closed")
	}
	w.closed = true
	return w.Flush()
}

type ScanBuffer struct {
	sw    *ScanWriter
	texts []string
}

func NewScanBuffer() *ScanBuffer {
	b := &ScanBuffer{}

	b.sw = NewScanWriter(xio.WriterFunc(func(p []byte) (n int, err error) {
		b.texts = append(b.texts, string(p))
		return len(p), nil
	}))

	return b
}

func (b *ScanBuffer) Write(p []byte) (n int, err error) {
	return b.sw.Write(p)
}

// Flush emits a pending partial line as a complete text without closing the
// buffer (see ScanWriter.Flush).
func (b *ScanBuffer) Flush() error {
	return b.sw.Flush()
}

func (b *ScanBuffer) Close() error {
	return b.sw.Close()
}

func (b *ScanBuffer) Texts() []string {
	return b.texts
}

func (b *ScanBuffer) Next(n int) []string {
	n = min(len(b.texts), n)
	texts := b.texts[:n]
	b.texts = b.texts[n:]
	return texts
}

func (b *ScanBuffer) AllNext() []string {
	return b.Next(len(b.texts))
}
