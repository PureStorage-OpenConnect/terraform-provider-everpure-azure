// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestSSHPublicKeyProblem(t *testing.T) {
	const keyData = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

	tests := []struct {
		name    string
		value   string
		wantBad bool
	}{
		{name: "ssh-rsa", value: "ssh-rsa " + keyData},
		{name: "ssh-ed25519 with comment", value: "ssh-ed25519 " + keyData + " user@host"},
		{name: "ecdsa", value: "ecdsa-sha2-nistp256 " + keyData},
		{name: "security key ecdsa", value: "sk-ecdsa-sha2-nistp256@openssh.com " + keyData},
		{name: "security key ed25519", value: "sk-ssh-ed25519@openssh.com " + keyData + " yubikey"},
		{name: "trailing newline, as from file()", value: "ssh-ed25519 " + keyData + "\n"},
		{name: "several keys, one per line", value: "ssh-ed25519 " + keyData + "\nssh-rsa " + keyData + " second\n"},
		{name: "blank lines between keys", value: "\nssh-ed25519 " + keyData + "\n\n\nssh-rsa " + keyData + "\n\n"},
		{name: "CRLF line endings", value: "ssh-ed25519 " + keyData + "\r\nssh-rsa " + keyData + "\r\n"},
		{name: "authorized_keys line with options", value: `command="/bin/true",no-pty ssh-ed25519 ` + keyData + " restricted"},
		{name: "unknown future key type", value: "ssh-futuretype " + keyData},

		{name: "empty", value: "", wantBad: true},
		{name: "whitespace only", value: "  \n\t\n", wantBad: true},
		{name: "private key", value: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n", wantBad: true},
		{name: "PEM public key", value: "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----", wantBad: true},
		{name: "PEM block after a valid key", value: "ssh-ed25519 " + keyData + "\n-----BEGIN RSA PRIVATE KEY-----\nabc def\n", wantBad: true},
		{name: "bare base64 blob", value: keyData, wantBad: true},
		{name: "one valid key and one bare blob", value: "ssh-ed25519 " + keyData + "\n" + keyData + "\n", wantBad: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyErr := sshPublicKeyCause(tt.value)
			if (keyErr != nil) != tt.wantBad {
				t.Fatalf("sshPublicKeyCause() = %v, wantBad = %v", keyErr, tt.wantBad)
			}
			if keyErr != nil && strings.Contains(keyErr.Error(), keyData) {
				t.Errorf("the message quotes the value: %q", keyErr)
			}
		})
	}
}

func TestSSHPublicKeyValidatorSkipsUnknownAndNull(t *testing.T) {
	for name, v := range map[string]types.String{"null": types.StringNull(), "unknown": types.StringUnknown()} {
		t.Run(name, func(t *testing.T) {
			var resp validator.StringResponse
			sshPublicKeyValidator{}.ValidateString(context.Background(), validator.StringRequest{ConfigValue: v}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatal("the validator rejected a value that is not known yet")
			}
		})
	}
}

func TestBuildSSHPlatformConsoleAuthConfig(t *testing.T) {
	key := "ssh-ed25519 AAAA key-a\nssh-rsa BBBB key-b\n"
	raw, err := json.Marshal(buildSSHPlatformConsoleAuthConfig("tfadmin", "array_admin", key))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{
		"authType":  "ssh",
		"username":  "tfadmin",
		"role":      "array_admin",
		"publicKey": key,
	}
	if len(got) != len(want) {
		t.Errorf("body = %v, want exactly %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("body[%q] = %v, want %v", k, got[k], w)
		}
	}
}

func TestPlatformConsoleSSHUserProgressMessage(t *testing.T) {
	got := platformConsoleSSHUserProgressMessage("tfadmin", "read_only", "/subscriptions/s/pool")
	want := "Created platform console user tfadmin (role read_only) on storage pool /subscriptions/s/pool"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// failingTransport answers every request with a bare HTTP response of status and
// counts the attempts. The fake server cannot stand in here: an error it returns is
// marked non-retriable by the SDK, so it would never be retried and a "not retried"
// assertion on it would pass whatever the call's retry settings.
type failingTransport struct {
	status   int
	attempts atomic.Int32
}

func (f *failingTransport) Do(req *http.Request) (*http.Response, error) {
	f.attempts.Add(1)
	return &http.Response{
		StatusCode: f.status,
		Header:     http.Header{},
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// invokeFixture runs the action's Invoke against a fresh fake backend with a client
// whose retry delay is tiny, so a test that does see retries stays fast.
type invokeFixture struct {
	store    *fakeStoragePoolStore
	action   *CreatePlatformConsoleSSHUserAction
	progress []string
}

func newInvokeFixture(t *testing.T) *invokeFixture {
	t.Helper()

	store := newFakeStoragePoolStore(testSubscriptionID)
	seedTestStoragePool(store)
	srv := store.newStoragePoolsServer()
	return newInvokeFixtureWithTransport(store, psbfake.NewStoragePoolsServerTransport(&srv))
}

func newInvokeFixtureWithTransport(store *fakeStoragePoolStore, transport policy.Transporter) *invokeFixture {
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: transport,
		Retry:     policy.RetryOptions{RetryDelay: time.Millisecond, MaxRetryDelay: time.Millisecond},
	}}

	a := &CreatePlatformConsoleSSHUserAction{}
	a.clients = &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: testSubscriptionID,
		correlationID:  "test-correlation-id",
	}
	return &invokeFixture{store: store, action: a}
}

func (f *invokeFixture) invoke(t *testing.T, poolID, username, role, key string) action.InvokeResponse {
	t.Helper()
	ctx := context.Background()

	var schemaResp action.SchemaResponse
	f.action.Schema(ctx, action.SchemaRequest{}, &schemaResp)
	objType, ok := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatalf("action schema is not an object type")
	}
	vals := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(typ, nil)
	}
	vals["storage_pool_id"] = tftypes.NewValue(tftypes.String, poolID)
	vals["username"] = tftypes.NewValue(tftypes.String, username)
	vals["role"] = tftypes.NewValue(tftypes.String, role)
	vals["public_key"] = tftypes.NewValue(tftypes.String, key)

	resp := action.InvokeResponse{SendProgress: func(e action.InvokeProgressEvent) {
		f.progress = append(f.progress, e.Message)
	}}
	f.action.Invoke(ctx, action.InvokeRequest{
		Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: tftypes.NewValue(objType, vals)},
	}, &resp)
	return resp
}

func TestCreatePlatformConsoleSSHUserInvoke(t *testing.T) {
	const key = "ssh-ed25519 AAAA key-a\n"
	poolID := testStoragePoolIDForVG()

	t.Run("success sends one request with the ssh body and reports progress", func(t *testing.T) {
		f := newInvokeFixture(t)
		resp := f.invoke(t, poolID, "tfadmin", "array_admin", key)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}

		reqs := f.store.consoleAuthRequestsSnapshot()
		if len(reqs) != 1 {
			t.Fatalf("got %d requests, want 1", len(reqs))
		}
		if reqs[0].ResourceGroup != testResourceGroup || reqs[0].Pool != testStoragePoolName {
			t.Errorf("request went to %s/%s", reqs[0].ResourceGroup, reqs[0].Pool)
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if body["authType"] != "ssh" || body["username"] != "tfadmin" || body["role"] != "array_admin" || body["publicKey"] != key {
			t.Errorf("body = %v", body)
		}

		wantMsg := "Created platform console user tfadmin (role array_admin) on storage pool " + poolID
		if len(f.progress) != 1 || f.progress[0] != wantMsg {
			t.Errorf("progress = %q, want [%q]", f.progress, wantMsg)
		}
	})

	t.Run("an existing user is a 409 that is explained and carries the correlation id", func(t *testing.T) {
		f := newInvokeFixture(t)
		f.store.seedConsoleUser(testResourceGroup, testStoragePoolName, "tfadmin")
		resp := f.invoke(t, poolID, "tfadmin", "read_only", key)

		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		d := resp.Diagnostics.Errors()[0]
		firstParagraph, _, _ := strings.Cut(newPlatformConsoleUserExistsError("tfadmin", apiFailure{}).Error(), "\n\n")
		for _, want := range []string{
			firstParagraph,
			"tfadmin",
			"x-ms-correlation-request-id: test-correlation-id",
		} {
			if !strings.Contains(d.Detail(), want) {
				t.Errorf("detail %q lacks %q", d.Detail(), want)
			}
		}
		if len(f.progress) != 0 {
			t.Errorf("progress sent on failure: %q", f.progress)
		}
	})

	t.Run("a pool without the platform console says so, not that the user exists", func(t *testing.T) {
		f := newInvokeFixture(t)
		f.store.disablePlatformConsole(testResourceGroup, testStoragePoolName)
		resp := f.invoke(t, poolID, "tfadmin", "read_only", key)

		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		d := resp.Diagnostics.Errors()[0]
		op := operation{action: actionCreate, kind: kindPlatformConsoleUser, name: "tfadmin", in: poolID}
		if want := diag.WithPath(path.Root("storage_pool_id"), op.fail(newPlatformConsoleDisabledError(poolID, apiFailure{}))); d.Summary() != want.Summary() {
			t.Errorf("summary = %q", d.Summary())
		}
		for _, want := range []string{"`platform_console` block", "x-ms-correlation-request-id: test-correlation-id"} {
			if !strings.Contains(d.Detail(), want) {
				t.Errorf("detail %q lacks %q", d.Detail(), want)
			}
		}
		if strings.Contains(d.Detail(), "only creates new users") {
			t.Errorf("detail %q carries the existing-user explanation", d.Detail())
		}
	})

	t.Run("an unrecognised 409 is passed through as returned", func(t *testing.T) {
		f := newInvokeFixture(t)
		f.store.setConsoleAuthFailStatus(http.StatusConflict)
		resp := f.invoke(t, poolID, "tfadmin", "read_only", key)

		op := operation{action: actionCreate, kind: kindPlatformConsoleUser, name: "tfadmin", in: poolID}
		if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != op.fail(newApiCallFailedError(op, apiFailure{})).Summary() {
			t.Fatalf("diagnostics = %v", resp.Diagnostics)
		}
	})

	t.Run("a missing pool is the not-found diagnostic", func(t *testing.T) {
		f := newInvokeFixture(t)
		other := strings.Replace(poolID, testStoragePoolName, "no-such-pool", 1)
		resp := f.invoke(t, other, "tfadmin", "read_only", key)

		op := operation{action: actionCreate, kind: kindPlatformConsoleUser, name: "tfadmin", in: other}
		want := op.fail(newNotFoundError(kindStoragePool, other, nil, apiFailure{}))
		if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != want.Summary() {
			t.Fatalf("diagnostics = %v", resp.Diagnostics)
		}
		if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), other) {
			t.Errorf("detail %q does not name the missing pool %q", resp.Diagnostics.Errors()[0].Detail(), other)
		}
	})

	t.Run("a pool in another subscription is refused before any request", func(t *testing.T) {
		f := newInvokeFixture(t)
		other := strings.Replace(poolID, testSubscriptionID, "22222222-2222-2222-2222-222222222222", 1)
		resp := f.invoke(t, other, "tfadmin", "read_only", key)

		if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "provider alias") {
			t.Fatalf("diagnostics = %v", resp.Diagnostics)
		}
		if n := len(f.store.consoleAuthRequestsSnapshot()); n != 0 {
			t.Errorf("%d requests sent", n)
		}
	})

	t.Run("a bad key is refused at invoke before any request", func(t *testing.T) {
		// A key unknown at plan time skips the validator, so Invoke must check it itself.
		f := newInvokeFixture(t)
		resp := f.invoke(t, poolID, "tfadmin", "read_only", "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n")

		if want := diag.WithPath(path.Root("public_key"), invalidAttributeValueDiagnostic(newSshPublicKeyHasPEMBlockError())); !resp.Diagnostics.Contains(want) {
			t.Fatalf("diagnostics = %v, want %v", resp.Diagnostics, want)
		}
		if n := len(f.store.consoleAuthRequestsSnapshot()); n != 0 {
			t.Errorf("%d requests sent", n)
		}
	})

	t.Run("a server error is not retried", func(t *testing.T) {
		transport := &failingTransport{status: http.StatusServiceUnavailable}
		f := newInvokeFixtureWithTransport(newFakeStoragePoolStore(testSubscriptionID), transport)
		resp := f.invoke(t, poolID, "tfadmin", "read_only", key)

		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		if n := transport.attempts.Load(); n != 1 {
			t.Errorf("got %d attempts, want exactly 1 (no retries)", n)
		}
	})

	t.Run("without the retry override the SDK does retry the same failure", func(t *testing.T) {
		// Guards the test above against passing for the wrong reason: the same transport
		// and client options must produce several attempts when the call is made plainly.
		transport := &failingTransport{status: http.StatusServiceUnavailable}
		f := newInvokeFixtureWithTransport(newFakeStoragePoolStore(testSubscriptionID), transport)
		client, err := f.action.clients.StoragePools()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_, err = client.ConfigurePlatformConsoleAuth(context.Background(), testResourceGroup, testStoragePoolName,
			buildSSHPlatformConsoleAuthConfig("tfadmin", "read_only", key), nil)
		if err == nil {
			t.Fatal("expected an error")
		}
		if n := transport.attempts.Load(); n < 2 {
			t.Errorf("got %d attempts, want the SDK's default retries (more than 1)", n)
		}
	})

	t.Run("an unconfigured provider is a diagnostic, not a panic", func(t *testing.T) {
		f := newInvokeFixture(t)
		f.action.clients = nil
		resp := f.invoke(t, poolID, "tfadmin", "read_only", key)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
	})
}
