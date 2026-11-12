// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

// This file holds error texts, and the branching that picks between the texts of one
// cause, such as a soft-deleted object with or without an eradication time. It holds no
// other logic. Which cause applies is decided by the caller, using the classifiers in
// api_errors.go.
//
// A diagnostic is a frame around a cause. The frame is the summary (the title line) and
// where the error points, chosen by what the user was doing, e.g. Failed to Create Volume
// Group "vg1". The cause is the detail, chosen by why it happened, e.g. Storage pool … was
// not found. One cause fits every frame, so the same reason reads the same wherever it is
// reported.
//
// To edit a text, change the words and keep every %s, %q and %d, in the same order: the
// arguments are passed in that order. A cause that wraps an Azure response ends with the
// response as the service sent it, then the request IDs support asks for.
//
// Style: a summary is in Title Case with no final period, like the framework's validators'.
// A detail is a capitalised sentence ending with a period. The exceptions are the reasons
// finishing a validator's "Attribute <path> <reason>, got: <value>", which follow that
// template, and a cause that ends with text it embeds, such as an Azure response.

const docsBaseURL = "https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs"

const (
	withResponse   = "\n\n%s\n\nRequest IDs (include these when contacting support): %s"
	withRequestIDs = "\n\nRequest IDs (include these when contacting support): %s"
)

// Validation rules: the reason of a validator from the framework's validator package, the
// end of its sentence "Attribute <path> <reason>, got: <value>".
const (
	storagePoolNameRule = "must be 1-127 characters long and contain only letters, numbers, hyphens and underscores"
	locationRule        = "must be the canonical short region name (e.g. \"eastus2\"), not a display name " +
		"like \"East US 2\" — ARM stores the canonical form, so a display name would " +
		"drift into a replacement on every plan"
	zoneRule              = "must be a positive integer, e.g. \"1\", or empty for a region with no availability zones"
	storageObjectNameRule = "must be 2-40 characters, start with a letter, end with a letter or digit, " +
		"and contain only letters, numbers and hyphens"
	snapshotLookupNameRule = "must be 1-64 characters, start and end with a letter or digit, and contain only letters, numbers and hyphens"
	notMultipleOf512Rule   = "must be a multiple of 512"
	notISODurationRule     = "must be an ISO 8601 duration in days or smaller, e.g. P1D or PT12H"
)

// detailError is a failure the provider can explain: an error whose text is the detail of a
// diagnostic. It is built only by the constructors in this file, one per cause, so no
// other error, such as one straight from the SDK, is reported before it is classified.
// code names the cause and does not change when its words do; tests compare it instead of
// the text.
type detailError struct{ code, message string }

func newDetailError(code, message string) detailError {
	return detailError{code: code, message: message}
}

func (e detailError) Error() string { return e.message }

// Frames.

// operationDiagnostic is the frame for a failure of the operation the user asked for, on
// the object named name: Failed to Create Volume Group "vg1". An unnamed operation has no
// single object, such as a list, and its summary drops the name: Failed to List Volumes.
type operationDiagnostic struct {
	op     operation
	detail detailError
}

// fail frames c as a failure of op.
func (op operation) fail(c detailError) operationDiagnostic {
	return operationDiagnostic{op: op, detail: c}
}

func (d operationDiagnostic) Severity() diag.Severity { return diag.SeverityError }
func (d operationDiagnostic) Summary() string         { return operationSummary(d.op) }
func (d operationDiagnostic) Detail() string          { return d.detail.Error() }

func (d operationDiagnostic) Equal(other diag.Diagnostic) bool {
	o, ok := other.(operationDiagnostic)
	return ok && d.Summary() == o.Summary() && d.Detail() == o.Detail()
}

// operationSummary quotes the name of every operation that is not unnamed, even when it
// is empty: a name that went missing then shows as "" in the summary, instead of silently
// vanishing as it would if an empty name meant unnamed.
func operationSummary(op operation) string {
	if op.unnamed {
		return fmt.Sprintf("Failed to %s %s", op.action.summary, op.kind.summary)
	}
	return fmt.Sprintf("Failed to %s %s %q", op.action.summary, op.kind.summary, op.name)
}

// framedDiagnostic is a frame whose summary does not depend on an operation.
type framedDiagnostic struct {
	summary string
	detail  detailError
}

func (d framedDiagnostic) Severity() diag.Severity { return diag.SeverityError }
func (d framedDiagnostic) Summary() string         { return d.summary }
func (d framedDiagnostic) Detail() string          { return d.detail.Error() }

func (d framedDiagnostic) Equal(other diag.Diagnostic) bool {
	o, ok := other.(framedDiagnostic)
	return ok && d.Summary() == o.Summary() && d.Detail() == o.Detail()
}

// invalidAttributeValueDiagnostic is the frame for a value found wrong before anything is
// applied, in validation or in a plan check. It is the wording Terraform's own
// validators use.
func invalidAttributeValueDiagnostic(c detailError) framedDiagnostic {
	return framedDiagnostic{summary: "Invalid Attribute Value", detail: c}
}

// invalidAttributeCombinationDiagnostic is invalidAttributeValueDiagnostic for attributes
// that are wrong together rather than one by one: one that requires, or excludes, another.
func invalidAttributeCombinationDiagnostic(c detailError) framedDiagnostic {
	return framedDiagnostic{summary: "Invalid Attribute Combination", detail: c}
}

// attributeValidatorError is the frame of a custom validator for one attribute. Its detail is
// the template Terraform's own validators use, so it reads the same as theirs: reason is
// the end of a sentence about the attribute, such as "must be a multiple of 512".
func attributeValidatorError(attr path.Path, reason string, value string) diag.Diagnostic {
	return diag.NewAttributeErrorDiagnostic(attr, "Invalid Attribute Value",
		fmt.Sprintf("Attribute %s %s, got: %s", attr, reason, value))
}

// providerInternalDiagnostic is the frame for a check that only fires when the provider's
// own code is wrong.
func providerInternalDiagnostic(c detailError) framedDiagnostic {
	return framedDiagnostic{summary: "Provider Internal Error", detail: c}
}

// deletionProtectedDiagnostic blocks a destroy or a replace. Nothing failed, so it is not
// an operation failure. A replace points it at the attribute whose change forces it, with
// diag.WithPath.
func deletionProtectedDiagnostic(k kind, c detailError) framedDiagnostic {
	return framedDiagnostic{summary: k.summary + " Is Deletion-Protected", detail: c}
}

// unexpectedResponseDiagnostic is the frame for an answer from Azure that the provider
// cannot use. It is never a user mistake.
func unexpectedResponseDiagnostic(c detailError) framedDiagnostic {
	return framedDiagnostic{summary: "Unexpected API Response", detail: c}
}

// Operations. summary is the Title Case form in a summary, detail the lower case form in
// a sentence.

// verb is the action word of an operation. It is not called action because the
// framework package of that name is imported in this package.
type verb struct{ summary, detail string }

type kind struct{ summary, detail string }

// operation is what the user asked the provider to do. It is always the user's own
// operation, never the internal call that failed. name is the object's short name, in the
// summary; id is its full ARM ID, in the detail, once it is known. in is the ARM ID of the
// container the operation acts in, such as the parent a list lists. unnamed marks an
// operation without a single object to name, such as a list.
type operation struct {
	action  verb
	kind    kind
	name    string
	id      string
	in      string
	unnamed bool
}

// target is where the operation's detail says it happened: the object's own ID, or, while
// that is not known, the container it acts in.
func (op operation) target() string {
	if op.id != "" {
		return op.id
	}
	return "in " + op.in
}

// storedIn is where Terraform holds a value the provider reads back.
type storedIn struct{ detail string }

var (
	storedInState = storedIn{"state"}
	storedInPlan  = storedIn{"plan"}
)

// step is a call on another object that an operation made and that failed.
type step struct {
	action verb
	kind   kind
	id     string
}

var (
	actionPlan      = verb{"Plan", "plan"}
	actionCreate    = verb{"Create", "create"}
	actionRetrieve  = verb{"Retrieve", "retrieve"}
	actionList      = verb{"List", "list"}
	actionUpdate    = verb{"Update", "update"}
	actionDelete    = verb{"Delete", "delete"}
	actionEradicate = verb{"Eradicate", "eradicate"}
	actionImport    = verb{"Import", "import"}
	actionMove      = verb{"Move", "move"}
	actionConfigure = verb{"Configure", "configure"}
)

var (
	kindStoragePool                     = kind{"Storage Pool", "storage pool"}
	kindStoragePools                    = kind{"Storage Pools", "storage pools"}
	kindReservation                     = kind{"Reservation", "reservation"}
	kindReservations                    = kind{"Reservations", "reservations"}
	kindVolumeGroup                     = kind{"Volume Group", "volume group"}
	kindVolumeGroups                    = kind{"Volume Groups", "volume groups"}
	kindVolume                          = kind{"Volume", "volume"}
	kindVolumes                         = kind{"Volumes", "volumes"}
	kindSnapshot                        = kind{"Snapshot", "snapshot"}
	kindSnapshots                       = kind{"Snapshots", "snapshots"}
	kindRecoverableVolumeGroup          = kind{"Recoverable Volume Group", "recoverable volume group"}
	kindRecoverableVolumeGroups         = kind{"Recoverable Volume Groups", "recoverable volume groups"}
	kindVirtualMachine                  = kind{"Virtual Machine", "virtual machine"}
	kindResourceGroup                   = kind{"Resource Group", "resource group"}
	kindVirtualMachines                 = kind{"Virtual Machines", "virtual machines"}
	kindVirtualMachineExtension         = kind{"Virtual Machine Extension", "virtual machine extension"}
	kindVirtualMachineExtensions        = kind{"Virtual Machine Extensions", "virtual machine extensions"}
	kindVirtualMachineConnection        = kind{"Virtual Machine Connection", "virtual machine connection"}
	kindStoragePoolStatus               = kind{"Storage Pool Status", "storage pool status"}
	kindVolumeGroupStatus               = kind{"Volume Group Status", "volume group status"}
	kindVolumeGroupConnectionParameters = kind{"Volume Group Connection Parameters", "volume group connection parameters"}
	kindPlatformConsoleActivationCode   = kind{"Platform Console Activation Code", "platform console activation code"}
	kindPlatformConsoleUser             = kind{"Platform Console User", "platform console user"}
	kindProvider                        = kind{"Provider", "provider"}
)

// operationConfigureProvider is the operation of the provider block, and of anything
// that needs the provider's clients before the provider has any.
var operationConfigureProvider = operation{action: actionConfigure, kind: kindProvider, unnamed: true}

// Causes. One constructor per cause, each returning a detailError by value. A detailError
// has no pointer form of its own: a function that may have no cause returns *detailError,
// and nil is the absence.

// Causes: provider configuration.

// newProviderNotConfiguredError can't name the unknown value: Configure defers while
// subscription_id or any authentication attribute is still unknown. A data source's
// Read and an ephemeral resource's Open hit it during plan itself, so the plan fails
// until whatever the provider configuration depends on has been applied.
func newProviderNotConfiguredError() detailError {
	return newDetailError("provider_not_configured", "The provider configuration contains values that are not yet known in this plan. "+
		"This resolves once they are known, e.g. after the resource they depend on is applied.")
}

func newSubscriptionIDNotSetError() detailError {
	return newDetailError("subscription_id_not_set", "The subscription_id must be set in the provider block or via ARM_SUBSCRIPTION_ID or AZURE_SUBSCRIPTION_ID.")
}

func newAzureCredentialFailedError(reason error) detailError {
	return newDetailError("azure_credential_failed", fmt.Sprintf("Could not create the Azure credential: %s", reason))
}

// Causes: authentication attributes, found wrong before any token is requested.

func newClientIDNotSetError() detailError {
	return newDetailError("client_id_not_set", "The client_id is required when client_secret, client_secret_file_path, client_certificate, "+
		"client_certificate_path, use_oidc or use_aks_workload_identity is set. Set client_id or "+
		"client_id_file_path, or ARM_CLIENT_ID / AZURE_CLIENT_ID (the AKS pod identity webhook sets "+
		"AZURE_CLIENT_ID automatically when use_aks_workload_identity is true).")
}

func newTenantIDNotSetError() detailError {
	return newDetailError("tenant_id_not_set", "The tenant_id is required when client_secret, client_secret_file_path, client_certificate, "+
		"client_certificate_path, use_oidc or use_aks_workload_identity is set. Set tenant_id, or "+
		"ARM_TENANT_ID / AZURE_TENANT_ID (the AKS pod identity webhook sets AZURE_TENANT_ID "+
		"automatically when use_aks_workload_identity is true).")
}

func newAzurePipelinesRequestTokenNotSetError() detailError {
	return newDetailError("azure_pipelines_request_token_not_set", "The oidc_azure_service_connection_id (ARM_ADO_PIPELINE_SERVICE_CONNECTION_ID) is set, but "+
		"oidc_request_token did not resolve. Azure Pipelines requires both: set oidc_request_token, "+
		"ARM_OIDC_REQUEST_TOKEN, or run where SYSTEM_ACCESSTOKEN is set. Azure Pipelines only exposes "+
		"SYSTEM_ACCESSTOKEN to a step that maps it explicitly, with "+
		"`env: SYSTEM_ACCESSTOKEN: $(System.AccessToken)`.")
}

func newNotInAzurePipelinesError() detailError {
	return newDetailError("not_in_azure_pipelines", "The oidc_azure_service_connection_id is set, but SYSTEM_OIDCREQUESTURI is not present in the "+
		"environment. Azure Pipelines sets this automatically for every job, so this flow only "+
		"works when running inside an Azure Pipelines job.")
}

func newOidcInputsNotSetError() detailError {
	return newDetailError("oidc_inputs_not_set", "The use_oidc (ARM_USE_OIDC) is true, but none of oidc_token (ARM_OIDC_TOKEN), oidc_token_file_path "+
		"(ARM_OIDC_TOKEN_FILE_PATH), oidc_request_url (ARM_OIDC_REQUEST_URL) together with "+
		"oidc_request_token (ARM_OIDC_REQUEST_TOKEN), or oidc_azure_service_connection_id "+
		"(ARM_ADO_PIPELINE_SERVICE_CONNECTION_ID) together with oidc_request_token, resolved to a value.")
}

func newAksTokenFileNotSetError() detailError {
	return newDetailError("aks_token_file_not_set", "The use_aks_workload_identity (ARM_USE_AKS_WORKLOAD_IDENTITY) is true, but oidc_token_file_path did not "+
		"resolve. Set oidc_token_file_path, ARM_OIDC_TOKEN_FILE_PATH, or run where AZURE_FEDERATED_TOKEN_FILE "+
		"is set, as the AKS pod identity webhook does.")
}

func newAksTenantMismatchError(tenantID string, azureTenantID string) detailError {
	return newDetailError("aks_tenant_mismatch", fmt.Sprintf(
		"The tenant_id (or ARM_TENANT_ID) resolved to %q, but use_aks_workload_identity "+
			"(ARM_USE_AKS_WORKLOAD_IDENTITY) is true and AZURE_TENANT_ID is %q. The federated token AKS "+
			"mounts is only valid for the tenant AZURE_TENANT_ID names. Remove tenant_id and ARM_TENANT_ID, "+
			"or set either to the same value as AZURE_TENANT_ID.",
		tenantID, azureTenantID,
	))
}

func newNoAuthMethodConfiguredError() detailError {
	return newDetailError("no_auth_method_configured", "The use_cli (ARM_USE_CLI) is false and no other authentication method resolved: set client_secret, "+
		"client_certificate / client_certificate_path, use_oidc with its inputs, use_msi or "+
		"use_aks_workload_identity, or leave use_cli at its default of true.")
}

// newAuthFileUnreadableError never includes the file's contents, only its path: the file may
// hold a secret.
func newAuthFileUnreadableError(attr string, envDesc string, path string, reason error) detailError {
	return newDetailError("auth_file_unreadable", fmt.Sprintf("The %s (%s) is set to %q, which could not be opened: %s.", attr, envDesc, path, reason))
}

func newClientCertificateUnreadableError(reason error) detailError {
	return newDetailError("client_certificate_unreadable", fmt.Sprintf("The client certificate could not be read: %s.", reason))
}

// newClientCertificateUnparseableError: both certificate parsers report a wrong password
// exactly like a corrupt file, so the text names the password as the likely cause, the
// more common mistake. passwordGiven picks which way: whether to set one, or to check it.
func newClientCertificateUnparseableError(source string, passwordGiven bool) detailError {
	if passwordGiven {
		return newDetailError("client_certificate_unparseable", fmt.Sprintf(
			"The %s could not be parsed as a PKCS#12 (.pfx/.p12) bundle or a PEM file with an unencrypted "+
				"private key. If it is a password-protected PKCS#12 bundle, check that client_certificate_password "+
				"(or ARM_CLIENT_CERTIFICATE_PASSWORD / AZURE_CLIENT_CERTIFICATE_PASSWORD) is correct.",
			source,
		))
	}
	return newDetailError("client_certificate_unparseable", fmt.Sprintf(
		"The %s could not be parsed as a PKCS#12 (.pfx/.p12) bundle or a PEM file with an unencrypted "+
			"private key. No client_certificate_password was given; set client_certificate_password, "+
			"ARM_CLIENT_CERTIFICATE_PASSWORD or AZURE_CLIENT_CERTIFICATE_PASSWORD if the bundle is password-protected.",
		source,
	))
}

func newClientCreationFailedError(kind kind, failure apiFailure) detailError {
	return newDetailError("client_creation_failed", fmt.Sprintf("Could not create the %s client."+withResponse, kind.detail, failure.response, failure.requestIDs))
}

// Causes: IDs, import and subscription.

// An ID the parser rejected. noun is the kind of object the ID was expected to name,
// wantFormat an example of its shape.

func newIdNotValidError(input string, noun string, wantFormat string) detailError {
	return newDetailError("id_not_valid", fmt.Sprintf("%q is not a valid %s ID: expected %s", input, noun, wantFormat))
}

func newIdHasNoParentError(input string, noun string) detailError {
	return newDetailError("id_has_no_parent", fmt.Sprintf("%q is a %s ID, which has no parent resource.", input, noun))
}

func newIdNotNestedError(input string, noun string, wantNoun string) detailError {
	return newDetailError("id_not_nested", fmt.Sprintf("%q is a %s ID, not nested under a %s.", input, noun, wantNoun))
}

// newInvalidStoredIDError is for an ID Terraform already holds, in its state or in the plan;
// idErr is why the parser rejected it.
func newInvalidStoredIDError(where storedIn, idErr detailError) detailError {
	return newDetailError("invalid_stored_id", fmt.Sprintf("The ID in the %s is not valid: %s", where.detail, idErr))
}

func newMissingImportIDError() detailError {
	return newDetailError("missing_import_id", "An import ID or an identity is required.")
}

func newInvalidImportIdentityError(rebuiltID string) detailError {
	return newDetailError("invalid_import_identity", fmt.Sprintf(
		"The identity does not describe a valid resource (rebuilt ID %q). Check each attribute for stray '/' characters.",
		rebuiltID))
}

// Causes: a moved block from another provider's resource. The source is described by what
// the move request carries: its provider address, resource type and state.

// newMoveSourceNotSupportedError names what the target does accept: accepted holds one
// description per supported source ("<type> of the <namespace>/<type> provider").
func newMoveSourceNotSupportedError(sourceProvider string, sourceType string, accepted []string) detailError {
	supported := "The only supported source is an " + strings.Join(accepted, ", ") + "."
	if len(accepted) > 1 {
		supported = "The supported sources are " + strings.Join(accepted, " and ") + "."
	}
	return newDetailError("move_source_not_supported", fmt.Sprintf(
		"Moving from %q of provider %q is not supported. %s", sourceType, sourceProvider, supported))
}

// newMoveSourceStateUnreadableError, newMoveSourceIDMissingError and newInvalidMoveSourceIDError
// take the type name of the source resource, which differs between the sources a target accepts.
func newMoveSourceStateUnreadableError(sourceType string, reason error) detailError {
	return newDetailError("move_source_state_unreadable", fmt.Sprintf("The state of the %s could not be read: %s", sourceType, reason))
}

// newMoveSourceNotMountExtensionError is for an azurerm_virtual_machine_extension that installs
// another extension than the Everpure iSCSI mount extension. publisher and extensionType are
// what its state holds.
func newMoveSourceNotMountExtensionError(publisher string, extensionType string) detailError {
	return newDetailError("move_source_not_mount_extension", fmt.Sprintf(
		"The azurerm_virtual_machine_extension has the publisher %q and the type %q, which is not the Everpure "+
			"iSCSI mount extension (publisher %s, type %s or %s).",
		publisher, extensionType, extensionPublisher, extensionTypeLinux, extensionTypeWindows))
}

func newMoveSourceIDMissingError(sourceType string) detailError {
	return newDetailError("move_source_id_missing", fmt.Sprintf("The state of the %s has no id.", sourceType))
}

// newInvalidMoveSourceIDError is for the id in the source resource's state; idErr is why the
// parser rejected it.
func newInvalidMoveSourceIDError(sourceType string, idErr detailError) detailError {
	return newDetailError("invalid_move_source_id", fmt.Sprintf("The id in the state of the %s is not valid: %s", sourceType, idErr))
}

// newSubscriptionMismatchError is for an ID in another subscription than the one the provider
// is configured for. The provider's clients are pinned to its own subscription.
func newSubscriptionMismatchError(kind kind, id string, idSubscription string, providerSubscription string) detailError {
	return newDetailError("subscription_mismatch", fmt.Sprintf(
		"The %s %s is in subscription %s, but the provider is configured for subscription %s. "+
			"Use a provider alias with subscription_id = %s to proceed with this action.",
		kind.detail, id, idSubscription, providerSubscription, idSubscription))
}

// Causes: not found.

// newNotFoundError is a 404 for an object of kind, at id. in is the kind of the parent it was
// looked up in by name, or nil when it was given by its own ID: the advice differs because a
// name can only be wrong inside the parent it was looked up in.
func newNotFoundError(kind kind, id string, in *kind, failure apiFailure) detailError {
	var msg string
	if in == nil {
		msg = fmt.Sprintf("The %s %s was not found. Check that the ID is spelled correctly and that the %s has not been deleted.",
			kind.detail, id, kind.detail)
	} else {
		msg = fmt.Sprintf("The %s %s was not found. Check the name, and that the %s itself exists.",
			kind.detail, id, in.detail)
	}
	if kind == kindVolumeGroup {
		msg += " If the volume group was deleted, it may be soft-deleted: restore it from its recoverable volume group."
	}
	return newDetailError("not_found", msg+fmt.Sprintf(withRequestIDs, failure.requestIDs))
}

// newStoragePoolUnavailableError is the pool of a volume group the service reports as missing,
// deleted or unreachable.
func newStoragePoolUnavailableError(poolID string, volumeGroupID string, failure apiFailure) detailError {
	return newDetailError("storage_pool_unavailable", fmt.Sprintf(
		"The storage pool %s of volume group %s was not found, has been deleted, or cannot be reached. "+
			"Check that the storage pool exists."+withRequestIDs,
		poolID, volumeGroupID, failure.requestIDs))
}

// Causes: lifecycle.

// newDeletedOutsideTerraformError is for a 404 from an update: the object was deleted after the
// plan was made. Update cannot drop it from state itself; Terraform would report that as
// a provider bug.
func newDeletedOutsideTerraformError(kind kind, id string) detailError {
	return newDetailError("deleted_outside_terraform", fmt.Sprintf(
		"The %s %s was deleted outside Terraform. Run terraform plan to refresh state.", kind.detail, id))
}

// newDeletionProtectedError is why a destroy, or a replace that destroys first, is blocked.
func newDeletionProtectedError(kind kind, name string) detailError {
	return newDetailError("deletion_protected", fmt.Sprintf(
		"The %s %q has deletion_protection = true, which blocks destroying it. Set deletion_protection = false and apply, then retry.",
		kind.detail, name))
}

func newPlatformConsoleDisabledError(poolID string, failure apiFailure) detailError {
	return newDetailError("platform_console_disabled", fmt.Sprintf(
		"Storage pool %s does not have the platform console enabled. Enable it with the storage "+
			"pool's `platform_console` block, then try again."+withResponse,
		poolID, failure.response, failure.requestIDs))
}

func newStoragePoolHasChildrenError(poolID string, failure apiFailure) detailError {
	return newDetailError("storage_pool_has_children", fmt.Sprintf(
		"Storage pool %s still contains volume groups, volumes or other resources. Delete them first, or "+
			"import them into Terraform and destroy them, then run terraform destroy again."+withResponse,
		poolID, failure.response, failure.requestIDs))
}

func newStoragePoolContainsDataError(poolID string, retentionSeconds int64, failure apiFailure) detailError {
	return newDetailError("storage_pool_contains_data", fmt.Sprintf(
		"The service reports objects remaining on storage pool %s. If none are visible, they are soft-deleted "+
			"(destroyed but not yet eradicated) volume groups or volumes that Terraform "+
			"cannot see. Eradicate them in the Azure Portal or CLI, or wait for automatic eradication (data "+
			"retention period: %d seconds), then run terraform destroy again. See the Deletion policies "+
			"guide: "+docsBaseURL+"/guides/deletion-policies"+withResponse,
		poolID, retentionSeconds, failure.response, failure.requestIDs))
}

func newReservationHasStoragePoolsError(reservationID string, failure apiFailure) detailError {
	return newDetailError("reservation_has_storage_pools", fmt.Sprintf(
		"Storage pools still exist on reservation %s. Delete them first, then run terraform destroy again."+withResponse,
		reservationID, failure.response, failure.requestIDs))
}

// newReservationPlanChangeFailedError is for a failed plan change, the one update that re-sends
// the whole reservation. A reservation whose Marketplace SaaS resource is visible refuses any
// offer details in an update, and the provider cannot tell the two kinds of reservation apart
// before it sends the request, so every failure of the plan change carries the hint.
func newReservationPlanChangeFailedError(reservationID string, failure apiFailure) detailError {
	return newDetailError("reservation_plan_change_failed", fmt.Sprintf(
		"Could not change the plan of reservation %s. If the reservation's SaaS resource is visible in "+
			"the Azure Portal (under SaaS, in the reservation's resource group), its plan is changed on that "+
			"SaaS resource, not through this resource; update offer.plan_id in the configuration afterwards "+
			"to match."+withResponse,
		reservationID, failure.response, failure.requestIDs))
}

// newReservationOfferChangedError is for a planned change to the offer of an existing
// reservation. fields names the changed attributes, the rest is the offer the reservation
// has now. Terraform cannot tell an edit in the configuration from the service having
// changed the offer underneath it, so the text covers both.
func newReservationOfferChangedError(name string, fields []string, offerID string, planID string, termUnit string) detailError {
	return newDetailError("reservation_offer_changed", fmt.Sprintf(
		"The %s of reservation %q differs from the reservation's current offer (offer_id %q, plan_id %q, "+
			"term_unit %q). A different offer or term is a different purchase and cannot be applied to "+
			"an existing reservation. If the offer was changed on the service side, update the configuration "+
			"to the values above; the previous offer may no longer exist. To stop Terraform from managing "+
			"the offer, add `lifecycle { ignore_changes = [offer] }`.",
		strings.Join(fields, " and "), name, offerID, planID, termUnit))
}

func newPlatformConsoleUserExistsError(username string, failure apiFailure) detailError {
	return newDetailError("platform_console_user_exists", fmt.Sprintf(
		"User %s already exists on this storage pool. This action only creates new users; to add or change "+
			"SSH keys or the role of an existing user, use the platform console's GUI or REST API."+withResponse,
		username, failure.response, failure.requestIDs))
}

// Causes: configuration validation. A source_type names the one source attribute that
// must be set.

func newSourceAttributeRequiredError(attr path.Path, sourceType string) detailError {
	return newDetailError("source_attribute_required", fmt.Sprintf("%s is required when source_type = %q.", attr, sourceType))
}

// newSourceAttributeNotAllowedError is for a source attribute set next to another source_type.
// orUnset is for the default source_type, which the user may have left out.
func newSourceAttributeNotAllowedError(attr path.Path, sourceType string, orUnset bool) detailError {
	if orUnset {
		return newDetailError("source_attribute_not_allowed", fmt.Sprintf("%s must not be set when source_type is %q (or unset).", attr, sourceType))
	}
	return newDetailError("source_attribute_not_allowed", fmt.Sprintf("%s must not be set when source_type = %q.", attr, sourceType))
}

func newVolumeSizeRequiredError() detailError {
	return newDetailError("volume_size_required", `One of provisioned_size_in_gib or provisioned_size_in_bytes is required when source_type = "none".`)
}

func newVolumeSizeNotAllowedError(sourceType string) detailError {
	return newDetailError("volume_size_not_allowed", fmt.Sprintf(
		"provisioned_size_in_gib and provisioned_size_in_bytes must not be set when source_type = %q; "+
			"the new volume's size is inherited from its source.",
		sourceType))
}

func newVolumeSizeDecreaseError(requestedBytes int64, currentBytes int64) detailError {
	return newDetailError("volume_size_decrease", fmt.Sprintf(
		"The requested size (%d bytes) is smaller than the current size (%d bytes). Shrinking a volume is "+
			"not supported; remove the size attribute to leave it unchanged, or set it to a larger value.",
		requestedBytes, currentBytes))
}

// newSourceInOtherStoragePoolError is for a clone or restore source in another storage pool than
// the volume's own volume group.
func newSourceInOtherStoragePoolError(attr path.Path, sourcePoolID string, poolID string) detailError {
	return newDetailError("source_in_other_storage_pool", fmt.Sprintf(
		"%s names a resource in storage pool %s, but volume_group_id is in storage pool %s: "+
			"cloning across storage pools is not supported.",
		attr, sourcePoolID, poolID))
}

// newRecoverySourceInOtherVolumeGroupError is for a soft-deleted object of kind k, to be
// recovered, in another volume group than the one it is recovered into.
func newRecoverySourceInOtherVolumeGroupError(attr path.Path, kind kind, sourceVolumeGroupID string, volumeGroupID string) detailError {
	return newDetailError("recovery_source_in_other_volume_group", fmt.Sprintf(
		"%s names a %s in volume group %s, but volume_group_id is %s: a %s can only be recovered within its own volume group.",
		attr, kind.detail, sourceVolumeGroupID, volumeGroupID, kind.detail))
}

// The SSH public key causes never quote the value: a rejected one may be a private key.

func newSshPublicKeyHasPEMBlockError() detailError {
	return newDetailError("ssh_public_key_has_pem_block", "The value contains a PEM block (a line starting with \"-----BEGIN\"). A private key must "+
		"never be used here, and a PEM or PKCS#8 public key is not in OpenSSH format; use the "+
		"one-line form, e.g. the contents of a `.pub` file.")
}

func newSshPublicKeyLineWithoutKeyDataError() detailError {
	return newDetailError("ssh_public_key_line_without_key_data", "Each line must hold a key type followed by the key data, e.g. \"ssh-ed25519 AAAA... comment\"; "+
		"a line has only one field.")
}

func newSshPublicKeyMissingError() detailError {
	return newDetailError("ssh_public_key_missing", "The value must hold at least one OpenSSH public key, e.g. the contents of a `.pub` file.")
}

func newConsoleGUIAPIMismatchError() detailError {
	return newDetailError("console_guiapi_mismatch", "GUI and REST API access are enabled together: gui_enabled and api_enabled must be set to the same value.")
}

func newConsoleServiceRequiredError() detailError {
	return newDetailError("console_service_required", "At least one of gui_enabled, api_enabled or cli_enabled must be true; remove the platform_console "+
		"block instead to disable console access entirely.")
}

// Causes: snapshots.

// newRecoverySourceNotFoundError is a 404 for the soft-deleted snapshot a snapshot is to be
// recovered from.
func newRecoverySourceNotFoundError(id string, failure apiFailure) detailError {
	return newDetailError("recovery_source_not_found", fmt.Sprintf(
		"Snapshot %s was not found. Only a soft-deleted snapshot that has not been eradicated can be recovered."+withRequestIDs,
		id, failure.requestIDs))
}

func newRecoverySourceCreatedByScheduleError(snapshotID string) detailError {
	return newDetailError("recovery_source_created_by_schedule", fmt.Sprintf(
		"Snapshot %s was created by the volume group's snapshot schedule; recovering it through Terraform "+
			"is not supported. Recover it through the Azure Portal or CLI instead.",
		snapshotID))
}

func newRecoverySourceNotSoftDeletedError(snapshotID string) detailError {
	return newDetailError("recovery_source_not_soft_deleted", fmt.Sprintf("Snapshot %s is not soft-deleted. Only a soft-deleted snapshot can be recovered.", snapshotID))
}

func newSnapshotOwnedByScheduleError(snapshotID string) detailError {
	return newDetailError("snapshot_owned_by_schedule", fmt.Sprintf(
		"Snapshot %s was created by the volume group's snapshot schedule, which also deletes it, so it "+
			"cannot be imported or moved into an everpure_azure_snapshot resource. Read it with the "+
			"everpure_azure_snapshot or everpure_azure_snapshots data source instead.",
		snapshotID))
}

// Causes: soft-deleted objects met by a lookup. name is what the lookup was configured
// with, previousName the name the object had before it was deleted, recoverableID what
// source_recoverable_*_id needs to recover it, eradicationTime when it is eradicated
// (empty when the service gave none).

func newVolumeSoftDeletedError(name string, previousName string, recoverableID string, eradicationTime string) detailError {
	if eradicationTime != "" {
		return newDetailError("volume_soft_deleted", fmt.Sprintf(
			"%q is a soft-deleted copy of volume %q, kept recoverable until eradication. "+
				"It is scheduled to be eradicated at %s. "+
				"Recover it with an everpure_azure_volume resource using source_type = \"recoverableVolume\" "+
				"and source_recoverable_volume_id = %q.",
			name, previousName, eradicationTime, recoverableID))
	}
	return newDetailError("volume_soft_deleted", fmt.Sprintf(
		"%q is a soft-deleted copy of volume %q, kept recoverable until eradication. "+
			"Recover it with an everpure_azure_volume resource using source_type = \"recoverableVolume\" "+
			"and source_recoverable_volume_id = %q.",
		name, previousName, recoverableID))
}

// newSnapshotSoftDeletedError is newVolumeSoftDeletedError for a snapshot. createdByPolicy is for
// one the volume group's snapshot schedule took, which cannot be recovered through
// Terraform, so its text says so instead of naming recoverableID.
func newSnapshotSoftDeletedError(name string, previousName string, recoverableID string, eradicationTime string, createdByPolicy bool) detailError {
	switch {
	case createdByPolicy && eradicationTime != "":
		return newDetailError("snapshot_soft_deleted", fmt.Sprintf(
			"%q is a soft-deleted copy of snapshot %q, kept recoverable until eradication. "+
				"It is scheduled to be eradicated at %s. "+
				"It was created by the volume group's snapshot schedule; recovering it through Terraform is not "+
				"supported. Recover it through the Azure Portal or CLI instead.",
			name, previousName, eradicationTime))
	case createdByPolicy:
		return newDetailError("snapshot_soft_deleted", fmt.Sprintf(
			"%q is a soft-deleted copy of snapshot %q, kept recoverable until eradication. "+
				"It was created by the volume group's snapshot schedule; recovering it through Terraform is not "+
				"supported. Recover it through the Azure Portal or CLI instead.",
			name, previousName))
	case eradicationTime != "":
		return newDetailError("snapshot_soft_deleted", fmt.Sprintf(
			"%q is a soft-deleted copy of snapshot %q, kept recoverable until eradication. "+
				"It is scheduled to be eradicated at %s. "+
				"Recover it with an everpure_azure_snapshot resource using source_recoverable_snapshot_id = %q.",
			name, previousName, eradicationTime, recoverableID))
	default:
		return newDetailError("snapshot_soft_deleted", fmt.Sprintf(
			"%q is a soft-deleted copy of snapshot %q, kept recoverable until eradication. "+
				"Recover it with an everpure_azure_snapshot resource using source_recoverable_snapshot_id = %q.",
			name, previousName, recoverableID))
	}
}

// Causes: storage pool status and volume group connection parameters.

// newStoragePoolNoMetricsError is for a pool whose health response carries no metrics. alerts is
// one per line, each with its level, for a pool that raised some: they are the only
// explanation of why, and may include info or warning entries next to the one that
// matters. It is empty for a pool that raised no alert.
func newStoragePoolNoMetricsError(poolID string, alerts string) detailError {
	if alerts != "" {
		return newDetailError("storage_pool_no_metrics", fmt.Sprintf("The service returned no metrics for storage pool %s. Its alerts:\n%s", poolID, alerts))
	}
	return newDetailError("storage_pool_no_metrics", fmt.Sprintf("The service returned no metrics for storage pool %s. It raised no alert explaining why.", poolID))
}

func newNoConnectionEndpointsError(vgID string) detailError {
	return newDetailError("no_connection_endpoints", fmt.Sprintf("Volume group %s returned zero iSCSI connection endpoints.", vgID))
}

// Causes: an object that already exists when a create is asked for. id is the object's ID
// as Azure returned it, resourceType the Terraform type that imports it.

func newAlreadyExistsError(k kind, id string, resourceType string) detailError {
	return newDetailError("already_exists", fmt.Sprintf(
		"A %s with the ID %q already exists. %s, or choose a different name.",
		k.detail, id, importAdvice(resourceType)))
}

// newAlreadyExistsAcceptedError is for an object whose creation or update Azure has accepted
// and not yet finished.
func newAlreadyExistsAcceptedError(k kind, id string, resourceType string) detailError {
	return newDetailError("already_exists_accepted", fmt.Sprintf(
		"A %s with the ID %q already exists, and Azure is still creating or updating it. Wait for that "+
			"to finish. %s. If it stays in the Accepted state, contact Everpure support.",
		k.detail, id, importAdvice(resourceType)))
}

func newAlreadyExistsDeletingError(k kind, id string) detailError {
	return newDetailError("already_exists_deleting", fmt.Sprintf(
		"A %s with the ID %q already exists, and Azure is deleting it. Wait for the deletion to finish, then retry.",
		k.detail, id))
}

// newAlreadyExistsFailedError is for an object whose provisioning ended without success.
func newAlreadyExistsFailedError(k kind, id string, state string) detailError {
	return newDetailError("already_exists_failed", fmt.Sprintf(
		"A %s with the ID %q already exists in the state %s. Contact Everpure support.",
		k.detail, id, state))
}

// newAlreadyExistsInStateError is for a provisioning state the provider does not know.
func newAlreadyExistsInStateError(k kind, id string, state string, resourceType string) detailError {
	return newDetailError("already_exists_in_state", fmt.Sprintf(
		"A %s with the ID %q already exists in the state %s. %s, or choose a different name.",
		k.detail, id, state, importAdvice(resourceType)))
}

// Causes: virtual machine connection.

func newVirtualMachineOSUnknownError(vmID string) detailError {
	return newDetailError("virtual_machine_os_unknown", fmt.Sprintf("Could not determine the operating system of virtual machine %s.", vmID))
}

func newVirtualMachineOSUnsupportedError(vmID string, os string) detailError {
	return newDetailError("virtual_machine_os_unsupported", fmt.Sprintf("Virtual machine %s has an unsupported operating system %q; only Linux and Windows are supported.", vmID, os))
}

func newExtensionReplacedOutOfBandError(extensionID string) detailError {
	return newDetailError("extension_replaced_out_of_band", fmt.Sprintf(
		"The extension at %s is no longer a PureStorage.Extensions iSCSI mount extension; it may have been "+
			"replaced by something else outside Terraform.",
		extensionID))
}

// newMovedExtensionNotMountExtensionError is for a resource moved from another provider whose
// extension turns out, on the first Read, not to be the Everpure iSCSI mount extension. The
// move could not tell, as it makes no API call.
func newMovedExtensionNotMountExtensionError(extensionID string) detailError {
	return newDetailError("moved_extension_not_mount_extension", fmt.Sprintf(
		"The extension at %s is not the Everpure iSCSI mount extension (publisher %s, type %s or %s), so "+
			"it cannot be moved into an everpure_azure_virtual_machine_connection resource.",
		extensionID, extensionPublisher, extensionTypeLinux, extensionTypeWindows))
}

// newExtensionAlreadyExistsError is for an extension of this resource's type that the virtual
// machine already has, under a name that is known. The wording is the one other
// providers use for a resource that has to be imported first.
func newExtensionAlreadyExistsError(extensionID string) detailError {
	return newDetailError("extension_already_exists", fmt.Sprintf(
		"An extension with the ID %q already exists on the virtual machine. %s, or remove it first.",
		extensionID, importAdvice("everpure_azure_virtual_machine_connection")))
}

// importAdvice is the start of the sentence that tells a user to import an object that
// already exists; the caller ends it, after the comma, with the other way out.
func importAdvice(resourceType string) string {
	return fmt.Sprintf("To manage it with Terraform, import it into state with \"terraform import\" (see the %s documentation)", resourceType)
}

// newExtensionOfTypeAlreadyExistsError is newExtensionAlreadyExistsError for an extension whose
// name is not known. failure is set for a duplicate that Azure reported itself, in response
// to the create, and nil for one found by listing.
func newExtensionOfTypeAlreadyExistsError(vmID string, extensionType string, failure *apiFailure) detailError {
	text := fmt.Sprintf(
		"Virtual machine %s already has an extension of type %s under a different name than this resource "+
			"is configured with. Azure allows only one instance of this extension per virtual machine. Find "+
			"its name (for example with `az vm extension list`) and import it instead of creating a new one.",
		vmID, extensionType)
	if failure != nil {
		text += fmt.Sprintf(withResponse, failure.response, failure.requestIDs)
	}
	return newDetailError("extension_of_type_already_exists", text)
}

func newIscsiClearingFailedError(failure apiFailure) detailError {
	return newDetailError("iscsi_clearing_failed", fmt.Sprintf(
		"Could not clear the iSCSI targets before removing the extension. The virtual machine must be running "+
			"with the Azure VM agent reporting for the extension to log out existing iSCSI sessions before "+
			"removal. Start the virtual machine and retry, or run `terraform state rm` on this resource to "+
			"remove it from state without disconnecting anything."+withResponse,
		failure.response, failure.requestIDs))
}

// newIscsiConfigurationFailedError is for an extension that reports a failed configuration,
// message is the extension's own.
func newIscsiConfigurationFailedError(message string) detailError {
	return newDetailError("iscsi_configuration_failed", message)
}

func newIscsiConfigurationStatusMissingError() detailError {
	return newDetailError("iscsi_configuration_status_missing", "The extension reported no instance view after its configuration succeeded.")
}

func newInstanceViewReadFailedError(failure apiFailure) detailError {
	return newDetailError("instance_view_read_failed", fmt.Sprintf(
		"Could not read the extension's instance view. The virtual machine must be running with the Azure VM "+
			"agent installed and reporting for the extension to configure iSCSI."+withResponse,
		failure.response, failure.requestIDs))
}

// newIscsiConfigurationStatusUnrecognizedError is for a status code the extension is not
// known to report, so the provider cannot tell whether iSCSI is configured. code and
// message are the extension's own.
func newIscsiConfigurationStatusUnrecognizedError(code, message string) detailError {
	return newDetailError("iscsi_configuration_status_unrecognized", fmt.Sprintf(
		"The extension reported a status the provider does not recognise, so it cannot tell whether iSCSI is "+
			"configured. Status code: %q. Message: %q. Check the extension's status on the virtual machine.",
		code, message))
}

// newIscsiConfigurationTimedOutError is for a timeout that elapsed while the extension was
// still configuring or rebooting. message is the extension's last reported one.
func newIscsiConfigurationTimedOutError(message string) detailError {
	return newDetailError("iscsi_configuration_timed_out", fmt.Sprintf(
		"The extension was still configuring iSCSI, or the virtual machine was still rebooting, when the "+
			"operation's timeout elapsed. Last reported message: %q. The virtual machine must be running with the "+
			"Azure VM agent installed and reporting; increase the timeout, or check on it and reapply once the "+
			"extension has finished.",
		message))
}

// Causes: Azure call failures. Azure's response alone does not say which object failed,
// so these hold the operation; the causes above already name their object.

// newApiCallFailedError is for a failure of the call that is the operation itself, on the
// object at op.id or, when the operation has none, inside the container at op.in.
func newApiCallFailedError(op operation, failure apiFailure) detailError {
	return newDetailError("api_call_failed", fmt.Sprintf("Could not %s %s %s."+withResponse,
		op.action.detail, op.kind.detail, op.target(), failure.response, failure.requestIDs))
}

// newApiStepFailedError is for a failure of a call on another object, made on the way to
// completing the operation.
func newApiStepFailedError(op operation, step step, failure apiFailure) detailError {
	return newDetailError("api_step_failed", fmt.Sprintf("Could not %s %s %s. The call to %s %s %s failed."+withResponse,
		op.action.detail, op.kind.detail, op.target(),
		step.action.detail, step.kind.detail, step.id, failure.response, failure.requestIDs))
}

// Causes: unexpected API responses. kind is the kind of object the response was about.

func newNoPropertiesError(kind kind) detailError {
	return newDetailError("no_properties", fmt.Sprintf("The API response for the %s had no properties.", kind.detail))
}

func newMissingResponseIDError(kind kind) detailError {
	return newDetailError("missing_response_id", fmt.Sprintf("The API returned a %s with no id.", kind.detail))
}

func newUnparseableResponseIDError(kind kind, idErr detailError) detailError {
	return newDetailError("unparseable_response_id", fmt.Sprintf("The API returned a %s with an unparseable id: %s", kind.detail, idErr))
}

func newEmptyLocationError(kind kind) detailError {
	return newDetailError("empty_location", fmt.Sprintf("The API returned a %s with no location.", kind.detail))
}

func newEmptyListEntryError(kind kind) detailError {
	return newDetailError("empty_list_entry", fmt.Sprintf("The API returned an empty entry in the %s list.", kind.detail))
}

func newUnparseableExtensionSettingsError(reason error) detailError {
	return newDetailError("unparseable_extension_settings", fmt.Sprintf("The virtual machine extension's settings could not be parsed: %s", reason))
}

// Causes: internal errors. technical is what a bug report needs, as the code knows it.

func newProviderBugError(technical string) detailError {
	return newDetailError("provider_bug", fmt.Sprintf(
		"An unexpected error occurred in the provider. This is always a bug in the provider, not in your "+
			"configuration. Please report it at "+
			"https://github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/issues, "+
			"including the following:\n\n%s",
		technical))
}

// Warnings. Each states a fact and is never a failure.

// softDeletedTwinNotFoundWarning is for a delete with eradicate_on_destroy that cannot
// find the soft-deleted copy of k its own first DELETE just created. It does not claim
// the copy was eradicated: finding it relies on destroyed objects appearing in the
// parent's listing, which could change.
func softDeletedTwinNotFoundWarning(k kind, name string) diag.Diagnostic {
	return diag.NewWarningDiagnostic(
		"Soft-Deleted "+k.summary+" Not Found",
		fmt.Sprintf(
			"Could not find a soft-deleted copy of %q to eradicate. It may already have been "+
				"eradicated automatically or by someone else, or the mechanism this depends on may "+
				"have changed. The %s itself is deleted either way.",
			name, k.detail))
}

func recoverableVolumeGroupNotFoundWarning(name string) diag.Diagnostic {
	return diag.NewWarningDiagnostic(
		"Recoverable Volume Group Not Found",
		fmt.Sprintf(
			"Could not find a recoverable volume group matching %q to eradicate. It may already have "+
				"been eradicated automatically or by someone else. The volume group itself is deleted "+
				"either way.",
			name))
}

func noConnectionEndpointsWarning(vgID string) diag.Diagnostic {
	return diag.NewWarningDiagnostic("Volume Group Has No Connection Endpoints", newNoConnectionEndpointsError(vgID).Error())
}

// largeSnapshotListWarning is for a snapshots read without limit that returned more than
// largeSnapshotListWarningThreshold snapshots.
func largeSnapshotListWarning(count int, vgID string) diag.Diagnostic {
	return diag.NewWarningDiagnostic(
		"Large Snapshot List",
		fmt.Sprintf(
			"Read %d snapshots from %s, paging through all of them on every plan and refresh. If the "+
				"configuration doesn't need every snapshot, set limit: the newest ones are returned first.",
			count, vgID))
}

func rebootAllowedNoEffectWarning() diag.Diagnostic {
	return diag.NewWarningDiagnostic(
		"reboot_allowed Has No Effect on This Virtual Machine",
		"reboot_allowed is effective on Windows only; the extension ignores it on this Linux virtual "+
			"machine. The value is still sent to the extension unchanged.")
}

func windowsSettingsNoEffectWarning() diag.Diagnostic {
	return diag.NewWarningDiagnostic(
		"windows_settings Has No Effect on This Virtual Machine",
		"windows_settings is effective on Windows only; the extension ignores it on this Linux virtual "+
			"machine. The values are still sent to the extension unchanged.")
}

func rebootRequiredWarning() diag.Diagnostic {
	return diag.NewWarningDiagnostic(
		"Reboot Required to Finish iSCSI Configuration",
		handlerMessageRebootRequired+" Reboot the virtual machine, or set reboot_allowed = true and apply "+
			"again, to finish setting it up. This warning repeats on every plan or refresh until the virtual "+
			"machine has rebooted and the extension reports the configuration complete.")
}

// instanceViewFailedWarning is for an extension that reports a failed iSCSI
// configuration outside this run's own apply, e.g. reapply_on_boot re-ran the handler
// after a later reboot and it failed. It is a warning because Read runs during refresh,
// and an error there would abort the plan before Terraform reaches Update, leaving no way
// to retry through the normal workflow. message is the extension's own.
func instanceViewFailedWarning(message string) diag.Diagnostic {
	return diag.NewWarningDiagnostic(
		"iSCSI Configuration Failed",
		message+" This is the extension's last reported guest-side status, not a failure from this Terraform "+
			"run. Investigate on the virtual machine, or force a retry with `terraform apply -replace` on this resource.")
}

// instanceViewUnrecognizedWarning is for an extension that reports a status code the
// provider does not recognise. It is a warning for the same reason as
// instanceViewFailedWarning: Read runs during refresh. code and message are the
// extension's own.
func instanceViewUnrecognizedWarning(code, message string) diag.Diagnostic {
	return diag.NewWarningDiagnostic(
		"Unrecognized iSCSI Configuration Status",
		fmt.Sprintf("The extension reports a status the provider does not recognise, so it cannot tell whether "+
			"iSCSI is configured. Status code: %q. Message: %q.", code, message))
}

// instanceViewFailedWithoutMessageWarning is instanceViewFailedWarning for a status
// without a message.
func instanceViewFailedWithoutMessageWarning() diag.Diagnostic {
	return instanceViewFailedWarning("The extension reports a failed status.")
}
