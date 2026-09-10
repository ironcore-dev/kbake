// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

const sample = `# CONFIG_FOO is not set
CONFIG_BAR=y
CONFIG_BAZ=m
CONFIG_STR="hello world"

# a comment
`

func TestParse(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get("FOO"); v != No {
		t.Errorf("FOO = %q, want %q", v, No)
	}
	if v, _ := c.Get("BAR"); v != Yes {
		t.Errorf("BAR = %q, want y", v)
	}
	if v, _ := c.Get("BAZ"); v != Module {
		t.Errorf("BAZ = %q, want m", v)
	}
	if v, _ := c.Get("STR"); v != "hello world" {
		t.Errorf("STR = %q", v)
	}
}

func TestSetExistingAndNew(t *testing.T) {
	c, _ := Parse([]byte(sample))
	c.Disable("BAR")      // flip existing
	c.Enable("NEW_THING") // add new
	out := string(c.Bytes())
	if strings.Contains(out, "CONFIG_BAR=y") {
		t.Error("BAR still enabled")
	}
	if !strings.Contains(out, "# CONFIG_BAR is not set") {
		t.Error("BAR not disabled in output")
	}
	if !strings.Contains(out, "CONFIG_NEW_THING=y") {
		t.Error("NEW_THING not added")
	}
	// Comment and blank line preserved.
	if !strings.Contains(out, "# a comment") {
		t.Error("comment dropped")
	}
}

func TestSetModule(t *testing.T) {
	c := New()
	c.Module("VXLAN")
	if v, _ := c.Get("VXLAN"); v != Module {
		t.Fatalf("VXLAN = %q, want m", v)
	}
	if !strings.Contains(string(c.Bytes()), "CONFIG_VXLAN=m") {
		t.Error("module not rendered")
	}
}

func TestSetString(t *testing.T) {
	c := New()
	c.Set("LOCALVERSION", "-kbake")
	out := string(c.Bytes())
	if !strings.Contains(out, `CONFIG_LOCALVERSION="-kbake"`) {
		t.Errorf("string not quoted properly: %s", out)
	}
}

func TestDuplicateSymbolLastWins(t *testing.T) {
	c, err := Parse([]byte("CONFIG_A=y\nCONFIG_A=m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get("A"); v != Module {
		t.Errorf("expected last value m, got %q", v)
	}
}

func TestSymbolsSorted(t *testing.T) {
	c := New()
	c.Enable("ZEBRA")
	c.Enable("ALPHA")
	c.Enable("MIKE")
	syms := c.Symbols()
	if syms[0] != "CONFIG_ALPHA" || syms[1] != "CONFIG_MIKE" || syms[2] != "CONFIG_ZEBRA" {
		t.Errorf("symbols not sorted: %v", syms)
	}
}
