// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package config reads and writes the kernel .config file. The kernel .config
// is a flat file of lines like:
//
//	# CONFIG_FOO is not set
//	CONFIG_BAR=y
//	CONFIG_BAZ=m
//	CONFIG_QUX="some string"
//
// kbake manipulates it in place so that comments and ordering are preserved
// where possible, which keeps the result diff-friendly and reproducible.
package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Value kinds.
const (
	Yes    = "y"
	Module = "m"
	No     = "n"
)

// Config is an in-memory view of a kernel .config file.
type Config struct {
	// lines preserves the original text for non-symbol lines (comments,
	// blanks) and the symbol lines in order.
	lines []line
	// vals maps CONFIG_NAME -> parsed value (y/m/n, or the quoted string
	// contents, or "" for unset).
	vals map[string]string
	// idx maps CONFIG_NAME -> index into lines.
	idx map[string]int
}

type line struct {
	raw    string
	symbol string // CONFIG_ name without leading "# ", or "" if not a symbol line
}

// New returns an empty Config (equivalent to a tree with no .config yet).
func New() *Config {
	return &Config{
		vals: map[string]string{},
		idx:  map[string]int{},
	}
}

// Read parses an existing .config file from path. If the file does not exist
// an empty Config is returned.
func Read(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(), nil
		}
		return nil, err
	}
	return Parse(data)
}

// Parse decodes a .config from data.
func Parse(data []byte) (*Config, error) {
	c := New()
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		c.appendRaw(sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) appendRaw(raw string) {
	idx := len(c.lines)
	c.lines = append(c.lines, line{raw: raw})

	sym, val, isSet := parseLine(raw)
	if sym == "" {
		return
	}
	// If a symbol appears twice, prefer the last definition (matches kconfig
	// semantics) and drop the prior index mapping.
	if prev, ok := c.idx[sym]; ok && prev != idx {
		c.lines[prev].symbol = ""
	}
	c.idx[sym] = idx
	c.vals[sym] = val
	_ = isSet
}

func parseLine(raw string) (sym, val string, isSet bool) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.HasPrefix(s, "#") && !strings.HasPrefix(s, "# CONFIG_") {
		return "", "", false
	}
	if strings.HasPrefix(s, "# CONFIG_") && strings.HasSuffix(s, " is not set") {
		// "# CONFIG_FOO is not set"
		inner := strings.TrimPrefix(s, "# ")
		name := strings.TrimSuffix(inner, " is not set")
		return name, No, false
	}
	if strings.HasPrefix(s, "CONFIG_") {
		before, after, ok := strings.Cut(s, "=")
		if !ok {
			return "", "", false
		}
		name := before
		v := after
		if strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"") && len(v) >= 2 {
			v = strings.Trim(v, "\"")
		}
		return name, v, true
	}
	return "", "", false
}

// Get returns the value of a symbol. The name may be given with or without
// the CONFIG_ prefix. The boolean reports whether the symbol is present.
func (c *Config) Get(name string) (string, bool) {
	name = canon(name)
	v, ok := c.vals[name]
	return v, ok
}

// IsEnabled reports whether the symbol is set to y.
func (c *Config) IsEnabled(name string) bool {
	v, ok := c.Get(name)
	return ok && v == Yes
}

// Set sets a symbol to a tristate or string value. value must be "y", "m", "n"
// or a string value. A value of "n" disables the symbol (emitted as
// "# CONFIG_X is not set").
func (c *Config) Set(name, value string) {
	name = canon(name)
	if idx, ok := c.idx[name]; ok {
		c.lines[idx] = line{raw: format(name, value), symbol: name}
		c.vals[name] = value
		return
	}
	c.lines = append(c.lines, line{raw: format(name, value), symbol: name})
	c.idx[name] = len(c.lines) - 1
	c.vals[name] = value
}

// Enable sets a symbol to y.
func (c *Config) Enable(name string) { c.Set(name, Yes) }

// Disable sets a symbol to n.
func (c *Config) Disable(name string) { c.Set(name, No) }

// Module sets a symbol to m.
func (c *Config) Module(name string) { c.Set(name, Module) }

// Symbols returns all known symbol names in sorted order.
func (c *Config) Symbols() []string {
	out := make([]string, 0, len(c.vals))
	for k := range c.vals {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Bytes renders the .config back to bytes.
func (c *Config) Bytes() []byte {
	var b bytes.Buffer
	for _, l := range c.lines {
		b.WriteString(l.raw)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// Write writes the .config to path atomically.
func (c *Config) Write(path string) error {
	data := c.Bytes()
	return os.WriteFile(path, data, 0o644)
}

func canon(name string) string {
	if !strings.HasPrefix(name, "CONFIG_") {
		return "CONFIG_" + name
	}
	return name
}

func format(name, value string) string {
	switch value {
	case Yes, Module:
		return name + "=" + value
	case No, "":
		return "# " + name + " is not set"
	default:
		// String value: quote it.
		return fmt.Sprintf("%s=%q", name, value)
	}
}
