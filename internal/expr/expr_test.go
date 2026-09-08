// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package expr

import "testing"

func TestSubst(t *testing.T) {
	sc := Scope{"arch": "arm64", "ver": "6.10"}
	cases := map[string]string{
		"arch is $arch":      "arch is arm64",
		"$arch/$ver":         "arm64/6.10",
		"no vars here":       "no vars here",
		"price $5":           "price $5",
		"literal $$arch":     "literal $arch",
		"unknown $foo":       "unknown $foo",
		"embedded ${{arch}}": "embedded arm64",
	}
	for in, want := range cases {
		got := sc.Subst(in)
		if got != want {
			t.Errorf("Subst(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEvalCondition(t *testing.T) {
	sc := Scope{"arch": "arm"}
	cases := []struct {
		expr string
		want bool
	}{
		{"", true},
		{"${{ arch == 'arm' }}", true},
		{"arch == 'arm'", true},
		{"arch == 'arm64'", false},
		{"arch != 'x86'", true},
		{"${{ arch == 'arm' || arch == 'arm64' }}", true},
		{"${{ arch == 'arm' && arch != 'x86' }}", true},
		{"${{ !(arch == 'amd64') }}", true},
		{"${{ arch == 'amd64' || arch == 'arm64' }}", false},
	}
	for _, c := range cases {
		got, err := EvalBool(c.expr, sc)
		if err != nil {
			t.Errorf("EvalCondition(%q) error: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("EvalCondition(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestEvalConditionErrors(t *testing.T) {
	sc := Scope{"arch": "arm"}
	bad := []string{
		"${{ arch == 'arm",    // unterminated
		"${{ foo == 'arm' }}", // unknown identifier
		"${{ arch = 'arm' }}", // bad operator
	}
	for _, b := range bad {
		if _, err := EvalBool(b, sc); err == nil {
			t.Errorf("EvalCondition(%q) expected error", b)
		}
	}
}

func TestEvalString(t *testing.T) {
	sc := Scope{"arch": "amd64"}
	cases := map[string]string{
		"arch":            "amd64",
		"arch == 'amd64'": "true",
		"arch == 'arm'":   "false",
	}
	for in, want := range cases {
		got, err := EvalString(in, sc)
		if err != nil {
			t.Errorf("EvalString(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("EvalString(%q) = %q, want %q", in, got, want)
		}
	}
}
