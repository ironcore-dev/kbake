// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package meta holds the fixed build environment kbake applies to the kernel
// compile phase to get reproducible output.
//
// Reproducibility needs every input kbuild consults to be constant. The
// toolchain defaults vary per build (KBUILD_BUILD_TIMESTAMP defaults to the
// wall clock and lands in the `uname -v` banner), so a constant env is the
// whole guarantee. The timestamp is pinned to the unix epoch (@0, the
// conventional reproducible-build sentinel); because it never changes, it is
// a fixed constant here rather than derived from the source commit — kbake
// trades provenance in the version banner for having no plumbing that a
// caller could forget to wire up.
package meta

// EnvMap returns the fixed kbuild environment as "KEY=value" strings,
// suitable for exec.Cmd.Env.
func EnvMap() []string {
	return []string{
		"KBUILD_BUILD_USER=kbake",
		"KBUILD_BUILD_HOST=kbake",
		"KBUILD_BUILD_VERSION=1",
		"SOURCE_DATE_EPOCH=0",
		"KBUILD_BUILD_TIMESTAMP=@0",
		"TZ=UTC",
		"LC_ALL=C",
	}
}
