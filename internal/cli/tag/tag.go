// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tag

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
		Use:   "tag SOURCE_REF TARGET_REF",
		Short: "Create a tag TARGET_REF that refers to SOURCE_REF",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			srcRef := args[0]
			dstRef := args[1]
			local, err := getLocal()
			if err != nil {
				return clierr.Wrap(err)
			}
			return clierr.Wrap(Run(ctx, local, stdout, stderr, srcRef, dstRef))
		},
	}

	return cmd
}

func Run(ctx context.Context, local *oci.Store, stdout, stderr io.Writer, srcRef, dstRef string) error {
	desc, err := local.Resolve(ctx, srcRef)
	if err != nil {
		return fmt.Errorf("resolving source reference: %w", err)
	}

	if err := local.Tag(ctx, desc, dstRef); err != nil {
		return fmt.Errorf("tagging reference: %w", err)
	}

	_, _ = fmt.Fprintf(stdout, "Created tag %q\n", dstRef)
	return nil
}
