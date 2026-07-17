// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ls

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/ironcore-dev/kbake/internal/cli/clierr"
	"github.com/ironcore-dev/kbake/internal/image"
	"github.com/ironcore-dev/kbake/internal/xoras/images"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/content/oci"
)

func Command(
	getLocal func() (*oci.Store, error),
	stdout, stderr io.Writer,
) *cobra.Command {
	cmd := &cobra.Command{
		Use: "ls",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			local, err := getLocal()
			if err != nil {
				return clierr.Wrap(err)
			}

			return clierr.Wrap(Run(ctx, stdout, local))
		},
	}

	return cmd
}

func Run(ctx context.Context, stdout io.Writer, local *oci.Store) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 1, ' ', 0)
	defer func() { _ = tw.Flush() }()

	_, _ = fmt.Fprintln(tw, "IMAGE\tID\tARCH")
	return local.Tags(ctx, "", func(tags []string) error {
		for _, tag := range tags {
			desc, err := local.Resolve(ctx, tag)
			if err != nil {
				return fmt.Errorf("resolving tag %q: %w", tag, err)
			}

			archSet := make(map[string]struct{})
			for p, err := range image.Platforms(ctx, local, desc, image.PlatformsOptions{
				OnIterateError: images.IgnoreImageOrIndexNotFound,
			}) {
				if err != nil {
					return err
				}
				archSet[p.Architecture] = struct{}{}
			}
			if len(archSet) == 0 {
				continue
			}

			archs := slices.AppendSeq(make([]string, 0, len(archSet)), maps.Keys(archSet))
			slices.Sort(archs)
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", tag, ShortEncoded(desc.Digest), strings.Join(archs, ","))
		}
		return nil
	})
}

func ShortEncoded(d digest.Digest) string {
	enc := d.Encoded()
	return enc[:min(12, len(enc))]
}
