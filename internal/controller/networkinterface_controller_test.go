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

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
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

// newEgressClaim is the binding a location's egress was decided into: the one
// claim per network context, naming the one shard it egresses through.
func newEgressClaim(shardName string) *cloudv1alpha1.EgressShardClaim {
	claim := &cloudv1alpha1.EgressShardClaim{}
	claim.Namespace = egressTestNamespace
	claim.Name = "default-us-central-1"
	claim.Labels = map[string]string{cloudv1alpha1.LabelEgressShardClaimShard: shardName}
	claim.Spec = cloudv1alpha1.EgressShardClaimSpec{
		Network:        cloudv1alpha1.NetworkRef{Name: "default"},
		NetworkContext: cloudv1alpha1.NetworkContextRef{Name: "default-us-central-1"},
		ClassName:      "shared",
		Sharing:        cloudv1alpha1.EgressSharingShared,
		Families:       []cloudv1alpha1.InternetEgressAddressFamily{cloudv1alpha1.InternetEgressAddressFamilyIPv6},
	}
	if shardName != "" {
		claim.Status.ShardRef = &cloudv1alpha1.EgressShardReference{
			Namespace: egressShardNamespace,
			Name:      shardName,
		}
	}
	return claim
}

// The route a node installs, read off the binding rather than selected here.
func TestResolveInternetEgressReadsTheBoundShard(t *testing.T) {
	r := newEgressReconciler(t,
		newEgressClaim("shard-b"),
		newEgressShard("shard-b", "2001:db8:ff02::", "2001:db8:f00d::200", poolLabels()),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()),
	)

	egress, err := r.resolveInternetEgress(t.Context(),
		newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled))
	if err != nil {
		t.Fatalf("resolveInternetEgress: %v", err)
	}
	if egress == nil {
		t.Fatal("a bound claim resolved no egress")
	}
	// One entry, the bound shard's, even though another shard sorts ahead of it
	// by name and matches the same class.
	want := []string{"2001:db8:ff02::"}
	if !slices.Equal(egress.shardSIDs, want) {
		t.Errorf("shard SIDs: got %v, want %v", egress.shardSIDs, want)
	}
}

// The bug binding removes. The node installs the first shard whose SID it can
// resolve a route toward, which is not the first shard by name: a node that is
// itself a shard can never resolve a route to its own advertised SID, and every
// compute node runs the translator. Reporting the first shard by name therefore
// told a workload on such a node one source address while its packets left on
// another, breaking any allow-list built on the value. One bound shard is one
// answer on both sides.
func TestResolveInternetEgressReportsTheBoundShardsOwnAddress(t *testing.T) {
	r := newEgressReconciler(t,
		newEgressClaim("shard-b"),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()),
		newEgressShard("shard-b", "2001:db8:ff02::", "2001:db8:f00d::200", poolLabels()),
	)

	egress, err := r.resolveInternetEgress(t.Context(),
		newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled))
	if err != nil {
		t.Fatalf("resolveInternetEgress: %v", err)
	}
	addresses := egress.status().Internet.SourceAddresses
	if len(addresses) != 1 || addresses[0].Address != "2001:db8:f00d::200" {
		t.Fatalf("got %v, want only the bound shard's address", addresses)
	}
	// The address reported and the SID the node routes toward have to come from
	// the same shard, which is the property the divergence broke.
	if !slices.Equal(egress.shardSIDs, []string{"2001:db8:ff02::"}) {
		t.Errorf("shard SIDs: got %v, want the bound shard's", egress.shardSIDs)
	}
}

// Every reason a network reaches nothing renders the same absent block. A node
// that receives no block installs no route.
func TestResolveInternetEgressYieldsNothingWhenUnbound(t *testing.T) {
	otherImplementation := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	otherImplementation.Spec.Egress.Internet.ParametersRef.Kind = "SomeOtherParameters"

	noParameters := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	noParameters.Spec.Egress.Internet.ParametersRef = nil

	unprojected := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	unprojected.Spec.Egress = nil

	noInternet := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	noInternet.Spec.Egress.Internet = nil

	boundShard := newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels())

	tests := []struct {
		name           string
		networkContext *networkingv1alpha.NetworkContext
		objects        []client.Object
	}{
		{
			name:           "disabled",
			networkContext: newEgressContext(networkingv1alpha.NetworkInternetEgressDisabled),
			objects:        []client.Object{newEgressClaim("shard-a"), boundShard},
		},
		{
			name:           "mode never projected",
			networkContext: newEgressContext(""),
			objects:        []client.Object{newEgressClaim("shard-a"), boundShard},
		},
		{
			name:           "egress never projected",
			networkContext: unprojected,
			objects:        []client.Object{newEgressClaim("shard-a"), boundShard},
		},
		{
			name:           "no internet egress projected",
			networkContext: noInternet,
			objects:        []client.Object{newEgressClaim("shard-a"), boundShard},
		},
		{
			name:           "class names no parameters",
			networkContext: noParameters,
			objects:        []client.Object{newEgressClaim("shard-a"), boundShard},
		},
		{
			name:           "parameters owned by another implementation",
			networkContext: otherImplementation,
			objects:        []client.Object{newEgressClaim("shard-a"), boundShard},
		},
		{
			name:           "nothing claimed a shard for this location",
			networkContext: newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
			objects:        []client.Object{boundShard},
		},
		{
			name:           "the claim is still waiting for a shard",
			networkContext: newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
			objects:        []client.Object{newEgressClaim(""), boundShard},
		},
		{
			name:           "the bound shard is gone",
			networkContext: newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
			objects:        []client.Object{newEgressClaim("shard-a")},
		},
		{
			name:           "the bound shard reports no identifier",
			networkContext: newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
			objects: []client.Object{newEgressClaim("unprogrammed"),
				newEgressShard("unprogrammed", "", "2001:db8:f00d::100", poolLabels())},
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
				t.Errorf("got %v, want no egress", egress.shardSIDs)
			}
			// Absence has to reach both sides: a node that receives no block
			// installs no route, and a consumer who reads no address has none
			// to act on.
			if egress.conflist() != nil {
				t.Error("no egress resolved but a conflist block rendered")
			}
			if egress.status() != nil {
				t.Error("no egress resolved but an address was published")
			}
		})
	}
}

// The invariant the datapath depends on: intent is a function of (VPC, cell)
// and nothing else, so every attachment of a VPC computes the same value and
// the install is idempotent. This asserts the property at the only seam where
// it could be broken — the resolver takes the context and nothing else.
func TestResolveInternetEgressIsAFunctionOfTheNetworkContextAlone(t *testing.T) {
	r := newEgressReconciler(t,
		newEgressClaim("shard-a"),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()),
		newEgressShard("shard-b", "2001:db8:ff02::", "2001:db8:f00d::200", poolLabels()),
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
	if first == nil || second == nil || !slices.Equal(first.shardSIDs, second.shardSIDs) {
		t.Errorf("two attachments of one VPC resolved %v and %v", first, second)
	}
}

// The ordered array shape stays even though binding yields one entry, so a
// binder recording a standby shard later needs no change on the node.
func TestResolveInternetEgressRendersAnOrderedCandidateList(t *testing.T) {
	r := newEgressReconciler(t,
		newEgressClaim("shard-a"),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()),
	)

	egress, err := r.resolveInternetEgress(t.Context(),
		newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled))
	if err != nil {
		t.Fatalf("resolveInternetEgress: %v", err)
	}
	block := egress.conflist()
	if block == nil {
		t.Fatal("a bound claim rendered no conflist block")
	}
	if !slices.Equal(block.ShardSIDs, []string{"2001:db8:ff01::"}) {
		t.Errorf("shardSIDs: got %v, want a one-entry list", block.ShardSIDs)
	}
}

// The address a consumer reads back, and the contract that qualifies it. The
// stability is derived here rather than by the consumer, so this is the only
// place the class's sharing is interpreted.
func TestResolveInternetEgressPublishesTheSourceAddress(t *testing.T) {
	tests := []struct {
		name    string
		sharing networkingv1alpha.InternetEgressSharing
		want    cloudv1alpha1.InternetEgressAddressStability
	}{
		{"shared", networkingv1alpha.InternetEgressSharingShared,
			cloudv1alpha1.InternetEgressAddressStabilityNone},
		{"dedicated", networkingv1alpha.InternetEgressSharingDedicated,
			cloudv1alpha1.InternetEgressAddressStabilityNetwork},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := newEgressReconciler(t,
				newEgressClaim("shard-a"),
				newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()),
			)
			networkContext := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
			networkContext.Spec.Egress.Internet.Sharing = test.sharing

			egress, err := r.resolveInternetEgress(t.Context(), networkContext)
			if err != nil {
				t.Fatalf("resolveInternetEgress: %v", err)
			}
			status := egress.status()
			if status == nil || status.Internet == nil {
				t.Fatal("a bound shard reporting an address published nothing")
			}
			addresses := status.Internet.SourceAddresses
			if len(addresses) != 1 {
				t.Fatalf("source addresses: got %d, want 1", len(addresses))
			}
			if addresses[0].Family != cloudv1alpha1.InternetEgressAddressFamilyIPv6 {
				t.Errorf("family: got %q, want IPv6", addresses[0].Family)
			}
			if addresses[0].Address != "2001:db8:f00d::100" {
				t.Errorf("address: got %q, want %q", addresses[0].Address, "2001:db8:f00d::100")
			}
			if addresses[0].Stability != test.want {
				t.Errorf("stability: got %q, want %q", addresses[0].Stability, test.want)
			}
		})
	}
}

// Egress that works and an address that cannot yet be stated are different
// facts. The node is told where to route; the consumer is told nothing rather
// than a value they might allow-list.
func TestResolveInternetEgressWithholdsAnAddressItCannotState(t *testing.T) {
	tests := []struct {
		name    string
		sharing networkingv1alpha.InternetEgressSharing
		address string
	}{
		{"shard has reported no address", networkingv1alpha.InternetEgressSharingShared, ""},
		{"sharing was never projected", "", "2001:db8:f00d::100"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := newEgressReconciler(t,
				newEgressClaim("shard-a"),
				newEgressShard("shard-a", "2001:db8:ff01::", test.address, poolLabels()),
			)
			networkContext := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
			networkContext.Spec.Egress.Internet.Sharing = test.sharing

			egress, err := r.resolveInternetEgress(t.Context(), networkContext)
			if err != nil {
				t.Fatalf("resolveInternetEgress: %v", err)
			}
			if egress == nil || egress.conflist() == nil {
				t.Fatal("a bound shard rendered no route for the node")
			}
			if status := egress.status(); status != nil {
				t.Errorf("published %v, want no address", status.Internet.SourceAddresses)
			}
		})
	}
}

// Egress withdrawn has to be egress unreported. An address left behind on the
// attachment is one a consumer keeps allow-listing after the path is gone.
func TestPublishAttachmentStatusWithdrawsAnUnboundAddress(t *testing.T) {
	attachment := &cloudv1alpha1.VPCAttachment{}
	attachment.Namespace = egressTestNamespace
	attachment.Name = "web-eth0"
	attachment.Spec.VPC = cloudv1alpha1.VPCRef{Name: "default-us-central-1"}
	attachment.Spec.Interface.Name = "eth0"
	attachment.Status.Egress = &cloudv1alpha1.VPCAttachmentEgressStatus{
		Internet: &cloudv1alpha1.VPCAttachmentInternetEgressStatus{
			SourceAddresses: []cloudv1alpha1.InternetEgressSourceAddress{{
				Family:    cloudv1alpha1.InternetEgressAddressFamilyIPv6,
				Address:   "2001:db8:f00d::100",
				Stability: cloudv1alpha1.InternetEgressAddressStabilityNone,
			}},
		},
	}

	scheme := runtime.NewScheme()
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the cloud scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&cloudv1alpha1.VPCAttachment{}).
		WithObjects(attachment).Build()
	r := &NetworkInterfaceReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}

	vpc := &cloudv1alpha1.VPC{}
	vpc.Status.VPC = "0000000jU"
	nad := &nadv1.NetworkAttachmentDefinition{}
	nad.Name = attachment.Name
	nad.Labels = map[string]string{LabelVPCAttachment: "01a"}

	if err := r.publishAttachmentStatus(t.Context(), attachment, vpc, nad, nil); err != nil {
		t.Fatalf("publishAttachmentStatus: %v", err)
	}

	stored := &cloudv1alpha1.VPCAttachment{}
	if err := fakeClient.Get(t.Context(), client.ObjectKeyFromObject(attachment), stored); err != nil {
		t.Fatalf("read the attachment back: %v", err)
	}
	if stored.Status.Egress != nil {
		t.Errorf("egress still reported after it was withdrawn: %v", stored.Status.Egress)
	}
	// The other field set this reconciler owns still has to land.
	if stored.Status.VPC != "0000000jU" || stored.Status.VPCAttachment != "01a" {
		t.Errorf("identifiers: got %q/%q", stored.Status.VPC, stored.Status.VPCAttachment)
	}
}

// The BGPAdvertisement reconciler writes a disjoint field set on this same
// status, and both writers do a whole-object update. Neither may drop the
// other's fields, which is the property that makes two writers safe without
// server-side apply.
func TestPublishAttachmentStatusKeepsTheOtherWritersFields(t *testing.T) {
	attachment := &cloudv1alpha1.VPCAttachment{}
	attachment.Namespace = egressTestNamespace
	attachment.Name = "web-eth0"
	attachment.Spec.VPC = cloudv1alpha1.VPCRef{Name: "default-us-central-1"}
	attachment.Spec.Interface.Name = "eth0"
	attachment.Status.Node = "node-1"
	attachment.Status.HostInterface = "G0000000jU01aH"
	attachment.Status.PodSubnet = "fd00:10:ff01:0:1::/80"

	scheme := runtime.NewScheme()
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the cloud scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&cloudv1alpha1.VPCAttachment{}).
		WithObjects(attachment).Build()
	r := &NetworkInterfaceReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}

	vpc := &cloudv1alpha1.VPC{}
	vpc.Status.VPC = "0000000jU"
	nad := &nadv1.NetworkAttachmentDefinition{}
	nad.Name = attachment.Name
	nad.Labels = map[string]string{LabelVPCAttachment: "01a"}
	egress := &internetEgress{
		shardSIDs: []string{"2001:db8:ff01::"},
		sourceAddress: &cloudv1alpha1.InternetEgressSourceAddress{
			Family:    cloudv1alpha1.InternetEgressAddressFamilyIPv6,
			Address:   "2001:db8:f00d::100",
			Stability: cloudv1alpha1.InternetEgressAddressStabilityNone,
		},
	}

	if err := r.publishAttachmentStatus(t.Context(), attachment, vpc, nad, egress); err != nil {
		t.Fatalf("publishAttachmentStatus: %v", err)
	}

	stored := &cloudv1alpha1.VPCAttachment{}
	if err := fakeClient.Get(t.Context(), client.ObjectKeyFromObject(attachment), stored); err != nil {
		t.Fatalf("read the attachment back: %v", err)
	}
	if stored.Status.Node != "node-1" || stored.Status.HostInterface != "G0000000jU01aH" ||
		stored.Status.PodSubnet != "fd00:10:ff01:0:1::/80" {
		t.Errorf("the data plane's field set was dropped: %+v", stored.Status)
	}
	if stored.Status.Egress == nil ||
		stored.Status.Egress.Internet.SourceAddresses[0].Address != "2001:db8:f00d::100" {
		t.Errorf("egress address: got %v", stored.Status.Egress)
	}
}
