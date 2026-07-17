// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package targz writes deterministic tar.gz archives. All entries are emitted
// in sorted path order with a fixed modification time, owner uid/gid 0 and
// normalized modes, so that two runs over the same content produce
// byte-identical output — a prerequisite for reproducible builds.
package targz

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func CreateFile(name, root string) error {
	f, err := os.Create(name)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	if err := Create(f, root); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return fmt.Errorf("create archive: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close file: %w", err)
	}
	return nil
}

// Create creates reproducible tar.gz archives of the given root.
func Create(w io.Writer, root string) (retErr error) {
	root = filepath.Clean(root)
	entries, err := collect(root)
	if err != nil {
		return err
	}
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)
	defer func() {
		closeErr := errors.Join(tw.Close(), gw.Close())
		if retErr == nil {
			retErr = closeErr
		}
	}()

	mt := time.Unix(0, 0).UTC()
	for _, rel := range entries {
		full := filepath.Join(root, rel)
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		hdr.ModTime = mt
		hdr.AccessTime = mt
		hdr.ChangeTime = mt
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""
		if info.Mode().IsRegular() {
			hdr.Mode = 0o644
		} else if info.IsDir() {
			hdr.Mode = 0o755
			if !strings.HasSuffix(hdr.Name, "/") {
				hdr.Name += "/"
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if err := func() error {
				data, err := os.Open(full)
				if err != nil {
					return err
				}
				defer func() { _ = data.Close() }()
				_, err = io.Copy(tw, data)
				return err
			}(); err != nil {
				return err
			}
		}
	}
	return nil
}

func collect(root string) ([]string, error) {
	var entries []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		entries = append(entries, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	if len(entries) == 0 {
		return nil, errors.New("tarball: nothing to archive")
	}
	return entries, nil
}
