/*
Copyright © 2026 Datum Technology, Inc. All rights reserved.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/identifier"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// A location's Subnet arrives by propagation, which carries spec and never
// status, so the allocated range has to be readable from either.
func TestSubnetRangePrefersStatusAndFallsBackToSpec(t *testing.T) {
	start := "fd00::"
	var length int32 = 48

	withStatus := &networkingv1alpha.Subnet{
		Spec:   networkingv1alpha.SubnetSpec{StartAddress: "fd20::", PrefixLength: 64},
		Status: networkingv1alpha.SubnetStatus{StartAddress: &start, PrefixLength: &length},
	}
	gotStart, gotLength, ok := subnetRange(withStatus)
	if !ok || gotStart != "fd00::" || gotLength != 48 {
		t.Fatalf("status should win when present, got %q/%d ok=%v", gotStart, gotLength, ok)
	}

	specOnly := &networkingv1alpha.Subnet{
		Spec: networkingv1alpha.SubnetSpec{StartAddress: "fd20::", PrefixLength: 64},
	}
	gotStart, gotLength, ok = subnetRange(specOnly)
	if !ok || gotStart != "fd20::" || gotLength != 64 {
		t.Fatalf("a propagated copy carries spec only, got %q/%d ok=%v", gotStart, gotLength, ok)
	}

	if _, _, ok := subnetRange(&networkingv1alpha.Subnet{}); ok {
		t.Fatal("an unallocated subnet should yield nothing")
	}
}

// A network allocated a fabric identity must resolve to the same VPC
// identifier in every cell that holds it. Two cells reaching different
// identifiers is the defect this replaces: the edge VRF device is named from
// the VPC alone, and the Route Target is derived from it, so a network whose
// locations disagree neither shares a device nor exchanges routes.
func TestFabricIdentityDerivesTheSameVPCIdentifierInEveryCell(t *testing.T) {
	const allocated int64 = 0x1A2B3C4D5E6F

	first, err := vpcIdentifierFor(allocated)
	if err != nil {
		t.Fatalf("derive in the first cell: %v", err)
	}
	second, err := vpcIdentifierFor(allocated)
	if err != nil {
		t.Fatalf("derive in the second cell: %v", err)
	}
	if first != second {
		t.Fatalf("two cells derived %q and %q from the same identity", first, second)
	}

	other, err := vpcIdentifierFor(allocated + 1)
	if err != nil {
		t.Fatalf("derive a neighbouring identity: %v", err)
	}
	if other == first {
		t.Fatalf("distinct identities both derived %q", first)
	}
}

// The identifier lands in a nine-character slot in the galactic VRF device
// name, so nothing the allocator can hand out may render wider than that.
func TestDerivedVPCIdentifierFitsTheVRFDeviceName(t *testing.T) {
	for _, allocated := range []int64{1, 2, 1000, 1 << 24, int64(identifier.MaxVPC) - 1} {
		derived, err := vpcIdentifierFor(allocated)
		if err != nil {
			t.Fatalf("derive %d: %v", allocated, err)
		}
		if len(derived) > 9 {
			t.Fatalf("identity %d rendered %d characters, wider than the slot holds", allocated, len(derived))
		}
	}
}

// Values the fabric cannot represent are refused rather than folded into
// something that collides with another network's identifier.
func TestUnrepresentableFabricIdentityIsRefused(t *testing.T) {
	for _, allocated := range []int64{-1, int64(identifier.MaxVPC), int64(identifier.MaxVPC) + 1} {
		if _, err := vpcIdentifierFor(allocated); err == nil {
			t.Fatalf("identity %d should have been refused", allocated)
		}
	}
}

// Networks that predate the allocator carry no identity, and a context written
// before the field existed carries nothing either. Both read as unallocated.
func TestAbsentFabricIdentityReadsAsUnallocated(t *testing.T) {
	absent, err := fabricIdentityFrom(map[string]any{"spec": map[string]any{}})
	if err != nil || absent != 0 {
		t.Fatalf("an unset field should read as 0, got %d err=%v", absent, err)
	}

	noSpec, err := fabricIdentityFrom(map[string]any{})
	if err != nil || noSpec != 0 {
		t.Fatalf("a context with no spec should read as 0, got %d err=%v", noSpec, err)
	}

	present, err := fabricIdentityFrom(map[string]any{
		"spec": map[string]any{"fabricIdentity": int64(4242)},
	})
	if err != nil || present != 4242 {
		t.Fatalf("a projected identity should read back, got %d err=%v", present, err)
	}

	if _, err := fabricIdentityFrom(map[string]any{
		"spec": map[string]any{"fabricIdentity": "not-a-number"},
	}); err == nil {
		t.Fatal("a non-integer identity should be refused")
	}
}

func fabricTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := cloudv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("register cloud types: %v", err)
	}
	if err := networkingv1alpha.AddToScheme(s); err != nil {
		t.Fatalf("register networking types: %v", err)
	}
	return s
}

// projectedIdentityReader serves the NetworkContext as it appears on the wire,
// carrying the projected identity. The fake client stores objects through their
// registered Go type, which discards a field that type does not carry yet —
// the very reason the reconciler reads this field untyped.
type projectedIdentityReader struct {
	client.Reader
	fabricIdentity int64
}

func (r projectedIdentityReader) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	raw, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return r.Reader.Get(ctx, key, obj, opts...)
	}
	raw.Object = map[string]any{"spec": map[string]any{}}
	if r.fabricIdentity != 0 {
		raw.Object["spec"] = map[string]any{"fabricIdentity": r.fabricIdentity}
	}
	return nil
}

func networkContextWithSubnet() []client.Object {
	networkContext := &networkingv1alpha.NetworkContext{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-us-central-1",
			Namespace: "project-a",
			UID:       "0c1d2e3f-4a5b-6c7d-8e9f-0a1b2c3d4e5f",
		},
		Spec: networkingv1alpha.NetworkContextSpec{
			Network: networkingv1alpha.LocalNetworkRef{Name: "default"},
		},
	}

	subnet := &networkingv1alpha.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "default-v6", Namespace: "project-a"},
		Spec: networkingv1alpha.SubnetSpec{
			NetworkContext: networkingv1alpha.LocalNetworkContextRef{Name: "default-us-central-1"},
			StartAddress:   "fd00::",
			PrefixLength:   48,
		},
	}
	return []client.Object{networkContext, subnet}
}

func reconcileNetworkContext(
	t *testing.T, fabricIdentity int64, objects ...client.Object,
) *cloudv1alpha1.VPC {
	t.Helper()
	s := fabricTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objects...).
		WithStatusSubresource(&cloudv1alpha1.VPC{}).
		Build()

	r := &NetworkContextReconciler{
		Client: c, Scheme: s,
		APIReader: projectedIdentityReader{Reader: c, fabricIdentity: fabricIdentity},
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{
		Name: "default-us-central-1", Namespace: "project-a",
	}}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	vpc := &cloudv1alpha1.VPC{}
	if err := c.Get(context.Background(), request.NamespacedName, vpc); err != nil {
		t.Fatalf("read back the VPC: %v", err)
	}
	return vpc
}

// The allocated identity is what the VPC ends up carrying, which is the whole
// point: every cell reconciling this network reaches the same identifier.
func TestReconcileUsesTheAllocatedFabricIdentity(t *testing.T) {
	const allocated int64 = 0x1A2B3C4D5E6F

	expected, err := vpcIdentifierFor(allocated)
	if err != nil {
		t.Fatalf("derive the expected identifier: %v", err)
	}

	vpc := reconcileNetworkContext(t, allocated, networkContextWithSubnet()...)
	if vpc.Status.VPC != expected {
		t.Fatalf("VPC carries %q, want the derived %q", vpc.Status.VPC, expected)
	}
}

// Nothing has allocated identities yet and existing networks have none, so a
// context without one must still get an identifier exactly as it does today.
func TestReconcileFallsBackToARandomIdentifierWithoutAnAllocatedIdentity(t *testing.T) {
	vpc := reconcileNetworkContext(t, 0, networkContextWithSubnet()...)
	if vpc.Status.VPC == "" {
		t.Fatal("a context with no allocated identity should still receive an identifier")
	}
	if _, err := identifier.Base62ToHex(vpc.Status.VPC); err != nil {
		t.Fatalf("identifier %q is not base62: %v", vpc.Status.VPC, err)
	}
}

// A VPC already carrying an identifier keeps it. Rewriting one renames the
// edge VRF device and changes the Route Target under running traffic.
func TestReconcileLeavesAnAlreadyAllocatedVPCAlone(t *testing.T) {
	const existing = "3fA2bQ71x"

	objects := append(networkContextWithSubnet(), &cloudv1alpha1.VPC{
		ObjectMeta: metav1.ObjectMeta{Name: "default-us-central-1", Namespace: "project-a"},
		Spec:       cloudv1alpha1.VPCSpec{Networks: []cloudv1alpha1.Network{"fd00::/48"}},
		Status:     cloudv1alpha1.VPCStatus{VPC: existing},
	})

	vpc := reconcileNetworkContext(t, 0x1A2B3C4D5E6F, objects...)
	if vpc.Status.VPC != existing {
		t.Fatalf("a live VPC was renumbered from %q to %q", existing, vpc.Status.VPC)
	}
}
