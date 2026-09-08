// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package rm

import (
	"context"
	"fmt"
	"io"

	"github.com/ironcore-dev/kbake/internal/cli/clierr"

	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/content/oci"
)

func Command(
	getLocal func() (*oci.Store, error),
	stdout, stderr io.Writer,
) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "rm IMAGE [IMAGE...]",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			refs := args

			local, err := getLocal()
			if err != nil {
				return clierr.Wrap(err)
			}

			return clierr.Wrap(Run(ctx, stdout, stderr, local, refs))
		},
	}

	return cmd
}

func Run(ctx context.Context, stdout, stderr io.Writer, local *oci.Store, refs []string) error {
	for _, ref := range refs {
		if err := local.Untag(ctx, ref); err != nil {
			return fmt.Errorf("untag image %q: %w", ref, err)
		}
	}
	if err := local.GC(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: image GC failed\n")
	}
	return nil
}
