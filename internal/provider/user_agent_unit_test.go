// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	armruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
)

// recordingTransport records each outgoing request's header and answers with
// a fixed JSON body.
type recordingTransport struct {
	mu      sync.Mutex
	headers []http.Header
	body    string
}

func (r *recordingTransport) Do(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.headers = append(r.headers, req.Header.Clone())
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

// newTestPipeline builds a real azcore pipeline (telemetry policy included)
// with the given per-call policies and logging options, over transport.
func newTestPipeline(t *testing.T, transport policy.Transporter, perCall []policy.Policy, logging policy.LogOptions) runtime.Pipeline {
	t.Helper()
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport, Logging: logging}}
	opts.PerCallPolicies = perCall
	pl, err := armruntime.NewPipeline("armpurestorageblock", "v0.0.0", nil, runtime.PipelineOptions{}, opts)
	if err != nil {
		t.Fatalf("arm.NewPipeline: %v", err)
	}
	return pl
}

func doGet(t *testing.T, ctx context.Context, pl runtime.Pipeline) {
	t.Helper()
	req, err := runtime.NewRequest(ctx, http.MethodGet, "https://management.azure.com/subscriptions/x")
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := pl.Do(req)
	if err != nil {
		t.Fatalf("pipeline Do: %v", err)
	}
	_ = resp.Body.Close()
}

func TestUserAgentPolicy(t *testing.T) {
	tests := []struct {
		name             string
		version          string
		terraformVersion string
		appended         string
		wantPrefix       string
	}{
		{"version and terraform version", "1.2.3", "1.14.0", "", "terraform-provider-everpure-azure/1.2.3 Terraform/1.14.0 azsdk-go-armpurestorageblock/"},
		{"dev version", "0.1.1-dev.4.gabc1234", "1.14.0", "", "terraform-provider-everpure-azure/0.1.1-dev.4.gabc1234 Terraform/1.14.0 azsdk-go-armpurestorageblock/"},
		{"empty terraform version", "1.2.3", "", "", "terraform-provider-everpure-azure/1.2.3 azsdk-go-armpurestorageblock/"},
		{"appended value", "1.2.3", "1.14.0", "pipeline-42", "terraform-provider-everpure-azure/1.2.3 Terraform/1.14.0 pipeline-42 azsdk-go-armpurestorageblock/"},
		{"appended value without terraform version", "1.2.3", "", "pipeline-42", "terraform-provider-everpure-azure/1.2.3 pipeline-42 azsdk-go-armpurestorageblock/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingTransport{body: "{}"}
			pl := newTestPipeline(t, rec, []policy.Policy{newUserAgentPolicy(tt.version, tt.terraformVersion, tt.appended)}, policy.LogOptions{})
			doGet(t, context.Background(), pl)

			if len(rec.headers) != 1 {
				t.Fatalf("got %d requests, want 1", len(rec.headers))
			}
			got := rec.headers[0].Get("User-Agent")
			if !strings.HasPrefix(got, tt.wantPrefix) {
				t.Fatalf("User-Agent = %q, want prefix %q", got, tt.wantPrefix)
			}
			if strings.Contains(got, "Terraform/ ") {
				t.Errorf("User-Agent = %q, want no empty Terraform/ token", got)
			}
			// The SDK token azcore's telemetry policy wrote must survive,
			// platform info included.
			if rest := strings.TrimPrefix(got, tt.wantPrefix); !strings.Contains(rest, "(go") {
				t.Errorf("User-Agent = %q, want SDK token with platform info after the prefix", got)
			}
		})
	}
}

func TestUserAgentAppend(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"value", "pipeline-42", "pipeline-42"},
		{"surrounding whitespace", "  pipeline-42 \t\n", "pipeline-42"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TF_APPEND_USER_AGENT", tt.value)
			if got := userAgentAppend(); got != tt.want {
				t.Errorf("userAgentAppend() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSDKLoggingForwardsToTflog(t *testing.T) {
	const secretBody = `{"password":"s3cret-activation-code"}`

	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	installSDKLogging(ctx)

	rec := &recordingTransport{body: secretBody}
	pl := newTestPipeline(t, rec, []policy.Policy{&correlationRequestIDPolicy{id: "corr-log-1"}},
		policy.LogOptions{IncludeBody: false, AllowedHeaders: []string{"x-ms-correlation-request-id"}})
	doGet(t, ctx, pl)

	out := buf.String()
	if !strings.Contains(out, "GET https://management.azure.com/subscriptions/x") {
		t.Errorf("debug log has no Request line with method and URL:\n%s", out)
	}
	if !strings.Contains(out, "RESPONSE Status") {
		t.Errorf("debug log has no Response line:\n%s", out)
	}
	if !strings.Contains(out, "corr-log-1") {
		t.Errorf("debug log does not show the correlation ID:\n%s", out)
	}
	if strings.Contains(out, "s3cret-activation-code") {
		t.Errorf("debug log contains the response body:\n%s", out)
	}
	if !strings.Contains(out, `"@level":"debug"`) {
		t.Errorf("debug log has no debug-level entry:\n%s", out)
	}
}
