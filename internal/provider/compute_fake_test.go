// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	computefake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9/fake"
)

// computeLogEntry records one PUT or DELETE the fake received, in call order.
type computeLogEntry struct {
	Op   string                              // "PUT" or "DELETE"
	Key  string                              // resourceGroup/vmName/extensionName
	Body *armcompute.VirtualMachineExtension // the PUT's request body verbatim; nil for DELETE
}

// computePutOutcome is a one-shot override for the next BeginCreateOrUpdate call
// against one extension key, consumed when that call happens.
type computePutOutcome struct {
	// syncErr, when set, makes the call fail synchronously before any poller starts.
	syncErr *azcore.ResponseError

	// lroFailed, when true, makes the call's poller terminate with an error. It
	// carries no message; the scripted instanceViews entry for the same key is what a
	// following GET with $expand=instanceView reports.
	lroFailed bool
}

// fakeComputeStore is the in-memory backing store behind the integration tests'
// armcompute VirtualMachinesServer and VirtualMachineExtensionsServer fakes.
type fakeComputeStore struct {
	mu             sync.Mutex
	subscriptionID string
	vms            map[string]*armcompute.VirtualMachine          // key: resourceGroup/vmName
	extensions     map[string]*armcompute.VirtualMachineExtension // key: resourceGroup/vmName/extensionName

	// instanceViews scripts what GET $expand=instanceView returns for one extension,
	// consumed front to back; the last status keeps repeating once drained. Never
	// applied to the PUT response or a plain GET.
	instanceViews map[string][]*armcompute.InstanceViewStatus

	// nextPutOutcome is a one-shot override for the next BeginCreateOrUpdate call,
	// keyed by extension.
	nextPutOutcome map[string]*computePutOutcome

	// opLog records every PUT and DELETE this store has received, in call order.
	opLog []computeLogEntry

	// recaseIDs, when true, upper-cases every returned VM/extension ID and name,
	// regardless of what was requested.
	recaseIDs bool

	// expandedGetCounts counts how many times GET $expand=instanceView has been
	// served for one extension key.
	expandedGetCounts map[string]int
}

func newFakeComputeStore(subscriptionID string) *fakeComputeStore {
	return &fakeComputeStore{
		subscriptionID:    subscriptionID,
		vms:               map[string]*armcompute.VirtualMachine{},
		extensions:        map[string]*armcompute.VirtualMachineExtension{},
		instanceViews:     map[string][]*armcompute.InstanceViewStatus{},
		nextPutOutcome:    map[string]*computePutOutcome{},
		expandedGetCounts: map[string]int{},
	}
}

func computeVMKey(resourceGroupName, vmName string) string {
	return resourceGroupName + "/" + vmName
}

func computeExtensionKey(resourceGroupName, vmName, extensionName string) string {
	return resourceGroupName + "/" + vmName + "/" + extensionName
}

func (s *fakeComputeStore) fakeVMID(resourceGroupName, vmName string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s",
		s.subscriptionID, resourceGroupName, vmName,
	)
}

func (s *fakeComputeStore) fakeExtensionID(resourceGroupName, vmName, name string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s/extensions/%s",
		s.subscriptionID, resourceGroupName, vmName, name,
	)
}

// defaultExtensionSuccessStatus is the status reported once iSCSI configuration
// finishes cleanly.
func defaultExtensionSuccessStatus() *armcompute.InstanceViewStatus {
	return &armcompute.InstanceViewStatus{
		Code:    to.Ptr("ProvisioningState/succeeded"),
		Level:   to.Ptr(armcompute.StatusLevelTypesInfo),
		Message: to.Ptr("Enable succeeded: iSCSI configuration completed successfully"),
	}
}

// cloneExtension deep-copies Properties so a stored entry, a recorded PUT body and a
// GET response never share a pointer.
func cloneExtension(ext armcompute.VirtualMachineExtension) armcompute.VirtualMachineExtension {
	out := ext
	if ext.Properties != nil {
		props := *ext.Properties
		out.Properties = &props
	}
	return out
}

// recasedVM upper-cases the VM's ID and name when recaseIDs is enabled.
func (s *fakeComputeStore) recasedVM(vm armcompute.VirtualMachine) armcompute.VirtualMachine {
	if !s.recaseIDs {
		return vm
	}
	out := vm
	if vm.ID != nil {
		out.ID = to.Ptr(strings.ToUpper(*vm.ID))
	}
	if vm.Name != nil {
		out.Name = to.Ptr(strings.ToUpper(*vm.Name))
	}
	return out
}

// recasedExtension upper-cases the extension's ID and name when recaseIDs is enabled.
func (s *fakeComputeStore) recasedExtension(ext armcompute.VirtualMachineExtension) armcompute.VirtualMachineExtension {
	if !s.recaseIDs {
		return ext
	}
	out := ext
	if ext.ID != nil {
		out.ID = to.Ptr(strings.ToUpper(*ext.ID))
	}
	if ext.Name != nil {
		out.Name = to.Ptr(strings.ToUpper(*ext.Name))
	}
	return out
}

// nextInstanceView pops the front of key's scripted instanceViews queue, leaving the
// last entry in place once drained. Returns nil if nothing was scripted for key.
// Callers must hold s.mu.
func (s *fakeComputeStore) nextInstanceView(key string) *armcompute.VirtualMachineExtensionInstanceView {
	queue := s.instanceViews[key]
	if len(queue) == 0 {
		return nil
	}
	status := queue[0]
	if len(queue) > 1 {
		s.instanceViews[key] = queue[1:]
	}
	return &armcompute.VirtualMachineExtensionInstanceView{
		Statuses: []*armcompute.InstanceViewStatus{status},
	}
}

// newFakeResponseError builds an *azcore.ResponseError whose Error() text embeds
// message verbatim. azcorefake's error responders rebuild RawResponse from just a
// status/error code before returning, discarding any body set here, so Error() is
// called once up front to cache the rendered text before that happens.
func newFakeResponseError(httpStatus int, errorCode, message string) *azcore.ResponseError {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, errorCode, message)
	respErr := &azcore.ResponseError{
		ErrorCode:  errorCode,
		StatusCode: httpStatus,
		RawResponse: &http.Response{
			StatusCode: httpStatus,
			Status:     fmt.Sprintf("%d %s", httpStatus, http.StatusText(httpStatus)),
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(body)),
		},
	}
	_ = respErr.Error()
	return respErr
}

// duplicateExtensionMessage builds the error text ARM returns for a second VMExtension
// instance of the same publisher+type on one VM.
func duplicateExtensionMessage(osType, handler, newName string) string {
	return fmt.Sprintf(
		"Multiple VMExtensions per handler not supported for OS type '%s'. VMExtension '%s' with handler '%s' already added or specified in input.",
		osType, newName, handler,
	)
}

// duplicateHandler returns the synchronous 400 ARM raises when extName would become a
// second instance of the same publisher+type on vmName. Returns nil when no collision
// exists. Callers must hold s.mu.
func (s *fakeComputeStore) duplicateHandler(resourceGroupName, vmName, extName string, params armcompute.VirtualMachineExtension) *azcore.ResponseError {
	if params.Properties == nil || params.Properties.Publisher == nil || params.Properties.Type == nil {
		return nil
	}
	prefix := resourceGroupName + "/" + vmName + "/"
	ownKey := prefix + extName
	for key, existing := range s.extensions {
		if key == ownKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		if existing.Properties == nil || existing.Properties.Publisher == nil || existing.Properties.Type == nil {
			continue
		}
		if *existing.Properties.Publisher != *params.Properties.Publisher || *existing.Properties.Type != *params.Properties.Type {
			continue
		}

		osType := ""
		if vm, ok := s.vms[computeVMKey(resourceGroupName, vmName)]; ok && vm.Properties != nil &&
			vm.Properties.StorageProfile != nil && vm.Properties.StorageProfile.OSDisk != nil &&
			vm.Properties.StorageProfile.OSDisk.OSType != nil {
			osType = string(*vm.Properties.StorageProfile.OSDisk.OSType)
		}
		handler := *params.Properties.Publisher + "." + *params.Properties.Type
		return newFakeResponseError(http.StatusBadRequest, "BadRequest", duplicateExtensionMessage(osType, handler, extName))
	}
	return nil
}

// seedVirtualMachine inserts a VM directly into the store, as if it already existed in
// Azure.
func (s *fakeComputeStore) seedVirtualMachine(resourceGroupName, vmName string, osType armcompute.OperatingSystemTypes, location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vms[computeVMKey(resourceGroupName, vmName)] = &armcompute.VirtualMachine{
		ID:       to.Ptr(s.fakeVMID(resourceGroupName, vmName)),
		Name:     to.Ptr(vmName),
		Location: to.Ptr(location),
		Properties: &armcompute.VirtualMachineProperties{
			StorageProfile: &armcompute.StorageProfile{
				OSDisk: &armcompute.OSDisk{OSType: to.Ptr(osType)},
			},
		},
	}
}

// removeVirtualMachine deletes a VM directly, as if it were removed outside Terraform.
func (s *fakeComputeStore) removeVirtualMachine(resourceGroupName, vmName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vms, computeVMKey(resourceGroupName, vmName))
}

// seedExtension inserts an extension directly into the store, as if it already existed
// in Azure. settings is stored verbatim. Its instance view defaults to a success
// status; override it with setInstanceViewSequence.
func (s *fakeComputeStore) seedExtension(resourceGroupName, vmName, name, publisher, extensionType string, settings any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var location *string
	if vm, ok := s.vms[computeVMKey(resourceGroupName, vmName)]; ok {
		location = vm.Location
	}

	key := computeExtensionKey(resourceGroupName, vmName, name)
	s.extensions[key] = &armcompute.VirtualMachineExtension{
		ID:       to.Ptr(s.fakeExtensionID(resourceGroupName, vmName, name)),
		Name:     to.Ptr(name),
		Location: location,
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher:         to.Ptr(publisher),
			Type:              to.Ptr(extensionType),
			Settings:          settings,
			ProvisioningState: to.Ptr("Succeeded"),
		},
	}
	if _, ok := s.instanceViews[key]; !ok {
		s.instanceViews[key] = []*armcompute.InstanceViewStatus{defaultExtensionSuccessStatus()}
	}
}

// mutateExtensionSettings overwrites one extension's settings out of band, without
// going through BeginCreateOrUpdate.
func (s *fakeComputeStore) mutateExtensionSettings(resourceGroupName, vmName, name string, settings any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ext, ok := s.extensions[computeExtensionKey(resourceGroupName, vmName, name)]; ok && ext.Properties != nil {
		ext.Properties.Settings = settings
	}
}

// setExtensionProvisioningState overwrites one extension's provisioning state out of
// band, standing in for a failed run the guest reported after the PUT was accepted.
func (s *fakeComputeStore) setExtensionProvisioningState(resourceGroupName, vmName, name, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ext, ok := s.extensions[computeExtensionKey(resourceGroupName, vmName, name)]; ok && ext.Properties != nil {
		ext.Properties.ProvisioningState = to.Ptr(state)
	}
}

// extensionForceUpdateTag returns the forceUpdateTag of the extension's stored model,
// that is, of the last PUT it received.
func (s *fakeComputeStore) extensionForceUpdateTag(resourceGroupName, vmName, name string) *string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ext, ok := s.extensions[computeExtensionKey(resourceGroupName, vmName, name)]; ok && ext.Properties != nil {
		return ext.Properties.ForceUpdateTag
	}
	return nil
}

// removeExtensionOutOfBand deletes an entry directly, without going through
// BeginDelete.
func (s *fakeComputeStore) removeExtensionOutOfBand(resourceGroupName, vmName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := computeExtensionKey(resourceGroupName, vmName, name)
	delete(s.extensions, key)
	delete(s.instanceViews, key)
}

// setInstanceViewSequence scripts what GET $expand=instanceView returns for one
// extension across successive calls, consumed front to back; the last status keeps
// repeating once drained.
func (s *fakeComputeStore) setInstanceViewSequence(resourceGroupName, vmName, name string, statuses ...*armcompute.InstanceViewStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instanceViews[computeExtensionKey(resourceGroupName, vmName, name)] = statuses
}

// queueLROFailure arranges the next BeginCreateOrUpdate call for one extension to
// terminate its poller with an error, and scripts status as what a following GET with
// $expand=instanceView reports.
func (s *fakeComputeStore) queueLROFailure(resourceGroupName, vmName, name string, status *armcompute.InstanceViewStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := computeExtensionKey(resourceGroupName, vmName, name)
	s.nextPutOutcome[key] = &computePutOutcome{lroFailed: true}
	s.instanceViews[key] = []*armcompute.InstanceViewStatus{status}
}

// queueLROFailurePreservingInstanceView is like queueLROFailure but leaves whatever
// instance view is already scripted for key untouched.
func (s *fakeComputeStore) queueLROFailurePreservingInstanceView(resourceGroupName, vmName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := computeExtensionKey(resourceGroupName, vmName, name)
	s.nextPutOutcome[key] = &computePutOutcome{lroFailed: true}
}

// queuePutFailure arranges the next BeginCreateOrUpdate call for one extension to fail
// synchronously, before any poller starts.
func (s *fakeComputeStore) queuePutFailure(resourceGroupName, vmName, name string, httpStatus int, errorCode, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := computeExtensionKey(resourceGroupName, vmName, name)
	s.nextPutOutcome[key] = &computePutOutcome{syncErr: newFakeResponseError(httpStatus, errorCode, message)}
}

// expandedInstanceViewGetCount returns how many times GET $expand=instanceView has
// been served for one extension key so far.
func (s *fakeComputeStore) expandedInstanceViewGetCount(resourceGroupName, vmName, name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expandedGetCounts[computeExtensionKey(resourceGroupName, vmName, name)]
}

// operationLog returns a copy of every PUT and DELETE this store has received, in call
// order.
func (s *fakeComputeStore) operationLog() []computeLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]computeLogEntry, len(s.opLog))
	copy(out, s.opLog)
	return out
}

// newVirtualMachinesServer builds the fake.VirtualMachinesServer wired to this store.
// Only Get is implemented; every other method is left nil so an unexpected use fails
// loudly.
func (s *fakeComputeStore) newVirtualMachinesServer() computefake.VirtualMachinesServer {
	return computefake.VirtualMachinesServer{
		Get: func(
			_ context.Context, resourceGroupName, vmName string, _ *armcompute.VirtualMachinesClientGetOptions,
		) (azcorefake.Responder[armcompute.VirtualMachinesClientGetResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			vm, ok := s.vms[computeVMKey(resourceGroupName, vmName)]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(http.StatusNotFound, "ResourceNotFound")
				return azcorefake.Responder[armcompute.VirtualMachinesClientGetResponse]{}, errResp
			}

			var responder azcorefake.Responder[armcompute.VirtualMachinesClientGetResponse]
			responder.SetResponse(http.StatusOK, armcompute.VirtualMachinesClientGetResponse{VirtualMachine: s.recasedVM(*vm)}, nil)
			return responder, azcorefake.ErrorResponder{}
		},
	}
}

// newVirtualMachineExtensionsServer builds the fake.VirtualMachineExtensionsServer
// wired to this store: Get, List, BeginCreateOrUpdate and BeginDelete. BeginUpdate is
// left nil so an unexpected use fails loudly.
func (s *fakeComputeStore) newVirtualMachineExtensionsServer() computefake.VirtualMachineExtensionsServer {
	return computefake.VirtualMachineExtensionsServer{
		Get: func(
			_ context.Context, resourceGroupName, vmName, extensionName string, options *armcompute.VirtualMachineExtensionsClientGetOptions,
		) (azcorefake.Responder[armcompute.VirtualMachineExtensionsClientGetResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			key := computeExtensionKey(resourceGroupName, vmName, extensionName)
			ext, ok := s.extensions[key]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(http.StatusNotFound, "ResourceNotFound")
				return azcorefake.Responder[armcompute.VirtualMachineExtensionsClientGetResponse]{}, errResp
			}

			out := cloneExtension(*ext)
			if options != nil && options.Expand != nil && strings.Contains(*options.Expand, "instanceView") {
				s.expandedGetCounts[key]++
				out.Properties.InstanceView = s.nextInstanceView(key)
			}
			out = s.recasedExtension(out)

			var responder azcorefake.Responder[armcompute.VirtualMachineExtensionsClientGetResponse]
			responder.SetResponse(http.StatusOK, armcompute.VirtualMachineExtensionsClientGetResponse{VirtualMachineExtension: out}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		List: func(
			_ context.Context, resourceGroupName, vmName string, _ *armcompute.VirtualMachineExtensionsClientListOptions,
		) (azcorefake.Responder[armcompute.VirtualMachineExtensionsClientListResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			prefix := resourceGroupName + "/" + vmName + "/"
			var keys []string
			for key := range s.extensions {
				if strings.HasPrefix(key, prefix) {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)

			values := make([]*armcompute.VirtualMachineExtension, 0, len(keys))
			for _, key := range keys {
				out := s.recasedExtension(cloneExtension(*s.extensions[key]))
				values = append(values, &out)
			}

			var responder azcorefake.Responder[armcompute.VirtualMachineExtensionsClientListResponse]
			responder.SetResponse(http.StatusOK, armcompute.VirtualMachineExtensionsClientListResponse{
				VirtualMachineExtensionsListResult: armcompute.VirtualMachineExtensionsListResult{Value: values},
			}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginCreateOrUpdate: func(
			_ context.Context, resourceGroupName, vmName, extensionName string, extensionParameters armcompute.VirtualMachineExtension,
			_ *armcompute.VirtualMachineExtensionsClientBeginCreateOrUpdateOptions,
		) (azcorefake.PollerResponder[armcompute.VirtualMachineExtensionsClientCreateOrUpdateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			var responder azcorefake.PollerResponder[armcompute.VirtualMachineExtensionsClientCreateOrUpdateResponse]

			key := computeExtensionKey(resourceGroupName, vmName, extensionName)
			bodyCopy := cloneExtension(extensionParameters)
			s.opLog = append(s.opLog, computeLogEntry{Op: "PUT", Key: key, Body: &bodyCopy})

			// A real PUT needs a parent virtual machine and a non-empty Location;
			// reject both like ARM does.
			if _, ok := s.vms[computeVMKey(resourceGroupName, vmName)]; !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(http.StatusNotFound, "ResourceNotFound")
				return responder, errResp
			}
			if extensionParameters.Location == nil || *extensionParameters.Location == "" {
				var errResp azcorefake.ErrorResponder
				errResp.SetError(newFakeResponseError(http.StatusBadRequest, "BadRequest", "Location is required."))
				return responder, errResp
			}

			if dup := s.duplicateHandler(resourceGroupName, vmName, extensionName, extensionParameters); dup != nil {
				var errResp azcorefake.ErrorResponder
				errResp.SetError(dup)
				return responder, errResp
			}

			outcome := s.nextPutOutcome[key]
			delete(s.nextPutOutcome, key)

			if outcome != nil && outcome.syncErr != nil {
				var errResp azcorefake.ErrorResponder
				errResp.SetError(outcome.syncErr)
				return responder, errResp
			}

			ext := bodyCopy
			ext.ID = to.Ptr(s.fakeExtensionID(resourceGroupName, vmName, extensionName))
			ext.Name = to.Ptr(extensionName)
			if ext.Properties == nil {
				ext.Properties = &armcompute.VirtualMachineExtensionProperties{}
			}
			ext.Properties.InstanceView = nil // the PUT response never carries an instance view
			if outcome != nil && outcome.lroFailed {
				ext.Properties.ProvisioningState = to.Ptr("Failed")
			} else {
				ext.Properties.ProvisioningState = to.Ptr("Succeeded")
			}
			s.extensions[key] = &ext
			if _, ok := s.instanceViews[key]; !ok {
				s.instanceViews[key] = []*armcompute.InstanceViewStatus{defaultExtensionSuccessStatus()}
			}

			if outcome != nil && outcome.lroFailed {
				// A real PUT-based LRO is always accepted first and fails only on a
				// later poll, so send one non-terminal response before the terminal
				// error.
				responder.AddNonTerminalResponse(http.StatusOK, nil)
				responder.SetTerminalError(http.StatusOK, "VMExtensionProvisioningError")
				return responder, azcorefake.ErrorResponder{}
			}

			responder.SetTerminalResponse(http.StatusOK, armcompute.VirtualMachineExtensionsClientCreateOrUpdateResponse{
				VirtualMachineExtension: s.recasedExtension(cloneExtension(ext)),
			}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginDelete: func(
			_ context.Context, resourceGroupName, vmName, extensionName string, _ *armcompute.VirtualMachineExtensionsClientBeginDeleteOptions,
		) (azcorefake.PollerResponder[armcompute.VirtualMachineExtensionsClientDeleteResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			key := computeExtensionKey(resourceGroupName, vmName, extensionName)
			s.opLog = append(s.opLog, computeLogEntry{Op: "DELETE", Key: key})

			var responder azcorefake.PollerResponder[armcompute.VirtualMachineExtensionsClientDeleteResponse]
			if _, ok := s.extensions[key]; !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(http.StatusNotFound, "ResourceNotFound")
				return responder, errResp
			}

			delete(s.extensions, key)
			delete(s.instanceViews, key)

			responder.SetTerminalResponse(http.StatusOK, armcompute.VirtualMachineExtensionsClientDeleteResponse{}, nil)
			return responder, azcorefake.ErrorResponder{}
		},
	}
}

// multiModuleTransport dispatches an ARM fake request to the right SDK module's
// fake.ServerFactoryTransport, keyed by the client name stamped into the request
// context under runtime.CtxAPINameKey.
type multiModuleTransport struct {
	purestorageblock policy.Transporter
	compute          policy.Transporter
}

func (t *multiModuleTransport) Do(req *http.Request) (*http.Response, error) {
	name, _ := req.Context().Value(runtime.CtxAPINameKey{}).(string)
	if strings.HasPrefix(name, "VirtualMachine") {
		return t.compute.Do(req)
	}
	return t.purestorageblock.Do(req)
}

// selfHealingTransport rebuilds a generated fake's transport after any failed call: the
// generated dispatch code never clears its poller tracker on a terminal error, so a later
// call to the same URL would otherwise reuse the drained poller. build must return a
// transport over the same stores each time, so nothing stored is lost by a rebuild.
type selfHealingTransport struct {
	build func() policy.Transporter

	mu      sync.Mutex
	current policy.Transporter
}

func (t *selfHealingTransport) Do(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if t.current == nil {
		t.current = t.build()
	}
	current := t.current
	t.mu.Unlock()

	resp, err := current.Do(req)
	if err != nil {
		t.mu.Lock()
		t.current = nil
		t.mu.Unlock()
	}
	return resp, err
}

// newSelfHealingComputeTransport builds the compute-side transport integration tests
// should use in place of a single shared computefake.NewServerFactoryTransport.
func (s *fakeComputeStore) newSelfHealingComputeTransport() policy.Transporter {
	return &selfHealingTransport{build: func() policy.Transporter {
		return computefake.NewServerFactoryTransport(&computefake.ServerFactory{
			VirtualMachinesServer:          s.newVirtualMachinesServer(),
			VirtualMachineExtensionsServer: s.newVirtualMachineExtensionsServer(),
		})
	}}
}
