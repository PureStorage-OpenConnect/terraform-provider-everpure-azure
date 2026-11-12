// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// platformEnv returns the process environment for a subprocess analyzing the
// given GOOS/GOARCH, overriding whatever the host's own GOOS/GOARCH are.
func platformEnv(goos, goarch string) []string {
	return append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch)
}

// runGoLicenses runs the go-licenses binary at bin for the given platform,
// with its own stdout/stderr passed straight through to the terminal — this
// tool never needs go-licenses' stdout, and its warnings (e.g. about
// non-Go assembly files it can't inspect) should stay visible.
func runGoLicenses(bin, goos, goarch string, args ...string) error {
	cmd := exec.Command(bin, args...)
	cmd.Env = platformEnv(goos, goarch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", bin, args, err)
	}
	return nil
}

// runGoLicensesOutput is runGoLicenses but captures stdout (go-licenses'
// warnings still go to stderr/the terminal) for the caller to parse.
func runGoLicensesOutput(bin, goos, goarch string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = platformEnv(goos, goarch)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %v: %w", bin, args, err)
	}
	return out.Bytes(), nil
}

// goListModuleDir returns the local module cache directory for modulePath,
// the same directory `go list -m` reports it extracted the module into.
func goListModuleDir(modulePath string) (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", modulePath)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go list -m %s: %w", modulePath, err)
	}
	dir := string(bytes.TrimSpace(out))
	if dir == "" {
		return "", fmt.Errorf("go list -m %s returned an empty directory", modulePath)
	}
	return dir, nil
}

// currentModulePath returns the import path of whatever Go module contains
// the current working directory (`go list -m` with no argument) -- used
// when the caller wasn't given an explicit --module override, e.g. when
// this tool runs from a tools/licenses copied unmodified into another repo.
func currentModulePath() (string, error) {
	cmd := exec.Command("go", "list", "-m")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go list -m: %w", err)
	}
	module := string(bytes.TrimSpace(out))
	if module == "" {
		return "", errors.New("go list -m returned an empty module path -- run this from within a Go module, or pass --module explicitly")
	}
	return module, nil
}

// ensureGoLicenses returns the path to a go-licenses binary at version,
// installing it into a per-version cache directory if it isn't already
// there. Installed on demand rather than pinned in go.mod's tool block:
// go-licenses v1.6.0 (its latest tagged release) doesn't build against the
// newer otiai10/copy that other tool-block dependencies in this repo pull
// in, and `go install pkg@version` builds in its own isolated module
// context, so that conflict never arises here. The cache directory is
// keyed by version, so re-running with the same version is a no-op, and a
// version bump gets a fresh install rather than reusing a stale binary.
func ensureGoLicenses(version string) (string, error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		cacheRoot = os.TempDir()
	}
	dir := filepath.Join(cacheRoot, "go-licenses-registry", "go-licenses@"+version)
	binName := "go-licenses"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	bin := filepath.Join(dir, binName)

	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	cmd := exec.Command("go", "install", "github.com/google/go-licenses@"+version) //nolint:gosec // version is the --go-licenses-version flag this tool was told to install, not user input.
	cmd.Env = append(os.Environ(), "GOBIN="+dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("installing go-licenses@%s into %s: %w", version, dir, err)
	}
	return bin, nil
}
