// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMergeNoClobber(t *testing.T) {
	t.Run("copies new files, preserving nested directories", func(t *testing.T) {
		src := t.TempDir()
		dst := t.TempDir()

		writeFile(t, filepath.Join(src, "github.com", "foo", "LICENSE"), "foo license", 0o644)
		writeFile(t, filepath.Join(src, "golang.org", "x", "sys", "LICENSE"), "x/sys license", 0o644)

		if err := mergeNoClobber(src, dst); err != nil {
			t.Fatalf("mergeNoClobber: %v", err)
		}

		assertFileContent(t, filepath.Join(dst, "github.com", "foo", "LICENSE"), "foo license")
		assertFileContent(t, filepath.Join(dst, "golang.org", "x", "sys", "LICENSE"), "x/sys license")
	})

	t.Run("an existing destination file is not overwritten", func(t *testing.T) {
		src := t.TempDir()
		dst := t.TempDir()

		writeFile(t, filepath.Join(dst, "github.com", "foo", "LICENSE"), "original", 0o644)
		writeFile(t, filepath.Join(src, "github.com", "foo", "LICENSE"), "from a later platform", 0o644)

		if err := mergeNoClobber(src, dst); err != nil {
			t.Fatalf("mergeNoClobber: %v", err)
		}

		assertFileContent(t, filepath.Join(dst, "github.com", "foo", "LICENSE"), "original")
	})

	t.Run("preserves the source file mode", func(t *testing.T) {
		src := t.TempDir()
		dst := t.TempDir()

		writeFile(t, filepath.Join(src, "LICENSE"), "text", 0o640)

		if err := mergeNoClobber(src, dst); err != nil {
			t.Fatalf("mergeNoClobber: %v", err)
		}

		info, err := os.Stat(filepath.Join(dst, "LICENSE"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			t.Fatalf("copied file mode = %v, want %v", got, os.FileMode(0o640))
		}
	})
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s content = %q, want %q", path, got, want)
	}
}
