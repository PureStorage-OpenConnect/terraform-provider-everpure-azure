// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"slices"
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

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// fakeSnapshotStore is the in-memory backing store behind the integration tests'
// armpurestorageblock/fake.VolumeGroupSnapshotsServer. Like volumes, a snapshot has no
// separate recoverable collection: a soft-deleted snapshot stays in this same map under a
// renamed key, a second delete on that renamed name eradicates it, and the list serves
// live and destroyed entries together. Two behaviours are specific to snapshots:
//
//   - a create with sourceSnapshotResourceId does not copy anything: it un-destroys the
//     named soft-deleted snapshot and renames it to the requested name;
//   - volumeSnapshots is returned by Get only, never by the list.
type fakeSnapshotStore struct {
	mu             sync.Mutex
	subscriptionID string
	snapshots      map[string]*armpurestorageblock.VolumeGroupSnapshot // key: resourceGroup/pool/volumeGroup/name

	// members is what a plain create captures as the snapshot's volumeSnapshots: one
	// entry per volume in the volume group at that moment.
	members []*armpurestorageblock.VolumeSnapshotInfo

	// lastCreate captures the exact PUT body a test observed, unmodified by anything
	// this store fills in afterwards.
	lastCreate *armpurestorageblock.VolumeGroupSnapshot

	// createCallCount counts every BeginCreate call, so a test can assert a PUT was (or
	// was not) sent at all.
	createCallCount int

	// deleteCallCount counts every BeginDelete call, for the same reason.
	deleteCallCount int

	// getCallCount counts every Get call, so a test can assert a check that is meant to
	// skip the API really did.
	getCallCount int

	// skipTwinOnDelete makes BeginDelete remove a live entry outright with no renamed
	// destroyed copy, simulating the soft-deleted twin having already been eradicated
	// (or never having existed) by the time eradicate_on_destroy goes looking for it.
	skipTwinOnDelete bool

	// getFailStatus, when non-zero, makes every Get fail with this HTTP status, for a
	// test of how a non-404 read error surfaces.
	getFailStatus int

	// deleteFailOnCall, when non-zero, makes the BeginDelete call with this 1-based
	// ordinal fail with a 500 before touching the store, so a test can fail the first
	// (soft) delete or the second (eradicating) one.
	deleteFailOnCall int

	// failCreatePoll makes BeginCreate accept the request but fail the poll, storing
	// nothing: the shape of a create operation that fails after the PUT was accepted.
	failCreatePoll bool

	// failCreatePollKeepObject makes BeginCreate store the object exactly as a
	// successful create would, then fail the poll: the shape of a create that fails after
	// Azure finished creating the object. Unlike failCreatePoll, a later refresh finds it.
	failCreatePollKeepObject bool

	// missingVolumeGroups marks a volume group (keyed by volumeGroupKey) whose list
	// pager must 404 instead of returning an empty page.
	missingVolumeGroups map[string]bool

	// lastListOptions captures the options of the most recent list call.
	lastListOptions *armpurestorageblock.VolumeGroupSnapshotsClientListByVolumeGroupOptions

	// listCallCount counts every list call (one per page requested), so a test can
	// assert how many pages the provider read.
	listCallCount int

	// ignoreSkip makes the list ignore $skip and answer every call with the first page,
	// like a backend that doesn't support it.
	ignoreSkip bool

	// beforeListCall, when set, runs at the start of every list call with that call's
	// number (from 1), without the lock held, so a test can change the store between
	// the pages of one listing.
	beforeListCall func(call int)
}

func newFakeSnapshotStore(subscriptionID string) *fakeSnapshotStore {
	return &fakeSnapshotStore{
		subscriptionID:      subscriptionID,
		snapshots:           map[string]*armpurestorageblock.VolumeGroupSnapshot{},
		missingVolumeGroups: map[string]bool{},
		members: []*armpurestorageblock.VolumeSnapshotInfo{
			{Name: to.Ptr("member-vol"), ProvisionedSize: to.Ptr(int64(bytesPerGiB)), SerialNumber: to.Ptr("FAKESERIAL00000000000001"), Space: &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(300)), Unique: to.Ptr(int64(200)), Snapshots: to.Ptr(int64(100)), Shared: to.Ptr(int64(50))}},
		},
	}
}

func snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, name string) string {
	return volumeGroupKey(resourceGroupName, storagePoolName, volumeGroupName) + "/" + name
}

func (s *fakeSnapshotStore) fakeSnapshotID(resourceGroupName, storagePoolName, volumeGroupName, name string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/%s/snapshots/%s",
		s.subscriptionID, resourceGroupName, storagePoolName, volumeGroupName, name,
	)
}

// markVolumeGroupMissing makes the snapshots list pager 404 for this volume group.
func (s *fakeSnapshotStore) markVolumeGroupMissing(resourceGroupName, storagePoolName, volumeGroupName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missingVolumeGroups[volumeGroupKey(resourceGroupName, storagePoolName, volumeGroupName)] = true
}

// mutateSnapshot mutates a stored entry in place, for tests that simulate drift.
func (s *fakeSnapshotStore) mutateSnapshot(resourceGroupName, storagePoolName, volumeGroupName, name string, mutate func(*armpurestorageblock.VolumeGroupSnapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap, ok := s.snapshots[snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, name)]; ok {
		mutate(snap)
	}
}

// removeSnapshot deletes an entry directly, simulating "removed outside Terraform".
func (s *fakeSnapshotStore) removeSnapshot(resourceGroupName, storagePoolName, volumeGroupName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshots, snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, name))
}

// softDeleteLocked replaces a live entry with its destroyed twin, renamed to
// {name}-{YYYYMMDDHHmmss} as the real delete does, and returns the twin's name. The
// caller holds s.mu and has already removed the entry's old key.
func (s *fakeSnapshotStore) softDeleteLocked(resourceGroupName, storagePoolName, volumeGroupName string, snap *armpurestorageblock.VolumeGroupSnapshot, ownName string) string {
	now := time.Now().UTC()
	twinName := fmt.Sprintf("%s-%s", ownName, now.Format("20060102150405"))
	if snap.Properties == nil {
		snap.Properties = &armpurestorageblock.VolumeGroupSnapshotProperties{}
	}
	snap.Name = to.Ptr(twinName)
	snap.ID = to.Ptr(s.fakeSnapshotID(resourceGroupName, storagePoolName, volumeGroupName, twinName))
	snap.Properties.SoftDeletion = &armpurestorageblock.DestroyedStateProperties{
		Destroyed:            to.Ptr(true),
		DestroyedAt:          to.Ptr(now),
		PreviousName:         to.Ptr(ownName),
		EradicationTimestamp: to.Ptr(now.Add(30 * 24 * time.Hour)),
	}
	s.snapshots[snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, twinName)] = snap
	return twinName
}

// destroySnapshot soft-deletes a stored live entry the way a delete outside Terraform
// would: the original name is freed and a renamed, destroyed twin takes its place. It
// returns the twin's name, or "" when no such entry exists.
func (s *fakeSnapshotStore) destroySnapshot(resourceGroupName, storagePoolName, volumeGroupName, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, name)
	snap, ok := s.snapshots[key]
	if !ok {
		return ""
	}
	delete(s.snapshots, key)
	return s.softDeleteLocked(resourceGroupName, storagePoolName, volumeGroupName, snap, name)
}

// seedSnapshot adds a live entry directly, without going through a real create.
// createdByPolicy marks it as taken by the volume group's snapshot schedule.
func (s *fakeSnapshotStore) seedSnapshot(resourceGroupName, storagePoolName, volumeGroupName, name string, createdAt time.Time, createdByPolicy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, name)] = &armpurestorageblock.VolumeGroupSnapshot{
		ID:   to.Ptr(s.fakeSnapshotID(resourceGroupName, storagePoolName, volumeGroupName, name)),
		Name: to.Ptr(name),
		Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
			CreatedAt:         to.Ptr(createdAt),
			CreatedByPolicy:   to.Ptr(createdByPolicy),
			ProvisioningState: to.Ptr(armpurestorageblock.ProvisioningStateSucceeded),
			Space:             fakeSnapshotSpace(),
			VolumeSnapshots:   cloneVolumeSnapshotInfos(s.members),
		},
	}
}

// seedDestroyedSnapshot adds a destroyed entry directly, without going through a real
// delete: a decoy with a different previousName, or one that already existed before the
// delete under test runs, or a recovery source.
func (s *fakeSnapshotStore) seedDestroyedSnapshot(resourceGroupName, storagePoolName, volumeGroupName, twinName, previousName string, destroyedAt time.Time, createdByPolicy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, twinName)] = &armpurestorageblock.VolumeGroupSnapshot{
		ID:   to.Ptr(s.fakeSnapshotID(resourceGroupName, storagePoolName, volumeGroupName, twinName)),
		Name: to.Ptr(twinName),
		Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
			CreatedAt:         to.Ptr(destroyedAt.Add(-time.Hour)),
			CreatedByPolicy:   to.Ptr(createdByPolicy),
			ProvisioningState: to.Ptr(armpurestorageblock.ProvisioningStateSucceeded),
			Space:             fakeSnapshotSpace(),
			VolumeSnapshots:   cloneVolumeSnapshotInfos(s.members),
			SoftDeletion: &armpurestorageblock.DestroyedStateProperties{
				Destroyed:            to.Ptr(true),
				DestroyedAt:          to.Ptr(destroyedAt),
				PreviousName:         to.Ptr(previousName),
				EradicationTimestamp: to.Ptr(destroyedAt.Add(30 * 24 * time.Hour)),
			},
		},
	}
}

func cloneVolumeSnapshotInfos(in []*armpurestorageblock.VolumeSnapshotInfo) []*armpurestorageblock.VolumeSnapshotInfo {
	out := make([]*armpurestorageblock.VolumeSnapshotInfo, 0, len(in))
	for _, info := range in {
		copied := *info
		out = append(out, &copied)
	}
	return out
}

// cloneSnapshot deep-copies the parts of a snapshot this store mutates or hands back,
// so a stored entry, a recorded request body and the caller's struct never share a
// Properties pointer.
func cloneSnapshot(snap armpurestorageblock.VolumeGroupSnapshot) armpurestorageblock.VolumeGroupSnapshot {
	out := snap
	if snap.Properties == nil {
		return out
	}
	props := *snap.Properties
	props.VolumeSnapshots = cloneVolumeSnapshotInfos(snap.Properties.VolumeSnapshots)
	out.Properties = &props
	return out
}

// withoutVolumeSnapshots returns a copy of snap with volumeSnapshots and the
// create-only sourceSnapshotResourceId removed: the shape the list serves.
func withoutVolumeSnapshots(snap armpurestorageblock.VolumeGroupSnapshot) armpurestorageblock.VolumeGroupSnapshot {
	out := snap
	if snap.Properties == nil {
		return out
	}
	props := *snap.Properties
	props.VolumeSnapshots = nil
	props.SourceSnapshotResourceID = nil
	out.Properties = &props
	return out
}

// newVolumeGroupSnapshotsServer builds the fake.VolumeGroupSnapshotsServer wired to this
// store.
func (s *fakeSnapshotStore) newVolumeGroupSnapshotsServer() psbfake.VolumeGroupSnapshotsServer {
	return psbfake.VolumeGroupSnapshotsServer{
		BeginCreate: func(
			_ context.Context, resourceGroupName, storagePoolName, volumeGroupName, name string, resource armpurestorageblock.VolumeGroupSnapshot,
			_ *armpurestorageblock.VolumeGroupSnapshotsClientBeginCreateOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.VolumeGroupSnapshotsClientCreateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			reqCopy := cloneSnapshot(resource)
			s.lastCreate = &reqCopy
			s.createCallCount++

			var responder azcorefake.PollerResponder[armpurestorageblock.VolumeGroupSnapshotsClientCreateResponse]
			fail := func(status int, code string) (azcorefake.PollerResponder[armpurestorageblock.VolumeGroupSnapshotsClientCreateResponse], azcorefake.ErrorResponder) {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(status, code)
				return responder, errResp
			}

			if s.failCreatePoll {
				responder.AddNonTerminalResponse(201, nil)
				responder.SetTerminalError(400, "SnapshotCreationFailed")
				return responder, azcorefake.ErrorResponder{}
			}

			// A name already used by any snapshot, live or destroyed, is rejected.
			key := snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, name)
			if _, exists := s.snapshots[key]; exists {
				return fail(409, "SnapshotAlreadyExists")
			}

			var snap *armpurestorageblock.VolumeGroupSnapshot
			if resource.Properties != nil && resource.Properties.SourceSnapshotResourceID != nil {
				// Recovery: un-destroy the named soft-deleted snapshot and rename it to
				// the requested name. Nothing is copied, so the snapshot keeps its own
				// createdAt and member volumes.
				source, err := armids.ParseVolumeGroupSnapshotID(*resource.Properties.SourceSnapshotResourceID)
				if err != nil {
					return fail(400, "InvalidSourceSnapshot")
				}
				if !strings.EqualFold(source.ResourceGroupName, resourceGroupName) ||
					!strings.EqualFold(source.StoragePoolName, storagePoolName) ||
					!strings.EqualFold(source.VolumeGroupName, volumeGroupName) {
					return fail(400, "SourceInDifferentVolumeGroup")
				}
				sourceKey := snapshotKey(source.ResourceGroupName, source.StoragePoolName, source.VolumeGroupName, source.SnapshotName)
				existing, ok := s.snapshots[sourceKey]
				if !ok {
					return fail(404, "ResourceNotFound")
				}
				if existing.Properties == nil || !isDestroyed(existing.Properties.SoftDeletion) {
					return fail(400, "SnapshotNotDestroyed")
				}
				delete(s.snapshots, sourceKey)
				snap = existing
				snap.Properties.SoftDeletion = nil
			} else {
				snap = &armpurestorageblock.VolumeGroupSnapshot{Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
					CreatedAt:       to.Ptr(time.Now().UTC()),
					CreatedByPolicy: to.Ptr(false),
					Space:           fakeSnapshotSpace(),
					VolumeSnapshots: cloneVolumeSnapshotInfos(s.members),
				}}
			}
			snap.ID = to.Ptr(s.fakeSnapshotID(resourceGroupName, storagePoolName, volumeGroupName, name))
			snap.Name = to.Ptr(name)
			snap.Properties.ProvisioningState = to.Ptr(armpurestorageblock.ProvisioningStateSucceeded)
			s.snapshots[key] = snap

			if s.failCreatePollKeepObject {
				responder.AddNonTerminalResponse(201, nil)
				responder.SetTerminalError(400, "SnapshotCreationFailed")
				return responder, azcorefake.ErrorResponder{}
			}

			// The create response carries no member volumes: only a single GET does, so
			// the provider has to read the snapshot back to fill them in.
			responder.SetTerminalResponse(200, armpurestorageblock.VolumeGroupSnapshotsClientCreateResponse{
				VolumeGroupSnapshot: withoutVolumeSnapshots(*snap),
			}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		Get: func(
			_ context.Context, resourceGroupName, storagePoolName, volumeGroupName, name string,
			_ *armpurestorageblock.VolumeGroupSnapshotsClientGetOptions,
		) (azcorefake.Responder[armpurestorageblock.VolumeGroupSnapshotsClientGetResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.getCallCount++

			if s.getFailStatus != 0 {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(s.getFailStatus, "InternalServerError")
				return azcorefake.Responder[armpurestorageblock.VolumeGroupSnapshotsClientGetResponse]{}, errResp
			}

			snap, ok := s.snapshots[snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, name)]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "ResourceNotFound")
				return azcorefake.Responder[armpurestorageblock.VolumeGroupSnapshotsClientGetResponse]{}, errResp
			}

			// Get, unlike the list, returns the member volumes. The create-only source
			// property is never echoed.
			out := cloneSnapshot(*snap)
			if out.Properties != nil {
				out.Properties.SourceSnapshotResourceID = nil
			}

			var responder azcorefake.Responder[armpurestorageblock.VolumeGroupSnapshotsClientGetResponse]
			responder.SetResponse(200, armpurestorageblock.VolumeGroupSnapshotsClientGetResponse{VolumeGroupSnapshot: out}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginDelete: func(
			_ context.Context, resourceGroupName, storagePoolName, volumeGroupName, name string,
			_ *armpurestorageblock.VolumeGroupSnapshotsClientBeginDeleteOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.VolumeGroupSnapshotsClientDeleteResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.deleteCallCount++

			key := snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, name)
			var responder azcorefake.PollerResponder[armpurestorageblock.VolumeGroupSnapshotsClientDeleteResponse]

			if s.deleteFailOnCall == s.deleteCallCount {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(500, "InternalServerError")
				return responder, errResp
			}

			// A missing snapshot, or one whose parent is missing, deletes as a no-op
			// success. So does a second delete on a soft-deleted snapshot's original
			// name, since that name no longer exists.
			snap, ok := s.snapshots[key]
			if ok {
				destroyed := snap.Properties != nil && isDestroyed(snap.Properties.SoftDeletion)

				// The fake looks entries up by exact name; the stored spelling is
				// assumed to be what the twin's name is built from, which the entry's
				// Name field stands in for.
				ownName := name
				if snap.Name != nil {
					ownName = *snap.Name
				}

				// The first delete of a live entry frees its name immediately and leaves
				// a renamed, destroyed twin; the second delete, of that twin (destroyed
				// already true), eradicates it.
				delete(s.snapshots, key)
				if !destroyed && !s.skipTwinOnDelete {
					s.softDeleteLocked(resourceGroupName, storagePoolName, volumeGroupName, snap, ownName)
				}
			}

			// The generated client's deleteOperation only accepts 202 or 204 as the
			// initial response (not 200), so an immediate, unblocked delete's terminal
			// response must be 204.
			responder.SetTerminalResponse(204, armpurestorageblock.VolumeGroupSnapshotsClientDeleteResponse{}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		NewListByVolumeGroupPager: func(
			resourceGroupName, storagePoolName, volumeGroupName string, options *armpurestorageblock.VolumeGroupSnapshotsClientListByVolumeGroupOptions,
		) azcorefake.PagerResponder[armpurestorageblock.VolumeGroupSnapshotsClientListByVolumeGroupResponse] {
			s.mu.Lock()
			s.lastListOptions = options
			s.listCallCount++
			call, hook, ignoreSkip := s.listCallCount, s.beforeListCall, s.ignoreSkip
			s.mu.Unlock()
			if hook != nil {
				hook(call)
			}
			s.mu.Lock()
			var pager azcorefake.PagerResponder[armpurestorageblock.VolumeGroupSnapshotsClientListByVolumeGroupResponse]
			if s.missingVolumeGroups[volumeGroupKey(resourceGroupName, storagePoolName, volumeGroupName)] {
				s.mu.Unlock()
				pager.AddResponseError(404, "ResourceNotFound")
				return pager
			}

			prefix := snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, "")
			var entries []*armpurestorageblock.VolumeGroupSnapshot
			for k, snap := range s.snapshots {
				if !strings.HasPrefix(k, prefix) {
					continue
				}
				out := withoutVolumeSnapshots(*snap)
				entries = append(entries, &out)
			}
			s.mu.Unlock()

			// Applied as the backend does: filter, then order, then $skip and $top over the
			// result, answered as one page with no nextLink, $top defaulting to 25. Any
			// other filter or order expression is a test bug. Without a filter, live and
			// destroyed entries come together, told apart only by softDeletion.destroyed;
			// never with member volumes.
			if options == nil {
				options = &armpurestorageblock.VolumeGroupSnapshotsClientListByVolumeGroupOptions{}
			}
			if options.Filter != nil {
				var drop func(armpurestorageblock.VolumeGroupSnapshot) bool
				switch *options.Filter {
				case liveSnapshotsFilter:
					drop = isSnapshotSoftDeleted
				case manualSnapshotsFilter:
					drop = func(e armpurestorageblock.VolumeGroupSnapshot) bool {
						return isSnapshotSoftDeleted(e) || snapshotCreatedByPolicy(e)
					}
				default:
					panic("fake snapshot list: unsupported $filter " + *options.Filter)
				}
				// An entry with no properties is kept by either filter: nothing marks it
				// destroyed or schedule-created.
				entries = slices.DeleteFunc(entries, func(e *armpurestorageblock.VolumeGroupSnapshot) bool { return drop(*e) })
			}
			if options.Orderby != nil {
				if *options.Orderby != snapshotsOrderBy {
					panic("fake snapshot list: unsupported $orderby " + *options.Orderby)
				}
				createdAt := func(e *armpurestorageblock.VolumeGroupSnapshot) time.Time {
					if e.Properties == nil || e.Properties.CreatedAt == nil {
						return time.Time{}
					}
					return *e.Properties.CreatedAt
				}
				slices.SortFunc(entries, func(a, b *armpurestorageblock.VolumeGroupSnapshot) int {
					if c := createdAt(b).Compare(createdAt(a)); c != 0 {
						return c
					}
					// Snapshots never share a creation time on the real backend; tests seed
					// many at the same instant, so ties fall back to name to stay deterministic.
					return strings.Compare(*a.Name, *b.Name)
				})
			}
			skip, top := 0, 25
			if options.Skip != nil && !ignoreSkip {
				skip = min(int(*options.Skip), len(entries))
			}
			if options.Top != nil {
				top = int(*options.Top)
			}
			entries = entries[skip:]
			entries = entries[:min(top, len(entries))]

			pager.AddPage(200, armpurestorageblock.VolumeGroupSnapshotsClientListByVolumeGroupResponse{
				VolumeGroupSnapshotListResult: armpurestorageblock.VolumeGroupSnapshotListResult{Value: entries},
			}, nil)
			return pager
		},
	}
}

// snapshotsUnderPrefix returns every entry store currently holds in the volume group,
// for tests that need to inspect the backing store directly.
func snapshotsUnderPrefix(store *fakeSnapshotStore, resourceGroupName, storagePoolName, volumeGroupName string) []*armpurestorageblock.VolumeGroupSnapshot {
	store.mu.Lock()
	defer store.mu.Unlock()

	prefix := snapshotKey(resourceGroupName, storagePoolName, volumeGroupName, "")
	var out []*armpurestorageblock.VolumeGroupSnapshot
	for k, snap := range store.snapshots {
		if strings.HasPrefix(k, prefix) {
			out = append(out, snap)
		}
	}
	return out
}

// newTestSnapshotClient builds a VolumeGroupSnapshotsClient wired to store, with no
// provider or terraform involved.
func newTestSnapshotClient(t *testing.T, store *fakeSnapshotStore) *armpurestorageblock.VolumeGroupSnapshotsClient {
	t.Helper()
	srv := store.newVolumeGroupSnapshotsServer()
	transport := psbfake.NewVolumeGroupSnapshotsServerTransport(&srv)
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}

	client, err := armpurestorageblock.NewVolumeGroupSnapshotsClient(store.subscriptionID, &azcorefake.TokenCredential{}, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return client
}

// TestFakeSnapshotStore_LifecycleAndRecover drives Create, a Get and a list (proving
// volumeSnapshots comes back on Get only), a soft delete, a recovery of the destroyed
// twin under a new name, and an eradication — end to end through the generated client
// against this store's fake transport, with no terraform or provider code involved.
func TestFakeSnapshotStore_LifecycleAndRecover(t *testing.T) {
	store := newFakeSnapshotStore(testSubscriptionID)
	client := newTestSnapshotClient(t, store)
	ctx := context.Background()
	const rg, pool, vg = testResourceGroup, "pool1", "vg1"

	createPoller, err := client.BeginCreate(ctx, rg, pool, vg, "snap1", armpurestorageblock.VolumeGroupSnapshot{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	created, err := createPoller.PollUntilDone(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(created.Properties.VolumeSnapshots) != 0 {
		t.Fatalf("expected the create response to carry no volumeSnapshots, got %+v", created.Properties.VolumeSnapshots)
	}

	got, err := client.Get(ctx, rg, pool, vg, "snap1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Properties.VolumeSnapshots) != 1 || *got.Properties.VolumeSnapshots[0].Name != "member-vol" {
		t.Fatalf("expected Get to return the member volume, got %+v", got.Properties.VolumeSnapshots)
	}

	list := func() []*armpurestorageblock.VolumeGroupSnapshot {
		var all []*armpurestorageblock.VolumeGroupSnapshot
		pager := client.NewListByVolumeGroupPager(rg, pool, vg, nil)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			all = append(all, page.Value...)
		}
		return all
	}
	listed := list()
	if len(listed) != 1 || len(listed[0].Properties.VolumeSnapshots) != 0 {
		t.Fatalf("expected the list to omit volumeSnapshots, got %+v", listed)
	}

	deletePoller, err := client.BeginDelete(ctx, rg, pool, vg, "snap1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := deletePoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := client.Get(ctx, rg, pool, vg, "snap1", nil); err == nil {
		t.Fatal("expected Get on the original name to 404 after a soft delete")
	}
	listed = list()
	if len(listed) != 1 || !isDestroyed(listed[0].Properties.SoftDeletion) || *listed[0].Properties.SoftDeletion.PreviousName != "snap1" {
		t.Fatalf("expected one destroyed twin of snap1 in the list, got %+v", listed)
	}
	twinName := *listed[0].Name

	// Recover the twin under a different name than it had before deletion.
	recoverPoller, err := client.BeginCreate(ctx, rg, pool, vg, "snap2", armpurestorageblock.VolumeGroupSnapshot{
		Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{SourceSnapshotResourceID: listed[0].ID},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := recoverPoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := client.Get(ctx, rg, pool, vg, twinName, nil); err == nil {
		t.Fatal("expected the destroyed twin's name to be gone after a recovery: recovery renames, it does not copy")
	}
	recovered, err := client.Get(ctx, rg, pool, vg, "snap2", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isDestroyed(recovered.Properties.SoftDeletion) {
		t.Fatalf("expected the recovered snapshot to be live, got %+v", recovered.Properties.SoftDeletion)
	}
	if len(list()) != 1 {
		t.Fatalf("expected exactly one snapshot after a recovery, got %d", len(list()))
	}

	// Recovering a snapshot that is not destroyed, or into a taken name, is refused.
	for name, source := range map[string]*string{"snap3": recovered.ID, "snap2": recovered.ID} {
		_, err := client.BeginCreate(ctx, rg, pool, vg, name, armpurestorageblock.VolumeGroupSnapshot{
			Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{SourceSnapshotResourceID: source},
		}, nil)
		if err == nil {
			t.Fatalf("expected recovering into %q from a live snapshot to fail", name)
		}
	}
}

// TestFakeSnapshotStore_Hooks exercises the test-only hooks (seeding live, policy and
// destroyed entries, mutating, removing, destroying, skipping the twin on
// delete and the missing volume group) so the integration tests that depend on them
// have a proven harness.
func TestFakeSnapshotStore_Hooks(t *testing.T) {
	store := newFakeSnapshotStore(testSubscriptionID)
	client := newTestSnapshotClient(t, store)
	ctx := context.Background()
	const rg, pool, vg = testResourceGroup, "pool1", "vg1"

	createdAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store.seedSnapshot(rg, pool, vg, "17", createdAt, true)
	store.seedDestroyedSnapshot(rg, pool, vg, "snap1-20260901000000", "snap1", createdAt, false)

	policy, err := client.Get(ctx, rg, pool, vg, "17", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !snapshotCreatedByPolicy(policy.VolumeGroupSnapshot) {
		t.Fatal("expected the seeded policy snapshot to report createdByPolicy")
	}

	store.mutateSnapshot(rg, pool, vg, "17", func(s *armpurestorageblock.VolumeGroupSnapshot) {
		s.Properties.CreatedAt = to.Ptr(createdAt.Add(time.Hour))
	})
	policy, _ = client.Get(ctx, rg, pool, vg, "17", nil)
	if !policy.Properties.CreatedAt.Equal(createdAt.Add(time.Hour)) {
		t.Fatalf("expected mutateSnapshot to be visible via Get, got %v", policy.Properties.CreatedAt)
	}

	twinName := store.destroySnapshot(rg, pool, vg, "17")
	if _, err := client.Get(ctx, rg, pool, vg, "17", nil); !isNotFound(err) {
		t.Fatalf("expected destroySnapshot to free the original name, got %v", err)
	}
	policy, err = client.Get(ctx, rg, pool, vg, twinName, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isSnapshotSoftDeleted(policy.VolumeGroupSnapshot) || *policy.Properties.SoftDeletion.PreviousName != "17" {
		t.Fatalf("expected destroySnapshot to leave a destroyed twin of 17, got %+v", policy.Properties.SoftDeletion)
	}
	store.removeSnapshot(rg, pool, vg, twinName)

	missingPoller, err := client.BeginDelete(ctx, rg, pool, vg, "never-existed", nil)
	if err != nil {
		t.Fatalf("expected a delete of a missing snapshot to succeed, got %v", err)
	}
	if _, err := missingPoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("expected a delete of a missing snapshot to succeed, got %v", err)
	}

	store.seedSnapshot(rg, pool, vg, "removed", createdAt, false)
	store.removeSnapshot(rg, pool, vg, "removed")
	if _, err := client.Get(ctx, rg, pool, vg, "removed", nil); err == nil {
		t.Fatal("expected Get to fail after removeSnapshot")
	}

	store.seedSnapshot(rg, pool, vg, "snap2", createdAt, false)
	store.skipTwinOnDelete = true
	deletePoller, err := client.BeginDelete(ctx, rg, pool, vg, "snap2", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := deletePoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if remaining := snapshotsUnderPrefix(store, rg, pool, vg); len(remaining) != 1 {
		t.Fatalf("expected skipTwinOnDelete to leave only the seeded destroyed entry, got %d entries", len(remaining))
	}

	store.markVolumeGroupMissing(rg, pool, vg)
	pager := client.NewListByVolumeGroupPager(rg, pool, vg, nil)
	if _, err := pager.NextPage(ctx); !isNotFound(err) {
		t.Fatalf("expected a 404 from the list of a missing volume group, got %v", err)
	}
}

// fakeSnapshotSpace is the space every fake snapshot reports. The four values differ so
// a test that maps one field onto another's attribute fails.
func fakeSnapshotSpace() *armpurestorageblock.Space {
	return &armpurestorageblock.Space{
		TotalUsed: to.Ptr(int64(4096)),
		Unique:    to.Ptr(int64(2048)),
		Snapshots: to.Ptr(int64(1024)),
		Shared:    to.Ptr(int64(512)),
	}
}
