// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	dstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ datasource.DataSource              = &StoragePoolStatusDataSource{}
	_ datasource.DataSourceWithConfigure = &StoragePoolStatusDataSource{}
)

// defaultStoragePoolStatusReadTimeout is far shorter than the pool read default: the
// read is a single short POST, and a check block runs it on every plan, so an
// unreachable pool must not stall each plan through a long retry window.
const defaultStoragePoolStatusReadTimeout = 1 * time.Minute

// StoragePoolStatusDataSourceModel is the Terraform-side representation of the
// everpure_azure_storage_pool_status data source.
type StoragePoolStatusDataSourceModel struct {
	StoragePoolID               armids.IDValue   `tfsdk:"storage_pool_id"`
	UsedCapacityPercentage      types.Float64    `tfsdk:"used_capacity_percentage"`
	DataReductionRatio          types.Float64    `tfsdk:"data_reduction_ratio"`
	EstimatedMaxCapacityInBytes types.Int64      `tfsdk:"estimated_max_capacity_in_bytes"`
	BandwidthUsage              types.Object     `tfsdk:"bandwidth_usage"`
	IopsUsage                   types.Object     `tfsdk:"iops_usage"`
	Space                       types.Object     `tfsdk:"space"`
	Alerts                      types.List       `tfsdk:"alerts"`
	Timeouts                    dstimeouts.Value `tfsdk:"timeouts"`
}

var (
	bandwidthUsageObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
		"current_in_bytes_per_second":     types.Int64Type,
		"provisioned_in_bytes_per_second": types.Int64Type,
		"max_in_bytes_per_second":         types.Int64Type,
	}}

	iopsUsageObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
		"current":     types.Int64Type,
		"provisioned": types.Int64Type,
		"max":         types.Int64Type,
	}}

	healthAlertObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
		"level":   types.StringType,
		"message": types.StringType,
	}}
)

// StoragePoolStatusDataSource reads a storage pool's status: capacity, throughput and
// IOPS usage plus the service's own alerts. It is meant for Terraform `check` blocks.
type StoragePoolStatusDataSource struct {
	// dataSourceClientsHolder supplies Configure and storagePoolsClient via promotion;
	// see its own doc comment.
	dataSourceClientsHolder
}

func NewStoragePoolStatusDataSource() datasource.DataSource {
	return &StoragePoolStatusDataSource{}
}

func (d *StoragePoolStatusDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_storage_pool_status"
}

func (d *StoragePoolStatusDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the status of an existing Storage Pool: capacity, bandwidth and IOPS usage, and the " +
			"alerts the service raises for it.\n\n" +
			"The values can be used in any condition Terraform evaluates. In a " +
			"[`check` block](https://developer.hashicorp.com/terraform/language/checks), a failed assertion is " +
			"reported as a warning during plan and apply, and the run continues. In a `precondition` or " +
			"`postcondition`, a failed condition stops the run.\n\n" +
			"The `current` values of `bandwidth_usage` and `iops_usage` are the peak over the last 10 minutes. " +
			"`alerts` holds the warnings and errors the service raises for the pool, for example when used " +
			"capacity is high. Their text is meant for people and may change, so match on `level`, not on " +
			"`message`.\n\n" +
			"If the storage pool is unreachable, the read fails with an error that carries the service's alert " +
			"message. Inside a `check` block that error is reported as a warning.",
		Attributes: map[string]schema.Attribute{
			"storage_pool_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				Validators: []validator.String{
					armids.TypeStoragePool.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the Storage Pool. Must be in the " +
					"provider's own `subscription_id`.",
			},
			"used_capacity_percentage": schema.Float64Attribute{
				Computed: true,
				MarkdownDescription: "Percentage of the pool's maximum capacity that is in use, from 0 to 100. " +
					"The service also raises alerts when this is high; see `alerts`.",
			},
			"data_reduction_ratio": schema.Float64Attribute{
				Computed: true,
				MarkdownDescription: "Data reduction ratio. It is 1 for an empty pool and can be below 1. The service " +
					"rounds it down.",
			},
			"estimated_max_capacity_in_bytes": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Estimated maximum capacity of the pool, in bytes. The service derives it from " +
					"the used space and `used_capacity_percentage` with single-precision arithmetic, so it is " +
					"approximate.",
			},
			"bandwidth_usage": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Bandwidth usage, in bytes per second.",
				Attributes: map[string]schema.Attribute{
					"current_in_bytes_per_second": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Peak read plus write bandwidth over the last 10 minutes, in bytes per second.",
					},
					"provisioned_in_bytes_per_second": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Provisioned bandwidth of the pool, in bytes per second.",
					},
					"max_in_bytes_per_second": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Largest bandwidth the service offers, in bytes per second.",
					},
				},
			},
			"iops_usage": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "IOPS usage, in operations per second.",
				Attributes: map[string]schema.Attribute{
					"current": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Peak IOPS over the last 10 minutes.",
					},
					"provisioned": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Provisioned IOPS.",
					},
					"max": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Largest IOPS the service offers.",
					},
				},
			},
			"space": spaceAttribute(),
			"alerts": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "Alerts the service currently raises for the pool, in the order the service " +
					"returns them. Empty when there are none. Do not rely on the position of an alert; filter on " +
					"`level` instead.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"level": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Severity of the alert: `info`, `warning` or `error`.",
						},
						"message": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Human-readable description of the alert.",
						},
					},
				},
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the pool's status", defaultStoragePoolStatusReadTimeout),
		},
	}
}

func (d *StoragePoolStatusDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data StoragePoolStatusDataSourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultStoragePoolStatusReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	poolID := data.StoragePoolID.ValueString()
	op := operation{action: actionRetrieve, kind: kindStoragePoolStatus, id: poolID, unnamed: true}

	pool, idErr := parseIDInSubscription(armids.ParseStoragePoolID, poolID, kindStoragePool, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*idErr)))
		return
	}

	health, err := getStoragePoolHealth(ctx, client, pool.ResourceGroupName, pool.StoragePoolName)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newNotFoundError(kindStoragePool, poolID, nil, d.clients.apiFailure(err)))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	if healthReportsNoMetrics(health) {
		resp.Diagnostics.Append(op.fail(noMetricsCause(poolID, health.Alerts)))
		return
	}

	resp.Diagnostics.Append(applyStoragePoolStatus(&data, health)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// storage_pool_id is left untouched from config: Terraform core rejects a data
	// source result that changes a configured, non-Computed value.
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// getStoragePoolHealth reads the pool's health endpoint. The service sends integer-typed fields,
// such as the bandwidth and IOPS figures, as JSON floats like 1073741824.0, which the
// SDK's int64 fields reject with a 200 response and a decode error. In that one case the
// captured body is normalized (integral floats rewritten to integers) and decoded again
// with the SDK's own unmarshaller. Every other error, including any HTTP error status,
// is returned unchanged, as is the original decode error if the retry cannot succeed.
func getStoragePoolHealth(ctx context.Context, client *armpurestorageblock.StoragePoolsClient, resourceGroup, name string) (armpurestorageblock.StoragePoolHealthInfo, error) {
	var raw *http.Response
	res, err := client.GetHealthStatus(policy.WithCaptureResponse(ctx, &raw), resourceGroup, name, nil)
	if err == nil {
		return res.StoragePoolHealthInfo, nil
	}
	if raw == nil || raw.StatusCode != http.StatusOK {
		return armpurestorageblock.StoragePoolHealthInfo{}, err
	}
	body, perr := runtime.Payload(raw)
	if perr != nil {
		return armpurestorageblock.StoragePoolHealthInfo{}, err
	}
	normalized, nerr := normalizeIntegralJSONNumbers(body)
	if nerr != nil {
		return armpurestorageblock.StoragePoolHealthInfo{}, err
	}
	var info armpurestorageblock.StoragePoolHealthInfo
	if uerr := json.Unmarshal(normalized, &info); uerr != nil {
		return armpurestorageblock.StoragePoolHealthInfo{}, err
	}
	return info, nil
}

// normalizeIntegralJSONNumbers rewrites every JSON number with an integral value
// (1073741824.0, -3.00, 1.073741824e9) to its plain integer text, so a decoder with an
// integer field accepts it. Non-integral numbers are left as they are, so they still fail
// such a decoder instead of being truncated. Integrality is decided on the exact decimal
// value, not through float64, which cannot hold integers near the int64 limit.
func normalizeIntegralJSONNumbers(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the top-level JSON value")
	}
	return json.Marshal(normalizeIntegralNumbers(v))
}

func normalizeIntegralNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeIntegralNumbers(e)
		}
	case []any:
		for i, e := range t {
			t[i] = normalizeIntegralNumbers(e)
		}
	case json.Number:
		if r, ok := new(big.Rat).SetString(t.String()); ok && r.IsInt() {
			return json.Number(r.Num().String())
		}
	}
	return v
}

// healthReportsNoMetrics reports whether the response carries no usable metrics. A pool
// the service cannot report on (creation failed, backing storage unreachable) comes back
// 200 with every metric zeroed, which a threshold check would read as an empty, healthy
// pool. A working pool always has nonzero provisioned bandwidth and IOPS, so both being
// zero (or absent) means the zeros are not measurements.
func healthReportsNoMetrics(info armpurestorageblock.StoragePoolHealthInfo) bool {
	h := info.Health
	if h == nil {
		return true
	}
	var bandwidth, iops int64
	if h.BandwidthUsage != nil && h.BandwidthUsage.Provisioned != nil {
		bandwidth = *h.BandwidthUsage.Provisioned
	}
	if h.IopsUsage != nil && h.IopsUsage.Provisioned != nil {
		iops = *h.IopsUsage.Provisioned
	}
	return bandwidth == 0 && iops == 0
}

// noMetricsCause picks the cause for a pool without metrics, quoting every alert with its
// level.
func noMetricsCause(poolID string, alerts []*armpurestorageblock.Alert) detailError {
	var lines []string
	for _, a := range alerts {
		if a == nil || a.Message == nil {
			continue
		}
		if a.Level == nil {
			lines = append(lines, *a.Message)
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s] %s", *a.Level, *a.Message))
	}
	if len(lines) == 0 {
		return newStoragePoolNoMetricsError(poolID, "")
	}
	return newStoragePoolNoMetricsError(poolID, strings.Join(lines, "\n"))
}

// applyStoragePoolStatus maps the API response onto every computed field of data. Nil
// pointers map to null values, a nil Health maps to null objects, nil alert entries are
// skipped, and the alerts keep the API's order. An empty alert list is an empty,
// non-null list.
func applyStoragePoolStatus(data *StoragePoolStatusDataSourceModel, info armpurestorageblock.StoragePoolHealthInfo) diag.Diagnostics {
	var diags diag.Diagnostics

	h := info.Health
	if h == nil {
		h = &armpurestorageblock.HealthDetails{}
	}

	data.UsedCapacityPercentage = types.Float64PointerValue(h.UsedCapacityPercentage)
	data.DataReductionRatio = types.Float64PointerValue(h.DataReductionRatio)
	data.EstimatedMaxCapacityInBytes = types.Int64PointerValue(h.EstimatedMaxCapacity)

	var d diag.Diagnostics
	if b := h.BandwidthUsage; b != nil {
		data.BandwidthUsage, d = types.ObjectValue(bandwidthUsageObjectType.AttrTypes, map[string]attr.Value{
			"current_in_bytes_per_second":     types.Int64PointerValue(b.Current),
			"provisioned_in_bytes_per_second": types.Int64PointerValue(b.Provisioned),
			"max_in_bytes_per_second":         types.Int64PointerValue(b.Max),
		})
		diags.Append(d...)
	} else {
		data.BandwidthUsage = types.ObjectNull(bandwidthUsageObjectType.AttrTypes)
	}

	if i := h.IopsUsage; i != nil {
		data.IopsUsage, d = types.ObjectValue(iopsUsageObjectType.AttrTypes, map[string]attr.Value{
			"current":     types.Int64PointerValue(i.Current),
			"provisioned": types.Int64PointerValue(i.Provisioned),
			"max":         types.Int64PointerValue(i.Max),
		})
		diags.Append(d...)
	} else {
		data.IopsUsage = types.ObjectNull(iopsUsageObjectType.AttrTypes)
	}

	data.Space, d = spaceFromAPI(h.Space)
	diags.Append(d...)

	elems := make([]attr.Value, 0, len(info.Alerts))
	for _, a := range info.Alerts {
		if a == nil {
			continue
		}
		var level *string
		if a.Level != nil {
			level = to.Ptr(string(*a.Level))
		}
		obj, d := types.ObjectValue(healthAlertObjectType.AttrTypes, map[string]attr.Value{
			"level":   types.StringPointerValue(level),
			"message": types.StringPointerValue(a.Message),
		})
		diags.Append(d...)
		elems = append(elems, obj)
	}
	data.Alerts, d = types.ListValue(healthAlertObjectType, elems)
	diags.Append(d...)

	return diags
}
