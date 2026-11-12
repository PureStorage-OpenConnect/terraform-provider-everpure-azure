// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestDataSourceResourceSchemaParity keeps each data source exposing exactly its
// resource's read-side attributes, no more and no fewer: for each resource/data source
// pair, every data source attribute other than "timeouts" must
// exist in the resource schema with an equal attr.Type (a resource block's type, via
// schema.Schema.Type(), is the same object/list type a data source's equivalent
// ListNestedAttribute produces), or be named in that pair's dataSourceOnly allow-list,
// and every resource attribute must either appear in the data source or be named in that
// resource's own write-only allow-list. A new resource attribute therefore fails this
// test until it is either added to the data source or added to the allow-list — the
// parity decision is enforced by the build, not just by convention.
func TestDataSourceResourceSchemaParity(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		// writeOnly lists resource attributes that are deliberately absent from the
		// data source: provider-only deletion controls, create-only source
		// selection, and provisioned_size_in_gib (a volume created outside
		// Terraform can be any multiple of 512 bytes, so a computed GiB value
		// would have to round or be null).
		writeOnly map[string]bool
		// dataSourceOnly lists data source attributes that are deliberately absent from
		// the resource, for one of two reasons: the data source reports information about
		// an object it does not own, so the resource can never hold a non-trivial value
		// for it; or the value changes on its own (space), so the resource could hold it
		// but it would show as drift on every refresh.
		dataSourceOnly map[string]bool
		// widened maps an attribute present on both sides, whose element is an object
		// type, to the extra element attributes the data source adds. The resource's
		// element attributes must all exist, with equal types, in the data source's
		// element, and the data source's remaining attributes must be exactly this set.
		widened      map[string]map[string]bool
		resourceType resource.Resource
		dataSource   datasource.DataSource
	}{
		{
			name:         "storage pool",
			writeOnly:    map[string]bool{},
			resourceType: NewStoragePoolResource(),
			dataSource:   NewStoragePoolDataSource(),
		},
		{
			name: "reservation",
			// user is personal data: the resource keeps it out of state, and the data
			// source must not return it either.
			writeOnly:    map[string]bool{"user": true},
			resourceType: NewReservationResource(),
			dataSource:   NewReservationDataSource(),
		},
		{
			name: "volume group",
			writeOnly: map[string]bool{
				"source_type":                        true,
				"source_volume_group_id":             true,
				"source_snapshot_id":                 true,
				"source_recoverable_volume_group_id": true,
				"deletion_protection":                true,
				"eradicate_on_destroy":               true,
			},
			resourceType: NewVolumeGroupResource(),
			dataSource:   NewVolumeGroupDataSource(),
		},
		{
			name: "volume",
			writeOnly: map[string]bool{
				"provisioned_size_in_gib":      true,
				"source_type":                  true,
				"source_volume_id":             true,
				"source_serial_number":         true,
				"source_recoverable_volume_id": true,
				"source_snapshot":              true,
				"deletion_protection":          true,
				"eradicate_on_destroy":         true,
			},
			// space is volatile (it changes with every write and with deduplication), so
			// the resource can't hold it without producing a plan diff on every refresh;
			// a data source is read afresh each time and can report it.
			dataSourceOnly: map[string]bool{"space": true},
			resourceType:   NewVolumeResource(),
			dataSource:     NewVolumeDataSource(),
		},
		{
			name: "snapshot",
			writeOnly: map[string]bool{
				"source_recoverable_snapshot_id": true,
				"deletion_protection":            true,
				"eradicate_on_destroy":           true,
			},
			// The resource cannot create or import a snapshot taken by the volume group's
			// snapshot schedule, so created_by_policy would always be false on it. space is
			// volatile (it changes as the volumes and their deduplication change), so the
			// resource can't hold it without a plan diff on refresh.
			dataSourceOnly: map[string]bool{"created_by_policy": true, "space": true},
			// Each volume_snapshots element also carries its own space, for the same reason.
			widened:      map[string]map[string]bool{"volume_snapshots": {"space": true}},
			resourceType: NewSnapshotResource(),
			dataSource:   NewSnapshotDataSource(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resourceSchemaResp resource.SchemaResponse
			tt.resourceType.Schema(ctx, resource.SchemaRequest{}, &resourceSchemaResp)
			if resourceSchemaResp.Diagnostics.HasError() {
				t.Fatalf("unexpected diagnostics building resource schema: %v", resourceSchemaResp.Diagnostics)
			}
			resourceObjType, ok := resourceSchemaResp.Schema.Type().(types.ObjectType)
			if !ok {
				t.Fatalf("resource schema Type() = %T, want types.ObjectType", resourceSchemaResp.Schema.Type())
			}

			var dsSchemaResp datasource.SchemaResponse
			tt.dataSource.Schema(ctx, datasource.SchemaRequest{}, &dsSchemaResp)
			if dsSchemaResp.Diagnostics.HasError() {
				t.Fatalf("unexpected diagnostics building data source schema: %v", dsSchemaResp.Diagnostics)
			}
			dsObjType, ok := dsSchemaResp.Schema.Type().(types.ObjectType)
			if !ok {
				t.Fatalf("data source schema Type() = %T, want types.ObjectType", dsSchemaResp.Schema.Type())
			}

			for name, dsAttrType := range dsObjType.AttrTypes {
				if name == "timeouts" {
					continue
				}
				resourceAttrType, ok := resourceObjType.AttrTypes[name]
				if !ok {
					if tt.dataSourceOnly[name] {
						continue
					}
					t.Errorf("data source attribute %q has no counterpart in the resource schema", name)
					continue
				}
				if extras, isWidened := tt.widened[name]; isWidened {
					resList, ok := resourceAttrType.(types.ListType)
					if !ok {
						t.Errorf("widened attribute %q: resource type %s, want a list", name, resourceAttrType)
						continue
					}
					dsList, ok := dsAttrType.(types.ListType)
					if !ok {
						t.Errorf("widened attribute %q: data source type %s, want a list", name, dsAttrType)
						continue
					}
					resElem, ok := resList.ElemType.(types.ObjectType)
					if !ok {
						t.Errorf("widened attribute %q: resource element type %s, want an object", name, resList.ElemType)
						continue
					}
					dsElem, ok := dsList.ElemType.(types.ObjectType)
					if !ok {
						t.Errorf("widened attribute %q: data source element type %s, want an object", name, dsList.ElemType)
						continue
					}
					for elemName, resElemAttr := range resElem.AttrTypes {
						dsElemAttr, ok := dsElem.AttrTypes[elemName]
						if !ok {
							t.Errorf("widened attribute %q: resource element attribute %q missing from the data source element", name, elemName)
						} else if !resElemAttr.Equal(dsElemAttr) {
							t.Errorf("widened attribute %q.%q: resource type %s, data source type %s, want equal", name, elemName, resElemAttr, dsElemAttr)
						}
					}
					for elemName := range dsElem.AttrTypes {
						if _, inResource := resElem.AttrTypes[elemName]; !inResource && !extras[elemName] {
							t.Errorf("widened attribute %q: data source element attribute %q is not declared as an extra", name, elemName)
						}
					}
					for elemName := range extras {
						if _, inDS := dsElem.AttrTypes[elemName]; !inDS {
							t.Errorf("widened attribute %q: declared extra %q missing from the data source element", name, elemName)
						}
					}
					continue
				}
				if !resourceAttrType.Equal(dsAttrType) {
					t.Errorf("attribute %q: resource type %s, data source type %s, want equal", name, resourceAttrType, dsAttrType)
				}
			}

			for name := range resourceObjType.AttrTypes {
				if name == "timeouts" || tt.writeOnly[name] {
					continue
				}
				if _, ok := dsObjType.AttrTypes[name]; !ok {
					t.Errorf("resource attribute %q is neither in the data source schema nor in the write-only allow-list", name)
				}
			}
		})
	}
}

// nestedObjectAttrTypes extracts a schema.NestedAttributeObject's attribute types, the
// same way TestDataSourceResourceSchemaParity extracts a top-level schema's.
func nestedObjectAttrTypes(t *testing.T, obj schema.NestedAttributeObject) map[string]attr.Type {
	t.Helper()
	objType, ok := obj.Type().(types.ObjectType)
	if !ok {
		t.Fatalf("NestedAttributeObject.Type() = %T, want types.ObjectType", obj.Type())
	}
	return objType.AttrTypes
}

// singularDataSourceAttributes runs ds's own Schema function and returns its attribute
// map with "timeouts" dropped, so a parity check compares against the real schema
// Terraform sees rather than against whatever map happened to build it.
func singularDataSourceAttributes(t *testing.T, ds datasource.DataSource) map[string]schema.Attribute {
	t.Helper()
	var resp datasource.SchemaResponse
	ds.Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics building data source schema: %v", resp.Diagnostics)
	}
	attrs := make(map[string]schema.Attribute, len(resp.Schema.Attributes))
	for name, a := range resp.Schema.Attributes {
		if name == "timeouts" {
			continue
		}
		attrs[name] = a
	}
	return attrs
}

// TestPluralDataSourceElementSchemaParity keeps each plural data source's list element
// exposing exactly its singular counterpart's attribute set (minus "timeouts"), plus a
// small, explicit allow-list of attributes that exist only because a list can represent
// something a single lookup never has to (soft_deletion, on a volume list element,
// since a soft-deleted volume's own timestamped name is an error on the singular
// lookup, never a returned object — see data_source_volume.go), and a singularOnly
// allow-list of singular attributes a list element deliberately omits (volume_snapshots,
// on a snapshot). The recoverable volume group element has no singular data source of
// its own, so it is pinned to the volume group element instead, minus the two attributes
// the API doesn't return for a recoverable volume group (tags, internal_id).
func TestPluralDataSourceElementSchemaParity(t *testing.T) {
	tests := []struct {
		name         string
		singularDS   datasource.DataSource
		plural       schema.NestedAttributeObject
		extraAllowed map[string]bool
		// singularOnly lists singular attributes a plural element deliberately lacks.
		singularOnly map[string]bool
	}{
		{
			name:       "storage pools",
			singularDS: NewStoragePoolDataSource(),
			plural:     storagePoolsElementNestedObject(),
		},
		{
			name:       "reservations",
			singularDS: NewReservationDataSource(),
			plural:     reservationsElementNestedObject(),
		},
		{
			name:       "volume groups",
			singularDS: NewVolumeGroupDataSource(),
			plural:     volumeGroupsElementNestedObject(),
		},
		{
			name:         "volumes",
			singularDS:   NewVolumeDataSource(),
			plural:       volumesElementNestedObject(),
			extraAllowed: map[string]bool{"soft_deletion": true},
		},
		{
			name:       "snapshots",
			singularDS: NewSnapshotDataSource(),
			plural:     snapshotsElementNestedObject(),
			// The list API never returns a snapshot's member volumes. Filling them in
			// would take one extra GET per element, and a volume group with an hourly
			// snapshot schedule can hold hundreds of snapshots, so a list element omits
			// them and a caller looks up the one snapshot it needs by name.
			singularOnly: map[string]bool{"volume_snapshots": true},
			extraAllowed: map[string]bool{"soft_deletion": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			singularSchema := schema.Schema{Attributes: singularDataSourceAttributes(t, tt.singularDS)}
			singularObjType, ok := singularSchema.Type().(types.ObjectType)
			if !ok {
				t.Fatalf("singular schema Type() = %T, want types.ObjectType", singularSchema.Type())
			}

			pluralAttrTypes := nestedObjectAttrTypes(t, tt.plural)

			for name, wantType := range singularObjType.AttrTypes {
				gotType, ok := pluralAttrTypes[name]
				if !ok {
					if tt.singularOnly[name] {
						continue
					}
					t.Errorf("plural element is missing attribute %q, present on the singular data source", name)
					continue
				}
				if !wantType.Equal(gotType) {
					t.Errorf("attribute %q: singular type %s, plural element type %s, want equal", name, wantType, gotType)
				}
			}

			for name := range pluralAttrTypes {
				if _, ok := singularObjType.AttrTypes[name]; ok {
					continue
				}
				if tt.extraAllowed[name] {
					continue
				}
				t.Errorf("plural element attribute %q has no counterpart on the singular data source and is not in extraAllowed", name)
			}
		})
	}

	t.Run("recoverable volume groups (pinned to the volume group element)", func(t *testing.T) {
		vgAttrTypes := nestedObjectAttrTypes(t, volumeGroupsElementNestedObject())
		rvgAttrTypes := nestedObjectAttrTypes(t, recoverableVolumeGroupsElementNestedObject())

		notReturnedByAPI := map[string]bool{"tags": true, "internal_id": true}
		extraAllowed := map[string]bool{"soft_deletion": true}

		for name, wantType := range vgAttrTypes {
			if notReturnedByAPI[name] {
				continue
			}
			gotType, ok := rvgAttrTypes[name]
			if !ok {
				t.Errorf("recoverable volume group element is missing attribute %q, present on the volume group element", name)
				continue
			}
			if !wantType.Equal(gotType) {
				t.Errorf("attribute %q: volume group element type %s, recoverable element type %s, want equal", name, wantType, gotType)
			}
		}

		for name := range rvgAttrTypes {
			if _, ok := vgAttrTypes[name]; ok {
				continue
			}
			if extraAllowed[name] {
				continue
			}
			t.Errorf("recoverable volume group element attribute %q has no counterpart on the volume group element and is not in extraAllowed", name)
		}
	})
}
