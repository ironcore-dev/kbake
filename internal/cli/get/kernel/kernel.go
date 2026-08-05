// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/ironcore-dev/kbake/internal/cli/clierr"
	"github.com/ironcore-dev/kbake/internal/cli/common"
	"github.com/ironcore-dev/kbake/internal/image"
	"github.com/ironcore-dev/kbake/internal/xoras/images"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

func Command(
	getLocal func() (*oci.Store, error),
	newRepo common.NewRepositoryFunc,
	stdout, stderr io.Writer,
) *cobra.Command {
	var (
		output    string
		arch      string
		plainHTTP bool
	)

	cmd := &cobra.Command{
		Use:   "kernel IMAGE",
		Short: "Get a kbake image's kernel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			ref := args[0]

			local, err := getLocal()
			if err != nil {
				return clierr.Wrap(err)
			}

			return clierr.Wrap(Run(ctx, stdout, local, newRepo, ref, arch, output, plainHTTP))
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", output, "Write to a file instead of stdout")
	cmd.Flags().StringVarP(&arch, "arch", "a", runtime.GOARCH, "Kernel architecture to save")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "Use plain HTTP instead of HTTPS when pulling from a registry")

	return cmd
}

func Run(ctx context.Context, stdout io.Writer, local oras.Target, newRepo common.NewRepositoryFunc, ref, arch, output string, plainHTTP bool) error {
	matchPlatform := images.MatchPlatform(&ocispec.Platform{OS: "linux", Architecture: arch})

	p, err := image.Resolve(ctx, local, ref, image.ResolveOptions{
		MatchPlatform:  matchPlatform,
		OnIterateError: images.IgnoreImageOrIndexNotFound,
	})
	if err != nil {
		if !errors.Is(err, errdef.ErrNotFound) {
			return fmt.Errorf("resolving image: %w", err)
		}

		registryRef, err := registry.ParseReference(ref)
		if err != nil {
			return fmt.Errorf("image %q not in local store and not a valid registry reference: %w", ref, err)
		}

		repo, err := newRepo(registryRef)
		if err != nil {
			return fmt.Errorf("creating registry repository: %w", err)
		}
		repo.PlainHTTP = plainHTTP

		p, err = image.Pull(ctx, repo, local, ref, image.PullOptions{
			MatchPlatform:  matchPlatform,
			OnIterateError: images.IgnoreImageOrIndexNotFound,
		})
		if err != nil {
			return fmt.Errorf("pull image: %w", err)
		}
	}

	manifest, _, err := image.Manifest(ctx, local, p.Base())
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}

	var kernelDesc ocispec.Descriptor
	for _, l := range manifest.Layers {
		if l.MediaType == image.MediaTypeKernel {
			kernelDesc = l
			break
		}
	}
	if kernelDesc.Digest == "" {
		return fmt.Errorf("image %s has no kernel layer", p.Base().Digest)
	}

	rc, err := local.Fetch(ctx, kernelDesc)
	if err != nil {
		return fmt.Errorf("fetch kernel: %w", err)
	}
	defer func() { _ = rc.Close() }()

	rc = struct {
		io.Reader
		io.Closer
	}{
		Reader: content.NewVerifyReader(rc, kernelDesc),
		Closer: rc,
	}

	if output != "" {
		w, err := os.Create(output)
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}

		_, err = io.Copy(w, rc)
		if err != nil {
			_ = w.Close()
			_ = os.Remove(output)
			return fmt.Errorf("pipe to output file: %w", err)
		}

		if err := w.Close(); err != nil {
			_ = os.Remove(output)
			return fmt.Errorf("close output file: %w", err)
		}
		return nil
	}

	_, err = io.Copy(stdout, rc)
	if err != nil {
		return fmt.Errorf("write to stdout: %w", err)
	}
	return nil
}
