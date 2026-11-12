// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
)

// terraformCLI drives the real terraform binary against an in-process provider
// server (reattached the same way terraform-plugin-testing does it). It exists for
// the one thing resource.Test cannot do: run `terraform query -generate-config-out`,
// whose output is what a practitioner actually imports from.
type terraformCLI struct {
	t   *testing.T
	bin string
	dir string
	env []string
}

// newTerraformCLI serves the provider behind factories["everpure"] and returns a CLI
// working in a fresh directory. It skips the test when no pinned terraform binary is
// configured: `make test` sets TF_ACC_TERRAFORM_PATH, and a bare `go test` on a
// machine without it would otherwise pick up whatever terraform is on PATH, which may
// be too old for `terraform query`.
func newTerraformCLI(t *testing.T, factories map[string]func() (tfprotov6.ProviderServer, error)) *terraformCLI {
	t.Helper()

	bin := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if bin == "" {
		t.Skip("TF_ACC_TERRAFORM_PATH is not set; run through `make test`")
	}

	server, err := factories["everpure"]()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	configCh := make(chan *plugin.ReattachConfig)
	closeCh := make(chan struct{})
	go func() {
		_ = tf6server.Serve(
			"registry.terraform.io/hashicorp/everpure",
			func() tfprotov6.ProviderServer { return server },
			tf6server.WithDebug(ctx, configCh, closeCh),
		)
	}()
	t.Cleanup(func() {
		cancel()
		<-closeCh
	})
	cfg := <-configCh

	reattach, err := json.Marshal(map[string]any{
		"registry.terraform.io/hashicorp/everpure": map[string]any{
			"Protocol":        string(cfg.Protocol),
			"ProtocolVersion": cfg.ProtocolVersion,
			"Pid":             cfg.Pid,
			"Test":            true,
			"Addr": map[string]string{
				"Network": cfg.Addr.Network(),
				"String":  cfg.Addr.String(),
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dir := t.TempDir()
	emptyRC := filepath.Join(dir, "empty.tfrc")
	if err := os.WriteFile(emptyRC, nil, 0o600); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	return &terraformCLI{
		t:   t,
		bin: bin,
		dir: work,
		env: append(os.Environ(),
			"TF_REATTACH_PROVIDERS="+string(reattach),
			"TF_CLI_CONFIG_FILE="+emptyRC,
			"TF_IN_AUTOMATION=1",
			"CHECKPOINT_DISABLE=1",
		),
	}
}

// fork returns a CLI for a second, empty working directory served by the same
// provider, so a test can make objects through one directory's state and then find them
// from another directory that has never seen them.
func (c *terraformCLI) fork() *terraformCLI {
	c.t.Helper()
	work, err := os.MkdirTemp(c.t.TempDir(), "work")
	if err != nil {
		c.t.Fatalf("unexpected error: %v", err)
	}
	return &terraformCLI{t: c.t, bin: c.bin, dir: work, env: append([]string(nil), c.env...)}
}

// setEnv adds an environment variable to every later terraform invocation.
func (c *terraformCLI) setEnv(name, value string) {
	c.env = append(c.env, name+"="+value)
}

func (c *terraformCLI) write(name, content string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(content), 0o600); err != nil {
		c.t.Fatalf("unexpected error: %v", err)
	}
}

func (c *terraformCLI) read(name string) string {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		c.t.Fatalf("unexpected error: %v", err)
	}
	return string(b)
}

// run executes terraform with args and returns its combined output and whether it
// exited zero.
func (c *terraformCLI) run(args ...string) (string, bool) {
	c.t.Helper()
	cmd := exec.Command(c.bin, args...) //nolint:gosec // bin is the pinned terraform test binary from TF_ACC_TERRAFORM_PATH, args are literals from the tests
	cmd.Dir = c.dir
	cmd.Env = c.env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err == nil
}

// mustRun is run, failing the test on a non-zero exit.
func (c *terraformCLI) mustRun(args ...string) string {
	c.t.Helper()
	out, ok := c.run(args...)
	if !ok {
		c.t.Fatalf("terraform %v failed:\n%s", args, out)
	}
	return out
}
