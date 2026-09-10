// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package xos

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// CopyTree copies src into dst (a full kernel tree copy). It shells out to
// `cp -a` rather than walking the tree in Go so the copy can use filesystem
// copy-on-write: `--reflink=auto` on Linux (btrfs/xfs/ext4)
// turn an ~80k-file tree copy into an O(1) extent clone,
// and `cp -a` falls back to a regular (still single-pass, large-buffer)
// copy elsewhere. The per-arch File resources then mutate only the inodes
// they touch; the untouched majority stays shared at ~0 cost.
//
// The source package strips `.git` from storeDir (git/tarball never carry
// one, fetchLocal removes it), so no exclusion is needed here.
func CopyTree(src, dst string, w io.Writer) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("cp", "-a", "--reflink=auto", src+"/.", dst+"/")
	if w != nil {
		cmd.Stderr = w
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copy tree %s: %w", src, err)
	}
	return nil
}

// CopyFile copies a single file, preserving the source permission bits (unlike
// os.Create, which would silently drop e.g. the executable bit on scripts).
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}
