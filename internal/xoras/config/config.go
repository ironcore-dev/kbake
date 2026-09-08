// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"encoding/json"
	"fmt"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

// Decoder decodes the Config of a given image specified by its config descriptor.
type Decoder interface {
	Decode(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (Config, error)
}

type RecognizingDecoder interface {
	Decoder
	RecognizesDescriptor(desc ocispec.Descriptor) (ok, unknown bool, err error)
}

type decoder struct {
	decoders []Decoder
}

func NewDecoder(decoders ...Decoder) Decoder {
	return &decoder{decoders}
}

func (d *decoder) RecognizesDescriptor(desc ocispec.Descriptor) (bool, bool, error) {
	var (
		lastErr    error
		anyUnknown bool
	)
	for _, r := range d.decoders {
		switch r := r.(type) {
		case RecognizingDecoder:
			ok, unknown, err := r.RecognizesDescriptor(desc)
			if err != nil {
				lastErr = err
				continue
			}
			anyUnknown = anyUnknown || unknown
			if !ok {
				continue
			}
			return true, false, nil
		}
	}
	return false, anyUnknown, lastErr
}

func (d *decoder) Decode(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (Config, error) {
	var (
		lastErr error
		skipped []Decoder
	)

	for _, r := range d.decoders {
		switch r := r.(type) {
		case RecognizingDecoder:
			ok, unknown, err := r.RecognizesDescriptor(desc)
			if err != nil {
				lastErr = err
				continue
			}
			if unknown {
				skipped = append(skipped, r)
				continue
			}
			if !ok {
				continue
			}
			return r.Decode(ctx, fetcher, desc)
		default:
			skipped = append(skipped, r)
		}
	}

	for _, r := range skipped {
		cfg, err := r.Decode(ctx, fetcher, desc)
		if err != nil {
			lastErr = err
			continue
		}
		return cfg, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no decoder matched the provided descriptor")
	}
	return nil, lastErr
}

type Config interface {
	GetPlatform() *ocispec.Platform
}

type imageDecoder struct{}

var ImageDecoder Decoder = imageDecoder{}

func (imageDecoder) RecognizesDescriptor(desc ocispec.Descriptor) (bool, bool, error) {
	return desc.MediaType == ocispec.MediaTypeImageConfig, false, nil
}

func (imageDecoder) Decode(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (Config, error) {
	if desc.MediaType != ocispec.MediaTypeImageConfig {
		return nil, fmt.Errorf("%w: not an image config media type (%s)", errdef.ErrUnsupported, desc.MediaType)
	}

	cfgData, err := content.FetchAll(ctx, fetcher, desc)
	if err != nil {
		return nil, err
	}

	image := &ocispec.Image{}
	if err := json.Unmarshal(cfgData, image); err != nil {
		return nil, err
	}

	return &imageConfig{image}, nil
}

type imageConfig struct {
	image *ocispec.Image
}

func (c *imageConfig) GetPlatform() *ocispec.Platform {
	return &c.image.Platform
}
