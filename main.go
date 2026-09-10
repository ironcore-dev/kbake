// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Command kbake builds a Linux kernel from a declarative Kernelfile.
//
// See DESIGN.md for the full specification. The actual logic lives in
// kbake/internal/*; this file is a thin entry point.
package main

import (
	"os"

	"github.com/ironcore-dev/kbake/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args, os.Stdout, os.Stderr))
}
