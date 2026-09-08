// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package common

import (
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

// NewRepositoryFunc creates an authenticated registry handle for the given
// reference — the composition root (the CLI) decides how credentials are
// sourced, so this command package never learns about docker configs or
// credential helpers.
type NewRepositoryFunc func(ref registry.Reference) (*remote.Repository, error)
