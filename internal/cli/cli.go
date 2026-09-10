// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package cli implements the kbake command-line interface.
//
// The CLI is built on github.com/spf13/cobra. Each subcommand lives in its
// own package — internal/cli/build (the "build" command) and internal/cli/render
// (the "render" command) — and exposes a NewCommand constructor returning a
// *cobra.Command. This package wires those subcommands onto the root command,
// owns the trivial "version" command, and maps errors returned by cobra to the
// process exit code.
//
// Exit codes:
//
//	0  success
//	1  runtime error (bad Kernelfile, build failure, ...)
//	2  usage error (unknown command, bad flags, no subcommand)
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/ironcore-dev/kbake/internal/cli/get"
	"github.com/ironcore-dev/kbake/internal/cli/image"
	"github.com/ironcore-dev/kbake/internal/cli/pull"
	"github.com/ironcore-dev/kbake/internal/cli/push"
	"github.com/ironcore-dev/kbake/internal/cli/tag"
	"github.com/ironcore-dev/kbake/internal/ociauth"

	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/content/oci"

	"github.com/ironcore-dev/kbake/internal/cli/build"
	"github.com/ironcore-dev/kbake/internal/cli/clierr"
)

var (
	DefaultLocalDir string
)

func init() {
	userHome, err := os.UserHomeDir()
	if err == nil {
		DefaultLocalDir = filepath.Join(userHome, ".kbake", "repo")
	}
}

func localGetter(localDir *string) func() (*oci.Store, error) {
	return func() (*oci.Store, error) {
		if *localDir == "" {
			return nil, fmt.Errorf("must specify --local-dir")
		}

		local, err := oci.New(*localDir)
		if err != nil {
			return nil, fmt.Errorf("creating local oci repository: %w", err)
		}

		return local, nil
	}
}

// Command builds the root kbake command tree, writing normal output to
// stdout and errors/usage to stderr.
func Command(stdout, stderr io.Writer) *cobra.Command {
	var (
		localDir string
	)

	root := &cobra.Command{
		Use:   "kbake",
		Short: "A declarative Linux kernel builder",
		Long: `kbake builds a Linux kernel from a declarative Kernelfile (YAML).

For one or more architectures it produces a kernel binary (always named
"kernel") and the matching "modules.tar", reproducibly. See DESIGN.md for the
full specification.`,
	}
	// Disable the default "completion" command to keep the surface limited to
	// build / render / version.
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetOut(stdout)
	root.SetErr(stderr)

	root.AddCommand(
		build.Command(localGetter(&localDir), stdout, stderr),
		get.Command(localGetter(&localDir), ociauth.NewRepository, stdout, stderr),
		image.Command(localGetter(&localDir), stdout, stderr),
		pull.Command(localGetter(&localDir), ociauth.NewRepository, stdout, stderr),
		push.Command(localGetter(&localDir), ociauth.NewRepository, stdout, stderr),
		tag.Command(localGetter(&localDir), stdout, stderr),
		newVersionCmd(stdout),
	)

	root.PersistentFlags().StringVar(&localDir, "local-dir", DefaultLocalDir, "Local repo directory")

	return root
}

// newVersionCmd returns the trivial "version" subcommand.
func newVersionCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the kbake version",
		Args:  cobra.NoArgs,
		Run: func(_ *cobra.Command, _ []string) {
			var version string
			info, ok := debug.ReadBuildInfo()
			if !ok || info == nil || info.Main.Version != "" {
				version = "(unknown)"
			} else {
				version = info.Main.Version
			}

			_, _ = fmt.Fprintf(stdout, "kbake %s\n", version)
		},
	}
}

// Run is the entry point used by main. It parses argv, dispatches to the
// matching subcommand, and returns the process exit code.
func Run(argv []string, stdout, stderr io.Writer) int {
	root := Command(stdout, stderr)
	// Silence cobra's own error/usage printing so we control the output format
	// ("kbake: <error>") and the exit codes.
	root.SilenceErrors = true
	root.SilenceUsage = true

	args := argv[1:] // strip the program name
	if len(args) == 0 {
		// No subcommand: print help to stderr and treat it as a usage error,
		// matching the original flag-based CLI.
		root.SetOut(stderr)
		_ = root.Help()
		return 2
	}
	root.SetArgs(args)

	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintf(stderr, "kbake: %v\n", err)
		// A wrapped runtime error is a logic failure (exit 1); anything else
		// (unknown command, bad flags) is a usage error (exit 2).
		var re *clierr.RuntimeError
		if errors.As(err, &re) {
			return 1
		}
		return 2
	}
	return 0
}
