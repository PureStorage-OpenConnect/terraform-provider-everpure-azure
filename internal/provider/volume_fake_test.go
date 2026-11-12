// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
)

// fakeVolumeStore is the in-memory backing store behind the integration tests'
// armpurestorageblock/fake.VolumesServer. Unlike volume groups, a volume has no
// separate recoverable collection: a soft-deleted volume stays in this same map under
// a renamed key, and a second delete on that renamed name removes it for good
// (eradication). NewListByVolumeGroupPager therefore serves live and destroyed entries
// from the one map with no filtering, matching the real operation, which takes no
// filter parameter.
type fakeVolumeStore struct {
	mu             sync.Mutex
	subscriptionID string
	volumes        map[string]*armpurestorageblock.Volume // key: resourceGroup/pool/volumeGroup/name

	// nextSerial is incremented on every create and turned into a deterministic
	// 24-character serial number; it is stored on the volume rather than derived from
	// its key, so the serial survives the soft-delete rename intact, matching the real
	// backend (a renamed volume is demonstrably the same volume, not a copy).
	nextSerial int

	// lastCreate captures the exact PUT body a test observed, unmodified by anything
	// this store fills in afterwards (id, name, serial, ...).
	lastCreate *armpurestorageblock.Volume

	// lastUpdate captures the most recent PATCH body a test observed, to assert on
	// exactly what the resource sent.
	lastUpdate *armpurestorageblock.VolumeUpdate

	// updateCallCount counts every BeginUpdate call this store has received, so a test
	// can assert a PATCH was (or was not) sent at all, independent of what body it
	// carried.
	updateCallCount int

	// skipTwinOnDelete makes BeginDelete remove a live entry outright with no renamed
	// destroyed copy, simulating the soft-deleted twin having already been eradicated
	// (or never having existed) by the time eradicate_on_destroy goes looking for it.
	skipTwinOnDelete bool

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

	// getFailStatus, when non-zero, makes every Get fail with this HTTP status.
	getFailStatus int

	// recaseIDs makes every returned ID, name and source ID field come back
	// upper-cased regardless of what was requested, standing in for ARM's own casing
	// normalization so a mapping test can assert the provider keeps the casing already
	// in its config/state rather than adopting whatever the API happens to echo.
	recaseIDs bool

	// nilPropertiesOnGet makes Get return a Volume with a nil Properties, standing in
	// for a degraded API response so a mapping test can assert applyVolume nil-checks
	// before dereferencing it.
	nilPropertiesOnGet bool

	// missingVolumeGroups marks a volume group (keyed by volumeGroupKey) whose list
	// pager must 404 instead of returning an empty page, standing in for a list
	// against a volume group that doesn't exist — for the plural
	// everpure_azure_volumes data source's "missing parent" test, since this store
	// otherwise has no notion of volume group existence at all (only of the volumes on
	// one).
	missingVolumeGroups map[string]bool

	// pageSize, when > 0 and smaller than the number of matching entries, splits a
	// list pager's response across that many entries per page instead of one page
	// holding everything — see paginate in fake_pager_test.go.
	pageSize int
}

// volumeGroupKey identifies a volume group for missingVolumeGroups, distinct from
// volumeKey (which additionally names a volume within it).
func volumeGroupKey(resourceGroupName, storagePoolName, volumeGroupName string) string {
	return resourceGroupName + "/" + storagePoolName + "/" + volumeGroupName
}

// markVolumeGroupMissing makes the volumes list pager 404 for this volume group, for a
// test exercising the plural everpure_azure_volumes data source's "missing parent" case.
func (s *fakeVolumeStore) markVolumeGroupMissing(resourceGroupName, storagePoolName, volumeGroupName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missingVolumeGroups[volumeGroupKey(resourceGroupName, storagePoolName, volumeGroupName)] = true
}

func newFakeVolumeStore(subscriptionID string) *fakeVolumeStore {
	return &fakeVolumeStore{
		subscriptionID:      subscriptionID,
		volumes:             map[string]*armpurestorageblock.Volume{},
		missingVolumeGroups: map[string]bool{},
	}
}

func volumeKey(resourceGroupName, storagePoolName, volumeGroupName, name string) string {
	return resourceGroupName + "/" + storagePoolName + "/" + volumeGroupName + "/" + name
}

func (s *fakeVolumeStore) fakeVolumeID(resourceGroupName, storagePoolName, volumeGroupName, name string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/%s/volumes/%s",
		s.subscriptionID, resourceGroupName, storagePoolName, volumeGroupName, name,
	)
}

// nextSerialNumber returns a fixed-width 24-character serial, unique per call within a
// store's lifetime.
func (s *fakeVolumeStore) nextSerialNumber() string {
	s.nextSerial++
	return fmt.Sprintf("FAKESERIAL%014d", s.nextSerial)
}

// mutateVolume mutates a stored entry in place, for tests that need to simulate drift
// (e.g. a size changed outside Terraform) without going through BeginUpdate.
func (s *fakeVolumeStore) mutateVolume(resourceGroupName, storagePoolName, volumeGroupName, name string, mutate func(*armpurestorageblock.Volume)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.volumes[volumeKey(resourceGroupName, storagePoolName, volumeGroupName, name)]; ok {
		mutate(v)
	}
}

// removeVolume deletes an entry directly, for tests simulating "removed outside
// Terraform" without a real BeginDelete round trip.
func (s *fakeVolumeStore) removeVolume(resourceGroupName, storagePoolName, volumeGroupName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.volumes, volumeKey(resourceGroupName, storagePoolName, volumeGroupName, name))
}

// seedVolume adds a live entry directly, without going through a real create, for
// plural-data-source tests that just need several volumes to list rather than to also
// prove the create path.
func (s *fakeVolumeStore) seedVolume(resourceGroupName, storagePoolName, volumeGroupName, name string, provisionedSize int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volumes[volumeKey(resourceGroupName, storagePoolName, volumeGroupName, name)] = &armpurestorageblock.Volume{
		ID:   to.Ptr(s.fakeVolumeID(resourceGroupName, storagePoolName, volumeGroupName, name)),
		Name: to.Ptr(name),
		Properties: &armpurestorageblock.AzureVolumeProperties{
			ProvisionedSize: to.Ptr(provisionedSize),
			SerialNumber:    to.Ptr(s.nextSerialNumber()),
			CreatedAt:       to.Ptr(time.Now().UTC()),
			Space:           fakeVolumeSpace(),
		},
	}
}

// fakeVolumeSpace is the space every seeded fake volume reports. The four values differ
// so a test that maps one field onto another's attribute fails.
func fakeVolumeSpace() *armpurestorageblock.Space {
	return &armpurestorageblock.Space{
		TotalUsed: to.Ptr(int64(8192)),
		Unique:    to.Ptr(int64(4096)),
		Snapshots: to.Ptr(int64(2048)),
		Shared:    to.Ptr(int64(1024)),
	}
}

// seedDestroyedVolume adds a destroyed entry directly, without going through a real
// delete, for tests exercising soft-deleted-copy selection independent of the delete
// flow itself: a decoy with a different previousName, an older destroyedAt, or one
// that already existed before the delete under test runs.
func (s *fakeVolumeStore) seedDestroyedVolume(resourceGroupName, storagePoolName, volumeGroupName, twinName, previousName string, destroyedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volumes[volumeKey(resourceGroupName, storagePoolName, volumeGroupName, twinName)] = &armpurestorageblock.Volume{
		ID:   to.Ptr(s.fakeVolumeID(resourceGroupName, storagePoolName, volumeGroupName, twinName)),
		Name: to.Ptr(twinName),
		Properties: &armpurestorageblock.AzureVolumeProperties{
			ProvisionedSize: to.Ptr(int64(1073741824)),
			SerialNumber:    to.Ptr(s.nextSerialNumber()),
			CreatedAt:       to.Ptr(destroyedAt),
			SoftDeletion: &armpurestorageblock.DestroyedStateProperties{
				Destroyed:            to.Ptr(true),
				DestroyedAt:          to.Ptr(destroyedAt),
				PreviousName:         to.Ptr(previousName),
				EradicationTimestamp: to.Ptr(destroyedAt.Add(30 * 24 * time.Hour)),
			},
		},
	}
}

// cloneVolume deep-copies the parts of a Volume this store mutates or hands back, so
// that a stored entry, a recorded request body and the caller's own struct never share
// a Properties pointer. Without it, the serial number and timestamps the store stamps
// on a created volume would appear inside lastCreate as if the caller had sent them,
// and a test asserting on the request body would pass against fields it never set.
func cloneVolume(v armpurestorageblock.Volume) armpurestorageblock.Volume {
	out := v
	if v.Properties == nil {
		return out
	}
	props := *v.Properties
	if v.Properties.SourceVolumeSnapshot != nil {
		snapshot := *v.Properties.SourceVolumeSnapshot
		props.SourceVolumeSnapshot = &snapshot
	}
	out.Properties = &props
	return out
}

// stripSourceFields returns a copy of v with the create-only source* fields removed, and
// provisioningState too unless the volume is unsettled. Every read path (Get and
// ListByVolumeGroup alike) returns a volume in this shape once it exists: the source
// fields carry create-only mutability and are echoed only by the Create response itself,
// and a settled volume carries no provisioningState.
func stripSourceFields(v armpurestorageblock.Volume) armpurestorageblock.Volume {
	out := v
	if v.Properties == nil {
		return out
	}
	out.Properties = &armpurestorageblock.AzureVolumeProperties{
		ProvisionedSize: v.Properties.ProvisionedSize,
		SerialNumber:    v.Properties.SerialNumber,
		CreatedAt:       v.Properties.CreatedAt,
		SoftDeletion:    v.Properties.SoftDeletion,
		Space:           v.Properties.Space,
	}
	if state := v.Properties.ProvisioningState; state != nil && *state != armpurestorageblock.ProvisioningStateSucceeded {
		out.Properties.ProvisioningState = state
	}
	return out
}

// recased simulates ARM's own casing normalization: when enabled, the volume's ID,
// name and any source ID fields still present come back upper-cased regardless of what
// the caller sent, so a mapping test can assert the provider keeps the casing from its
// own config/state rather than adopting whatever casing the API echoes.
func (s *fakeVolumeStore) recased(v armpurestorageblock.Volume) armpurestorageblock.Volume {
	if !s.recaseIDs {
		return v
	}
	out := v
	if v.ID != nil {
		out.ID = to.Ptr(strings.ToUpper(*v.ID))
	}
	if v.Name != nil {
		out.Name = to.Ptr(strings.ToUpper(*v.Name))
	}
	if v.Properties == nil {
		return out
	}
	propsCopy := *v.Properties
	if v.Properties.SourceVolumeResourceID != nil {
		propsCopy.SourceVolumeResourceID = to.Ptr(strings.ToUpper(*v.Properties.SourceVolumeResourceID))
	}
	if v.Properties.SourceVolumeGroupResourceID != nil {
		propsCopy.SourceVolumeGroupResourceID = to.Ptr(strings.ToUpper(*v.Properties.SourceVolumeGroupResourceID))
	}
	if v.Properties.SourceRecoverableVolumeResourceID != nil {
		propsCopy.SourceRecoverableVolumeResourceID = to.Ptr(strings.ToUpper(*v.Properties.SourceRecoverableVolumeResourceID))
	}
	if v.Properties.SourceVolumeSnapshot != nil {
		snap := *v.Properties.SourceVolumeSnapshot
		if snap.VolumeGroupSnapshotResourceID != nil {
			snap.VolumeGroupSnapshotResourceID = to.Ptr(strings.ToUpper(*snap.VolumeGroupSnapshotResourceID))
		}
		propsCopy.SourceVolumeSnapshot = &snap
	}
	out.Properties = &propsCopy
	return out
}

// newVolumesServer builds the fake.VolumesServer wired to this store.
func (s *fakeVolumeStore) newVolumesServer() psbfake.VolumesServer {
	return psbfake.VolumesServer{
		BeginCreate: func(
			_ context.Context, resourceGroupName, storagePoolName, volumeGroupName, name string, resource armpurestorageblock.Volume,
			_ *armpurestorageblock.VolumesClientBeginCreateOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.VolumesClientCreateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			reqCopy := cloneVolume(resource)
			s.lastCreate = &reqCopy
			s.createCallCount++

			if s.failCreatePoll {
				var responder azcorefake.PollerResponder[armpurestorageblock.VolumesClientCreateResponse]
				responder.AddNonTerminalResponse(201, nil)
				responder.SetTerminalError(400, "ResourceProvisioningFailed")
				return responder, azcorefake.ErrorResponder{}
			}

			// The real backend rejects cloning from a serial number that belongs to a
			// volume it already manages: only a direct-API volume (one with no
			// Azure-assigned identity) may be used as a serial-number clone source,
			// since a bare serial cannot be RBAC-checked the way an ARM ID can. This
			// mirrors that rejection for whichever serial number this store itself
			// already handed out on an earlier create.
			if resource.Properties != nil && resource.Properties.SourceType != nil &&
				*resource.Properties.SourceType == armpurestorageblock.VolumeSourceTypeSerialNumber &&
				resource.Properties.SourceSerialNumber != nil {
				for _, existing := range s.volumes {
					if existing.Properties != nil && existing.Properties.SerialNumber != nil &&
						*existing.Properties.SerialNumber == *resource.Properties.SourceSerialNumber {
						var errResp azcorefake.ErrorResponder
						errResp.SetResponseError(400, "ErrNativeVMVolumeSerialSourceNotAllowed")
						return azcorefake.PollerResponder[armpurestorageblock.VolumesClientCreateResponse]{}, errResp
					}
				}
			}

			v := cloneVolume(resource)
			v.ID = to.Ptr(s.fakeVolumeID(resourceGroupName, storagePoolName, volumeGroupName, name))
			v.Name = to.Ptr(name)
			if v.Properties == nil {
				v.Properties = &armpurestorageblock.AzureVolumeProperties{}
			}
			v.Properties.ProvisioningState = to.Ptr(armpurestorageblock.ProvisioningStateSucceeded)
			v.Properties.SerialNumber = to.Ptr(s.nextSerialNumber())
			v.Properties.CreatedAt = to.Ptr(time.Now().UTC())
			if v.Properties.ProvisionedSize == nil {
				// Every source type but "none" leaves provisionedSize unset in the
				// request: the real backend inherits the new volume's size from
				// wherever it's cloned or restored from, so a real create always comes
				// back with a real, non-zero size regardless of source_type. Simulate
				// that inheritance with a fixed value so callers can assert the size
				// round-trips through state instead of staying null.
				v.Properties.ProvisionedSize = to.Ptr(int64(5 * 1073741824))
			}
			v.Properties.Space = &armpurestorageblock.Space{
				Shared:    to.Ptr(int64(0)),
				Snapshots: to.Ptr(int64(0)),
				TotalUsed: to.Ptr(int64(0)),
				Unique:    to.Ptr(int64(0)),
			}
			s.volumes[volumeKey(resourceGroupName, storagePoolName, volumeGroupName, name)] = &v

			var responder azcorefake.PollerResponder[armpurestorageblock.VolumesClientCreateResponse]
			if s.failCreatePollKeepObject {
				responder.AddNonTerminalResponse(201, nil)
				responder.SetTerminalError(400, "ResourceProvisioningFailed")
				return responder, azcorefake.ErrorResponder{}
			}
			// The real PUT response does echo the source* fields (only later GETs omit
			// them), so the create response is the full, un-stripped volume.
			responder.SetTerminalResponse(200, armpurestorageblock.VolumesClientCreateResponse{Volume: s.recased(v)}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		Get: func(
			_ context.Context, resourceGroupName, storagePoolName, volumeGroupName, name string, _ *armpurestorageblock.VolumesClientGetOptions,
		) (azcorefake.Responder[armpurestorageblock.VolumesClientGetResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			if s.getFailStatus != 0 {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(s.getFailStatus, "InternalServerError")
				return azcorefake.Responder[armpurestorageblock.VolumesClientGetResponse]{}, errResp
			}

			v, ok := s.volumes[volumeKey(resourceGroupName, storagePoolName, volumeGroupName, name)]
			if !ok {
				// The real backend 404s a soft-deleted (renamed away) or absent
				// volume's original name with this code, not "ResourceNotFound" as on
				// the volume group.
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "ResourceReadFailed")
				return azcorefake.Responder[armpurestorageblock.VolumesClientGetResponse]{}, errResp
			}

			out := s.recased(stripSourceFields(*v))
			if s.nilPropertiesOnGet {
				out.Properties = nil
			}

			var responder azcorefake.Responder[armpurestorageblock.VolumesClientGetResponse]
			responder.SetResponse(200, armpurestorageblock.VolumesClientGetResponse{Volume: out}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginUpdate: func(
			_ context.Context, resourceGroupName, storagePoolName, volumeGroupName, name string, properties armpurestorageblock.VolumeUpdate,
			_ *armpurestorageblock.VolumesClientBeginUpdateOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.VolumesClientUpdateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			// Copied rather than aliased for the same reason as lastCreate: the
			// recorded body must stay exactly what the caller sent.
			propsCopy := properties
			if properties.Properties != nil {
				updateProps := *properties.Properties
				propsCopy.Properties = &updateProps
			}
			s.lastUpdate = &propsCopy
			s.updateCallCount++

			var responder azcorefake.PollerResponder[armpurestorageblock.VolumesClientUpdateResponse]

			v, ok := s.volumes[volumeKey(resourceGroupName, storagePoolName, volumeGroupName, name)]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return responder, errResp
			}

			if properties.Properties != nil && properties.Properties.ProvisionedSize != nil {
				v.Properties.ProvisionedSize = to.Ptr(*properties.Properties.ProvisionedSize)
			}

			responder.SetTerminalResponse(200, armpurestorageblock.VolumesClientUpdateResponse{Volume: s.recased(stripSourceFields(*v))}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginDelete: func(
			_ context.Context, resourceGroupName, storagePoolName, volumeGroupName, name string, _ *armpurestorageblock.VolumesClientBeginDeleteOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.VolumesClientDeleteResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.deleteCallCount++

			key := volumeKey(resourceGroupName, storagePoolName, volumeGroupName, name)
			var responder azcorefake.PollerResponder[armpurestorageblock.VolumesClientDeleteResponse]

			v, ok := s.volumes[key]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return responder, errResp
			}

			destroyed := v.Properties != nil && v.Properties.SoftDeletion != nil &&
				v.Properties.SoftDeletion.Destroyed != nil && *v.Properties.SoftDeletion.Destroyed

			// The real backend resolves the path's name case-insensitively but renames
			// using the volume's own stored spelling, not whatever case the request
			// happened to spell it in — the entry's own Name field, captured before it's
			// overwritten below, stands in for that stored spelling, so a test can seed
			// a live entry whose Name differs in case from the map key (itself built
			// from the request's own name) to simulate state and server disagreeing on
			// case.
			ownName := name
			if v.Name != nil {
				ownName = *v.Name
			}

			// There is no separate recoverable collection for volumes: a soft-deleted
			// volume stays in this same map under a renamed key, so the first delete
			// of a live entry frees its name immediately and the second delete of the
			// renamed entry (destroyed already true) is what eradicates it.
			delete(s.volumes, key)

			if !destroyed && !s.skipTwinOnDelete {
				// The rename suffix mirrors the volume group's: plain 14 digits, no
				// fractional seconds.
				twinName := fmt.Sprintf("%s-%s", ownName, time.Now().UTC().Format("20060102150405"))
				now := time.Now().UTC()
				if v.Properties == nil {
					v.Properties = &armpurestorageblock.AzureVolumeProperties{}
				}
				v.Name = to.Ptr(twinName)
				v.ID = to.Ptr(s.fakeVolumeID(resourceGroupName, storagePoolName, volumeGroupName, twinName))
				v.Properties.SoftDeletion = &armpurestorageblock.DestroyedStateProperties{
					Destroyed:            to.Ptr(true),
					DestroyedAt:          to.Ptr(now),
					PreviousName:         to.Ptr(ownName),
					EradicationTimestamp: to.Ptr(now.Add(30 * 24 * time.Hour)),
				}
				s.volumes[volumeKey(resourceGroupName, storagePoolName, volumeGroupName, twinName)] = v
			}

			// The generated client's deleteOperation only accepts 202 or 204 as the
			// initial response (not 200), so an immediate, unblocked delete's terminal
			// response must be 204.
			responder.SetTerminalResponse(204, armpurestorageblock.VolumesClientDeleteResponse{}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		NewListByVolumeGroupPager: func(
			resourceGroupName, storagePoolName, volumeGroupName string, _ *armpurestorageblock.VolumesClientListByVolumeGroupOptions,
		) azcorefake.PagerResponder[armpurestorageblock.VolumesClientListByVolumeGroupResponse] {
			s.mu.Lock()
			var pager azcorefake.PagerResponder[armpurestorageblock.VolumesClientListByVolumeGroupResponse]
			if s.missingVolumeGroups[volumeGroupKey(resourceGroupName, storagePoolName, volumeGroupName)] {
				s.mu.Unlock()
				pager.AddResponseError(404, "ResourceNotFound")
				return pager
			}

			prefix := volumeKey(resourceGroupName, storagePoolName, volumeGroupName, "")
			var entries []*armpurestorageblock.Volume
			for k, v := range s.volumes {
				if !strings.HasPrefix(k, prefix) {
					continue
				}
				out := s.recased(stripSourceFields(*v))
				entries = append(entries, &out)
			}
			pageSize := s.pageSize
			s.mu.Unlock()

			// Live and destroyed entries together, undistinguished except by
			// softDeletion.destroyed on each entry: the real operation takes only its
			// four path parameters and has no filter parameter.
			for _, page := range paginate(entries, pageSize) {
				pager.AddPage(200, armpurestorageblock.VolumesClientListByVolumeGroupResponse{
					VolumeListResult: armpurestorageblock.VolumeListResult{Value: page},
				}, nil)
			}
			return pager
		},
	}
}

// TestFakeVolumeStore_SoftDeleteListEradicate drives Create, a soft delete, a list
// that must show the destroyed twin, a Get and a second delete on that twin's renamed
// name, and a final list proving eradication — end to end through the generated
// VolumesClient against this store's fake transport, with no terraform or provider
// code involved. It exists to prove the harness itself works before anything else in
// the package depends on it.
func TestFakeVolumeStore_SoftDeleteListEradicate(t *testing.T) {
	store := newFakeVolumeStore(testSubscriptionID)
	srv := store.newVolumesServer()
	transport := psbfake.NewVolumesServerTransport(&srv)
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}

	client, err := armpurestorageblock.NewVolumesClient(store.subscriptionID, &azcorefake.TokenCredential{}, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx := context.Background()
	const rg, pool, vg, name = testResourceGroup, "pool1", "vg1", "vol1"

	createPoller, err := client.BeginCreate(ctx, rg, pool, vg, name, armpurestorageblock.Volume{
		Properties: &armpurestorageblock.AzureVolumeProperties{
			ProvisionedSize: to.Ptr(int64(1073741824)),
			SourceType:      to.Ptr(armpurestorageblock.VolumeSourceTypeNone),
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	created, err := createPoller.PollUntilDone(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created.Properties == nil || created.Properties.SerialNumber == nil || len(*created.Properties.SerialNumber) != 24 {
		t.Fatalf("expected a 24-character serial number, got %+v", created.Properties)
	}
	if created.Properties.SourceType == nil || *created.Properties.SourceType != armpurestorageblock.VolumeSourceTypeNone {
		t.Fatalf("expected the create response to echo sourceType, got %+v", created.Properties)
	}
	if store.lastCreate == nil || store.lastCreate.Properties == nil || store.lastCreate.Properties.SourceType == nil {
		t.Fatalf("expected lastCreate to record the request body")
	}

	getResp, err := client.Get(ctx, rg, pool, vg, name, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if getResp.Properties.SourceType != nil {
		t.Fatalf("expected Get to omit sourceType, got %v", *getResp.Properties.SourceType)
	}

	deletePoller, err := client.BeginDelete(ctx, rg, pool, vg, name, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := deletePoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := client.Get(ctx, rg, pool, vg, name, nil); err == nil {
		t.Fatalf("expected Get on the original name to fail after soft delete")
	}

	pager := client.NewListByVolumeGroupPager(rg, pool, vg, nil)
	var twinName string
	found := 0
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, v := range page.Value {
			found++
			if v.Properties == nil || v.Properties.SoftDeletion == nil || v.Properties.SoftDeletion.PreviousName == nil {
				t.Fatalf("expected a destroyed entry with previousName set, got %+v", v.Properties)
			}
			if *v.Properties.SoftDeletion.PreviousName != name {
				t.Fatalf("expected previousName %q, got %q", name, *v.Properties.SoftDeletion.PreviousName)
			}
			twinName = *v.Name
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one entry in the list after soft delete, got %d", found)
	}

	if _, err := client.Get(ctx, rg, pool, vg, twinName, nil); err != nil {
		t.Fatalf("unexpected error getting the renamed twin: %v", err)
	}

	eradicatePoller, err := client.BeginDelete(ctx, rg, pool, vg, twinName, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := eradicatePoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("unexpected error eradicating: %v", err)
	}

	pager = client.NewListByVolumeGroupPager(rg, pool, vg, nil)
	found = 0
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		found += len(page.Value)
	}
	if found != 0 {
		t.Fatalf("expected the list to be empty after eradication, got %d entries", found)
	}
}

// TestFakeVolumeStore_Hooks exercises the test-only hooks (seeding a decoy destroyed
// entry, mutating a stored volume out of band, removing one outright, skipping the
// destroyed twin on delete, re-casing responses, and a nil-Properties Get) so the
// integration tests that will depend on them have a working, proven harness.
func TestFakeVolumeStore_Hooks(t *testing.T) {
	store := newFakeVolumeStore(testSubscriptionID)
	srv := store.newVolumesServer()
	transport := psbfake.NewVolumesServerTransport(&srv)
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}

	client, err := armpurestorageblock.NewVolumesClient(store.subscriptionID, &azcorefake.TokenCredential{}, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx := context.Background()
	const rg, pool, vg, name = testResourceGroup, "pool1", "vg2", "vol2"

	createPoller, err := client.BeginCreate(ctx, rg, pool, vg, name, armpurestorageblock.Volume{
		Properties: &armpurestorageblock.AzureVolumeProperties{
			ProvisionedSize: to.Ptr(int64(1073741824)),
			SourceType:      to.Ptr(armpurestorageblock.VolumeSourceTypeNone),
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := createPoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// mutateVolume: simulate a resize outside Terraform, for the drift test.
	store.mutateVolume(rg, pool, vg, name, func(v *armpurestorageblock.Volume) {
		v.Properties.ProvisionedSize = to.Ptr(int64(2147483648))
	})
	getResp, err := client.Get(ctx, rg, pool, vg, name, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if getResp.Properties.ProvisionedSize == nil || *getResp.Properties.ProvisionedSize != 2147483648 {
		t.Fatalf("expected mutateVolume's size change to be visible via Get, got %+v", getResp.Properties.ProvisionedSize)
	}

	// seedDestroyedVolume: plant a decoy destroyed entry that pre-dates the delete
	// under test, with its own previousName, distinguishable from a twin the delete
	// flow itself would create.
	decoyDestroyedAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	store.seedDestroyedVolume(rg, pool, vg, "vol2-decoy", "some-other-volume", decoyDestroyedAt)

	// skipTwinOnDelete: the delete below must remove the live entry with no destroyed
	// twin of its own, leaving only the decoy in the list.
	store.skipTwinOnDelete = true
	deletePoller, err := client.BeginDelete(ctx, rg, pool, vg, name, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := deletePoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pager := client.NewListByVolumeGroupPager(rg, pool, vg, nil)
	var previousNames []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, v := range page.Value {
			previousNames = append(previousNames, *v.Properties.SoftDeletion.PreviousName)
		}
	}
	if len(previousNames) != 1 || previousNames[0] != "some-other-volume" {
		t.Fatalf("expected skipTwinOnDelete to leave only the seeded decoy, got %v", previousNames)
	}

	// removeVolume: simulate the decoy being gone outside Terraform.
	store.removeVolume(rg, pool, vg, "vol2-decoy")
	if _, err := client.Get(ctx, rg, pool, vg, "vol2-decoy", nil); err == nil {
		t.Fatalf("expected Get to fail after removeVolume")
	}

	// recaseIDs: the next create's response must come back upper-cased.
	store.recaseIDs = true
	recasePoller, err := client.BeginCreate(ctx, rg, pool, vg, "vol3", armpurestorageblock.Volume{
		Properties: &armpurestorageblock.AzureVolumeProperties{
			ProvisionedSize: to.Ptr(int64(1073741824)),
			SourceType:      to.Ptr(armpurestorageblock.VolumeSourceTypeNone),
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	recased, err := recasePoller.PollUntilDone(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recased.Name == nil || *recased.Name != strings.ToUpper(*recased.Name) {
		t.Fatalf("expected recaseIDs to upper-case the response name, got %v", recased.Name)
	}
	store.recaseIDs = false

	// nilPropertiesOnGet: Get must come back with a nil Properties.
	store.nilPropertiesOnGet = true
	nilPropsResp, err := client.Get(ctx, rg, pool, vg, "vol3", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nilPropsResp.Properties != nil {
		t.Fatalf("expected nilPropertiesOnGet to force a nil Properties, got %+v", nilPropsResp.Properties)
	}
}
