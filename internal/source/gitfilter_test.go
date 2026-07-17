// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"bytes"
	"strings"
	"testing"
)

// The git-noise filter must drop server-side noise lines entirely but forward
// kept lines WITH their line terminator — downstream consumers (the build log
// file, the TTY tail's line scanner) are line-oriented.
func TestFilterGitNoiseWriter(t *testing.T) {
	var buf bytes.Buffer
	w := filterGitNoiseWriter(&buf)

	in := "remote: Enumerating objects: 5, done.\n" +
		"Receiving objects:  45% (10/22)\rReceiving objects: 100% (22/22), done.\n" +
		"remote: Counting objects: 100% (5/5), done.\n" +
		"remote: Compressing objects: 100% (4/4), done.\n" +
		"some other kept line\n" +
		"trailing partial" // no terminator; flushed by Close
	if _, err := w.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Exact output: noise lines gone (terminator included), kept lines intact
	// and newline-terminated (incl. the mid-line \r of the progress update and
	// the synthetic terminator on the trailing partial line).
	want := "Receiving objects:  45% (10/22)\rReceiving objects: 100% (22/22), done.\n" +
		"some other kept line\n" +
		"trailing partial\n"
	if got := buf.String(); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// A kept line split across multiple Writes must be reassembled with its
// terminator in the right place (i.e. appended to the line end, not to the
// first Write's fragment).
func TestFilterGitNoiseWriterChunked(t *testing.T) {
	var buf bytes.Buffer
	w := filterGitNoiseWriter(&buf)

	for _, chunk := range []string{"hel", "lo wor", "ld\nne", "xt\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if got, want := buf.String(), "hello world\nnext\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Count(buf.String(), "\n") != 2 {
		t.Errorf("expected exactly 2 terminators, got %q", buf.String())
	}
}
