// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package kernelmake maps go architecture names
// to the kernel's ARCH and CROSS_COMPILE values, and to the location of the
// built kernel image.
package kernelmake

import (
	"fmt"
	"sort"
	"strings"
)

// Info describes the kernel-side view of an architecture.
type Info struct {
	// Kernel is the value of ARCH= passed to make, e.g. "x86_64".
	Kernel string
	// SRCArch is the kernel's SRCARCH directory: the arch/<dir> subtree sourced
	// via `source "arch/$(SRCARCH)/Kconfig"` for this architecture. Both x86_64
	// and i386 map to "x86"; everything else maps 1:1 (the kernel's own Makefile
	// computes it as $(subst i386,x86,$(subst x86_64,x86,$(ARCH)))). Consumers
	// that walk the tree's Kconfig (e.g. internal/kconfig) need this to resolve
	// arch-only symbols (e.g. EFI, EFI_STUB), which are defined nowhere else.
	SRCArch string
	// CrossCompile is the value of CROSS_COMPILE= passed to make (may be empty
	// for the host architecture).
	CrossCompile string
	// Image is the path (relative to the kernel tree root) of the built
	// kernel image.
	Image string
	// Defconfig is the default defconfig target for this architecture.
	Defconfig string
}

var table = map[string]Info{
	"amd64":   {Kernel: "x86_64", SRCArch: "x86", CrossCompile: "x86_64-linux-gnu-", Image: "arch/x86/boot/bzImage", Defconfig: "defconfig"},
	"386":     {Kernel: "i386", SRCArch: "x86", CrossCompile: "i686-linux-gnu-", Image: "arch/x86/boot/bzImage", Defconfig: "defconfig"},
	"arm64":   {Kernel: "arm64", SRCArch: "arm64", CrossCompile: "aarch64-linux-gnu-", Image: "arch/arm64/boot/Image", Defconfig: "defconfig"},
	"arm":     {Kernel: "arm", SRCArch: "arm", CrossCompile: "arm-linux-gnueabihf-", Image: "arch/arm/boot/zImage", Defconfig: "multi_v7_defconfig"},
	"mips":    {Kernel: "mips", SRCArch: "mips", CrossCompile: "mips-linux-gnu-", Image: "vmlinux", Defconfig: "defconfig"},
	"mips64":  {Kernel: "mips", SRCArch: "mips", CrossCompile: "mips64-linux-gnu-", Image: "vmlinux", Defconfig: "64bit_defconfig"},
	"ppc64le": {Kernel: "powerpc", SRCArch: "powerpc", CrossCompile: "powerpc64le-linux-gnu-", Image: "arch/powerpc/boot/zImage", Defconfig: "powernv_defconfig"},
	"riscv64": {Kernel: "riscv", SRCArch: "riscv", CrossCompile: "riscv64-linux-gnu-", Image: "arch/riscv/boot/Image", Defconfig: "defconfig"},
	"s390x":   {Kernel: "s390", SRCArch: "s390", CrossCompile: "s390x-linux-gnu-", Image: "arch/s390/boot/bzImage", Defconfig: "defconfig"},
}

// InfoFor returns the info for the given arch descriptor.
func InfoFor(name string) (Info, error) {
	a, ok := table[name]
	if !ok {
		return Info{}, fmt.Errorf("unknown architecture %q (known: %s)", name, known())
	}
	return a, nil
}

// AllArchs returns every architecture kbake knows about, sorted for
// deterministic iteration. Callers that need to walk the full table (e.g. a
// toolchain preflight probing every arch's compiler) use this instead of
// reaching for the unexported map.
func AllArchs() []string {
	archs := make([]string, 0, len(table))
	for k := range table {
		archs = append(archs, k)
	}
	sort.Strings(archs)
	return archs
}

func known() string {
	return strings.Join(AllArchs(), ", ")
}
