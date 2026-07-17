// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package image

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/ironcore-dev/kbake/internal/xoras/config"
	"github.com/ironcore-dev/kbake/internal/xoras/images"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

const (
	ArtifactType = "application/vnd.ironcore.kbake.v1+json"

	MediaTypeKernel      = "application/vnd.ironcore.kernel.v1"
	MediaTypeModulesGzip = "application/vnd.ironcore.kernel.modules.v1.tar+gzip"
	MediaTypeConfig      = "application/vnd.ironcore.kbake.config.v1+json"
)

type configDecoder struct{}

var ConfigDecoder config.Decoder = configDecoder{}

func (configDecoder) Decode(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (config.Config, error) {
	img := &Image{}
	data, err := content.FetchAll(ctx, fetcher, desc)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, img); err != nil {
		return nil, err
	}
	return img, nil
}

type Image struct {
	ocispec.Platform
	Config map[string]string `json:"config"`
}

func (i *Image) GetPlatform() *ocispec.Platform {
	return &i.Platform
}

type ResolveOptions struct {
	MatchPlatform      func(*ocispec.Platform) bool
	CompareDescriptors func(a, b ocispec.Descriptor) int
	// OnIterateError specifies what to do when encountering an error during image iteration.
	OnIterateError func(p images.Path, err error) error
}

func Resolve(ctx context.Context, src oras.ReadOnlyTarget, ref string, opts ResolveOptions) (images.Path, error) {
	return images.Resolve(ctx, src, ref, images.ResolveOptions{
		MatchArtifactType:  images.MatchArtifactType(ArtifactType),
		MatchPlatform:      opts.MatchPlatform,
		CompareDescriptors: opts.CompareDescriptors,
		ConfigDecoder:      ConfigDecoder,
		OnIterateError:     opts.OnIterateError,
	})
}

type PullOptions struct {
	MatchPlatform      func(*ocispec.Platform) bool
	CompareDescriptors func(a, b ocispec.Descriptor) int
	// OnIterateError specifies what to do when encountering an error during image iteration.
	OnIterateError   func(p images.Path, err error) error
	MaxMetadataBytes int64
	Concurrency      int
}

func Pull(ctx context.Context, src oras.ReadOnlyTarget, dst oras.Target, ref string, opts PullOptions) (images.Path, error) {
	return images.Pull(ctx, src, dst, ref, images.PullOptions{
		MatchArtifactType:  images.MatchArtifactType(ArtifactType),
		MatchPlatform:      opts.MatchPlatform,
		CompareDescriptors: opts.CompareDescriptors,
		ConfigDecoder:      ConfigDecoder,
		OnIterateError:     opts.OnIterateError,
		MaxMetadataBytes:   opts.MaxMetadataBytes,
		Concurrency:        opts.Concurrency,
	})
}

type PlatformsOptions struct {
	OnIterateError func(p images.Path, err error) error
}

func Platforms(ctx context.Context, src content.ReadOnlyStorage, desc ocispec.Descriptor, opts PlatformsOptions) iter.Seq2[ocispec.Platform, error] {
	return images.Platforms(ctx, src, desc, images.PlatformsOptions{
		MatchArtifactType: images.MatchArtifactType(ArtifactType),
		ConfigDecoder:     ConfigDecoder,
		OnIterateError:    opts.OnIterateError,
	})
}

func Manifest(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (*ocispec.Manifest, *Image, error) {
	manifest, cfg, err := images.ImageManifest(ctx, fetcher, ConfigDecoder, desc)
	if err != nil {
		return nil, nil, err
	}

	img, ok := cfg.(*Image)
	if !ok {
		return nil, nil, fmt.Errorf("expected Image, got %T", cfg)
	}

	return manifest, img, nil
}
