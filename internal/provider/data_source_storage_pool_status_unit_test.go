// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func fullHealthInfo() armpurestorageblock.StoragePoolHealthInfo {
	return armpurestorageblock.StoragePoolHealthInfo{
		Alerts: []*armpurestorageblock.Alert{
			{Level: to.Ptr(armpurestorageblock.AlertLevelWarning), Message: to.Ptr("capacity above 80%")},
			{Level: to.Ptr(armpurestorageblock.AlertLevelInfo), Message: to.Ptr("connecting")},
			{Level: to.Ptr(armpurestorageblock.AlertLevelError), Message: to.Ptr("disconnected")},
		},
		Health: &armpurestorageblock.HealthDetails{
			BandwidthUsage:         &armpurestorageblock.BandwidthUsage{Current: to.Ptr(int64(100)), Provisioned: to.Ptr(int64(1073741824)), Max: to.Ptr(int64(2147483648))},
			DataReductionRatio:     to.Ptr(3.5),
			EstimatedMaxCapacity:   to.Ptr(int64(1000)),
			IopsUsage:              &armpurestorageblock.IopsUsage{Current: to.Ptr(int64(7)), Provisioned: to.Ptr(int64(10240)), Max: to.Ptr(int64(20480))},
			Space:                  &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(10)), Unique: to.Ptr(int64(4)), Snapshots: to.Ptr(int64(3)), Shared: to.Ptr(int64(3))},
			UsedCapacityPercentage: to.Ptr(81.5),
		},
	}
}

func int64Attr(t *testing.T, attrs map[string]attr.Value, name string) int64 {
	t.Helper()
	v, ok := attrs[name].(types.Int64)
	if !ok {
		t.Fatalf("attribute %q is %T, want types.Int64", name, attrs[name])
	}
	return v.ValueInt64()
}

func stringAttr(t *testing.T, attrs map[string]attr.Value, name string) string {
	t.Helper()
	v, ok := attrs[name].(types.String)
	if !ok {
		t.Fatalf("attribute %q is %T, want types.String", name, attrs[name])
	}
	return v.ValueString()
}

func TestApplyStoragePoolStatus(t *testing.T) {
	ctx := context.Background()

	t.Run("maps every field", func(t *testing.T) {
		var data StoragePoolStatusDataSourceModel
		if diags := applyStoragePoolStatus(&data, fullHealthInfo()); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		if got := data.UsedCapacityPercentage.ValueFloat64(); got != 81.5 {
			t.Errorf("used_capacity_percentage = %v, want 81.5", got)
		}
		if got := data.DataReductionRatio.ValueFloat64(); got != 3.5 {
			t.Errorf("data_reduction_ratio = %v, want 3.5", got)
		}
		if got := data.EstimatedMaxCapacityInBytes.ValueInt64(); got != 1000 {
			t.Errorf("estimated_max_capacity_in_bytes = %d, want 1000", got)
		}

		bw := data.BandwidthUsage.Attributes()
		for name, want := range map[string]int64{
			"current_in_bytes_per_second":     100,
			"provisioned_in_bytes_per_second": 1073741824,
			"max_in_bytes_per_second":         2147483648,
		} {
			if got := int64Attr(t, bw, name); got != want {
				t.Errorf("bandwidth_usage.%s = %d, want %d", name, got, want)
			}
		}

		iops := data.IopsUsage.Attributes()
		for name, want := range map[string]int64{"current": 7, "provisioned": 10240, "max": 20480} {
			if got := int64Attr(t, iops, name); got != want {
				t.Errorf("iops_usage.%s = %d, want %d", name, got, want)
			}
		}

		space := data.Space.Attributes()
		for name, want := range map[string]int64{
			"total_used_in_bytes": 10, "unique_in_bytes": 4, "snapshots_in_bytes": 3, "shared_in_bytes": 3,
		} {
			if got := int64Attr(t, space, name); got != want {
				t.Errorf("space.%s = %d, want %d", name, got, want)
			}
		}
	})

	t.Run("alerts keep the API's order", func(t *testing.T) {
		var data StoragePoolStatusDataSourceModel
		if diags := applyStoragePoolStatus(&data, fullHealthInfo()); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		var levels []string
		for _, e := range data.Alerts.Elements() {
			obj, ok := e.(types.Object)
			if !ok {
				t.Fatalf("alert element is %T, want types.Object", e)
			}
			levels = append(levels, stringAttr(t, obj.Attributes(), "level"))
		}
		want := []string{"warning", "info", "error"}
		if len(levels) != len(want) {
			t.Fatalf("levels = %v, want %v", levels, want)
		}
		for i := range want {
			if levels[i] != want[i] {
				t.Fatalf("levels = %v, want %v", levels, want)
			}
		}
	})

	t.Run("empty and nil alert lists are empty, non-null lists", func(t *testing.T) {
		for name, alerts := range map[string][]*armpurestorageblock.Alert{
			"empty": {},
			"nil":   nil,
		} {
			t.Run(name, func(t *testing.T) {
				var data StoragePoolStatusDataSourceModel
				info := fullHealthInfo()
				info.Alerts = alerts
				if diags := applyStoragePoolStatus(&data, info); diags.HasError() {
					t.Fatalf("unexpected diagnostics: %v", diags)
				}
				if data.Alerts.IsNull() || data.Alerts.IsUnknown() {
					t.Fatalf("alerts = %v, want an empty non-null list", data.Alerts)
				}
				if got := len(data.Alerts.Elements()); got != 0 {
					t.Fatalf("got %d alerts, want 0", got)
				}
			})
		}
	})

	t.Run("nil alert entries are skipped; nil alert fields map to null", func(t *testing.T) {
		var data StoragePoolStatusDataSourceModel
		info := fullHealthInfo()
		info.Alerts = []*armpurestorageblock.Alert{
			nil,
			{Message: to.Ptr("no level")},
			nil,
			{Level: to.Ptr(armpurestorageblock.AlertLevelError)},
		}
		if diags := applyStoragePoolStatus(&data, info); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		var got []types.Object
		if diags := data.Alerts.ElementsAs(ctx, &got, false); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(got) != 2 {
			t.Fatalf("got %d alerts, want 2", len(got))
		}
		if !got[0].Attributes()["level"].IsNull() || stringAttr(t, got[0].Attributes(), "message") != "no level" {
			t.Errorf("first alert = %v", got[0])
		}
		if stringAttr(t, got[1].Attributes(), "level") != "error" || !got[1].Attributes()["message"].IsNull() {
			t.Errorf("second alert = %v", got[1])
		}
	})

	t.Run("nil health maps to null scalars and objects", func(t *testing.T) {
		var data StoragePoolStatusDataSourceModel
		if diags := applyStoragePoolStatus(&data, armpurestorageblock.StoragePoolHealthInfo{}); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		for name, v := range map[string]attr.Value{
			"used_capacity_percentage":        data.UsedCapacityPercentage,
			"data_reduction_ratio":            data.DataReductionRatio,
			"estimated_max_capacity_in_bytes": data.EstimatedMaxCapacityInBytes,
			"bandwidth_usage":                 data.BandwidthUsage,
			"iops_usage":                      data.IopsUsage,
			"space":                           data.Space,
		} {
			if !v.IsNull() {
				t.Errorf("%s = %v, want null", name, v)
			}
		}
	})

	t.Run("nil fields inside a present object map to null attributes", func(t *testing.T) {
		var data StoragePoolStatusDataSourceModel
		info := armpurestorageblock.StoragePoolHealthInfo{Health: &armpurestorageblock.HealthDetails{
			BandwidthUsage: &armpurestorageblock.BandwidthUsage{Current: to.Ptr(int64(5))},
			IopsUsage:      &armpurestorageblock.IopsUsage{},
			Space:          &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(1))},
		}}
		if diags := applyStoragePoolStatus(&data, info); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		bw := data.BandwidthUsage.Attributes()
		if int64Attr(t, bw, "current_in_bytes_per_second") != 5 || !bw["max_in_bytes_per_second"].IsNull() {
			t.Errorf("bandwidth_usage = %v", data.BandwidthUsage)
		}
		if !data.IopsUsage.Attributes()["current"].IsNull() {
			t.Errorf("iops_usage = %v", data.IopsUsage)
		}
		if !data.Space.Attributes()["unique_in_bytes"].IsNull() {
			t.Errorf("space = %v", data.Space)
		}
	})
}

func TestHealthReportsNoMetrics(t *testing.T) {
	t.Parallel()

	zeroed := func() armpurestorageblock.StoragePoolHealthInfo {
		info := fullHealthInfo()
		info.Health.BandwidthUsage.Provisioned = to.Ptr(int64(0))
		info.Health.IopsUsage.Provisioned = to.Ptr(int64(0))
		return info
	}

	tests := []struct {
		name string
		info func() armpurestorageblock.StoragePoolHealthInfo
		want bool
	}{
		{"a pool with provisioned limits has metrics", fullHealthInfo, false},
		{"nil health has none", func() armpurestorageblock.StoragePoolHealthInfo { return armpurestorageblock.StoragePoolHealthInfo{} }, true},
		{"both provisioned limits zero has none", zeroed, true},
		{"both provisioned limits absent has none", func() armpurestorageblock.StoragePoolHealthInfo {
			return armpurestorageblock.StoragePoolHealthInfo{Health: &armpurestorageblock.HealthDetails{}}
		}, true},
		{"only bandwidth provisioned has metrics", func() armpurestorageblock.StoragePoolHealthInfo {
			info := zeroed()
			info.Health.BandwidthUsage.Provisioned = to.Ptr(int64(1))
			return info
		}, false},
		{"only IOPS provisioned has metrics", func() armpurestorageblock.StoragePoolHealthInfo {
			info := zeroed()
			info.Health.IopsUsage.Provisioned = to.Ptr(int64(1))
			return info
		}, false},
		{"zero usage on a pool with limits is still metrics", func() armpurestorageblock.StoragePoolHealthInfo {
			info := fullHealthInfo()
			info.Health.UsedCapacityPercentage = to.Ptr(0.0)
			info.Health.BandwidthUsage.Current = to.Ptr(int64(0))
			return info
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := healthReportsNoMetrics(tt.info()); got != tt.want {
				t.Errorf("healthReportsNoMetrics() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNoMetricsCause(t *testing.T) {
	t.Parallel()

	alert := func(level armpurestorageblock.AlertLevel, msg string) *armpurestorageblock.Alert {
		return &armpurestorageblock.Alert{Level: to.Ptr(level), Message: to.Ptr(msg)}
	}

	tests := []struct {
		name   string
		alerts []*armpurestorageblock.Alert
		want   detailError
	}{
		{"no alerts", nil, newStoragePoolNoMetricsError("/p", "")},
		{"nil and message-less alerts are skipped", []*armpurestorageblock.Alert{nil, {}}, newStoragePoolNoMetricsError("/p", "")},
		{"one alert carries its level", []*armpurestorageblock.Alert{alert(armpurestorageblock.AlertLevelError, "creation failed")}, newStoragePoolNoMetricsError("/p", "[error] creation failed")},
		{"every alert is listed with its own level, in order", []*armpurestorageblock.Alert{
			alert(armpurestorageblock.AlertLevelInfo, "connecting"), nil, alert(armpurestorageblock.AlertLevelError, "disconnected"),
		}, newStoragePoolNoMetricsError("/p", "[info] connecting\n[error] disconnected")},
		{"an alert without a level is listed by message alone", []*armpurestorageblock.Alert{{Message: to.Ptr("no level")}}, newStoragePoolNoMetricsError("/p", "no level")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := noMetricsCause("/p", tt.alerts); got != tt.want {
				t.Errorf("noMetricsCause() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeIntegralJSONNumbers(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "float with zero fraction", in: `{"a":1073741824.0}`, want: `{"a":1073741824}`},
		{name: "zero", in: `{"a":0.0}`, want: `{"a":0}`},
		{name: "negative", in: `{"a":-3.00}`, want: `{"a":-3}`},
		{name: "exponent form", in: `{"a":1.073741824e9}`, want: `{"a":1073741824}`},
		{name: "near int64 max stays exact", in: `{"a":9223372036854775807.0}`, want: `{"a":9223372036854775807}`},
		{name: "plain integer", in: `{"a":42}`, want: `{"a":42}`},
		{name: "non-integral left as is", in: `{"a":1.5,"b":0.25}`, want: `{"a":1.5,"b":0.25}`},
		{name: "nested objects and arrays", in: `{"o":{"x":[1.0,{"y":2.0},2.5]}}`, want: `{"o":{"x":[1,{"y":2},2.5]}}`},
		{name: "non-number values untouched", in: `{"s":"1.0","b":true,"n":null,"l":[]}`, want: `{"b":true,"l":[],"n":null,"s":"1.0"}`},
		{name: "top-level array", in: `[1.0,"x"]`, want: `[1,"x"]`},
		{name: "invalid JSON", in: `{"a":`, wantErr: true},
		{name: "trailing data", in: `{"a":1.0} {"b":2}`, wantErr: true},
		{name: "empty body", in: ``, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeIntegralJSONNumbers([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNormalizedProductionHealthBodyDecodes(t *testing.T) {
	const body = `{"alerts":[],"health":{"bandwidthUsage":{"current":0.0,"max":2147483648.0,"provisioned":1073741824.0},"dataReductionRatio":1.0,"estimatedMaxCapacity":0,"iopsUsage":{"current":0.0,"max":150000.0,"provisioned":67948.0},"space":{"shared":0,"snapshots":0,"totalUsed":0,"unique":0},"usedCapacityPercentage":0.0}}`

	var direct armpurestorageblock.StoragePoolHealthInfo
	if err := json.Unmarshal([]byte(body), &direct); err == nil {
		t.Fatal("the SDK decoded the raw production body; the normalization is no longer needed")
	}

	normalized, err := normalizeIntegralJSONNumbers([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var info armpurestorageblock.StoragePoolHealthInfo
	if err := json.Unmarshal(normalized, &info); err != nil {
		t.Fatalf("decoding the normalized body: %v", err)
	}
	h := info.Health
	if h == nil || h.BandwidthUsage == nil || h.IopsUsage == nil || h.Space == nil {
		t.Fatalf("incomplete health: %+v", h)
	}
	ints := []struct {
		name string
		got  *int64
		want int64
	}{
		{"bandwidth current", h.BandwidthUsage.Current, 0},
		{"bandwidth provisioned", h.BandwidthUsage.Provisioned, 1073741824},
		{"bandwidth max", h.BandwidthUsage.Max, 2147483648},
		{"iops current", h.IopsUsage.Current, 0},
		{"iops provisioned", h.IopsUsage.Provisioned, 67948},
		{"iops max", h.IopsUsage.Max, 150000},
		{"estimated max capacity", h.EstimatedMaxCapacity, 0},
		{"space total used", h.Space.TotalUsed, 0},
	}
	for _, c := range ints {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %v, want %d", c.name, c.got, c.want)
		}
	}
	if h.DataReductionRatio == nil || *h.DataReductionRatio != 1 {
		t.Errorf("data reduction ratio = %v, want 1", h.DataReductionRatio)
	}
	if h.UsedCapacityPercentage == nil || *h.UsedCapacityPercentage != 0 {
		t.Errorf("used capacity percentage = %v, want 0", h.UsedCapacityPercentage)
	}
	if len(info.Alerts) != 0 {
		t.Errorf("alerts = %v, want none", info.Alerts)
	}
}
