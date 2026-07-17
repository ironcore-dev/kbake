// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package xbufio

import (
	"slices"
	"testing"
)

func TestScanBuffer_Texts(t *testing.T) {
	sb := NewScanBuffer()

	_, err := sb.Write([]byte("hello "))
	if err != nil {
		t.Fatal(err)
	}

	_, err = sb.Write([]byte("world\nhow are you\ndoing?"))
	if err != nil {
		t.Fatal(err)
	}

	actual := sb.Texts()
	expected := []string{"hello world", "how are you"}
	if !slices.Equal(actual, expected) {
		t.Errorf("got %v\nwant %v", actual, expected)
	}

	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}

	actual = sb.Texts()
	expected = []string{"hello world", "how are you", "doing?"}
	if !slices.Equal(actual, expected) {
		t.Errorf("got %v\nwant %v", actual, expected)
	}
}
