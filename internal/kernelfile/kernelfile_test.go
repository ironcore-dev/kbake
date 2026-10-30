// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package kernelfile

import (
	"slices"
	"testing"
)

func TestParseAddCompilerFlags(t *testing.T) {
	data := []byte(`
from: git://git.kernel.org/pub/scm/linux/kernel/git/stable/linux@v7.1
actions:
- if: ${{ arch == 'amd64' }}
  actions:
  - addCompilerFlags: { flags: [-march=x86-64] }
`)
	k, err := Parse(data, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(k.Actions) != 1 {
		t.Fatalf("actions = %d groups, want 1", len(k.Actions))
	}
	group := k.Actions[0]
	if group.If != "${{ arch == 'amd64' }}" {
		t.Errorf("if = %q", group.If)
	}
	if len(group.Actions) != 1 || group.Actions[0].AddCompilerFlags == nil {
		t.Fatalf("actions = %#v, want one addCompilerFlags action", group.Actions)
	}
	if got, want := group.Actions[0].AddCompilerFlags.Flags, []string{"-march=x86-64"}; !slices.Equal(got, want) {
		t.Errorf("flags = %q, want %q", got, want)
	}
}
