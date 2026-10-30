// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/ironcore-dev/kbake/internal/expr"
	"github.com/ironcore-dev/kbake/internal/kernelfile"
)

// evalAddFile runs a single addFile action through evalAction with an empty
// substitution scope.
func evalAddFile(t *testing.T, contextDir, treeDir, src, dst string) error {
	t.Helper()
	b := &Builder{}
	return b.evalAction(context.Background(), contextDir, expr.Scope{}, &kernelfile.Action{
		AddFile: &kernelfile.AddFile{Src: src, Dst: dst},
	}, treeDir, &buildSpec{config: Config{}})
}

func evalDeleteFile(t *testing.T, treeDir, path string) error {
	t.Helper()
	b := &Builder{}
	return b.evalAction(context.Background(), t.TempDir(), expr.Scope{}, &kernelfile.Action{
		DeleteFile: &kernelfile.DeleteFile{Path: path},
	}, treeDir, &buildSpec{config: Config{}})
}

func writeFile(t *testing.T, path string, mode os.FileMode, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestEvalActionAddFile(t *testing.T) {
	contextDir := t.TempDir()
	treeDir := t.TempDir()
	writeFile(t, filepath.Join(contextDir, "patch.c"), 0o644, "int x;\n")

	if err := evalAddFile(t, contextDir, treeDir, "patch.c", "net/patch.c"); err != nil {
		t.Fatalf("addFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(treeDir, "net", "patch.c"))
	if err != nil {
		t.Fatalf("reading copied file: %v", err)
	}
	if string(got) != "int x;\n" {
		t.Errorf("content mismatch: %q", got)
	}
}

func TestEvalActionAddFilePreservesMode(t *testing.T) {
	contextDir := t.TempDir()
	treeDir := t.TempDir()
	writeFile(t, filepath.Join(contextDir, "gen.sh"), 0o755, "#!/bin/sh\n")

	if err := evalAddFile(t, contextDir, treeDir, "gen.sh", "scripts/gen.sh"); err != nil {
		t.Fatalf("addFile: %v", err)
	}
	info, err := os.Stat(filepath.Join(treeDir, "scripts", "gen.sh"))
	if err != nil {
		t.Fatalf("stat copied file: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755 (executable bit must survive)", info.Mode().Perm())
	}
}

func TestEvalActionAddFileDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("xos.CopyTree shells out to GNU cp (--reflink), linux only")
	}
	contextDir := t.TempDir()
	treeDir := t.TempDir()
	writeFile(t, filepath.Join(contextDir, "subdir", "a.c"), 0o644, "a")

	if err := evalAddFile(t, contextDir, treeDir, "subdir", "drivers/subdir"); err != nil {
		t.Fatalf("addFile dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(treeDir, "drivers", "subdir", "a.c")); err != nil {
		t.Errorf("directory content missing: %v", err)
	}
}

func TestEvalActionAddFileRejections(t *testing.T) {
	outsideDir := t.TempDir()
	writeFile(t, filepath.Join(outsideDir, "secret.c"), 0o644, "secret")

	tests := []struct {
		name  string
		setup func(contextDir, treeDir string)
		src   string
		dst   string
	}{
		{name: "src escape", src: "../secret.c", dst: "net/secret.c"},
		{name: "src absolute", src: "/etc/hostname", dst: "net/hostname"},
		{name: "src empty", src: "", dst: "net/x"},
		{name: "src missing", src: "nope.c", dst: "net/nope.c"},
		{name: "dst empty", src: "ok.c", dst: ""},
		{name: "dst escape", src: "ok.c", dst: "../../../evil.c"},
		{name: "dst absolute", src: "ok.c", dst: "/etc/evil.c"},
		{
			name: "dst through out-pointing symlink",
			setup: func(_, treeDir string) {
				if err := os.Symlink(outsideDir, filepath.Join(treeDir, "link")); err != nil {
					t.Fatal(err)
				}
			},
			src: "ok.c",
			dst: "link/evil.c",
		},
		{
			name: "src symlink pointing outside context",
			setup: func(contextDir, _ string) {
				if err := os.Symlink(filepath.Join(outsideDir, "secret.c"), filepath.Join(contextDir, "linked.c")); err != nil {
					t.Fatal(err)
				}
			},
			src: "linked.c",
			dst: "net/linked.c",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contextDir := t.TempDir()
			treeDir := t.TempDir()
			writeFile(t, filepath.Join(contextDir, "ok.c"), 0o644, "ok")
			if tc.setup != nil {
				tc.setup(contextDir, treeDir)
			}
			if err := evalAddFile(t, contextDir, treeDir, tc.src, tc.dst); err == nil {
				t.Errorf("addFile src=%q dst=%q: expected error, got nil", tc.src, tc.dst)
			}
		})
	}
}

func TestEvalActionDeleteFile(t *testing.T) {
	treeDir := t.TempDir()
	writeFile(t, filepath.Join(treeDir, "drivers", "bad.c"), 0o644, "bad")

	// Existing file: removed.
	if err := evalDeleteFile(t, treeDir, "drivers/bad.c"); err != nil {
		t.Fatalf("deleteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(treeDir, "drivers", "bad.c")); !os.IsNotExist(err) {
		t.Errorf("file still exists (stat err = %v)", err)
	}

	// Absent semantics: deleting a non-existent path is a no-op.
	if err := evalDeleteFile(t, treeDir, "drivers/never-existed.c"); err != nil {
		t.Errorf("deleteFile of missing path should succeed (absent semantics): %v", err)
	}
}

func TestEvalActionDeleteFileRejections(t *testing.T) {
	for _, path := range []string{"", "../../../etc", "/etc/passwd"} {
		t.Run("path="+path, func(t *testing.T) {
			treeDir := t.TempDir()
			writeFile(t, filepath.Join(treeDir, "keep.c"), 0o644, "keep")
			if err := evalDeleteFile(t, treeDir, path); err == nil {
				t.Errorf("deleteFile path=%q: expected error, got nil", path)
			}
			// The tree itself must survive (empty path must not become RemoveAll(treeDir)).
			if _, err := os.Stat(filepath.Join(treeDir, "keep.c")); err != nil {
				t.Errorf("tree damaged by rejected deleteFile: %v", err)
			}
		})
	}
}

func TestEvalActionAddCompilerFlags(t *testing.T) {
	b := &Builder{}
	s := expr.Scope{"arch": "amd64"}
	spec := &buildSpec{config: Config{}}

	actions := []kernelfile.Actions{
		{
			If: "${{ arch == 'amd64' }}",
			Actions: []kernelfile.Action{
				{AddCompilerFlags: &kernelfile.AddCompilerFlags{Flags: []string{"-march=x86-64"}}},
				{AddCompilerFlags: &kernelfile.AddCompilerFlags{Flags: []string{"-DARCH_$arch", "-O2"}}},
			},
		},
		{
			If: "${{ arch == 'arm64' }}",
			Actions: []kernelfile.Action{
				{AddCompilerFlags: &kernelfile.AddCompilerFlags{Flags: []string{"-mbranch-protection=none"}}},
			},
		},
	}
	for i := range actions {
		if err := b.evalActions(context.Background(), t.TempDir(), s, &actions[i], t.TempDir(), spec); err != nil {
			t.Fatalf("evalActions[%d]: %v", i, err)
		}
	}

	want := []string{"-march=x86-64", "-DARCH_amd64", "-O2"}
	if !slices.Equal(spec.compilerFlags, want) {
		t.Errorf("compilerFlags = %q, want %q", spec.compilerFlags, want)
	}
	if len(spec.config) != 0 {
		t.Errorf("config = %v, want empty", spec.config)
	}
}

func TestEvalActionAddCompilerFlagsRejections(t *testing.T) {
	for _, flag := range []string{"", "-march=x86-64 -O2", "-DFOO=\tbar"} {
		t.Run("flag="+flag, func(t *testing.T) {
			b := &Builder{}
			spec := &buildSpec{config: Config{}}
			err := b.evalAction(context.Background(), t.TempDir(), expr.Scope{}, &kernelfile.Action{
				AddCompilerFlags: &kernelfile.AddCompilerFlags{Flags: []string{flag}},
			}, t.TempDir(), spec)
			if err == nil {
				t.Errorf("addCompilerFlags flag=%q: expected error, got nil", flag)
			}
			if len(spec.compilerFlags) != 0 {
				t.Errorf("compilerFlags = %q, want empty after rejection", spec.compilerFlags)
			}
		})
	}
}
