// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	azruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
)

// fakeStoragePoolStore is the in-memory backing store behind the
// integration tests' armpurestorageblock/fake.StoragePoolsServer. It stands
// in for ARM: Create/Update/Delete/Get all mutate or read the same map, and
// the two blocked-delete shapes are triggered by pre-marking a key in
// blockedSync/blockedLRO.
type fakeStoragePoolStore struct {
	mu             sync.Mutex
	subscriptionID string
	pools          map[string]*armpurestorageblock.StoragePool

	// Delete-blocking simulation, keyed by poolKey. Sync mimics ARM's own
	// synchronous 409 CannotDeleteResource; LRO mimics a poller that ends
	// Failed with the soft-delete-aware business error.
	blockedSync map[string]bool
	blockedLRO  map[string]bool

	// pollCreate, when true, makes BeginCreate return one non-terminal 202
	// response before its terminal one, so a test can exercise a real
	// PollUntilDone poll instead of a single round-trip create.
	pollCreate bool

	// createCallCount counts every BeginCreate call, so a test can assert a PUT was (or
	// was not) sent at all.
	createCallCount int

	// getFailStatus, when non-zero, makes every Get fail with this HTTP status.
	getFailStatus int

	// lastUpdate captures the most recent PATCH body a test observed, to
	// assert on exactly what the resource sent (e.g. platform console
	// disable payloads).
	lastUpdate *armpurestorageblock.StoragePoolUpdate

	// updateCallCount counts every BeginUpdate call this store has received, so
	// a test can assert a PATCH was (or was not) sent at all, independent of
	// what body it carried.
	updateCallCount int

	// stampExtraTagOnCreate makes BeginCreate's response carry one tag beyond
	// what was sent, simulating an environment's own Azure Policy, or the
	// resource provider layer, appending tags to every created pool.
	stampExtraTagOnCreate bool

	// failCreatePoll makes BeginCreate accept the request but fail the poll, storing
	// nothing: the shape of a create operation that fails after the PUT was accepted.
	failCreatePoll bool

	// pageSize, when > 0 and smaller than the number of matching entries, splits a
	// list pager's response across that many entries per page instead of one page
	// holding everything — so a test can prove the plural data sources' pager loops
	// actually drain every page rather than stopping after the first.
	pageSize int

	// health holds the bodies setHealth seeded for GetHealthStatus, keyed by poolKey.
	health map[string]armpurestorageblock.StoragePoolHealthInfo

	// activationCodeCalls counts every ListPlatformConsoleActivationCode call this
	// store has answered with a code, so a test can assert how many times Terraform
	// opened an ephemeral resource. An error (404, console disabled) is not counted.
	activationCodeCalls int

	// consoleUsers holds the usernames that already exist on each pool's platform
	// console, keyed by poolKey. ConfigurePlatformConsoleAuth adds to it and, like the
	// service, refuses a name already in it.
	consoleUsers map[string]map[string]bool

	// consoleAuthRequests records every ConfigurePlatformConsoleAuth request that
	// reached a pool, in order, whatever its outcome, with the body exactly as it went
	// over the wire.
	consoleAuthRequests []recordedConsoleAuthRequest

	// consoleAuthFailStatus, when non-zero, makes ConfigurePlatformConsoleAuth answer
	// that HTTP status without creating anything, to exercise error paths and the
	// retry policy.
	consoleAuthFailStatus int

	// consoleDisabled marks pools, keyed by poolKey, whose platform console is not
	// enabled; platform console calls on them get the service's 409.
	consoleDisabled map[string]bool
}

// recordedConsoleAuthRequest is one ConfigurePlatformConsoleAuth request as the fake
// received it. Body is the config marshalled the way the SDK sends it.
type recordedConsoleAuthRequest struct {
	ResourceGroup string
	Pool          string
	Body          string
}

// fakeActivationCodeUsername is the username every fake activation code carries.
const fakeActivationCodeUsername = "bootstrap-user"

// fakeActivationCodeExpiry is the expiry every fake activation code carries. It is
// deliberately given with a non-UTC offset: the service may answer in any offset, and
// the ephemeral resource has to normalise it.
var fakeActivationCodeExpiry = time.Date(2026, 10, 2, 16, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))

// fakeActivationCode is the code the fake returns on its n-th call (1-based).
func fakeActivationCode(n int) string {
	return fmt.Sprintf("fake-activation-code-%d", n)
}

func newFakeStoragePoolStore(subscriptionID string) *fakeStoragePoolStore {
	return &fakeStoragePoolStore{
		subscriptionID:  subscriptionID,
		pools:           map[string]*armpurestorageblock.StoragePool{},
		blockedSync:     map[string]bool{},
		blockedLRO:      map[string]bool{},
		health:          map[string]armpurestorageblock.StoragePoolHealthInfo{},
		consoleUsers:    map[string]map[string]bool{},
		consoleDisabled: map[string]bool{},
	}
}

// disablePlatformConsole makes the pool answer platform console calls as one whose
// platform console is not enabled.
func (s *fakeStoragePoolStore) disablePlatformConsole(resourceGroupName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consoleDisabled[poolKey(resourceGroupName, name)] = true
}

// fakeConsolePoolUUID stands in for the backend's internal pool ID, which its error
// messages name instead of the ARM resource name.
const fakeConsolePoolUUID = "e8ddb729-305e-4ea3-9be5-9b70d85bd442"

// fakeConsoleDisabledMessage is the backend's text for a pool without the platform
// console, as the live service sends it.
const fakeConsoleDisabledMessage = "Platform Console is not enabled for StoragePool " + fakeConsolePoolUUID +
	". Enable Platform Console before retrying this operation."

// platformConsoleConflict is a 409 shaped the way the live service sends one for a
// platform console call: error.code is "409", not the backend's own code, and only the
// message (the backend's text behind ARM's prefix) says what the conflict is. The
// fake's ErrorResponder.SetResponseError cannot carry a message, so the response error
// is built here and handed over with SetError. operation is the action's URL segment,
// description how ARM's prefix names it.
func (s *fakeStoragePoolStore) platformConsoleConflict(resourceGroupName, name, operation, description, backendMessage string) error {
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/%s",
		s.subscriptionID, resourceGroupName, name, operation), nil)
	if err != nil {
		return err
	}
	body := fmt.Sprintf(`{"error":{"code":"409","message":%q}}`,
		"Partner resource "+description+" for storage pool id "+fakeConsolePoolUUID+" failed with error "+backendMessage)
	return azruntime.NewResponseError(&http.Response{
		StatusCode: http.StatusConflict,
		Status:     "409 Conflict",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	})
}

// seedConsoleUser makes username exist on the pool's platform console already.
func (s *fakeStoragePoolStore) seedConsoleUser(resourceGroupName, name, username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := poolKey(resourceGroupName, name)
	if s.consoleUsers[key] == nil {
		s.consoleUsers[key] = map[string]bool{}
	}
	s.consoleUsers[key][username] = true
}

// consoleAuthRequestsSnapshot returns a copy of the recorded
// ConfigurePlatformConsoleAuth requests.
func (s *fakeStoragePoolStore) consoleAuthRequestsSnapshot() []recordedConsoleAuthRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedConsoleAuthRequest(nil), s.consoleAuthRequests...)
}

func (s *fakeStoragePoolStore) setConsoleAuthFailStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consoleAuthFailStatus = status
}

func poolKey(resourceGroupName, name string) string {
	return resourceGroupName + "/" + name
}

// findPool looks up a pool by exact key first, then falls back to a case-insensitive
// scan, mirroring ARM's own case-insensitive storage pool name lookup (confirmed live —
// unlike volumes, whose names do not fold). The exact-match attempt keeps every existing
// create/update/delete call site's behavior identical to before this fallback existed.
func (s *fakeStoragePoolStore) findPool(resourceGroupName, name string) (*armpurestorageblock.StoragePool, bool) {
	if pool, ok := s.pools[poolKey(resourceGroupName, name)]; ok {
		return pool, true
	}
	want := poolKey(resourceGroupName, name)
	for key, pool := range s.pools {
		if strings.EqualFold(key, want) {
			return pool, true
		}
	}
	return nil, false
}

func (s *fakeStoragePoolStore) fakePoolID(resourceGroupName, name string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s",
		s.subscriptionID, resourceGroupName, name,
	)
}

func (s *fakeStoragePoolStore) removePool(resourceGroupName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pools, poolKey(resourceGroupName, name))
}

func (s *fakeStoragePoolStore) mutatePool(resourceGroupName, name string, mutate func(*armpurestorageblock.StoragePool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pool, ok := s.pools[poolKey(resourceGroupName, name)]; ok {
		mutate(pool)
	}
}

// setHealth seeds the body GetHealthStatus returns for a pool. An existing pool with
// nothing seeded gets an idle, healthy pool: no usage, nonzero provisioned limits and
// an empty alert list.
func (s *fakeStoragePoolStore) setHealth(resourceGroupName, name string, info armpurestorageblock.StoragePoolHealthInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health[poolKey(resourceGroupName, name)] = info
}

// activationCodeCallCount returns how many activation codes the fake has issued.
func (s *fakeStoragePoolStore) activationCodeCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activationCodeCalls
}

// newStoragePoolsServer builds the fake.StoragePoolsServer wired to this store.
func (s *fakeStoragePoolStore) newStoragePoolsServer() psbfake.StoragePoolsServer {
	return psbfake.StoragePoolsServer{
		BeginCreate: func(
			_ context.Context, resourceGroupName, name string, resource armpurestorageblock.StoragePool,
			_ *armpurestorageblock.StoragePoolsClientBeginCreateOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.StoragePoolsClientCreateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.createCallCount++

			if s.failCreatePoll {
				var responder azcorefake.PollerResponder[armpurestorageblock.StoragePoolsClientCreateResponse]
				responder.AddNonTerminalResponse(201, nil)
				responder.SetTerminalError(400, "ResourceProvisioningFailed")
				return responder, azcorefake.ErrorResponder{}
			}

			pool := resource
			pool.ID = to.Ptr(s.fakePoolID(resourceGroupName, name))
			pool.Name = to.Ptr(name)
			pool.Properties.ProvisioningState = to.Ptr(armpurestorageblock.ProvisioningStateSucceeded)
			pool.Properties.ProvisionedIops = to.Ptr(*pool.Properties.ProvisionedBandwidthMbPerSec * 10)
			pool.Properties.DataRetentionPeriod = to.Ptr(int64(3600))
			pool.Properties.StoragePoolInternalID = to.Ptr("internal-" + name)
			populateSystemFields(&pool)
			if s.stampExtraTagOnCreate {
				if pool.Tags == nil {
					pool.Tags = map[string]*string{}
				}
				pool.Tags["stamped-by-rp"] = to.Ptr("true")
			}
			s.pools[poolKey(resourceGroupName, name)] = &pool

			var responder azcorefake.PollerResponder[armpurestorageblock.StoragePoolsClientCreateResponse]
			if s.pollCreate {
				// The generated client's createOperation only accepts 200 or
				// 201 as the initial response (not 202, unlike delete's
				// 202/204); 201 is what signals "still provisioning, keep
				// polling" here.
				responder.AddNonTerminalResponse(201, nil)
			}
			responder.SetTerminalResponse(200, armpurestorageblock.StoragePoolsClientCreateResponse{StoragePool: pool}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		Get: func(
			_ context.Context, resourceGroupName, name string, _ *armpurestorageblock.StoragePoolsClientGetOptions,
		) (azcorefake.Responder[armpurestorageblock.StoragePoolsClientGetResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			if s.getFailStatus != 0 {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(s.getFailStatus, "InternalServerError")
				return azcorefake.Responder[armpurestorageblock.StoragePoolsClientGetResponse]{}, errResp
			}

			pool, ok := s.findPool(resourceGroupName, name)
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return azcorefake.Responder[armpurestorageblock.StoragePoolsClientGetResponse]{}, errResp
			}

			var responder azcorefake.Responder[armpurestorageblock.StoragePoolsClientGetResponse]
			responder.SetResponse(200, armpurestorageblock.StoragePoolsClientGetResponse{StoragePool: *pool}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		GetHealthStatus: func(
			_ context.Context, resourceGroupName, name string, _ *armpurestorageblock.StoragePoolsClientGetHealthStatusOptions,
		) (azcorefake.Responder[armpurestorageblock.StoragePoolsClientGetHealthStatusResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			if _, ok := s.findPool(resourceGroupName, name); !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return azcorefake.Responder[armpurestorageblock.StoragePoolsClientGetHealthStatusResponse]{}, errResp
			}

			info, ok := s.health[poolKey(resourceGroupName, name)]
			if !ok {
				info = armpurestorageblock.StoragePoolHealthInfo{
					Alerts: []*armpurestorageblock.Alert{},
					Health: &armpurestorageblock.HealthDetails{
						BandwidthUsage:         &armpurestorageblock.BandwidthUsage{Current: to.Ptr(int64(0)), Max: to.Ptr(int64(2147483648)), Provisioned: to.Ptr(int64(1073741824))},
						DataReductionRatio:     to.Ptr(float64(1)),
						EstimatedMaxCapacity:   to.Ptr(int64(0)),
						IopsUsage:              &armpurestorageblock.IopsUsage{Current: to.Ptr(int64(0)), Max: to.Ptr(int64(150000)), Provisioned: to.Ptr(int64(67948))},
						Space:                  &armpurestorageblock.Space{Shared: to.Ptr(int64(0)), Snapshots: to.Ptr(int64(0)), TotalUsed: to.Ptr(int64(0)), Unique: to.Ptr(int64(0))},
						UsedCapacityPercentage: to.Ptr(float64(0)),
					},
				}
			}

			var responder azcorefake.Responder[armpurestorageblock.StoragePoolsClientGetHealthStatusResponse]
			responder.SetResponse(200, armpurestorageblock.StoragePoolsClientGetHealthStatusResponse{StoragePoolHealthInfo: info}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		ListPlatformConsoleActivationCode: func(
			_ context.Context, resourceGroupName, name string, _ *armpurestorageblock.StoragePoolsClientListPlatformConsoleActivationCodeOptions,
		) (azcorefake.Responder[armpurestorageblock.StoragePoolsClientListPlatformConsoleActivationCodeResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			if _, ok := s.findPool(resourceGroupName, name); !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return azcorefake.Responder[armpurestorageblock.StoragePoolsClientListPlatformConsoleActivationCodeResponse]{}, errResp
			}

			if s.consoleDisabled[poolKey(resourceGroupName, name)] {
				var errResp azcorefake.ErrorResponder
				errResp.SetError(s.platformConsoleConflict(resourceGroupName, name,
					"listPlatformConsoleActivationCode", "list platform console activation code", fakeConsoleDisabledMessage))
				return azcorefake.Responder[armpurestorageblock.StoragePoolsClientListPlatformConsoleActivationCodeResponse]{}, errResp
			}

			s.activationCodeCalls++
			var responder azcorefake.Responder[armpurestorageblock.StoragePoolsClientListPlatformConsoleActivationCodeResponse]
			responder.SetResponse(200, armpurestorageblock.StoragePoolsClientListPlatformConsoleActivationCodeResponse{
				PlatformConsoleActivationCode: armpurestorageblock.PlatformConsoleActivationCode{
					ActivationCode: to.Ptr(fakeActivationCode(s.activationCodeCalls)),
					ExpiresAt:      to.Ptr(fakeActivationCodeExpiry),
					Username:       to.Ptr(fakeActivationCodeUsername),
				},
			}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		ConfigurePlatformConsoleAuth: func(
			_ context.Context, resourceGroupName, name string, config armpurestorageblock.PlatformConsoleAuthConfigClassification,
			_ *armpurestorageblock.StoragePoolsClientConfigurePlatformConsoleAuthOptions,
		) (azcorefake.Responder[armpurestorageblock.StoragePoolsClientConfigurePlatformConsoleAuthResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			var none azcorefake.Responder[armpurestorageblock.StoragePoolsClientConfigurePlatformConsoleAuthResponse]

			if _, ok := s.findPool(resourceGroupName, name); !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return none, errResp
			}

			body, err := json.Marshal(config)
			if err != nil {
				var errResp azcorefake.ErrorResponder
				errResp.SetError(err)
				return none, errResp
			}
			s.consoleAuthRequests = append(s.consoleAuthRequests, recordedConsoleAuthRequest{
				ResourceGroup: resourceGroupName, Pool: name, Body: string(body),
			})

			if s.consoleAuthFailStatus != 0 {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(s.consoleAuthFailStatus, "InjectedFailure")
				return none, errResp
			}

			key := poolKey(resourceGroupName, name)
			if s.consoleDisabled[key] {
				var errResp azcorefake.ErrorResponder
				errResp.SetError(s.platformConsoleConflict(resourceGroupName, name,
					"configurePlatformConsoleAuth", "configure platform console auth", fakeConsoleDisabledMessage))
				return none, errResp
			}

			ssh, ok := config.(*armpurestorageblock.SSHPlatformConsoleAuthConfig)
			if !ok || ssh.Username == nil || ssh.Role == nil || ssh.PublicKey == nil {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(400, "InvalidRequest")
				return none, errResp
			}

			// Create-only, as the service is: a name already on the console is refused
			// whoever made it, and nothing about the existing user changes.
			if s.consoleUsers[key][*ssh.Username] {
				var errResp azcorefake.ErrorResponder
				errResp.SetError(s.platformConsoleConflict(resourceGroupName, name,
					"configurePlatformConsoleAuth", "configure platform console auth",
					fmt.Sprintf("user %q already exists", *ssh.Username)))
				return none, errResp
			}
			if s.consoleUsers[key] == nil {
				s.consoleUsers[key] = map[string]bool{}
			}
			s.consoleUsers[key][*ssh.Username] = true

			var responder azcorefake.Responder[armpurestorageblock.StoragePoolsClientConfigurePlatformConsoleAuthResponse]
			responder.SetResponse(200, armpurestorageblock.StoragePoolsClientConfigurePlatformConsoleAuthResponse{
				PlatformConsoleAuthResultClassification: &armpurestorageblock.SSHPlatformConsoleAuthResult{
					AuthType: to.Ptr(armpurestorageblock.PlatformConsoleAuthTypeSSH),
					Role:     ssh.Role,
					Username: ssh.Username,
				},
			}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginUpdate: func(
			_ context.Context, resourceGroupName, name string, properties armpurestorageblock.StoragePoolUpdate,
			_ *armpurestorageblock.StoragePoolsClientBeginUpdateOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.StoragePoolsClientUpdateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			propsCopy := properties
			s.lastUpdate = &propsCopy
			s.updateCallCount++

			var responder azcorefake.PollerResponder[armpurestorageblock.StoragePoolsClientUpdateResponse]

			pool, ok := s.pools[poolKey(resourceGroupName, name)]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return responder, errResp
			}

			if properties.Tags != nil {
				pool.Tags = properties.Tags
			}
			if properties.Properties != nil {
				if properties.Properties.ProvisionedBandwidthMbPerSec != nil {
					pool.Properties.ProvisionedBandwidthMbPerSec = properties.Properties.ProvisionedBandwidthMbPerSec
					pool.Properties.ProvisionedIops = to.Ptr(*properties.Properties.ProvisionedBandwidthMbPerSec * 10)
				}
				if properties.Properties.PlatformConsoleSettings != nil {
					pool.Properties.PlatformConsoleSettings = properties.Properties.PlatformConsoleSettings
					populateSystemFields(pool)
				}
			}

			responder.SetTerminalResponse(200, armpurestorageblock.StoragePoolsClientUpdateResponse{StoragePool: *pool}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginDelete: func(
			_ context.Context, resourceGroupName, name string, _ *armpurestorageblock.StoragePoolsClientBeginDeleteOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.StoragePoolsClientDeleteResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			key := poolKey(resourceGroupName, name)
			var responder azcorefake.PollerResponder[armpurestorageblock.StoragePoolsClientDeleteResponse]

			if _, ok := s.pools[key]; !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return responder, errResp
			}

			if s.blockedSync[key] {
				// Shape 1: ARM's own synchronous 409, returned directly from
				// the initial DELETE call, no polling involved.
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(409, errCodeCannotDeleteResource)
				return responder, errResp
			}

			if s.blockedLRO[key] {
				// Shape 2: the DELETE is accepted, but the LRO ends Failed.
				responder.AddNonTerminalResponse(202, nil)
				responder.SetTerminalError(412, errCodeDeleteStoragePoolContainsData)
				return responder, azcorefake.ErrorResponder{}
			}

			delete(s.pools, key)
			// The generated client's deleteOperation only accepts 202 or 204
			// on the initial response (not 200), so the terminal response
			// for an immediate, unblocked delete must be 204.
			responder.SetTerminalResponse(204, armpurestorageblock.StoragePoolsClientDeleteResponse{}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		NewListByResourceGroupPager: func(
			resourceGroupName string, _ *armpurestorageblock.StoragePoolsClientListByResourceGroupOptions,
		) azcorefake.PagerResponder[armpurestorageblock.StoragePoolsClientListByResourceGroupResponse] {
			s.mu.Lock()
			prefix := resourceGroupName + "/"
			var entries []*armpurestorageblock.StoragePool
			for key, pool := range s.pools {
				if !strings.HasPrefix(key, prefix) {
					continue
				}
				p := *pool
				entries = append(entries, &p)
			}
			pageSize := s.pageSize
			s.mu.Unlock()

			var pager azcorefake.PagerResponder[armpurestorageblock.StoragePoolsClientListByResourceGroupResponse]
			for _, page := range paginate(entries, pageSize) {
				pager.AddPage(200, armpurestorageblock.StoragePoolsClientListByResourceGroupResponse{
					StoragePoolListResult: armpurestorageblock.StoragePoolListResult{Value: page},
				}, nil)
			}
			return pager
		},

		NewListBySubscriptionPager: func(
			_ *armpurestorageblock.StoragePoolsClientListBySubscriptionOptions,
		) azcorefake.PagerResponder[armpurestorageblock.StoragePoolsClientListBySubscriptionResponse] {
			s.mu.Lock()
			entries := make([]*armpurestorageblock.StoragePool, 0, len(s.pools))
			for _, pool := range s.pools {
				p := *pool
				entries = append(entries, &p)
			}
			pageSize := s.pageSize
			s.mu.Unlock()

			var pager azcorefake.PagerResponder[armpurestorageblock.StoragePoolsClientListBySubscriptionResponse]
			for _, page := range paginate(entries, pageSize) {
				pager.AddPage(200, armpurestorageblock.StoragePoolsClientListBySubscriptionResponse{
					StoragePoolListResult: armpurestorageblock.StoragePoolListResult{Value: page},
				}, nil)
			}
			return pager
		},
	}
}

// populateSystemFields fills in the read-only, system-populated fields real
// ARM would compute, so applyStoragePool has something to map for platform
// console's default username / subnet IPs.
func populateSystemFields(pool *armpurestorageblock.StoragePool) {
	pcs := pool.Properties.PlatformConsoleSettings
	if pcs != nil && pcs.Enabled != nil && *pcs.Enabled {
		pcs.DefaultUsername = to.Ptr("pureuser")
		for _, s := range pcs.Subnets {
			s.ManagementIPAddress = to.Ptr("10.0.1.100")
			s.ServiceBackendIPs = []*string{to.Ptr("10.0.1.101")}
		}
	}
}
