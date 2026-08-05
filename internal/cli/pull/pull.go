// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pull

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/ironcore-dev/kbake/internal/cli/clierr"
	"github.com/ironcore-dev/kbake/internal/cli/common"
	"github.com/ironcore-dev/kbake/internal/image"
	"github.com/ironcore-dev/kbake/internal/xoras/images"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
)

func Command(
	getLocal func() (*oci.Store, error),
	newRepo common.NewRepositoryFunc,
	stdout, stderr io.Writer,
) *cobra.Command {
	var (
		plainHTTP bool
		arch      string
	)

	cmd := &cobra.Command{
		Use:   "pull TAG",
		Short: "Pull an image from a registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			tag := args[0]

			local, err := getLocal()
			if err != nil {
				return clierr.Wrap(err)
			}

			return clierr.Wrap(Run(ctx, stdout, local, newRepo, tag, arch, plainHTTP))
		},
	}

	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "Use plain HTTP instead of HTTPS when pulling from a registry")
	cmd.Flags().StringVarP(&arch, "arch", "a", runtime.GOARCH, "Kernel architecture to save")

	return cmd
}

func Run(ctx context.Context, stdout io.Writer, local oras.Target, newRepo common.NewRepositoryFunc, ref, arch string, plainHTTP bool) error {
	registryRef, err := registry.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("not a valid registry reference %q: %w", ref, err)
	}

	repo, err := newRepo(registryRef)
	if err != nil {
		return fmt.Errorf("creating registry repository: %w", err)
	}
	repo.PlainHTTP = plainHTTP

	matchPlatform := images.MatchPlatform(&ocispec.Platform{OS: "linux", Architecture: arch})

	_, err = image.Pull(ctx, repo, local, ref, image.PullOptions{
		MatchPlatform:  matchPlatform,
		OnIterateError: images.IgnoreImageOrIndexNotFound,
	})
	if err != nil {
		return fmt.Errorf("pulling image %q: %w", ref, err)
	}
	return nil
}
