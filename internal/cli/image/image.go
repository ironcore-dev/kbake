// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package image

import (
	"io"

	"github.com/ironcore-dev/kbake/internal/cli/image/ls"
	"github.com/ironcore-dev/kbake/internal/cli/image/rm"

	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/content/oci"
)

func Command(getLocal func() (*oci.Store, error), stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use: "image",
	}

	cmd.AddCommand(
		ls.Command(getLocal, stdout, stderr),
		rm.Command(getLocal, stdout, stderr),
	)

	return cmd
}
