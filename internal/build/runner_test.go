// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/ironcore-dev/kbake/internal/kernelmake"
)

// fakeLooker returns a LookPath-style function whose "PATH" contains exactly
// the binaries named in present.
func fakeLooker(present map[string]bool) func(string) (string, error) {
	return func(name string) (string, error) {
		if present[name] {
			return "/bin/" + name, nil
		}
		return "", fmt.Errorf("not found: %s", name)
	}
}

// allToolchainBins returns the binaries newDefaultRunnerWith probes for the
// given arch (native -> bare names, cross -> prefixed), mirroring
// crossCompilePrefix. It lets tests build a consistent "present" set without
// hard-coding which arch is native on the host running the test.
func allToolchainBins(t *testing.T, arch string) []string {
	t.Helper()
	info, err := kernelmake.InfoFor(arch)
	if err != nil {
		t.Fatalf("InfoFor(%q): %v", arch, err)
	}
	prefix := crossCompilePrefix(info, arch)
	bins := make([]string, 0, len(toolchainBins))
	for _, name := range toolchainBins {
		bins = append(bins, prefix+name)
	}
	return bins
}

func TestNewDefaultRunnerAllArchsSupported(t *testing.T) {
	present := map[string]bool{"make": true}
	for _, arch := range kernelmake.AllArchs() {
		for _, b := range allToolchainBins(t, arch) {
			present[b] = true
		}
	}

	r, err := newDefaultRunnerWith(defaultRunnerOptions{}, fakeLooker(present))
	if err != nil {
		t.Fatalf("newDefaultRunnerWith: %v", err)
	}
	if len(r.supported) != len(kernelmake.AllArchs()) {
		t.Errorf("supported = %d archs, want %d", len(r.supported), len(kernelmake.AllArchs()))
	}
	if len(r.missing) != 0 {
		t.Errorf("missing = %v, want empty", r.missing)
	}
}

func TestNewDefaultRunnerMissingLdExcludesArch(t *testing.T) {
	// Pick a cross arch (NOT the host's native arch) so the probe uses
	// prefixed names and the missing bin is unambiguously named.
	target := crossArch(t)
	info, _ := kernelmake.InfoFor(target)
	prefix := crossCompilePrefix(info, target)
	missingBin := prefix + "ld"

	present := map[string]bool{"make": true}
	for _, arch := range kernelmake.AllArchs() {
		for _, b := range allToolchainBins(t, arch) {
			if arch == target && b == missingBin {
				continue // leave the target arch's ld missing
			}
			present[b] = true
		}
	}

	r, err := newDefaultRunnerWith(defaultRunnerOptions{}, fakeLooker(present))
	if err != nil {
		t.Fatalf("newDefaultRunnerWith: %v", err)
	}

	if _, ok := r.supported[target]; ok {
		t.Errorf("arch %q should be unsupported (missing %s)", target, missingBin)
	}
	miss, ok := r.missing[target]
	if !ok {
		t.Fatalf("missing[%q] not recorded", target)
	}
	if len(miss) != 1 || miss[0] != missingBin {
		t.Errorf("missing[%q] = %v, want [%q]", target, miss, missingBin)
	}

	// Every other arch should be supported.
	for _, arch := range kernelmake.AllArchs() {
		if arch == target {
			continue
		}
		if _, ok := r.supported[arch]; !ok {
			t.Errorf("arch %q should be supported", arch)
		}
	}
}

func TestNewDefaultRunnerNativeUsesBareNames(t *testing.T) {
	// The host's native arch probes BARE names (gcc/as/…, not the table's
	// prefixed names), so providing only bare names + make must mark the
	// native arch supported.
	native := runtime.GOARCH
	present := map[string]bool{"make": true}
	for _, b := range allToolchainBins(t, native) {
		present[b] = true
	}

	r, err := newDefaultRunnerWith(defaultRunnerOptions{}, fakeLooker(present))
	if err != nil {
		t.Fatalf("newDefaultRunnerWith: %v", err)
	}
	if _, ok := r.supported[native]; !ok {
		t.Errorf("native arch %q should be supported via bare names", native)
	}
	// Every cross arch (which probes prefixed names we did NOT provide) must be
	// unsupported.
	for _, arch := range kernelmake.AllArchs() {
		if arch == native {
			continue
		}
		if _, ok := r.supported[arch]; ok {
			t.Errorf("cross arch %q should be unsupported (no prefixed bins provided)", arch)
		}
	}
}

func TestCrossCompilePrefix(t *testing.T) {
	native := runtime.GOARCH
	info, _ := kernelmake.InfoFor(native)
	if got := crossCompilePrefix(info, native); got != "" {
		t.Errorf("native %q: prefix = %q, want empty", native, got)
	}

	target := crossArch(t)
	ci, _ := kernelmake.InfoFor(target)
	if got := crossCompilePrefix(ci, target); got != ci.CrossCompile {
		t.Errorf("cross %q: prefix = %q, want %q", target, got, ci.CrossCompile)
	}
}

func TestNewDefaultRunnerMakeMissing(t *testing.T) {
	// make itself absent -> construction fails (the hard error that predates
	// the toolchain probe).
	_, err := newDefaultRunnerWith(defaultRunnerOptions{}, fakeLooker(map[string]bool{}))
	if err == nil {
		t.Fatal("expected error when make is missing")
	}
}

// crossArch returns an arch from the table that is not the host's native arch,
// or skips the test if none exists.
func crossArch(t *testing.T) string {
	t.Helper()
	native := runtime.GOARCH
	for _, a := range kernelmake.AllArchs() {
		if a != native {
			return a
		}
	}
	t.Skip("no non-native arch available")
	return ""
}
