// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"slices"
	"testing"
)

// The env must pin every kbuild input that would otherwise vary per build —
// above all KBUILD_BUILD_TIMESTAMP (wall clock by default) — so that identical
// inputs produce byte-identical artifacts. This test pins the exact set so an
// accidental removal is caught.
func TestEnvMap(t *testing.T) {
	want := []string{
		"KBUILD_BUILD_USER=kbake",
		"KBUILD_BUILD_HOST=kbake",
		"KBUILD_BUILD_VERSION=1",
		"SOURCE_DATE_EPOCH=0",
		"KBUILD_BUILD_TIMESTAMP=@0",
		"TZ=UTC",
		"LC_ALL=C",
	}
	if got := EnvMap(); !slices.Equal(got, want) {
		t.Errorf("EnvMap() = %v, want %v", got, want)
	}

	// Reproducibility needs stability: two calls must return identical content.
	if got, got2 := EnvMap(), EnvMap(); !slices.Equal(got, got2) {
		t.Errorf("EnvMap() unstable: %v != %v", got, got2)
	}
}
