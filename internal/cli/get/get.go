// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package get

import (
	"io"

	"github.com/ironcore-dev/kbake/internal/cli/common"
	"github.com/ironcore-dev/kbake/internal/cli/get/kernel"

	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/content/oci"
)

func Command(
	getLocal func() (*oci.Store, error),
	newRepo common.NewRepositoryFunc,
	stdout, stderr io.Writer,
) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Get kbake artifacts",
	}

	cmd.AddCommand(
		kernel.Command(getLocal, newRepo, stdout, stderr),
	)

	return cmd
}
