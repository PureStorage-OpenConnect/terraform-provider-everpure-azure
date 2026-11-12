// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
)

// fakeVolumeGroupStore is the in-memory backing store behind the integration tests'
// armpurestorageblock/fake.VolumeGroupsServer and fake.RecoverableVolumeGroupsServer. A
// successful delete moves an entry from groups into recoverable, renamed with a
// timestamp suffix, mirroring the real backend's soft-delete rename.
type fakeVolumeGroupStore struct {
	mu             sync.Mutex
	subscriptionID string
	groups         map[string]*armpurestorageblock.VolumeGroup              // key: rg/pool/name
	recoverable    map[string][]*armpurestorageblock.RecoverableVolumeGroup // key: rg/pool

	// lastUpdate captures the most recent PATCH body a test observed, to assert on
	// exactly what the resource sent (e.g. the protectionParameters {} reset).
	lastUpdate *armpurestorageblock.VolumeGroupUpdate

	// updateCallCount counts every BeginUpdate call this store has received, so a
	// test can assert a PATCH was (or was not) sent at all, independent of what
	// body it carried.
	updateCallCount int

	// skipTwinOnDelete makes BeginDelete not create a recoverable twin, simulating
	// auto-eradication having already won the race by the time eradicate_on_destroy
	// looks for one.
	skipTwinOnDelete bool

	// stampExtraTagOnCreate makes BeginCreate's response carry one tag beyond what
	// was sent, simulating the RP layer stamping tags (env/org/owner, verified
	// live) onto every created volume group.
	stampExtraTagOnCreate bool

	// failCreatePoll makes BeginCreate accept the request but fail the poll, storing
	// nothing: the shape of a create operation that fails after the PUT was accepted.
	failCreatePoll bool

	// failCreatePollKeepObject makes BeginCreate store the object exactly as a
	// successful create would, then fail the poll: the shape of a create that fails after
	// Azure finished creating the object. Unlike failCreatePoll, a later refresh finds it.
	failCreatePollKeepObject bool

	// createCallCount and deleteCallCount count every BeginCreate and BeginDelete call.
	createCallCount int
	deleteCallCount int

	// failGetOnce makes the next Get call fail with a transient-looking error
	// instead of returning the stored entry, then clears itself — standing in for a
	// single failed lookup (of whatever cause) without needing to model that cause.
	failGetOnce bool

	// connectionParameters backs ListConnectionParameters, keyed like groups. Absent
	// from the map (rather than an explicit empty entry) means "this volume group
	// doesn't exist" — a 404, standing in for both a volume group Terraform never
	// created and one deleted out of band (soft delete's own 404, see
	// fakeVolumeGroupStore's own doc comment on the recoverable twin rename).
	connectionParameters map[string]*armpurestorageblock.IscsiConnectionParameters

	// statuses backs GetStatus, keyed like groups; statusErrors makes GetStatus fail for
	// that volume group instead. A volume group with neither reads count 0 and zero space.
	statuses     map[string]armpurestorageblock.VolumeGroupStatus
	statusErrors map[string]fakeStatusError

	// missingStoragePools marks a pool (keyed by vgPoolKey) whose volume-group and
	// recoverable-volume-group list pagers must 404 instead of returning an empty
	// page, standing in for a list against a storage pool that doesn't exist — for the
	// plural data sources' "missing parent" test, since this store otherwise has no
	// notion of pool existence at all (only of the volume groups on one).
	missingStoragePools map[string]bool

	// pageSize, when > 0 and smaller than the number of matching entries, splits a
	// list pager's response across that many entries per page instead of one page
	// holding everything — see paginate in fake_pager_test.go.
	pageSize int
}

func newFakeVolumeGroupStore(subscriptionID string) *fakeVolumeGroupStore {
	return &fakeVolumeGroupStore{
		subscriptionID:       subscriptionID,
		groups:               map[string]*armpurestorageblock.VolumeGroup{},
		recoverable:          map[string][]*armpurestorageblock.RecoverableVolumeGroup{},
		connectionParameters: map[string]*armpurestorageblock.IscsiConnectionParameters{},
		missingStoragePools:  map[string]bool{},
		statuses:             map[string]armpurestorageblock.VolumeGroupStatus{},
		statusErrors:         map[string]fakeStatusError{},
	}
}

// fakeStatusError is an HTTP status and error.code GetStatus is seeded to fail with.
type fakeStatusError struct {
	httpStatus int
	code       string
}

// setStatus seeds the body GetStatus returns for a volume group.
func (s *fakeVolumeGroupStore) setStatus(resourceGroupName, storagePoolName, name string, status armpurestorageblock.VolumeGroupStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[vgKey(resourceGroupName, storagePoolName, name)] = status
}

// setStatusError makes GetStatus fail for a volume group with the given HTTP status and
// error code, whether or not the volume group exists.
func (s *fakeVolumeGroupStore) setStatusError(resourceGroupName, storagePoolName, name string, httpStatus int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusErrors[vgKey(resourceGroupName, storagePoolName, name)] = fakeStatusError{httpStatus: httpStatus, code: code}
}

// markStoragePoolMissing makes both the volume-group and recoverable-volume-group list
// pagers 404 for this pool, for a test exercising the "missing parent" case of the
// plural everpure_azure_volume_groups / everpure_azure_recoverable_volume_groups data
// sources.
func (s *fakeVolumeGroupStore) markStoragePoolMissing(resourceGroupName, storagePoolName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missingStoragePools[vgPoolKey(resourceGroupName, storagePoolName)] = true
}

// setConnectionParameters seeds the iSCSI endpoints ListConnectionParameters returns
// for one volume group, so a test can drive resolveVMExtensionTargets without a real ARM backend.
func (s *fakeVolumeGroupStore) setConnectionParameters(
	resourceGroupName, storagePoolName, name string, endpoints []*armpurestorageblock.IscsiEndpoint,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connectionParameters[vgKey(resourceGroupName, storagePoolName, name)] = &armpurestorageblock.IscsiConnectionParameters{
		Endpoints: endpoints,
	}
}

func vgKey(resourceGroupName, storagePoolName, name string) string {
	return resourceGroupName + "/" + storagePoolName + "/" + name
}

// findVolumeGroup looks up a group by exact key first, then falls back to a
// case-insensitive scan, mirroring ARM's own case-insensitive volume group name lookup
// (confirmed live). The exact-match attempt keeps every existing create/update/delete
// call site's behavior identical to before this fallback existed.
func (s *fakeVolumeGroupStore) findVolumeGroup(resourceGroupName, storagePoolName, name string) (*armpurestorageblock.VolumeGroup, bool) {
	if vg, ok := s.groups[vgKey(resourceGroupName, storagePoolName, name)]; ok {
		return vg, true
	}
	want := vgKey(resourceGroupName, storagePoolName, name)
	for key, vg := range s.groups {
		if strings.EqualFold(key, want) {
			return vg, true
		}
	}
	return nil, false
}

func vgPoolKey(resourceGroupName, storagePoolName string) string {
	return resourceGroupName + "/" + storagePoolName
}

func (s *fakeVolumeGroupStore) fakeVGID(resourceGroupName, storagePoolName, name string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/%s",
		s.subscriptionID, resourceGroupName, storagePoolName, name,
	)
}

func (s *fakeVolumeGroupStore) fakeRecoverableVGID(resourceGroupName, storagePoolName, twinName string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/recoverableVolumeGroups/%s",
		s.subscriptionID, resourceGroupName, storagePoolName, twinName,
	)
}

func (s *fakeVolumeGroupStore) mutateVolumeGroup(resourceGroupName, storagePoolName, name string, mutate func(*armpurestorageblock.VolumeGroup)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vg, ok := s.groups[vgKey(resourceGroupName, storagePoolName, name)]; ok {
		mutate(vg)
	}
}

func (s *fakeVolumeGroupStore) removeVolumeGroup(resourceGroupName, storagePoolName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.groups, vgKey(resourceGroupName, storagePoolName, name))
}

// seedRecoverableTwin adds a decoy or pre-existing recoverable twin directly, without
// going through a real delete, for tests exercising twin selection independent of the
// delete flow itself.
func (s *fakeVolumeGroupStore) seedRecoverableTwin(resourceGroupName, storagePoolName, twinName, previousName string, destroyedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := vgPoolKey(resourceGroupName, storagePoolName)
	s.recoverable[key] = append(s.recoverable[key], &armpurestorageblock.RecoverableVolumeGroup{
		ID:   to.Ptr(s.fakeRecoverableVGID(resourceGroupName, storagePoolName, twinName)),
		Name: to.Ptr(twinName),
		Properties: &armpurestorageblock.RecoverableVolumeGroupProperties{
			SoftDeletion: &armpurestorageblock.DestroyedStateProperties{
				Destroyed:    to.Ptr(true),
				DestroyedAt:  to.Ptr(destroyedAt),
				PreviousName: to.Ptr(previousName),
			},
		},
	})
}

// seedVolumeGroup inserts a volume group directly into the store, without going
// through BeginCreate, as if it already existed in Azure before this test run (e.g.
// created outside Terraform, or by an earlier apply in a different working
// directory) — for tests exercising import against a resource nothing here ever
// applied. Mirrors the minimal fields BeginCreate itself sets.
func (s *fakeVolumeGroupStore) seedVolumeGroup(resourceGroupName, storagePoolName, name, frequency, retention string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[vgKey(resourceGroupName, storagePoolName, name)] = &armpurestorageblock.VolumeGroup{
		ID:   to.Ptr(s.fakeVGID(resourceGroupName, storagePoolName, name)),
		Name: to.Ptr(name),
		Properties: &armpurestorageblock.VolumeGroupProperties{
			ProvisioningState:     to.Ptr(armpurestorageblock.ProvisioningStateSucceeded),
			StoragePoolInternalID: to.Ptr("internal-" + storagePoolName),
			VolumeGroupInternalID: to.Ptr("internal-" + name),
			ProtectionParameters: &armpurestorageblock.ProtectionParameters{
				Frequency: to.Ptr(frequency),
				Retention: to.Ptr(retention),
			},
		},
	}
}

// newVolumeGroupsServer builds the fake.VolumeGroupsServer wired to this store.
func (s *fakeVolumeGroupStore) newVolumeGroupsServer() psbfake.VolumeGroupsServer {
	return psbfake.VolumeGroupsServer{
		BeginCreate: func(
			_ context.Context, resourceGroupName, storagePoolName, name string, resource armpurestorageblock.VolumeGroup,
			_ *armpurestorageblock.VolumeGroupsClientBeginCreateOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.VolumeGroupsClientCreateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.createCallCount++

			if s.failCreatePoll {
				var responder azcorefake.PollerResponder[armpurestorageblock.VolumeGroupsClientCreateResponse]
				responder.AddNonTerminalResponse(201, nil)
				responder.SetTerminalError(400, "ResourceProvisioningFailed")
				return responder, azcorefake.ErrorResponder{}
			}

			vg := resource
			vg.ID = to.Ptr(s.fakeVGID(resourceGroupName, storagePoolName, name))
			vg.Name = to.Ptr(name)
			if vg.Properties == nil {
				vg.Properties = &armpurestorageblock.VolumeGroupProperties{}
			}
			vg.Properties.ProvisioningState = to.Ptr(armpurestorageblock.ProvisioningStateSucceeded)
			vg.Properties.StoragePoolInternalID = to.Ptr("internal-" + storagePoolName)
			vg.Properties.VolumeGroupInternalID = to.Ptr("internal-" + name)
			if s.stampExtraTagOnCreate {
				if vg.Tags == nil {
					vg.Tags = map[string]*string{}
				}
				vg.Tags["stamped-by-rp"] = to.Ptr("true")
			}
			s.groups[vgKey(resourceGroupName, storagePoolName, name)] = &vg

			var responder azcorefake.PollerResponder[armpurestorageblock.VolumeGroupsClientCreateResponse]
			if s.failCreatePollKeepObject {
				responder.AddNonTerminalResponse(201, nil)
				responder.SetTerminalError(400, "ResourceProvisioningFailed")
				return responder, azcorefake.ErrorResponder{}
			}
			responder.SetTerminalResponse(200, armpurestorageblock.VolumeGroupsClientCreateResponse{VolumeGroup: vg}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		Get: func(
			_ context.Context, resourceGroupName, storagePoolName, name string, _ *armpurestorageblock.VolumeGroupsClientGetOptions,
		) (azcorefake.Responder[armpurestorageblock.VolumeGroupsClientGetResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			if s.failGetOnce {
				s.failGetOnce = false
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(500, "InternalError")
				return azcorefake.Responder[armpurestorageblock.VolumeGroupsClientGetResponse]{}, errResp
			}

			vg, ok := s.findVolumeGroup(resourceGroupName, storagePoolName, name)
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return azcorefake.Responder[armpurestorageblock.VolumeGroupsClientGetResponse]{}, errResp
			}

			var responder azcorefake.Responder[armpurestorageblock.VolumeGroupsClientGetResponse]
			responder.SetResponse(200, armpurestorageblock.VolumeGroupsClientGetResponse{VolumeGroup: *vg}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginUpdate: func(
			_ context.Context, resourceGroupName, storagePoolName, name string, properties armpurestorageblock.VolumeGroupUpdate,
			_ *armpurestorageblock.VolumeGroupsClientBeginUpdateOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.VolumeGroupsClientUpdateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			propsCopy := properties
			s.lastUpdate = &propsCopy
			s.updateCallCount++

			var responder azcorefake.PollerResponder[armpurestorageblock.VolumeGroupsClientUpdateResponse]

			vg, ok := s.groups[vgKey(resourceGroupName, storagePoolName, name)]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return responder, errResp
			}

			// Mirrors the real backend: tags sent alongside properties are ignored;
			// only a tags-only PATCH updates them.
			if properties.Tags != nil && properties.Properties == nil {
				vg.Tags = properties.Tags
			}
			if properties.Properties != nil {
				if pp := properties.Properties.PerformanceParameters; pp != nil {
					vg.Properties.PerformanceParameters = normalizePerformanceParameters(pp)
				}
				if pp := properties.Properties.ProtectionParameters; pp != nil {
					vg.Properties.ProtectionParameters = normalizeProtectionParameters(pp)
				}
			}

			responder.SetTerminalResponse(200, armpurestorageblock.VolumeGroupsClientUpdateResponse{VolumeGroup: *vg}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginDelete: func(
			_ context.Context, resourceGroupName, storagePoolName, name string, _ *armpurestorageblock.VolumeGroupsClientBeginDeleteOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.VolumeGroupsClientDeleteResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.deleteCallCount++

			key := vgKey(resourceGroupName, storagePoolName, name)
			var responder azcorefake.PollerResponder[armpurestorageblock.VolumeGroupsClientDeleteResponse]

			vg, ok := s.groups[key]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return responder, errResp
			}

			// The real backend resolves the path's name case-insensitively but renames
			// using the volume group's own stored spelling, not whatever case the
			// request happened to spell it in — vg.Name, read before the entry is
			// removed below, stands in for that stored spelling, so a test can seed a
			// group whose Name differs in case from the map key (itself built from the
			// request's own name) to simulate state and server disagreeing on case.
			ownName := name
			if vg.Name != nil {
				ownName = *vg.Name
			}

			delete(s.groups, key)

			if !s.skipTwinOnDelete {
				// The real backend's rename suffix is plain 14 digits (verified
				// live, e.g. "...-20260917145432") — no fractional seconds.
				twinName := fmt.Sprintf("%s-%s", ownName, time.Now().UTC().Format("20060102150405"))
				poolKey := vgPoolKey(resourceGroupName, storagePoolName)
				twinProps := &armpurestorageblock.RecoverableVolumeGroupProperties{
					SoftDeletion: &armpurestorageblock.DestroyedStateProperties{
						Destroyed:    to.Ptr(true),
						DestroyedAt:  to.Ptr(time.Now().UTC()),
						PreviousName: to.Ptr(ownName),
					},
				}
				// The real backend fills a recoverable volume group's QoS/protection
				// with whatever the volume group had at the moment it was destroyed;
				// mirror that here by copying from the entry just removed above,
				// rather than leaving the twin's values nil.
				if vg.Properties != nil {
					twinProps.PerformanceParameters = vg.Properties.PerformanceParameters
					twinProps.ProtectionParameters = vg.Properties.ProtectionParameters
				}
				s.recoverable[poolKey] = append(s.recoverable[poolKey], &armpurestorageblock.RecoverableVolumeGroup{
					ID:         to.Ptr(s.fakeRecoverableVGID(resourceGroupName, storagePoolName, twinName)),
					Name:       to.Ptr(twinName),
					Properties: twinProps,
				})
			}

			// The generated client's deleteOperation only accepts 202 or 204 on the
			// initial response (not 200), so an immediate, unblocked delete's
			// terminal response must be 204.
			responder.SetTerminalResponse(204, armpurestorageblock.VolumeGroupsClientDeleteResponse{}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		ListConnectionParameters: func(
			_ context.Context, resourceGroupName, storagePoolName, name string,
			_ *armpurestorageblock.VolumeGroupsClientListConnectionParametersOptions,
		) (azcorefake.Responder[armpurestorageblock.VolumeGroupsClientListConnectionParametersResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			params, ok := s.connectionParameters[vgKey(resourceGroupName, storagePoolName, name)]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "ResourceNotFound")
				return azcorefake.Responder[armpurestorageblock.VolumeGroupsClientListConnectionParametersResponse]{}, errResp
			}

			var responder azcorefake.Responder[armpurestorageblock.VolumeGroupsClientListConnectionParametersResponse]
			responder.SetResponse(200, armpurestorageblock.VolumeGroupsClientListConnectionParametersResponse{
				ConnectionParametersResponse: armpurestorageblock.ConnectionParametersResponse{Iscsi: params},
			}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		GetStatus: func(
			_ context.Context, resourceGroupName, storagePoolName, name string,
			_ *armpurestorageblock.VolumeGroupsClientGetStatusOptions,
		) (azcorefake.Responder[armpurestorageblock.VolumeGroupsClientGetStatusResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			key := vgKey(resourceGroupName, storagePoolName, name)
			if e, ok := s.statusErrors[key]; ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(e.httpStatus, e.code)
				return azcorefake.Responder[armpurestorageblock.VolumeGroupsClientGetStatusResponse]{}, errResp
			}
			if _, ok := s.findVolumeGroup(resourceGroupName, storagePoolName, name); !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "volume_group_not_found")
				return azcorefake.Responder[armpurestorageblock.VolumeGroupsClientGetStatusResponse]{}, errResp
			}

			status, ok := s.statuses[key]
			if !ok {
				status = armpurestorageblock.VolumeGroupStatus{
					ConnectedHostCount: to.Ptr(int32(0)),
					Space:              &armpurestorageblock.Space{Shared: to.Ptr(int64(0)), Snapshots: to.Ptr(int64(0)), TotalUsed: to.Ptr(int64(0)), Unique: to.Ptr(int64(0))},
				}
			}

			var responder azcorefake.Responder[armpurestorageblock.VolumeGroupsClientGetStatusResponse]
			responder.SetResponse(200, armpurestorageblock.VolumeGroupsClientGetStatusResponse{VolumeGroupStatus: status}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		NewListByStoragePoolPager: func(
			resourceGroupName, storagePoolName string, _ *armpurestorageblock.VolumeGroupsClientListByStoragePoolOptions,
		) azcorefake.PagerResponder[armpurestorageblock.VolumeGroupsClientListByStoragePoolResponse] {
			s.mu.Lock()
			var pager azcorefake.PagerResponder[armpurestorageblock.VolumeGroupsClientListByStoragePoolResponse]
			if s.missingStoragePools[vgPoolKey(resourceGroupName, storagePoolName)] {
				s.mu.Unlock()
				pager.AddResponseError(404, "ResourceNotFound")
				return pager
			}

			prefix := vgPoolKey(resourceGroupName, storagePoolName) + "/"
			var entries []*armpurestorageblock.VolumeGroup
			for key, vg := range s.groups {
				if !strings.HasPrefix(key, prefix) {
					continue
				}
				g := *vg
				entries = append(entries, &g)
			}
			pageSize := s.pageSize
			s.mu.Unlock()

			for _, page := range paginate(entries, pageSize) {
				pager.AddPage(200, armpurestorageblock.VolumeGroupsClientListByStoragePoolResponse{
					VolumeGroupListResult: armpurestorageblock.VolumeGroupListResult{Value: page},
				}, nil)
			}
			return pager
		},
	}
}

// normalizePerformanceParameters mimics the API's own PATCH semantics: an explicit 0
// clears that field, so it never round-trips back from GET.
func normalizePerformanceParameters(pp *armpurestorageblock.PerformanceParameters) *armpurestorageblock.PerformanceParameters {
	result := &armpurestorageblock.PerformanceParameters{}
	if pp.BandwidthLimitMbPerSec != nil && *pp.BandwidthLimitMbPerSec != 0 {
		result.BandwidthLimitMbPerSec = pp.BandwidthLimitMbPerSec
	}
	if pp.IopsLimit != nil && *pp.IopsLimit != 0 {
		result.IopsLimit = pp.IopsLimit
	}
	if result.BandwidthLimitMbPerSec == nil && result.IopsLimit == nil {
		return nil
	}
	return result
}

// normalizeProtectionParameters mimics the API's disable-via-empty-object semantics:
// an empty {} (both fields nil) disables the schedule, which GET then reports as nil.
func normalizeProtectionParameters(pp *armpurestorageblock.ProtectionParameters) *armpurestorageblock.ProtectionParameters {
	if pp.Frequency == nil && pp.Retention == nil {
		return nil
	}
	return pp
}

// newRecoverableVolumeGroupsServer builds the fake.RecoverableVolumeGroupsServer wired
// to this store.
func (s *fakeVolumeGroupStore) newRecoverableVolumeGroupsServer() psbfake.RecoverableVolumeGroupsServer {
	return psbfake.RecoverableVolumeGroupsServer{
		NewListByStoragePoolPager: func(
			resourceGroupName, storagePoolName string, _ *armpurestorageblock.RecoverableVolumeGroupsClientListByStoragePoolOptions,
		) azcorefake.PagerResponder[armpurestorageblock.RecoverableVolumeGroupsClientListByStoragePoolResponse] {
			s.mu.Lock()
			var pager azcorefake.PagerResponder[armpurestorageblock.RecoverableVolumeGroupsClientListByStoragePoolResponse]
			if s.missingStoragePools[vgPoolKey(resourceGroupName, storagePoolName)] {
				s.mu.Unlock()
				pager.AddResponseError(404, "ResourceNotFound")
				return pager
			}

			twins := append([]*armpurestorageblock.RecoverableVolumeGroup{}, s.recoverable[vgPoolKey(resourceGroupName, storagePoolName)]...)
			pageSize := s.pageSize
			s.mu.Unlock()

			for _, page := range paginate(twins, pageSize) {
				pager.AddPage(200, armpurestorageblock.RecoverableVolumeGroupsClientListByStoragePoolResponse{
					RecoverableVolumeGroupListResult: armpurestorageblock.RecoverableVolumeGroupListResult{Value: page},
				}, nil)
			}
			return pager
		},

		BeginDelete: func(
			_ context.Context, resourceGroupName, storagePoolName, recoverableVolumeGroupName string,
			_ *armpurestorageblock.RecoverableVolumeGroupsClientBeginDeleteOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.RecoverableVolumeGroupsClientDeleteResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			key := vgPoolKey(resourceGroupName, storagePoolName)
			var responder azcorefake.PollerResponder[armpurestorageblock.RecoverableVolumeGroupsClientDeleteResponse]

			twins := s.recoverable[key]
			idx := -1
			for i, twin := range twins {
				if twin.Name != nil && *twin.Name == recoverableVolumeGroupName {
					idx = i
					break
				}
			}
			if idx == -1 {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return responder, errResp
			}

			s.recoverable[key] = append(twins[:idx], twins[idx+1:]...)
			responder.SetTerminalResponse(204, armpurestorageblock.RecoverableVolumeGroupsClientDeleteResponse{}, nil)
			return responder, azcorefake.ErrorResponder{}
		},
	}
}
