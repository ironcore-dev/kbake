// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package xio

import (
	"errors"
	"io"
)

type CloserFunc func() error

func (f CloserFunc) Close() error {
	return f()
}

type onEOFReader struct {
	r   io.Reader
	eof bool
	f   func()
}

func OnReadEOF(r io.Reader, f func()) io.Reader {
	return &onEOFReader{
		r: r,
		f: f,
	}
}

func (r *onEOFReader) Read(p []byte) (n int, err error) {
	n, err = r.r.Read(p)
	if errors.Is(err, io.EOF) && !r.eof {
		r.eof = true
		r.f()
	}
	return n, err
}
