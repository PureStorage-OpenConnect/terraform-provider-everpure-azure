// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// mergeNoClobber copies every file under src into the identical relative
// path under dst, skipping any file that already exists at the destination.
// Called once per platform, this is what makes the final licenses/ tree the
// union of every platform's go-licenses save output, regardless of which
// platform happened to run first.
func mergeNoClobber(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}
		dest := filepath.Join(dst, rel)
		if _, statErr := os.Stat(dest); statErr == nil {
			return nil // Already present from an earlier platform's output.
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", filepath.Dir(dest), err)
		}
		return copyFile(path, dest)
	})
}

// copyFile copies src to dest, preserving src's file mode.
func copyFile(src, dest string) (err error) {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copying %s to %s: %w", src, dest, err)
	}
	return nil
}
