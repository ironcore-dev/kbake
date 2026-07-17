// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/ironcore-dev/kbake/internal/config"
	"github.com/ironcore-dev/kbake/internal/expr"
	"github.com/ironcore-dev/kbake/internal/image"
	"github.com/ironcore-dev/kbake/internal/kernelfile"
	"github.com/ironcore-dev/kbake/internal/progress"
	"github.com/ironcore-dev/kbake/internal/source"
	"github.com/ironcore-dev/kbake/internal/targz"
	"github.com/ironcore-dev/kbake/internal/xos"

	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/errdef"
)

type Config map[string]string

type Builder struct {
	keepWork  bool
	numJobs   *int
	ccacheDir string

	fetcher    Fetcher
	makeRunner MakeRunner

	local   oras.Target
	workDir string
}

type BuilderOptions struct {
	NumJobs    *int
	CacheDir   string
	CCacheDir  string
	Fetcher    Fetcher
	WorkDir    string
	MakeRunner MakeRunner
	Local      oras.Target
	// KeepWork keeps the per-build work directory (under WorkDir) after the
	// build instead of removing it — for debugging build failures.
	KeepWork bool
}

type Fetcher interface {
	Fetch(ctx context.Context, ref, dst string) error
}

type defaultFetcher struct {
	cacheDir string
}

func (f *defaultFetcher) Fetch(ctx context.Context, ref, dst string) error {
	rep := progress.ReporterFromContext(ctx)

	res, err := source.Fetch(ctx, ref, f.cacheDir)
	if err != nil {
		return fmt.Errorf("fetching %q: %w", ref, err)
	}

	rep.Detail("Copy tree")
	if err := xos.CopyTree(res.Dir, dst, rep); err != nil {
		return fmt.Errorf("copying %q to %q: %w", ref, dst, err)
	}
	return nil
}

func NewBuilder(opts BuilderOptions) (*Builder, error) {
	if opts.Fetcher == nil {
		if opts.CacheDir == "" {
			return nil, errors.New("no cache dir and no fetcher provided")
		}
		opts.Fetcher = &defaultFetcher{
			cacheDir: opts.CacheDir,
		}
	}
	if opts.WorkDir == "" {
		return nil, errors.New("must specify WorkDir")
	}
	if opts.MakeRunner == nil {
		return nil, errors.New("must specify MakeRunner")
	}
	if opts.Local == nil {
		return nil, errors.New("must specify Local")
	}

	return &Builder{
		keepWork:   opts.KeepWork,
		numJobs:    opts.NumJobs,
		ccacheDir:  opts.CCacheDir,
		fetcher:    opts.Fetcher,
		makeRunner: opts.MakeRunner,
		local:      opts.Local,
		workDir:    opts.WorkDir,
	}, nil
}

func effectiveActions(f *kernelfile.Kernelfile) []kernelfile.Actions {
	hasModuleActions := len(f.Modules.Enable) > 0 || len(f.Modules.Disable) > 0 || len(f.Modules.Builtin) > 0

	hasOptionActions := len(f.Options.Enable) > 0 || len(f.Options.Disable) > 0

	hasFileActions := len(f.Files) > 0

	if !hasModuleActions && !hasOptionActions && !hasFileActions {
		return f.Actions
	}

	var initActions []kernelfile.Action

	if hasModuleActions {
		for _, m := range f.Modules.Enable {
			initActions = append(initActions, kernelfile.Action{
				SetConfig: &kernelfile.SetConfig{
					Name:  m,
					Value: kernelfile.ConfigValueModule,
				},
			})
		}
		for _, m := range f.Modules.Builtin {
			initActions = append(initActions, kernelfile.Action{
				SetConfig: &kernelfile.SetConfig{
					Name:  m,
					Value: kernelfile.ConfigValueYes,
				},
			})
		}
		for _, m := range f.Modules.Disable {
			initActions = append(initActions, kernelfile.Action{
				SetConfig: &kernelfile.SetConfig{
					Name:  m,
					Value: kernelfile.ConfigValueNo,
				},
			})
		}
	}

	if hasOptionActions {
		for _, o := range f.Options.Enable {
			initActions = append(initActions, kernelfile.Action{
				SetConfig: &kernelfile.SetConfig{
					Name:  o,
					Value: kernelfile.ConfigValueYes,
				},
			})
		}
		for _, o := range f.Options.Disable {
			initActions = append(initActions, kernelfile.Action{
				SetConfig: &kernelfile.SetConfig{
					Name:  o,
					Value: kernelfile.ConfigValueNo,
				},
			})
		}
	}

	if hasFileActions {
		for _, m := range f.Files {
			initActions = append(initActions, kernelfile.Action{
				AddFile:    m.Add,
				DeleteFile: m.Delete,
			})
		}
	}

	res := make([]kernelfile.Actions, 0, 1+len(f.Actions))
	res = append(res, kernelfile.Actions{Actions: initActions})
	res = append(res, f.Actions...)
	return res
}

// checkNoEscape reports whether rel — a path relative to root — resolves
// within root. os.Root.Stat rejects ..-escapes, absolute paths, and any
// symlink component that resolves outside the root, which is exactly the
// escape protection AddFile/DeleteFile need. A non-existent path (os.ErrNotExist)
// is tolerated: the path may not *name* anything yet but is still inside the
// root. Stat is always given the *relative* path — it rejects absolute ones.
func checkNoEscape(root, rel string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	if _, err := r.Stat(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// statInRoot is checkNoEscape with an existence requirement: it resolves rel
// within root and returns its FileInfo, erroring on escapes (like
// checkNoEscape) as well as on os.ErrNotExist.
func statInRoot(root, rel string) (os.FileInfo, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()

	return r.Stat(rel)
}

func toConfigValue(kValue kernelfile.ConfigValue) (string, error) {
	switch kValue {
	case kernelfile.ConfigValueYes:
		return config.Yes, nil
	case kernelfile.ConfigValueNo:
		return config.No, nil
	case kernelfile.ConfigValueModule:
		return config.Module, nil
	default:
		return "", fmt.Errorf("unknown value %q", kValue)
	}
}

func (b *Builder) evalAction(_ context.Context, contextDir string, s expr.Scope, a *kernelfile.Action, dir string, cfg Config) error {
	if cond := a.If; cond != "" {
		ok, err := expr.EvalBool(cond, s)
		if err != nil {
			return fmt.Errorf("eval condition: %w", err)
		}
		if !ok {
			return nil
		}
	}

	switch {
	case a.AddFile != nil:
		// Resolve src inside the build context and dst inside the tree. The
		// checks are against the *relative* forms: os.Root rejects escapes
		// above the root (and absolute paths outright).
		srcRel := s.Subst(a.AddFile.Src)
		dstRel := s.Subst(a.AddFile.Dst)
		if srcRel == "" {
			return fmt.Errorf("addFile: src must not be empty")
		}
		if dstRel == "" {
			return fmt.Errorf("addFile: dst must not be empty")
		}
		srcInfo, err := statInRoot(contextDir, srcRel)
		if err != nil {
			return fmt.Errorf("addFile: source %q (in context dir): %w", srcRel, err)
		}
		if err := checkNoEscape(dir, dstRel); err != nil {
			return fmt.Errorf("addFile: destination %q (in tree): %w", dstRel, err)
		}
		src := filepath.Join(contextDir, srcRel)
		dst := filepath.Join(dir, dstRel)
		if srcInfo.IsDir() {
			if err := xos.CopyTree(src, dst, nil); err != nil {
				return fmt.Errorf("addFile: copying directory %q to %q: %w", srcRel, dstRel, err)
			}
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("addFile: creating parent dir of %q: %w", dstRel, err)
		}
		if err := xos.CopyFile(src, dst); err != nil {
			return fmt.Errorf("addFile: copying file %q to %q: %w", srcRel, dstRel, err)
		}
		return nil
	case a.DeleteFile != nil:
		// Absent semantics: removing a path that doesn't exist is a no-op, so
		// re-running a Kernelfile against a changed tree is idempotent.
		rel := s.Subst(a.DeleteFile.Path)
		if rel == "" {
			return fmt.Errorf("deleteFile: path must not be empty")
		}
		if err := checkNoEscape(dir, rel); err != nil {
			return fmt.Errorf("deleteFile: %q (in tree): %w", rel, err)
		}
		if err := os.RemoveAll(filepath.Join(dir, rel)); err != nil {
			return fmt.Errorf("deleteFile: removing %q: %w", rel, err)
		}
		return nil
	case a.SetConfig != nil:
		name := s.Subst(a.SetConfig.Name)
		v, err := toConfigValue(a.SetConfig.Value)
		if err != nil {
			return err
		}
		cfg[name] = v
		return nil
	case a.DeleteConfig != nil:
		name := s.Subst(a.DeleteConfig.Name)
		delete(cfg, name)
		return nil
	default:
		return fmt.Errorf("invalid action %#+v", a)
	}
}

func (b *Builder) evalActions(ctx context.Context, contextDir string, s expr.Scope, actions *kernelfile.Actions, dir string, cfg Config) error {
	if cond := actions.If; cond != "" {
		ok, err := expr.EvalBool(cond, s)
		if err != nil {
			return fmt.Errorf("eval condition: %w", err)
		}
		if !ok {
			return nil
		}
	}

	for i, action := range actions.Actions {
		if err := b.evalAction(ctx, contextDir, s, &action, dir, cfg); err != nil {
			return fmt.Errorf("[action %d] %w", i, err)
		}
	}
	return nil
}

func (b *Builder) fetchRef(ctx context.Context, treeDir, arch, ref string) error {
	return fmt.Errorf("unimplemented")
}

func (b *Builder) fetchGit(ctx context.Context, treeDir, gitRef string) error {
	rep := progress.ReporterFromContext(ctx)
	rep.Detail(fmt.Sprintf("fetch git (%s)", gitRef))
	err := b.fetcher.Fetch(ctx, gitRef, treeDir)
	rep.Done(err)
	if err != nil {
		return fmt.Errorf("fetching git ref %s: %w", gitRef, err)
	}
	return nil
}

func (b *Builder) buildTree(ctx context.Context, f *kernelfile.Kernelfile, contextDir, arch, treeDir string) (Config, error) {
	rep := progress.ReporterFromContext(ctx)

	rep.Step("From")
	err := func() error {
		switch {
		case f.From.IsRef():
			return b.fetchRef(ctx, treeDir, arch, f.From.Ref())
		case f.From.IsScratch():
			return nil
		case f.From.IsGit():
			return b.fetchGit(ctx, treeDir, f.From.Git())
		default:
			return fmt.Errorf("unsupported from %q", f.From)
		}
	}()
	rep.Done(err)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}

	s := expr.Scope{"arch": arch}
	buildCfg := make(Config)
	rep.Step("Evaluate actions")
	err = func() error {
		for i, actions := range effectiveActions(f) {
			if err := b.evalActions(ctx, contextDir, s, &actions, treeDir, buildCfg); err != nil {
				return fmt.Errorf("[effective action %d] %w", i, err)
			}
		}
		return nil
	}()
	rep.Done(err)
	if err != nil {
		return nil, fmt.Errorf("evaluating actions: %w", err)
	}

	return buildCfg, nil
}

func (b *Builder) make(ctx context.Context, arch, treeDir, outDir string, cfg Config) (kernelBinary, modules string, retErr error) {
	rep := progress.ReporterFromContext(ctx)

	rep.Step(fmt.Sprintf("make %s", arch))
	defer func() {
		rep.Done(retErr)
	}()

	// 1. defconfig
	rep.Detail("defconfig")
	if err := b.makeRunner.Defconfig(ctx, treeDir, arch); err != nil {
		return "", "", fmt.Errorf("make defconfig: %w", err)
	}

	// 2. apply options/modules into .config
	cfgPath := filepath.Join(treeDir, ".config")
	kCfg, err := config.Read(cfgPath)
	if err != nil {
		return "", "", fmt.Errorf("read .config: %w", err)
	}

	for k, v := range cfg {
		kCfg.Set(k, v)
	}
	if err := kCfg.Write(cfgPath); err != nil {
		return "", "", fmt.Errorf("write .config: %w", err)
	}

	// 3. olddefconfig to resolve dependencies deterministically.
	rep.Detail("olddefconfig")
	if err := b.makeRunner.Olddefconfig(ctx, treeDir, arch); err != nil {
		return "", "", fmt.Errorf("make olddefconfig: %w", err)
	}

	// 4. build the kernel and modules.
	rep.Detail("compile")
	kernelBinary, err = b.makeRunner.Compile(ctx, treeDir, arch, CompileOptions{
		NumJobs:   b.numJobs,
		CCacheDir: b.ccacheDir,
	})
	if err != nil {
		return "", "", fmt.Errorf("make: %w", err)
	}

	modulesOut := filepath.Join(outDir, "modules")
	if err := os.MkdirAll(modulesOut, os.ModePerm); err != nil {
		return "", "", fmt.Errorf("mkdir modules out: %w", err)
	}

	// 6. modules_install -> tar root.
	rep.Detail("installing modules")
	if err := b.makeRunner.ModulesInstall(ctx, treeDir, arch, modulesOut); err != nil {
		return "", "", fmt.Errorf("make modules_install: %w", err)
	}

	modules = filepath.Join(modulesOut, "lib", "modules")
	return kernelBinary, modules, nil
}

func (b *Builder) pushConfig(ctx context.Context, arch string, cfg Config) (ocispec.Descriptor, error) {
	img := image.Image{
		Platform: ocispec.Platform{
			Architecture: arch,
			OS:           "linux",
		},
		Config: cfg,
	}
	data, err := json.Marshal(&img)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("marshal image config: %w", err)
	}

	return oras.PushBytes(ctx, b.local, image.MediaTypeConfig, data)
}

func (b *Builder) push(ctx context.Context, arch, dir string, cfg Config, kernelBinary, modules string) (desc ocispec.Descriptor, retErr error) {
	rep := progress.ReporterFromContext(ctx)
	rep.Step("Pushing")
	defer func() {
		rep.Done(retErr)
	}()

	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, os.ModePerm); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("make out dir: %w", err)
	}

	rep.Detail("Create modules.tar.gz")
	modulesTarGz := filepath.Join(outDir, "modules.tar.gz")
	if err := targz.CreateFile(modulesTarGz, modules); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("create modules.tar.gz: %w", err)
	}

	rep.Detail("kernel")
	kernelDesc, err := b.pushLayer(ctx, kernelBinary, image.MediaTypeKernel)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push kernel: %w", err)
	}

	rep.Detail("modules")
	modulesDesc, err := b.pushLayer(ctx, modulesTarGz, image.MediaTypeModulesGzip)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push modules: %w", err)
	}

	rep.Detail("config")
	cfgDesc, err := b.pushConfig(ctx, arch, cfg)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push config: %w", err)
	}

	rep.Detail("manifest")
	desc, err = oras.PackManifest(ctx, b.local, oras.PackManifestVersion1_1, image.ArtifactType, oras.PackManifestOptions{
		ConfigDescriptor: &cfgDesc,
		Layers:           []ocispec.Descriptor{kernelDesc, modulesDesc},
	})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pack manifest: %w", err)
	}
	return desc, nil
}

func (b *Builder) buildArch(ctx context.Context, f *kernelfile.Kernelfile, contextDir, arch, dir string) (ocispec.Descriptor, error) {
	treeDir := filepath.Join(dir, "tree")
	outDir := filepath.Join(dir, "out")
	for _, dir := range []string{treeDir, outDir} {
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	cfg, err := b.buildTree(ctx, f, contextDir, arch, treeDir)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("build tree: %w", err)
	}

	kernelBinary, modules, err := b.make(ctx, arch, treeDir, outDir, cfg)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("make: %w", err)
	}

	desc, err := b.push(ctx, arch, dir, cfg, kernelBinary, modules)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pushing: %w", err)
	}
	return desc, nil
}

func (b *Builder) pushLayer(ctx context.Context, path string, mediaType string) (ocispec.Descriptor, error) {
	f, err := os.Open(path)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("stat %s: %w", path, err)
	}

	d, err := digest.FromReader(f)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("creating digest of %s: %w", path, err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("seek %s: %w", path, err)
	}

	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Size:      stat.Size(),
		Digest:    d,
	}
	if err := b.local.Push(ctx, desc, f); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return desc, fmt.Errorf("push %s: %w", path, err)
	}
	return desc, nil
}

func (b *Builder) buildAndPushIndex(ctx context.Context, descs []ocispec.Descriptor) (desc ocispec.Descriptor, retErr error) {
	rep := progress.ReporterFromContext(ctx)
	rep.Step("Build / Push index")
	defer func() {
		rep.Done(retErr)
	}()

	idx := &ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: descs,
	}
	data, err := json.Marshal(idx)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("marshal index: %w", err)
	}

	desc, err = oras.PushBytes(ctx, b.local, ocispec.MediaTypeImageIndex, data)
	if err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return ocispec.Descriptor{}, fmt.Errorf("push index: %w", err)
	}
	return desc, nil
}

// Build builds the kernel for the given archs and pushes the resulting image
// index to the local store, returning its descriptor. The second return value
// is the per-build work directory — kept only when KeepWork is set (it is
// removed before Build returns otherwise), so callers can point the user at
// it for debugging.
func (b *Builder) Build(ctx context.Context, f *kernelfile.Kernelfile, contextDir string, archs []string) (ocispec.Descriptor, string, error) {
	if len(archs) == 0 {
		return ocispec.Descriptor{}, "", fmt.Errorf("must specify at least one architecture")
	}

	descs := make([]ocispec.Descriptor, 0, len(archs))
	slices.Sort(archs)

	buildUID := uuid.NewString()
	buildDir := filepath.Join(b.workDir, buildUID)
	if err := os.MkdirAll(buildDir, os.ModePerm); err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("creating build directory at %s: %w", buildDir, err)
	}
	if !b.keepWork {
		defer func() { _ = os.RemoveAll(buildDir) }()
	}

	for _, arch := range archs {
		archBuildDir := filepath.Join(buildDir, arch)
		if err := os.MkdirAll(archBuildDir, os.ModePerm); err != nil {
			return ocispec.Descriptor{}, "", fmt.Errorf("creating arch %s build directory at %s: %w", arch, archBuildDir, err)
		}

		desc, err := b.buildArch(ctx, f, contextDir, arch, archBuildDir)
		if err != nil {
			return ocispec.Descriptor{}, buildDir, fmt.Errorf("[arch %s] %w", arch, err)
		}

		desc.ArtifactType = image.ArtifactType
		desc.Platform = &ocispec.Platform{
			OS:           "linux",
			Architecture: arch,
		}
		descs = append(descs, desc)
	}

	desc, err := b.buildAndPushIndex(ctx, descs)
	if err != nil {
		return ocispec.Descriptor{}, buildDir, err
	}
	return desc, buildDir, nil
}
