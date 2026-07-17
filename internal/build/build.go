// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ironcore-dev/kbake/internal/kernelmake"
	"github.com/ironcore-dev/kbake/internal/meta"
	"github.com/ironcore-dev/kbake/internal/progress"
)

type CompileOptions struct {
	NumJobs *int
	// CCacheDir, when non-empty, enables ccache for the compile phase and
	// points ccache at this directory (CCACHE_DIR). Empty = ccache disabled.
	CCacheDir string
}

type MakeRunner interface {
	Defconfig(ctx context.Context, dir string, arch string) error
	Olddefconfig(ctx context.Context, dir string, arch string) error
	Compile(ctx context.Context, dir string, arch string, opts CompileOptions) (kernelBinary string, err error)
	ModulesInstall(ctx context.Context, dir string, arch string, modulesOut string) error
}

type MakeRunnerOptions struct {
	LocalExecutable string
}

func NewMakeRunner(opts MakeRunnerOptions) (MakeRunner, error) {
	return newDefaultRunner(defaultRunnerOptions{
		executable: opts.LocalExecutable,
	})
}

type defaultRunner struct {
	make string
	// supported is the set of archs whose full toolchain (gcc + binutils) was
	// found on PATH at construction.
	supported map[string]struct{}
	// missing records, per unsupported arch, the binaries the probe did not
	// find — used to build the Compile error message.
	missing map[string][]string
}

type defaultRunnerOptions struct {
	executable string
}

// toolchainBins is the set of binaries kbuild invokes for a kernel build,
// beyond the C compiler: the binutils AS/LD/AR/NM/OBJCOPY/OBJDUMP/STRIP. gcc
// is packaged separately from binutils in Debian, and a missing ld would
// otherwise slip past a gcc-only check and fail late in `make`, so the probe
// checks the full set.
var toolchainBins = []string{"gcc", "as", "ld", "ar", "nm", "objcopy", "objdump", "strip"}

// crossCompilePrefix returns the CROSS_COMPILE= value to pass to make for the
// given arch. Native builds (arch == runtime.GOARCH) pass an empty prefix so
// kbuild invokes the bare host toolchain (gcc/as/ld/…); cross builds use the
// arch's configured triplet prefix. This matches what newDefaultRunner probes,
// so a stock host with only bare gcc/as reports its native arch as supported
// rather than requiring the cross-named package for its own architecture.
func crossCompilePrefix(info kernelmake.Info, arch string) string {
	if arch == runtime.GOARCH {
		return ""
	}
	return info.CrossCompile
}

func newDefaultRunner(opts defaultRunnerOptions) (*defaultRunner, error) {
	return newDefaultRunnerWith(opts, exec.LookPath)
}

// newDefaultRunnerWith is the testable constructor: look is the binary-lookup
// seam (exec.LookPath in production). It resolves `make`, then probes every
// arch's toolchain to populate supported/missing. Discovery only — gating is
// deferred to Compile, the sole consumer of the cross toolchain (defconfig/
// olddefconfig run kconfig via hostcc, modules_install only copies objects).
func newDefaultRunnerWith(opts defaultRunnerOptions, look func(string) (string, error)) (*defaultRunner, error) {
	executable := cmp.Or(opts.executable, "make")
	makeBin, err := look(executable)
	if err != nil {
		return nil, fmt.Errorf("looking up executable %q: %w", executable, err)
	}

	supported := map[string]struct{}{}
	missing := map[string][]string{}
	for _, arch := range kernelmake.AllArchs() {
		info, _ := kernelmake.InfoFor(arch)
		prefix := crossCompilePrefix(info, arch)
		var miss []string
		for _, name := range toolchainBins {
			if _, err := look(prefix + name); err != nil {
				miss = append(miss, prefix+name)
			}
		}
		if len(miss) == 0 {
			supported[arch] = struct{}{}
		} else {
			missing[arch] = miss
		}
	}

	return &defaultRunner{
		make:      makeBin,
		supported: supported,
		missing:   missing,
	}, nil
}

func (d *defaultRunner) run(ctx context.Context, dir string, env []string, args []string, rep progress.Reporter) error {
	cmd := exec.CommandContext(ctx, d.make, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = rep
	cmd.Stderr = rep
	return cmd.Run()
}

func (d *defaultRunner) Defconfig(ctx context.Context, dir string, arch string) error {
	rep := progress.ReporterFromContext(ctx)

	info, err := kernelmake.InfoFor(arch)
	if err != nil {
		return err
	}
	prefix := crossCompilePrefix(info, arch)
	return d.run(ctx, dir, os.Environ(), []string{
		"ARCH=" + info.Kernel,
		"CROSS_COMPILE=" + prefix,
		info.Defconfig,
	}, rep)
}

func (d *defaultRunner) Olddefconfig(ctx context.Context, dir string, arch string) error {
	rep := progress.ReporterFromContext(ctx)

	info, err := kernelmake.InfoFor(arch)
	if err != nil {
		return err
	}
	prefix := crossCompilePrefix(info, arch)
	return d.run(ctx, dir, os.Environ(), []string{
		"ARCH=" + info.Kernel,
		"CROSS_COMPILE=" + prefix,
		"olddefconfig",
	}, rep)
}

func (d *defaultRunner) Compile(ctx context.Context, dir string, arch string, opts CompileOptions) (kernelBinary string, err error) {
	rep := progress.ReporterFromContext(ctx)

	info, err := kernelmake.InfoFor(arch)
	if err != nil {
		return "", err
	}

	if _, ok := d.supported[arch]; !ok {
		return "", fmt.Errorf("compile %s: missing toolchain: %s", arch, strings.Join(d.missing[arch], ", "))
	}
	prefix := crossCompilePrefix(info, arch)

	env := append(os.Environ(), meta.EnvMap()...)
	runArgs := []string{
		"ARCH=" + info.Kernel,
		"CROSS_COMPILE=" + prefix,
	}

	if opts.CCacheDir != "" {
		if _, err := exec.LookPath("ccache"); err != nil {
			return "", fmt.Errorf("ccache enabled by default but ccache not found in PATH: install it or pass --no-ccache: %w", err)
		}
		if err := os.MkdirAll(opts.CCacheDir, 0o755); err != nil {
			return "", fmt.Errorf("ccache dir: %w", err)
		}
		// CCACHE_DIR points ccache at its cache; CCACHE_BASEDIR + NOHASHDIR
		// make builds in different per-arch tree dirs (and across machines) hit
		// the same cache instead of hashing the absolute source path.
		env = append(env,
			"CCACHE_DIR="+opts.CCacheDir,
			"CCACHE_BASEDIR="+dir,
			"CCACHE_NOHASHDIR=1",
		)
		// CC is the ccache-wrapped gcc: native (empty prefix) -> "ccache gcc",
		// cross -> "ccache <prefix>gcc". CROSS_COMPILE still prefixes the
		// binutils AS/LD/AR/NM.
		runArgs = append(runArgs, "CC=ccache "+prefix+"gcc")
	}

	switch {
	case opts.NumJobs != nil && *opts.NumJobs != 0:
		runArgs = append(runArgs, fmt.Sprintf("-j%d", *opts.NumJobs))
	case opts.NumJobs == nil:
		runArgs = append(runArgs, fmt.Sprintf("-j%d", runtime.NumCPU()+1))
	}

	if err := d.run(ctx, dir, env, runArgs, rep); err != nil {
		return "", fmt.Errorf("compile: %w", err)
	}

	return filepath.Join(dir, info.Image), nil
}

func (d *defaultRunner) ModulesInstall(ctx context.Context, dir string, arch string, modulesOut string) error {
	rep := progress.ReporterFromContext(ctx)

	info, err := kernelmake.InfoFor(arch)
	if err != nil {
		return err
	}
	prefix := crossCompilePrefix(info, arch)
	if err := d.run(ctx, dir, os.Environ(), []string{
		"ARCH=" + info.Kernel,
		"CROSS_COMPILE=" + prefix,
		"INSTALL_MOD_PATH=" + modulesOut,
		"modules_install",
	}, rep); err != nil {
		return fmt.Errorf("modules_install: %w", err)
	}
	return nil
}
