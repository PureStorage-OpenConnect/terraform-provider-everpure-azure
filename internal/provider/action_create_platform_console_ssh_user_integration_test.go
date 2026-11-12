// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

const (
	testSSHActionAddr = "action.everpure_azure_create_platform_console_ssh_user.admin"
	testSSHPublicKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl tfadmin@test"
)

// sshUserActionBlock is an action block with the given config lines.
func sshUserActionBlock(poolID, extra string) string {
	return fmt.Sprintf(`
action "everpure_azure_create_platform_console_ssh_user" "admin" {
  config {
    storage_pool_id = %[1]q
    username        = "tfadmin"
    role            = "array_admin"
    public_key      = %[2]q
%[3]s  }
}
`, poolID, testSSHPublicKey, extra)
}

// flattenTerraformOutput joins terraform's boxed, wrapped diagnostics into single-spaced
// text, so a test can look for a phrase without caring where the CLI broke the line.
func flattenTerraformOutput(out string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(out, "│", " ")), " ")
}

// flatParagraph is the first paragraph of a diagnostic detail in the form
// flattenTerraformOutput gives the CLI's output, so the two can be compared.
func flatParagraph(detail string) string {
	firstParagraph, _, _ := strings.Cut(detail, "\n\n")
	return flattenTerraformOutput(firstParagraph)
}

func checkConsoleAuthRequestCount(store *fakeStoragePoolStore, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := len(store.consoleAuthRequestsSnapshot()); got != want {
			return fmt.Errorf("the fake received %d platform console auth requests, want %d", got, want)
		}
		return nil
	}
}

func TestCreatePlatformConsoleSSHUserActionTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	// Actions need Terraform 1.14 and `caller` needs 1.16.0; terraform-plugin-testing has
	// no constant for 1.16 yet.
	versionChecks := []tfversion.TerraformVersionCheck{tfversion.RequireAbove(version.Must(version.NewVersion("1.16.0")))}

	t.Run("an after_create trigger fires exactly one request, and later applies fire none", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		// The pool is only an object for the trigger to hang on: its own settings are
		// the smallest set the resource accepts.
		config := providerBlock() + fmt.Sprintf(`
action "everpure_azure_create_platform_console_ssh_user" "admin" {
  config {
    storage_pool_id = caller.id
    username        = "tfadmin"
    role            = "read_only"
    public_key      = %[4]q
  }
}

resource "everpure_azure_storage_pool" "this" {
  name                = "pool1"
  resource_group_name = %[1]q
  location            = "eastus2"
  zone                = "1"
  reservation_id      = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  platform_console {
    gui_enabled = true
    api_enabled = true
    cli_enabled = true
  }

  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.everpure_azure_create_platform_console_ssh_user.admin]
    }
  }
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1"), testSSHPublicKey)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			TerraformVersionChecks:   versionChecks,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						checkConsoleAuthRequestCount(store, 1),
						func(*terraform.State) error {
							req := store.consoleAuthRequestsSnapshot()[0]
							var body map[string]string
							if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
								return err
							}
							if req.Pool != "pool1" || body["authType"] != "ssh" || body["username"] != "tfadmin" ||
								body["role"] != "read_only" || body["publicKey"] != testSSHPublicKey {
								return fmt.Errorf("unexpected request %+v", req)
							}
							return nil
						},
					),
				},
				{
					// An unchanged configuration is a no-op apply: the pool is not
					// created again, so the trigger does not fire again.
					Config: config,
					Check:  checkConsoleAuthRequestCount(store, 1),
				},
			},
		})
	})
}

func TestCreatePlatformConsoleSSHUserActionInvoke(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	invokeArgs := []string{"apply", "-auto-approve", "-no-color", "-invoke=" + testSSHActionAddr}

	// newCLI starts a working directory with config written and initialised.
	newCLI := func(t *testing.T, seedPool bool, config string) (*terraformCLI, *fakeStoragePoolStore) {
		t.Helper()
		factories, store := newTestProviderFactories()
		if seedPool {
			seedTestStoragePool(store)
		}
		cli := newTerraformCLI(t, factories)
		cli.write("main.tf", requiredProvidersBlock+providerBlock()+config)
		cli.mustRun("init", "-no-color")
		return cli, store
	}

	t.Run("-invoke fires one request and reports progress", func(t *testing.T) {
		cli, store := newCLI(t, true, sshUserActionBlock(testStoragePoolIDForVG(), ""))

		out := cli.mustRun(invokeArgs...)

		if n := len(store.consoleAuthRequestsSnapshot()); n != 1 {
			t.Fatalf("got %d requests, want 1:\n%s", n, out)
		}
		want := "Created platform console user tfadmin (role array_admin) on storage pool " + testStoragePoolIDForVG()
		if !strings.Contains(flattenTerraformOutput(out), want) {
			t.Errorf("output lacks the progress message %q:\n%s", want, out)
		}
	})

	t.Run("invoking again for an existing user fails with the create-only explanation", func(t *testing.T) {
		cli, store := newCLI(t, true, sshUserActionBlock(testStoragePoolIDForVG(), ""))
		cli.mustRun(invokeArgs...)

		out, ok := cli.run(invokeArgs...)
		if ok {
			t.Fatalf("the second invoke succeeded:\n%s", out)
		}
		flat := flattenTerraformOutput(out)
		for _, want := range []string{
			flatParagraph(newPlatformConsoleUserExistsError("tfadmin", apiFailure{}).Error()),
			"tfadmin",
			"x-ms-correlation-request-id",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if n := len(store.consoleAuthRequestsSnapshot()); n != 2 {
			t.Errorf("got %d requests, want 2", n)
		}
	})

	t.Run("a user created elsewhere also conflicts", func(t *testing.T) {
		cli, store := newCLI(t, true, sshUserActionBlock(testStoragePoolIDForVG(), ""))
		store.seedConsoleUser(testResourceGroup, testStoragePoolName, "tfadmin")

		out, ok := cli.run(invokeArgs...)
		if ok || !strings.Contains(flattenTerraformOutput(out), "already exists on this storage pool") {
			t.Fatalf("expected the conflict error (ok=%v):\n%s", ok, out)
		}
	})

	t.Run("a pool without the platform console is reported as such", func(t *testing.T) {
		cli, store := newCLI(t, true, sshUserActionBlock(testStoragePoolIDForVG(), ""))
		store.disablePlatformConsole(testResourceGroup, testStoragePoolName)

		out, ok := cli.run(invokeArgs...)
		flat := flattenTerraformOutput(out)
		op := operation{action: actionCreate, kind: kindPlatformConsoleUser, name: "tfadmin", in: testStoragePoolIDForVG()}
		want := diag.WithPath(path.Root("storage_pool_id"), op.fail(newPlatformConsoleDisabledError(testStoragePoolIDForVG(), apiFailure{})))
		if ok || !strings.Contains(flat, want.Summary()) || !strings.Contains(flat, flatParagraph(want.Detail())) ||
			strings.Contains(flat, "already exists on this storage pool") {
			t.Fatalf("expected the console-disabled error (ok=%v):\n%s", ok, out)
		}
	})

	t.Run("a missing pool is reported as not found", func(t *testing.T) {
		cli, _ := newCLI(t, false, sshUserActionBlock(testStoragePoolIDForVG(), ""))

		out, ok := cli.run(invokeArgs...)
		op := operation{action: actionCreate, kind: kindPlatformConsoleUser, name: "tfadmin", in: testStoragePoolIDForVG()}
		want := op.fail(newNotFoundError(kindStoragePool, testStoragePoolIDForVG(), nil, apiFailure{}))
		flat := flattenTerraformOutput(out)
		if ok || !strings.Contains(flat, want.Summary()) || !strings.Contains(flat, flatParagraph(want.Detail())) {
			t.Fatalf("expected the not-found error (ok=%v):\n%s", ok, out)
		}
	})

	t.Run("any other API error is passed through with the correlation id", func(t *testing.T) {
		cli, store := newCLI(t, true, sshUserActionBlock(testStoragePoolIDForVG(), ""))
		store.setConsoleAuthFailStatus(500)

		out, ok := cli.run(invokeArgs...)
		flat := flattenTerraformOutput(out)
		op := operation{action: actionCreate, kind: kindPlatformConsoleUser, name: "tfadmin", in: testStoragePoolIDForVG()}
		want := op.fail(newApiCallFailedError(op, apiFailure{}))
		if ok || !strings.Contains(flat, want.Summary()) || !strings.Contains(flat, flatParagraph(want.Detail())) ||
			!strings.Contains(flat, "x-ms-correlation-request-id") {
			t.Fatalf("expected an API error (ok=%v):\n%s", ok, out)
		}
	})

	t.Run("a pool in another subscription gets the alias hint and sends nothing", func(t *testing.T) {
		otherPool := strings.Replace(testStoragePoolIDForVG(), testSubscriptionID, "22222222-2222-2222-2222-222222222222", 1)
		cli, store := newCLI(t, true, sshUserActionBlock(otherPool, ""))

		out, ok := cli.run(invokeArgs...)
		want := newSubscriptionMismatchError(kindStoragePool, otherPool, "22222222-2222-2222-2222-222222222222", testSubscriptionID)
		if ok || !strings.Contains(flattenTerraformOutput(out), flatParagraph(want.Error())) {
			t.Fatalf("expected the alias hint (ok=%v):\n%s", ok, out)
		}
		if n := len(store.consoleAuthRequestsSnapshot()); n != 0 {
			t.Errorf("got %d requests, want 0", n)
		}
	})

	t.Run("configuration errors are caught before any request", func(t *testing.T) {
		tests := []struct {
			name   string
			config string
			want   string
		}{
			{
				name:   "unknown role",
				config: strings.Replace(sshUserActionBlock(testStoragePoolIDForVG(), ""), `"array_admin"`, `"superuser"`, 1),
				want:   "array_admin",
			},
			{
				name:   "private key",
				config: strings.Replace(sshUserActionBlock(testStoragePoolIDForVG(), ""), testSSHPublicKey, "-----BEGIN OPENSSH PRIVATE KEY-----\\nAAAA\\n-----END OPENSSH PRIVATE KEY-----", 1),
				want:   "PEM block",
			},
			{
				name:   "bare base64",
				config: strings.Replace(sshUserActionBlock(testStoragePoolIDForVG(), ""), testSSHPublicKey, "AAAAC3NzaC1lZDI1NTE5AAAAIOMq", 1),
				want:   "only one field",
			},
			{
				name:   "not a storage pool ID",
				config: sshUserActionBlock(testVnetID(), ""),
				want:   "storage_pool_id",
			},
			{
				name:   "malformed timeout",
				config: sshUserActionBlock(testStoragePoolIDForVG(), "    timeouts {\n      invoke = \"soon\"\n    }\n"),
				want:   "must be a string containing a sequence of decimal numbers",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				cli, store := newCLI(t, true, tt.config)

				out, ok := cli.run(invokeArgs...)
				if ok || !strings.Contains(flattenTerraformOutput(out), tt.want) {
					t.Fatalf("expected an error mentioning %q (ok=%v):\n%s", tt.want, ok, out)
				}
				if n := len(store.consoleAuthRequestsSnapshot()); n != 0 {
					t.Errorf("got %d requests, want 0", n)
				}
			})
		}
	})

	t.Run("an explicit invoke timeout is accepted", func(t *testing.T) {
		cli, store := newCLI(t, true, sshUserActionBlock(testStoragePoolIDForVG(), "    timeouts {\n      invoke = \"2m\"\n    }\n"))

		cli.mustRun(invokeArgs...)
		if n := len(store.consoleAuthRequestsSnapshot()); n != 1 {
			t.Errorf("got %d requests, want 1", n)
		}
	})

	t.Run("several keys in one value are sent unchanged", func(t *testing.T) {
		cli, store := newCLI(t, true, strings.Replace(
			sshUserActionBlock(testStoragePoolIDForVG(), ""), testSSHPublicKey, "ssh-rsa AAAAB3 first\\nssh-ed25519 AAAAC3 second\\n", 1))

		cli.mustRun(invokeArgs...)
		reqs := store.consoleAuthRequestsSnapshot()
		if len(reqs) != 1 {
			t.Fatalf("got %d requests, want 1", len(reqs))
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "ssh-rsa AAAAB3 first\nssh-ed25519 AAAAC3 second\n"; body["publicKey"] != want {
			t.Errorf("publicKey = %q, want %q", body["publicKey"], want)
		}
	})
}
