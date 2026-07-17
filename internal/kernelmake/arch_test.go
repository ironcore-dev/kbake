// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package kernelmake

import (
	"testing"
)

func TestInfoFor(t *testing.T) {
	a, err := InfoFor("amd64")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kernel != "x86_64" {
		t.Errorf("amd64 kernel = %q, want x86_64", a.Kernel)
	}
	if a.CrossCompile == "" {
		t.Error("expected cross-compile prefix")
	}
	if a.Image != "arch/x86/boot/bzImage" {
		t.Errorf("amd64 image = %q", a.Image)
	}

	b, err := InfoFor("arm64")
	if err != nil {
		t.Fatal(err)
	}
	if b.Kernel != "arm64" {
		t.Errorf("arm64 kernel = %q, want arm64", b.Kernel)
	}
}

func TestInfoForInvalid(t *testing.T) {
	if _, err := InfoFor("nope"); err == nil {
		t.Fatal("expected error for unknown arch")
	}
}

func TestArmDefaultDefconfig(t *testing.T) {
	a, _ := InfoFor("arm")
	if a.Defconfig != "multi_v7_defconfig" {
		t.Errorf("arm defconfig = %q", a.Defconfig)
	}
}

func TestSrcArch(t *testing.T) {
	// The x86 quirk is the only non-1:1 ARCH->SRCARCH mapping: both x86_64
	// and i386 map to "x86" (the kernel's Makefile computes the same). Every
	// other arch maps 1:1.
	cases := map[string]string{
		"amd64":   "x86",
		"386":     "x86",
		"arm64":   "arm64",
		"arm":     "arm",
		"mips":    "mips",
		"mips64":  "mips",
		"ppc64le": "powerpc",
		"riscv64": "riscv",
		"s390x":   "s390",
	}
	for name, want := range cases {
		info, err := InfoFor(name)
		if err != nil {
			t.Fatalf("InfoFor(%q): %v", name, err)
		}
		if info.SRCArch != want {
			t.Errorf("InfoFor(%q).SRCArch = %q, want %q", name, info.SRCArch, want)
		}
	}
}
