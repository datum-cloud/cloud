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
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/galactic"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

func TestMasterPlugin(t *testing.T) {
	tests := []struct {
		mode cloudv1alpha1.VPCAttachmentInterfaceMode
		want string
	}{
		{cloudv1alpha1.VPCAttachmentInterfaceModeNetns, galactic.PluginVeth},
		{cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor, galactic.PluginTap},
		{cloudv1alpha1.VPCAttachmentInterfaceModeHypervisorDeclared, galactic.PluginTap},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			if got := masterPlugin(test.mode); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestClaimFulfilled(t *testing.T) {
	newInterface := func(phase networkingv1alpha.NetworkInterfacePhase, allocated metav1.ConditionStatus,
		context *networkingv1alpha.LocalNetworkContextRef) *networkingv1alpha.NetworkInterface {
		return &networkingv1alpha.NetworkInterface{
			Status: networkingv1alpha.NetworkInterfaceStatus{
				Phase:             phase,
				NetworkContextRef: context,
				Conditions: []metav1.Condition{{
					Type:   networkingv1alpha.NetworkInterfaceAllocated,
					Status: allocated,
					Reason: "Test",
				}},
			},
		}
	}
	context := &networkingv1alpha.LocalNetworkContextRef{Name: "default-us-central-1"}

	tests := []struct {
		name             string
		networkInterface *networkingv1alpha.NetworkInterface
		want             bool
	}{
		{"bound and allocated", newInterface(
			networkingv1alpha.NetworkInterfacePhaseBound, metav1.ConditionTrue, context), true},
		{"available", newInterface(
			networkingv1alpha.NetworkInterfacePhaseAvailable, metav1.ConditionTrue, context), false},
		{"not allocated", newInterface(
			networkingv1alpha.NetworkInterfacePhaseBound, metav1.ConditionFalse, context), false},
		{"no network context", newInterface(
			networkingv1alpha.NetworkInterfacePhaseBound, metav1.ConditionTrue, nil), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := claimFulfilled(test.networkInterface); got != test.want {
				t.Errorf("got %v, want %v", got, test.want)
			}
		})
	}
}

// The cell-wide mode is the only signal every attachment written until now
// carries, so it has to keep deciding for all of them. An interface that
// states its own mode is the exception.
func TestAttachmentModeFallsBackToTheCell(t *testing.T) {
	r := &NetworkInterfaceReconciler{
		AttachmentMode: cloudv1alpha1.VPCAttachmentInterfaceModeNetns,
	}

	tests := []struct {
		name  string
		iface networkingv1alpha.NetworkInterfaceAttachmentMode
		want  cloudv1alpha1.VPCAttachmentInterfaceMode
	}{
		{"unset", "", cloudv1alpha1.VPCAttachmentInterfaceModeNetns},
		{"netns", networkingv1alpha.NetworkInterfaceAttachmentModeNetns,
			cloudv1alpha1.VPCAttachmentInterfaceModeNetns},
		{"hypervisor", networkingv1alpha.NetworkInterfaceAttachmentModeHypervisor,
			cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor},
		{"declared", networkingv1alpha.NetworkInterfaceAttachmentModeHypervisorDeclared,
			cloudv1alpha1.VPCAttachmentInterfaceModeHypervisorDeclared},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			networkInterface := &networkingv1alpha.NetworkInterface{
				Spec: networkingv1alpha.NetworkInterfaceSpec{AttachmentMode: test.iface},
			}
			if got := r.attachmentMode(networkInterface); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

// Only the declared mode asks the tap plugin to describe the device.
// TestHostInterfaceAnnotationKey pins the annotation key this controller writes
// on the NAD. The key is a contract with the data plane: galactic writes the
// same one during CNI ADD, and a runtime reads it to learn its host device.
// Renaming it here alone would leave that runtime with nothing to read, and
// nothing else in this repository would fail.
func TestHostInterfaceAnnotationKey(t *testing.T) {
	const want = "k8s.v1.cni.cncf.io/host-interface"
	if AnnotationHostInterface != want {
		t.Errorf("AnnotationHostInterface = %q, want %q", AnnotationHostInterface, want)
	}
}

func TestDeclaresDevice(t *testing.T) {
	if declaresDevice(cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor) {
		t.Error("a discovered hypervisor attachment must not ask for a description")
	}
	if !declaresDevice(cloudv1alpha1.VPCAttachmentInterfaceModeHypervisorDeclared) {
		t.Error("a declared hypervisor attachment must ask for a description")
	}
}

const (
	egressTestNamespace  = "project-egress"
	egressShardNamespace = "galactic-system"
	egressTestParameters = "shared-ipv6"
)

// newEgressReconciler builds a reconciler over a cell holding the shards and
// the parameters given, so a test states only what it is about.
func newEgressReconciler(t *testing.T, objects ...client.Object) *NetworkInterfaceReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := networkingv1alpha.AddToScheme(scheme); err != nil {
		t.Fatalf("build the networking scheme: %v", err)
	}
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the cloud scheme: %v", err)
	}
	if err := bgpv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the fabric scheme: %v", err)
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &NetworkInterfaceReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}
}

// newEgressShard is a shard an operator labelled and whose node reported a SID.
func newEgressShard(name, sid, address string, shardLabels map[string]string) *bgpv1alpha1.EgressShard {
	shard := &bgpv1alpha1.EgressShard{}
	shard.Namespace = egressShardNamespace
	shard.Name = name
	shard.Labels = shardLabels
	shard.Spec.ShardAddressIPv6 = address
	shard.Status.ShardSID = sid
	shard.Status.ShardAddressIPv6 = address
	return shard
}

// newEgressParameters selects a pool in a cell, which is what an operator
// writes for a class this controller serves.
func newEgressParameters() *cloudv1alpha1.EgressShardParameters {
	parameters := &cloudv1alpha1.EgressShardParameters{}
	parameters.Name = egressTestParameters
	parameters.Spec.ShardNamespace = egressShardNamespace
	parameters.Spec.ShardSelector = metav1.LabelSelector{MatchLabels: map[string]string{
		bgpv1alpha1.LabelEgressShardPool: "shared",
		bgpv1alpha1.LabelEgressShardCell: "us-central-1",
	}}
	return parameters
}

// poolLabels are what an operator sets on a shard serving the selected pool.
func poolLabels() map[string]string {
	return map[string]string{
		bgpv1alpha1.LabelEgressShardPool: "shared",
		bgpv1alpha1.LabelEgressShardCell: "us-central-1",
		bgpv1alpha1.LabelEgressShardIPv6: bgpv1alpha1.LabelValueEgressFamilyServed,
	}
}

// newEgressContext is a location carrying resolved egress intent. Every field
// is written resolved upstream, so nothing under test selects a class or reads
// a default.
func newEgressContext(mode networkingv1alpha.NetworkInternetEgressMode) *networkingv1alpha.NetworkContext {
	networkContext := &networkingv1alpha.NetworkContext{}
	networkContext.Namespace = egressTestNamespace
	networkContext.Name = "default-us-central-1"
	networkContext.Spec.Egress = &networkingv1alpha.NetworkContextEgress{
		Internet: &networkingv1alpha.NetworkContextInternetEgress{
			Mode:      mode,
			Reach:     []networkingv1alpha.IPFamily{networkingv1alpha.IPv6Protocol},
			ClassName: "shared",
			Sharing:   networkingv1alpha.InternetEgressSharingShared,
			ParametersRef: &networkingv1alpha.InternetEgressClassParametersRef{
				Group: cloudv1alpha1.GroupVersion.Group,
				Kind:  cloudv1alpha1.KindEgressShardParameters,
				Name:  egressTestParameters,
			},
		},
	}
	return networkContext
}

func TestResolveInternetEgressSelectsMatchingShards(t *testing.T) {
	r := newEgressReconciler(t,
		newEgressParameters(),
		newEgressShard("shard-b", "2001:db8:ff02::", "2001:db8:1::2", poolLabels()),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:1::1", poolLabels()),
	)

	egress, err := r.resolveInternetEgress(t.Context(),
		newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled))
	if err != nil {
		t.Fatalf("resolveInternetEgress: %v", err)
	}
	if egress == nil {
		t.Fatal("two matching shards resolved no egress")
	}
	// Name order, not list order: two attachments of one VPC reconciled moments
	// apart have to compute the same list, because the VRF they share cannot
	// hold two answers.
	want := []string{"2001:db8:ff01::", "2001:db8:ff02::"}
	if !slices.Equal(egress.ShardSIDs, want) {
		t.Errorf("shard SIDs: got %v, want %v", egress.ShardSIDs, want)
	}
}

// Every reason a network reaches nothing renders the same absent block. A node
// that receives no block installs no route.
func TestResolveInternetEgressYieldsNothingWhenUnbound(t *testing.T) {
	otherImplementation := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	otherImplementation.Spec.Egress.Internet.ParametersRef.Kind = "SomeOtherParameters"

	noParameters := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	noParameters.Spec.Egress.Internet.ParametersRef = nil

	missingParameters := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	missingParameters.Spec.Egress.Internet.ParametersRef.Name = "not-in-this-cell"

	unprojected := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	unprojected.Spec.Egress = nil

	noInternet := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	noInternet.Spec.Egress.Internet = nil

	tests := []struct {
		name           string
		networkContext *networkingv1alpha.NetworkContext
		objects        []client.Object
	}{
		{
			name:           "disabled",
			networkContext: newEgressContext(networkingv1alpha.NetworkInternetEgressDisabled),
			objects: []client.Object{newEgressParameters(),
				newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:1::1", poolLabels())},
		},
		{
			name:           "mode never projected",
			networkContext: newEgressContext(""),
			objects: []client.Object{newEgressParameters(),
				newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:1::1", poolLabels())},
		},
		{
			name:           "egress never projected",
			networkContext: unprojected,
			objects:        []client.Object{newEgressParameters()},
		},
		{
			name:           "no internet egress projected",
			networkContext: noInternet,
			objects:        []client.Object{newEgressParameters()},
		},
		{
			name:           "class names no parameters",
			networkContext: noParameters,
			objects:        []client.Object{newEgressParameters()},
		},
		{
			name:           "parameters owned by another implementation",
			networkContext: otherImplementation,
			objects:        []client.Object{newEgressParameters()},
		},
		{
			name:           "parameters absent from this cell",
			networkContext: missingParameters,
			objects:        []client.Object{newEgressParameters()},
		},
		{
			name:           "no shard matches the selector",
			networkContext: newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
			objects: []client.Object{newEgressParameters(),
				newEgressShard("elsewhere", "2001:db8:ff01::", "2001:db8:1::1", map[string]string{
					bgpv1alpha1.LabelEgressShardPool: "shared",
					bgpv1alpha1.LabelEgressShardCell: "us-east-1",
					bgpv1alpha1.LabelEgressShardIPv6: bgpv1alpha1.LabelValueEgressFamilyServed,
				})},
		},
		{
			name:           "matching shard translates no IPv6",
			networkContext: newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
			objects: []client.Object{newEgressParameters(),
				newEgressShard("ipv4-only", "2001:db8:ff01::", "", map[string]string{
					bgpv1alpha1.LabelEgressShardPool: "shared",
					bgpv1alpha1.LabelEgressShardCell: "us-central-1",
					bgpv1alpha1.LabelEgressShardIPv4: bgpv1alpha1.LabelValueEgressFamilyServed,
				})},
		},
		{
			name:           "matching shard reports no SID",
			networkContext: newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
			objects: []client.Object{newEgressParameters(),
				newEgressShard("unprogrammed", "", "2001:db8:1::1", poolLabels())},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := newEgressReconciler(t, test.objects...)
			egress, err := r.resolveInternetEgress(t.Context(), test.networkContext)
			if err != nil {
				t.Fatalf("resolveInternetEgress: %v", err)
			}
			if egress != nil {
				t.Errorf("got %v, want no egress", egress.ShardSIDs)
			}
		})
	}
}

// A SID an operator typed onto two shards is one candidate, not two.
func TestResolveInternetEgressDeduplicatesShardSIDs(t *testing.T) {
	r := newEgressReconciler(t,
		newEgressParameters(),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:1::1", poolLabels()),
		newEgressShard("shard-b", "2001:db8:ff01::", "2001:db8:1::2", poolLabels()),
	)

	egress, err := r.resolveInternetEgress(t.Context(),
		newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled))
	if err != nil {
		t.Fatalf("resolveInternetEgress: %v", err)
	}
	if want := []string{"2001:db8:ff01::"}; egress == nil || !slices.Equal(egress.ShardSIDs, want) {
		t.Errorf("shard SIDs: got %v, want %v", egress, want)
	}
}

// The invariant the datapath depends on: intent is a function of (VPC, cell)
// and nothing else, so every attachment of a VPC computes the same value and
// the install is idempotent. This asserts the property at the only seam where
// it could be broken — the resolver takes the context and nothing else.
func TestResolveInternetEgressIsAFunctionOfTheNetworkContextAlone(t *testing.T) {
	r := newEgressReconciler(t,
		newEgressParameters(),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:1::1", poolLabels()),
		newEgressShard("shard-b", "2001:db8:ff02::", "2001:db8:1::2", poolLabels()),
	)
	networkContext := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)

	first, err := r.resolveInternetEgress(t.Context(), networkContext)
	if err != nil {
		t.Fatalf("resolveInternetEgress: %v", err)
	}
	second, err := r.resolveInternetEgress(t.Context(), networkContext)
	if err != nil {
		t.Fatalf("resolveInternetEgress: %v", err)
	}
	if first == nil || second == nil || !slices.Equal(first.ShardSIDs, second.ShardSIDs) {
		t.Errorf("two attachments of one VPC resolved %v and %v", first, second)
	}
}
