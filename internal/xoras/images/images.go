// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package images provides utility functions to work with image manifests
// and indexes.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"sync"

	"github.com/ironcore-dev/kbake/internal/xoras/config"
	"github.com/ironcore-dev/kbake/internal/xoras/internal/platform"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

const (
	// defaultConcurrency is the default concurrency to use.
	// The value is consistent with dockerd and containerd.
	defaultConcurrency int = 3

	// defaultMaxMetadataBytes is the default amount of bytes to use
	// for caching metadata.
	defaultMaxMetadataBytes int64 = 4 * 1024 * 1024 // 4 MiB

	// defaultMaxBytes is the default amount of bytes to use
	// for fetching content.
	defaultMaxBytes int64 = 4 * 1024 * 1024 // 4 MiB
)

// Path is a path of descriptors.
// Like in a file path all elements but the last are directory names, in a path all elements
// but the last are index descriptors.
type Path []ocispec.Descriptor

// Base returns the last element of the descriptor.
func (p Path) Base() ocispec.Descriptor {
	if len(p) == 0 {
		return ocispec.Descriptor{}
	}
	return p[len(p)-1]
}

// FetchBytesOptions are options for the FetchBytes function.
type FetchBytesOptions struct {
	// MatchArtifactType only matches image manifests with artifact types that pass this function.
	// Other images are skipped.
	MatchArtifactType func(string) bool
	// MatchPlatform only matches image manifests with platforms that pass this function.
	// If a nested index specifies a platform, it must match this, otherwise it's not visited.
	MatchPlatform func(*ocispec.Platform) bool
	// CompareDescriptors allows comparing multiple matching descriptors and ranking them.
	// If omitted, the first matching descriptor is returned.
	CompareDescriptors func(a, b ocispec.Descriptor) int
	// ConfigDecoder specifies how an image manifest's config descriptor is converted into
	// config.Config.
	ConfigDecoder config.Decoder
	// OnIterateError specifies what to do when encountering an error during image iteration.
	OnIterateError func(p Path, err error) error
	// MaxMetadataBytes is the maximum amount of bytes of metadata (images / indexes) to cache.
	MaxMetadataBytes int64
	// MaxBytes is the maximum amount of bytes of content to fetch.
	MaxBytes int64
}

// FetchBytes fetches the content bytes identified by the reference.
func FetchBytes(ctx context.Context, src oras.ReadOnlyTarget, ref string, opts FetchBytesOptions) (Path, []byte, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}

	p, rc, err := Fetch(ctx, src, ref, FetchOptions{
		MatchArtifactType:  opts.MatchArtifactType,
		MatchPlatform:      opts.MatchPlatform,
		CompareDescriptors: opts.CompareDescriptors,
		ConfigDecoder:      opts.ConfigDecoder,
		OnIterateError:     opts.OnIterateError,
		MaxMetadataBytes:   opts.MaxMetadataBytes,
	})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rc.Close() }()

	if p.Base().Size > opts.MaxBytes {
		return nil, nil, fmt.Errorf(
			"content size %v exceeds MaxBytes %v: %w",
			p.Base().Size,
			opts.MaxBytes,
			errdef.ErrSizeExceedsLimit)
	}
	data, err := content.ReadAll(rc, p.Base())
	if err != nil {
		return nil, nil, err
	}

	return p, data, nil
}

// FetchOptions are options for the Fetch function.
type FetchOptions struct {
	// MatchArtifactType only matches image manifests with artifact types that pass this function.
	// Other images are skipped.
	MatchArtifactType func(string) bool
	// MatchPlatform only matches image manifests with platforms that pass this function.
	// If a nested index specifies a platform, it must match this, otherwise it's not visited.
	MatchPlatform func(*ocispec.Platform) bool
	// CompareDescriptors allows comparing multiple matching descriptors and ranking them.
	// If omitted, the first matching descriptor is returned.
	CompareDescriptors func(a, b ocispec.Descriptor) int
	// ConfigDecoder specifies how an image manifest's config descriptor is converted into
	// config.Config.
	ConfigDecoder config.Decoder
	// OnIterateError specifies what to do when encountering an error during image iteration.
	OnIterateError func(p Path, err error) error
	// MaxMetadataBytes is the maximum amount of bytes of metadata (images / indexes) to cache.
	MaxMetadataBytes int64
}

// Fetch resolves a reference given the FetchOptions and then fetches the content the path base points at.
// On success, the matching path towards the image descriptor and the opened read closer is returned.
func Fetch(ctx context.Context, src oras.ReadOnlyTarget, ref string, opts FetchOptions) (Path, io.ReadCloser, error) {
	root, err := src.Resolve(ctx, ref)
	if err != nil {
		return nil, nil, err
	}

	if opts.MaxMetadataBytes <= 0 {
		opts.MaxMetadataBytes = defaultMaxMetadataBytes
	}
	proxy := newMetadataProxy(src, opts.MaxMetadataBytes)
	path, err := ResolveGraph(ctx, proxy, root, ResolveOptions{
		MatchArtifactType:  opts.MatchArtifactType,
		MatchPlatform:      opts.MatchPlatform,
		CompareDescriptors: opts.CompareDescriptors,
		ConfigDecoder:      opts.ConfigDecoder,
		OnIterateError:     opts.OnIterateError,
	})
	if err != nil {
		return nil, nil, err
	}
	rc, err := proxy.Fetch(ctx, path[len(path)-1])
	if err != nil {
		return nil, nil, err
	}
	return path, rc, nil
}

// CopyGraphOptions are options for the CopyGraph operation.
type CopyGraphOptions struct {
	// Concurrency limits the maximum number of concurrent copy tasks.
	// If less than or equal to 0, a default (currently 3) is used.
	Concurrency int
	// FindSuccessors finds the successors of the current node.
	// fetcher provides cached access to the source storage, and is suitable
	// for fetching non-leaf nodes like manifests. Since anything fetched from
	// fetcher will be cached in the memory, it is recommended to use original
	// source storage to fetch large blobs.
	// If FindSuccessors is nil, content.Successors will be used.
	FindSuccessors func(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) ([]ocispec.Descriptor, error)
	// MaxMetadataBytes is the maximum amount of bytes of metadata (images / indexes) to cache.
	MaxMetadataBytes int64
}

func copyNodeData(ctx context.Context, src content.ReadOnlyStorage, dst content.Storage, desc ocispec.Descriptor) error {
	exists, err := dst.Exists(ctx, desc)
	if err != nil || exists {
		return err
	}

	rc, err := src.Fetch(ctx, desc)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	if err := dst.Push(ctx, desc, rc); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return nil
}

type token struct{}

func copyNodes(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	src content.ReadOnlyStorage,
	dst content.Storage,
	descs []ocispec.Descriptor,
	recurse bool,
	sem chan token,
	opts CopyGraphOptions,
) error {
	return copyDispatch(ctx, cancel, src, dst, uniformNodes(descs, recurse), sem, opts)
}

func copyNode(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	src content.ReadOnlyStorage,
	dst content.Storage,
	desc ocispec.Descriptor,
	recurse bool,
	sem chan token,
	opts CopyGraphOptions,
) error {
	if recurse {
		children, err := opts.FindSuccessors(ctx, src, desc)
		if err != nil {
			<-sem
			return err
		}

		if len(children) > 0 {
			<-sem

			if err := copyNodes(ctx, cancel, src, dst, children, recurse, sem, opts); err != nil {
				return err
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case sem <- token{}:
			}
		}
	}

	defer func() { <-sem }()
	return copyNodeData(ctx, src, dst, desc)
}

func copyPath(ctx context.Context, src content.ReadOnlyStorage, dst content.Storage, path Path, opts CopyGraphOptions) error {
	if opts.FindSuccessors == nil {
		opts.FindSuccessors = content.Successors
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	return copyDispatch(ctx, cancel, src, dst, pathNodes(path), make(chan token, opts.Concurrency), opts)
}

// copyDispatch dispatches copy jobs for the given nodes concurrently, bounded
// by sem. The first node error both cancels the remaining work and is returned;
// an early context cancellation without a node error yields ctx.Err().
func copyDispatch(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	src content.ReadOnlyStorage,
	dst content.Storage,
	nodes iter.Seq2[ocispec.Descriptor, bool], // (descriptor, recurse)
	sem chan token,
	opts CopyGraphOptions,
) error {
	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		retErr  error
	)
Dispatch:
	for desc, recurse := range nodes {
		select {
		case <-ctx.Done():
			errOnce.Do(func() { retErr = ctx.Err() })
			break Dispatch
		case sem <- token{}:
			wg.Go(func() {
				if err := copyNode(ctx, cancel, src, dst, desc, recurse, sem, opts); err != nil {
					// record-then-cancel inside one Do: the real error must
					// win over the ctx.Done arm above.
					errOnce.Do(func() {
						retErr = err
						cancel(err)
					})
				}
			})
		}
	}
	wg.Wait()
	return retErr
}

// uniformNodes yields every descriptor with the same recurse flag, in order.
func uniformNodes(descs []ocispec.Descriptor, recurse bool) iter.Seq2[ocispec.Descriptor, bool] {
	return func(yield func(ocispec.Descriptor, bool) bool) {
		for _, d := range descs {
			if !yield(d, recurse) {
				return
			}
		}
	}
}

// pathNodes yields the path bottom-up: leaf first (with recurse), then ancestors.
func pathNodes(path Path) iter.Seq2[ocispec.Descriptor, bool] {
	return func(yield func(ocispec.Descriptor, bool) bool) {
		for i := len(path) - 1; i >= 0; i-- {
			if !yield(path[i], i == len(path)-1) {
				return
			}
		}
	}
}

// CopyGraph copies a path from a source to a destination storage while respecting the CopyGraphOptions.
// A path is copied bottom up, meaning that first the image at the bottom including its successors are copied and
// only then the remaining path bottom-up is copied.
func CopyGraph(ctx context.Context, src content.ReadOnlyStorage, dst content.Storage, path Path, opts CopyGraphOptions) error {
	if opts.MaxMetadataBytes <= 0 {
		opts.MaxMetadataBytes = defaultMaxMetadataBytes
	}

	src = newMetadataProxy(src, opts.MaxMetadataBytes)
	return copyPath(ctx, src, dst, path, opts)
}

// PullOptions are options for the Pull function.
type PullOptions struct {
	// MatchArtifactType only matches image manifests with artifact types that pass this function.
	// Other images are skipped.
	MatchArtifactType func(string) bool
	// MatchPlatform only matches image manifests with platforms that pass this function.
	// If a nested index specifies a platform, it must match this, otherwise it's not visited.
	MatchPlatform func(*ocispec.Platform) bool
	// CompareDescriptors allows comparing multiple matching descriptors and ranking them.
	// If omitted, the first matching descriptor is returned.
	CompareDescriptors func(a, b ocispec.Descriptor) int
	// ConfigDecoder specifies how an image manifest's config descriptor is converted into
	// config.Config.
	ConfigDecoder config.Decoder
	// OnIterateError specifies what to do when encountering an error during image iteration.
	OnIterateError func(p Path, err error) error
	// MaxMetadataBytes is the maximum amount of bytes of metadata (images / indexes) to cache.
	MaxMetadataBytes int64
	// Concurrency limits the maximum number of concurrent copy tasks.
	// If less than or equal to 0, a default (currently 3) is used.
	Concurrency int
	// FindSuccessors finds the successors of the current node.
	// fetcher provides cached access to the source storage, and is suitable
	// for fetching non-leaf nodes like manifests. Since anything fetched from
	// fetcher will be cached in the memory, it is recommended to use original
	// source storage to fetch large blobs.
	// If FindSuccessors is nil, content.Successors will be used.
	FindSuccessors func(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) ([]ocispec.Descriptor, error)
}

// Pull resolves the given ref in a source target. The returned descriptor is then resolved (via Resolve) to a
// target image respecting the specified PullOptions. On success, the resolved path is copied from
// source to target and the path towards the image descriptor is returned.
func Pull(ctx context.Context, src oras.ReadOnlyTarget, dst oras.Target, ref string, opts PullOptions) (Path, error) {
	if opts.MaxMetadataBytes <= 0 {
		opts.MaxMetadataBytes = defaultMaxMetadataBytes
	}

	root, err := src.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}

	resolveOpts := ResolveOptions{
		MatchArtifactType:  opts.MatchArtifactType,
		MatchPlatform:      opts.MatchPlatform,
		CompareDescriptors: opts.CompareDescriptors,
		ConfigDecoder:      opts.ConfigDecoder,
		OnIterateError:     opts.OnIterateError,
	}

	proxy := newMetadataProxy(src, opts.MaxMetadataBytes)
	path, err := ResolveGraph(ctx, proxy, root, resolveOpts)
	if err != nil {
		return nil, err
	}

	if err := copyPath(ctx, proxy, dst, path, CopyGraphOptions{
		Concurrency:    opts.Concurrency,
		FindSuccessors: opts.FindSuccessors,
	}); err != nil {
		return nil, err
	}

	return path, dst.Tag(ctx, root, ref)
}

// MatchArtifactType returns a function that only matches the wanted artifact type.
func MatchArtifactType(want string) func(got string) bool {
	return func(got string) bool {
		return want == got
	}
}

// MatchPlatform returns a function that only matches the wanted platform.
// If the specified platform is nil, will only match nil platforms.
func MatchPlatform(want *ocispec.Platform) func(p *ocispec.Platform) bool {
	return func(got *ocispec.Platform) bool {
		return platform.Match(got, want)
	}
}

// IgnoreImageOrIndexNotFound ignores errdef.ErrNotFound if it happened on
// a Path.Base descriptor that points to an image index or an image manifest.
func IgnoreImageOrIndexNotFound(p Path, err error) error {
	if err == nil || !errors.Is(err, errdef.ErrNotFound) || len(p) == 0 {
		return err
	}
	if p.Base().MediaType == ocispec.MediaTypeImageIndex || p.Base().MediaType == ocispec.MediaTypeImageManifest {
		return nil
	}
	return err
}

// ResolveOptions are options for resolving a graph.
type ResolveOptions struct {
	// MatchArtifactType only matches image manifests with artifact types that pass this function.
	// Other images are skipped.
	MatchArtifactType func(string) bool
	// MatchPlatform only matches image manifests with platforms that pass this function.
	// If a nested index specifies a platform, it must match this, otherwise it's not visited.
	MatchPlatform func(*ocispec.Platform) bool
	// CompareDescriptors allows comparing multiple matching descriptors and ranking them.
	// If omitted, the first matching descriptor is returned.
	CompareDescriptors func(a, b ocispec.Descriptor) int
	// ConfigDecoder specifies how an image manifest's config descriptor is converted into
	// config.Config.
	ConfigDecoder config.Decoder
	// OnIterateError specifies what to do when encountering an error during image iteration.
	OnIterateError func(p Path, err error) error
}

// Resolve resolves a reference to a Path descriptor respecting the given ResolveOptions.
// It does so by iterating the descriptor resolving the ref, traversing indexes & sub-indexes
// as necessary.
func Resolve(
	ctx context.Context,
	src oras.ReadOnlyTarget,
	ref string,
	opts ResolveOptions,
) (Path, error) {
	root, err := src.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}

	return ResolveGraph(ctx, src, root, opts)
}

// ResolveGraph resolves a descriptor to an image manifest descriptor respecting the given
// ResolveOptions.
// It does so by iterating the graph rooted at the given descriptor, traversing indexes & sub-indexes
// as necessary.
func ResolveGraph(
	ctx context.Context,
	src content.ReadOnlyStorage,
	root ocispec.Descriptor,
	opts ResolveOptions,
) (Path, error) {
	iterateOpts := IterateOptions{
		MatchArtifactType: opts.MatchArtifactType,
		MatchPlatform:     opts.MatchPlatform,
		ConfigDecoder:     opts.ConfigDecoder,
	}

	var matching []Path
	for p, err := range Iterate(ctx, src, root, iterateOpts) {
		if err != nil {
			if opts.OnIterateError != nil {
				if err := opts.OnIterateError(p, err); err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}

		if opts.CompareDescriptors == nil {
			return p, nil
		}
		matching = append(matching, p)
	}
	if len(matching) == 0 {
		return nil, errdef.ErrNotFound
	}
	slices.SortStableFunc(matching, func(a, b Path) int {
		return opts.CompareDescriptors(a.Base(), b.Base())
	})
	return matching[0], nil
}

// IterateOptions are options for the Iterate function.
type IterateOptions struct {
	// MatchArtifactType only matches image manifests with artifact types that pass this function.
	// Other images are skipped.
	MatchArtifactType func(string) bool
	// MatchPlatform only matches image manifests with platforms that pass this function.
	// If a nested index specifies a platform, it must match this, otherwise it's not visited.
	MatchPlatform func(*ocispec.Platform) bool
	// ConfigDecoder specifies how an image manifest's config descriptor is converted into
	// config.Config.
	ConfigDecoder config.Decoder
}

// Iterate iterates all image manifests by walking the graph rooted at the given descriptor respecting
// the given IterateOptions.
func Iterate(
	ctx context.Context,
	src content.ReadOnlyStorage,
	root ocispec.Descriptor,
	opts IterateOptions,
) iter.Seq2[Path, error] {
	return func(yield func(path Path, err error) bool) {
		// We can safely ignore the error here as it must be handled within the walk function.
		_ = Walk(ctx, src, root, func(path Path, err error) error {
			if err != nil {
				if !yield(path, err) {
					return ErrSkipAll
				}
				return nil
			}

			desc := path.Base()
			switch desc.MediaType {
			case ocispec.MediaTypeImageIndex:
				if opts.MatchPlatform != nil && desc.Platform != nil && !opts.MatchPlatform(desc.Platform) {
					return ErrSkipNode
				}
				return nil
			case ocispec.MediaTypeImageManifest:
				if opts.MatchArtifactType != nil && !opts.MatchArtifactType(desc.ArtifactType) {
					return nil
				}
				if opts.MatchPlatform != nil && !opts.MatchPlatform(desc.Platform) {
					return nil
				}
				if !yield(path, nil) {
					return ErrSkipAll
				}
				return nil
			default:
				// Unreachable by walk's contract: fn is called with a nil error only
				// for image indexes and manifests (unsupported types arrive via the
				// error path above). Kept as a safety net in case walk ever delivers
				// other node types successfully.
				err := fmt.Errorf("%w: not an index or image (%s)", errdef.ErrUnsupported, desc.MediaType)
				if !yield(path, err) {
					return ErrSkipAll
				}
				return nil
			}
		}, WalkOptions{
			ConfigDecoder: opts.ConfigDecoder,
		})
	}
}

// PlatformsOptions are options for the Platforms function.
type PlatformsOptions struct {
	// MatchArtifactType only matches image manifests with artifact types that pass this function.
	// Other images are skipped.
	MatchArtifactType func(string) bool
	// MatchPlatform only matches image manifests with platforms that pass this function.
	// If a nested index specifies a platform, it must match this, otherwise it's not visited.
	MatchPlatform func(*ocispec.Platform) bool
	// ConfigDecoder specifies how an image manifest's config descriptor is converted into
	// config.Config.
	ConfigDecoder config.Decoder
	// OnIterateError specifies what to do when encountering an error during image iteration.
	OnIterateError func(p Path, err error) error
}

// Platforms iterates all platforms of all image manifests rooted at the graph specified
// by the given descriptor respecting the given PlatformOptions.
func Platforms(
	ctx context.Context,
	src content.ReadOnlyStorage,
	desc ocispec.Descriptor,
	opts PlatformsOptions,
) iter.Seq2[ocispec.Platform, error] {
	return func(yield func(ocispec.Platform, error) bool) {
		for p, err := range Iterate(ctx, src, desc, IterateOptions{
			MatchArtifactType: opts.MatchArtifactType,
			MatchPlatform: func(p *ocispec.Platform) bool {
				if p == nil {
					return false
				}
				if opts.MatchPlatform != nil && !opts.MatchPlatform(p) {
					return false
				}
				return true
			},
			ConfigDecoder: opts.ConfigDecoder,
		}) {
			if err != nil {
				if opts.OnIterateError != nil {
					if err := opts.OnIterateError(p, err); err != nil {
						if !yield(ocispec.Platform{}, err) {
							return
						}
					}
					continue
				}
				if !yield(ocispec.Platform{}, err) {
					return
				}
				continue
			}
			if p.Base().Platform == nil {
				continue
			}
			if !yield(*p.Base().Platform, nil) {
				return
			}
		}
	}
}

var (
	// ErrSkipNode can be returned from WalkFunc to skip the current node only.
	ErrSkipNode = errors.New("skip node")
	// ErrSkipAll can be returned from WalkFunc to exit the walk early and skip all remaining nodes.
	ErrSkipAll = errors.New("skip all")
)

// WalkFunc is a function that is called on each step of Walk.
// A non-nil error indicates there was some error with the base descriptor of the path.
type WalkFunc func(path Path, err error) error

// WalkOptions are options for the Walk function.
type WalkOptions struct {
	ConfigDecoder config.Decoder
}

// Walk walks the graph located at the given descriptor.
// Walk traverses the graph in a depth-first manner.
// The given WalkFunc is called with all nodes of the graph.
// If err in the WalkFunc is nil, it is guaranteed that the node exists and is well-formed.
func Walk(
	ctx context.Context,
	src content.ReadOnlyStorage,
	desc ocispec.Descriptor,
	fn WalkFunc,
	opts WalkOptions,
) error {
	if opts.ConfigDecoder == nil {
		opts.ConfigDecoder = config.ImageDecoder
	}
	err := walk(ctx, src, fn, Path{desc}, opts)
	if errors.Is(err, ErrSkipAll) || errors.Is(err, ErrSkipNode) {
		return nil
	}
	return err
}

// joinPath returns a new Path with the descriptor appended.
// Always return a complete copy to allow in-place mutation (like in walk).
func joinPath(path Path, desc ocispec.Descriptor) Path {
	res := make(Path, len(path)+1)
	copy(res, path)
	res[len(res)-1] = desc
	return res
}

func walk(
	ctx context.Context,
	src content.ReadOnlyStorage,
	fn WalkFunc,
	path Path,
	opts WalkOptions,
) error {
	switch path.Base().MediaType {
	case ocispec.MediaTypeImageIndex:
		index, err := Index(ctx, src, path.Base())
		err1 := fn(path, err)
		if err != nil || err1 != nil {
			return err1
		}

		for _, child := range index.Manifests {
			if child.MediaType != ocispec.MediaTypeImageIndex && child.MediaType != ocispec.MediaTypeImageManifest {
				continue
			}
			if err := walk(ctx, src, fn, joinPath(path, child), opts); err != nil && !errors.Is(err, ErrSkipNode) {
				return err
			}
		}
		return nil
	case ocispec.MediaTypeImageManifest:
		manifest, cfg, err := ImageManifest(ctx, src, opts.ConfigDecoder, path.Base())
		if err != nil {
			return fn(path, err)
		}

		ref := &path[len(path)-1]
		ref.ArtifactType = manifest.ArtifactType
		ref.Platform = cfg.GetPlatform()

		return fn(path, nil)
	default:
		return fn(path, fmt.Errorf("%w: not an index or image (%s)", errdef.ErrUnsupported, path.Base().MediaType))
	}
}

// ImageManifest returns the image manifest and config located at the given descriptor.
func ImageManifest(ctx context.Context, fetcher content.Fetcher, cfgDecoder config.Decoder, desc ocispec.Descriptor) (*ocispec.Manifest, config.Config, error) {
	if desc.MediaType != ocispec.MediaTypeImageManifest {
		return nil, nil, fmt.Errorf("%w: not a manifest (%s)", errdef.ErrUnsupported, desc.MediaType)
	}
	data, err := content.FetchAll(ctx, fetcher, desc)
	if err != nil {
		return nil, nil, err
	}
	manifest := &ocispec.Manifest{}
	if err := json.Unmarshal(data, manifest); err != nil {
		return nil, nil, err
	}

	cfg, err := cfgDecoder.Decode(ctx, fetcher, manifest.Config)
	if err != nil {
		return nil, nil, err
	}
	return manifest, cfg, nil
}

// Index returns the index manifest located at the given descriptor.
func Index(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (*ocispec.Index, error) {
	if desc.MediaType != ocispec.MediaTypeImageIndex {
		return nil, fmt.Errorf("%w: not an index (%s)", errdef.ErrUnsupported, desc.MediaType)
	}
	data, err := content.FetchAll(ctx, fetcher, desc)
	if err != nil {
		return nil, err
	}
	index := &ocispec.Index{}
	if err := json.Unmarshal(data, index); err != nil {
		return nil, err
	}
	return index, nil
}
