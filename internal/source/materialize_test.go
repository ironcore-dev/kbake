// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"os"
	"path/filepath"
	"testing"
)

func assertNoCheckoutDebris(t *testing.T, parent string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(parent, ".kbake-checkout-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > 0 {
		t.Errorf("checkout debris left behind: %v", matches)
	}
}

func TestMaterializeAtomicHappyPath(t *testing.T) {
	hasGit(t)
	t.Parallel()
	src, _, tip := makeRepo(t, "v1.2.3")
	bare := makeBare(t, src)
	parent := t.TempDir()
	treeDir := filepath.Join(parent, "linux@v1.2.3")

	if err := materializeAtomic(bare, tip, treeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(treeDir, "a.txt")); err != nil {
		t.Errorf("materialized tree missing file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(treeDir, ".git")); err == nil {
		t.Error("materialized tree should have no .git")
	}
	assertNoCheckoutDebris(t, parent)
}

// The cache invariant: treeDir must never exist in a partially materialized
// state. A failed checkout (here: a commit the bare repo doesn't have) must
// leave neither treeDir nor temp debris behind.
func TestMaterializeAtomicFailureLeavesNoTrace(t *testing.T) {
	hasGit(t)
	t.Parallel()
	src, _, _ := makeRepo(t, "v1.2.3")
	bare := makeBare(t, src)
	parent := t.TempDir()
	treeDir := filepath.Join(parent, "linux@v1.2.3")

	err := materializeAtomic(bare, "0123456789012345678901234567890123456789", treeDir)
	if err == nil {
		t.Fatal("expected checkout failure")
	}
	if _, serr := os.Stat(treeDir); !os.IsNotExist(serr) {
		t.Errorf("treeDir exists after failed materialization (existence==completeness invariant broken)")
	}
	assertNoCheckoutDebris(t, parent)
}
