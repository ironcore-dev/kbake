// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package build implements the "kbake build" subcommand.
//
// It parses a Kernelfile and the build flags, resolves the target
// architectures and the build context, then delegates to the build
// orchestrator (kbake/internal/build) to fetch the source, configure and
// compile the kernel, and emit the per-arch artifacts.
package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/ironcore-dev/kbake/internal/progress"

	buildcore "github.com/ironcore-dev/kbake/internal/build"
	"github.com/ironcore-dev/kbake/internal/cli/clierr"
	"github.com/ironcore-dev/kbake/internal/kernelfile"

	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/content/oci"
)

var (
	DefaultCCacheDir   string
	DefaultModCacheDir string
	DefaultWorkDir     string
	DefaultTempDir     string
)

const (
	DefaultFile = "Kernelfile"
)

func init() {
	userCacheDir, err := os.UserCacheDir()
	if err == nil {
		DefaultCCacheDir = filepath.Join(userCacheDir, "kbake", "ccache")
		DefaultModCacheDir = filepath.Join(userCacheDir, "kbake", "mod")
		DefaultWorkDir = filepath.Join(userCacheDir, "kbake", "work")
	}
	DefaultTempDir = os.TempDir()
}

type Options struct {
	file      string
	archs     []string
	tags      []string
	jobs      int
	tempDir   string
	workDir   string
	keepWork  bool
	ccacheDir string
	noCcache  bool
	cacheDir  string
}

func NewOptions() *Options {
	return &Options{
		tempDir:   DefaultTempDir,
		workDir:   DefaultWorkDir,
		archs:     []string{runtime.GOARCH},
		ccacheDir: DefaultCCacheDir,
		cacheDir:  DefaultModCacheDir,
		file:      DefaultFile,
		jobs:      -1,
	}
}

func (o *Options) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVar(&o.tempDir, "temp-dir", o.tempDir, "Temp directory for build")
	flags.StringVar(&o.workDir, "work-dir", o.workDir, "Work directory for build")
	flags.StringVarP(&o.file, "file", "f", o.file, "path to the Kernelfile (- reads stdin)")
	flags.StringSliceVar(&o.archs, "arch", o.archs, "comma-separated list of architectures (e.g. amd64,arm64)")
	flags.StringSliceVarP(&o.tags, "tag", "t", o.tags, "comma-separated list of image tags")
	flags.IntVarP(&o.jobs, "jobs", "j", o.jobs, "parallel make jobs (-1 = number of CPUs)")
	flags.BoolVar(&o.keepWork, "keep-work", o.keepWork, "Keep the per-build work dir (path printed to stderr)")
	flags.StringVar(&o.ccacheDir, "ccache-dir", o.ccacheDir, "ccache cache directory")
	flags.BoolVar(&o.noCcache, "no-ccache", o.noCcache, "disable ccache")
	flags.StringVar(&o.cacheDir, "cache-dir", o.cacheDir, "go-style module cache root (layout: <host>/<path>/<repo>@<version>")
}

// Command returns the "build" subcommand. stdout and stderr receive build
// progress and make output, respectively.
func Command(
	getLocal func() (*oci.Store, error),
	stdout, stderr io.Writer,
) *cobra.Command {
	var (
		opts = NewOptions()
	)

	var cmd *cobra.Command
	cmd = &cobra.Command{
		Use:   "build CONTEXT",
		Short: "Build a kernel for one or more architectures",
		Long:  "Build a kernel (and its modules.tar) from a Kernelfile for the given architectures",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			contextDir := args[0]

			local, err := getLocal()
			if err != nil {
				return clierr.Wrap(err)
			}

			return clierr.Wrap(Run(ctx, local, stdout, stderr, contextDir, *opts))
		},
	}

	opts.AddFlags(cmd)

	return cmd
}

func Run(ctx context.Context, local *oci.Store, stdout, stderr io.Writer, contextDir string, opts Options) (retErr error) {
	if opts.workDir == "" {
		return fmt.Errorf("must specify --work-dir")
	}
	if opts.cacheDir == "" {
		return fmt.Errorf("must specify --cache-dir")
	}
	if opts.tempDir == "" {
		return fmt.Errorf("must specify --temp-dir")
	}

	k, err := kernelfile.Load(opts.file)
	if err != nil {
		return fmt.Errorf("load kernelfile: %w", err)
	}

	makeRunner, err := buildcore.NewMakeRunner(buildcore.MakeRunnerOptions{})
	if err != nil {
		return fmt.Errorf("creating make runner: %w", err)
	}

	var numJobs *int
	if opts.jobs >= 0 {
		numJobs = new(opts.jobs)
	}

	ccacheDir := opts.ccacheDir
	if opts.noCcache {
		ccacheDir = ""
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fullLog, err := os.CreateTemp(opts.tempDir, "build-*.log")
	if err != nil {
		return fmt.Errorf("creating build log: %w", err)
	}

	logRep := progress.NewFileReporter(fullLog)
	iRep, closeRep := progress.New(stderr, cancel)

	rep := progress.MultiReporter(logRep, iRep)

	defer func() {
		_ = logRep.Close()
		_ = closeRep()
		if retErr == nil {
			_ = os.Remove(fullLog.Name())
		} else {
			_, _ = fmt.Fprintf(stderr, "full build log at %s\n", fullLog.Name())
		}
	}()

	ctx = progress.NewContext(ctx, rep)

	dirsToCreate := []string{opts.cacheDir, opts.workDir}
	if ccacheDir != "" {
		dirsToCreate = append(dirsToCreate, ccacheDir)
	}
	for _, dirToCreate := range dirsToCreate {
		if err := os.MkdirAll(dirToCreate, os.ModePerm); err != nil {
			return fmt.Errorf("creating dir at %q: %w", dirToCreate, err)
		}
	}

	b, err := buildcore.NewBuilder(buildcore.BuilderOptions{
		NumJobs:    numJobs,
		CacheDir:   opts.cacheDir,
		CCacheDir:  ccacheDir,
		WorkDir:    opts.workDir,
		MakeRunner: makeRunner,
		Local:      local,
		KeepWork:   opts.keepWork,
	})
	if err != nil {
		return fmt.Errorf("creating builder: %w", err)
	}

	desc, buildDir, err := b.Build(ctx, k, contextDir, opts.archs)
	if opts.keepWork {
		// Point the user at the kept tree on success AND failure — inspecting a
		// failed build's work dir is the flag's main purpose.
		_, _ = fmt.Fprintf(stderr, "work dir kept at %s\n", buildDir)
	}
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "built %s\n", desc.Digest.String())

	if len(opts.tags) > 0 {
		rep.Step("Tagging")
		err := func() error {
			for _, tag := range opts.tags {
				rep.Detail(fmt.Sprintf("tag %s", tag))
				if err := local.Tag(ctx, desc, tag); err != nil {
					return fmt.Errorf("tagging %q: %w", tag, err)
				}

				_, _ = fmt.Fprintf(stdout, "tagged %s\n", tag)
			}
			return nil
		}()
		rep.Done(err)
		if err != nil {
			return err
		}
	}

	return nil
}
