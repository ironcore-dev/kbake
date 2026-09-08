// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package push

import (
	"context"
	"fmt"
	"io"

	"github.com/ironcore-dev/kbake/internal/cli/clierr"
	"github.com/ironcore-dev/kbake/internal/cli/common"
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
	)

	cmd := &cobra.Command{
		Use:   "push TAG",
		Short: "Push an image to a registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			tag := args[0]

			local, err := getLocal()
			if err != nil {
				return clierr.Wrap(err)
			}

			return clierr.Wrap(Run(ctx, stdout, local, newRepo, tag, plainHTTP))
		},
	}

	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "Use plain HTTP instead of HTTPS when accessing a registry")

	return cmd
}

func Run(ctx context.Context, stdout io.Writer, local oras.Target, newRepo common.NewRepositoryFunc, ref string, plainHTTP bool) error {
	registryRef, err := registry.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("image %q not in local store and not a valid registry reference: %w", ref, err)
	}

	repo, err := newRepo(registryRef)
	if err != nil {
		return fmt.Errorf("creating registry repository: %w", err)
	}
	repo.PlainHTTP = plainHTTP

	if _, err := oras.Copy(ctx, local, ref, repo, registryRef.Reference, oras.CopyOptions{}); err != nil {
		return fmt.Errorf("copying image %q to repo: %w", ref, err)
	}
	return nil
}
