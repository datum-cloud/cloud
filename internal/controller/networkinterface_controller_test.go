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
	egressTestNode       = "worker-3"
)

// newEgressReconciler builds a reconciler over a cell holding the shards
// given, so a test states only what it is about.
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

// newEgressShard is the shard an operator wrote for one node, whose process
// reported its address.
func newEgressShard(name, node, sid, address string) *bgpv1alpha1.EgressShard {
	shard := &bgpv1alpha1.EgressShard{}
	shard.Namespace = egressShardNamespace
	shard.Name = name
	shard.Spec.TargetRef.Name = node
	shard.Spec.ShardSID = sid
	shard.Spec.ShardAddressIPv6 = address
	shard.Status.ShardSID = sid
	shard.Status.ShardAddressIPv6 = address
	return shard
}

// newEgressContext is a location carrying the declaration projected onto it.
func newEgressContext(mode networkingv1alpha.NetworkInternetEgressMode) *networkingv1alpha.NetworkContext {
	networkContext := &networkingv1alpha.NetworkContext{}
	networkContext.Namespace = egressTestNamespace
	networkContext.Name = "default-us-central-1"
	networkContext.Spec.Egress = &networkingv1alpha.NetworkContextEgress{
		Internet: &networkingv1alpha.NetworkContextInternetEgress{
			Mode:  mode,
			Reach: []networkingv1alpha.IPFamily{networkingv1alpha.IPv6Protocol},
		},
	}
	return networkContext
}

// newEgressAttachment is an attachment that has, or has not yet, reported the
// node it landed on.
func newEgressAttachment(node string) *cloudv1alpha1.VPCAttachment {
	attachment := &cloudv1alpha1.VPCAttachment{}
	attachment.Namespace = egressTestNamespace
	attachment.Name = "web-eth0"
	attachment.Status.Node = node
	return attachment
}

// The node reads the declaration and nothing else. Whether it has a shard, and
// whether that shard has an address, is the node's to know and the consumer's
// to read back; neither changes what the node is told.
func TestResolveInternetEgressInstallsARouteWhenEnabled(t *testing.T) {
	r := newEgressReconciler(t)

	egress, err := r.resolveInternetEgress(t.Context(),
		newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled), newEgressAttachment(""))
	if err != nil {
		t.Fatalf("resolveInternetEgress: %v", err)
	}
	block := egress.conflist()
	if block == nil || block.Internet == nil || block.Internet.Mode != galactic.InternetEgressEnabled {
		t.Fatalf("an enabled network rendered %v, want the Enabled declaration", block)
	}
	if egress.status() != nil {
		t.Error("an attachment on no known node published an address")
	}
}

// Every reason a network reaches nothing renders the same absent block. A node
// that receives no block installs no route.
func TestResolveInternetEgressYieldsNothingWhenNotEnabled(t *testing.T) {
	unprojected := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	unprojected.Spec.Egress = nil

	noInternet := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	noInternet.Spec.Egress.Internet = nil

	tests := []struct {
		name           string
		networkContext *networkingv1alpha.NetworkContext
	}{
		{"disabled", newEgressContext(networkingv1alpha.NetworkInternetEgressDisabled)},
		{"mode never projected", newEgressContext("")},
		{"egress never projected", unprojected},
		{"no internet egress projected", noInternet},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := newEgressReconciler(t,
				newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:1::1"))

			egress, err := r.resolveInternetEgress(t.Context(), test.networkContext,
				newEgressAttachment(egressTestNode))
			if err != nil {
				t.Fatalf("resolveInternetEgress: %v", err)
			}
			if egress != nil {
				t.Errorf("resolved %v, want nothing", egress)
			}
			if egress.conflist() != nil {
				t.Error("no egress resolved but a block was rendered")
			}
			if egress.status() != nil {
				t.Error("no egress resolved but an address was published")
			}
		})
	}
}

// The address a consumer reads back is the one on the node their instance
// landed on, shared by every network there, which is what stability None
// states.
func TestResolveInternetEgressReportsTheNodesShardAddress(t *testing.T) {
	r := newEgressReconciler(t,
		newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

	egress, err := r.resolveInternetEgress(t.Context(),
		newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled), newEgressAttachment(egressTestNode))
	if err != nil {
		t.Fatalf("resolveInternetEgress: %v", err)
	}
	status := egress.status()
	if status == nil || status.Internet == nil {
		t.Fatal("a shard reporting an address published nothing")
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
	if addresses[0].Stability != cloudv1alpha1.InternetEgressAddressStabilityNone {
		t.Errorf("stability: got %q, want None", addresses[0].Stability)
	}
}

// Egress that is declared and an address that cannot yet be stated are
// different facts. The node is told to route; the consumer is told nothing
// rather than a value they might allow-list.
func TestResolveInternetEgressWithholdsAnAddressItCannotState(t *testing.T) {
	tests := []struct {
		name    string
		node    string
		objects []client.Object
	}{
		{"attachment has reported no node", "", []client.Object{
			newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100")}},
		{"node has no shard", egressTestNode, nil},
		{"shard has reported no address", egressTestNode, []client.Object{
			newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "")}},
		{"shard is on another node", egressTestNode, []client.Object{
			newEgressShard("worker-4-egress", "worker-4", "2001:db8:ff02::", "2001:db8:f00d::200")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := newEgressReconciler(t, test.objects...)

			egress, err := r.resolveInternetEgress(t.Context(),
				newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled), newEgressAttachment(test.node))
			if err != nil {
				t.Fatalf("resolveInternetEgress: %v", err)
			}
			if egress == nil || egress.conflist() == nil {
				t.Fatal("an enabled network rendered no declaration for the node")
			}
			if status := egress.status(); status != nil {
				t.Errorf("published %v, want no address", status.Internet.SourceAddresses)
			}
		})
	}
}

// Two shards naming one node is an operator error, and every attachment on that
// node has to compute the same answer from it.
func TestResolveInternetEgressIsDeterministicForOneNode(t *testing.T) {
	r := newEgressReconciler(t,
		newEgressShard("worker-3-egress-b", egressTestNode, "2001:db8:ff02::", "2001:db8:f00d::200"),
		newEgressShard("worker-3-egress-a", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

	for range 2 {
		egress, err := r.resolveInternetEgress(t.Context(),
			newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled), newEgressAttachment(egressTestNode))
		if err != nil {
			t.Fatalf("resolveInternetEgress: %v", err)
		}
		addresses := egress.status().Internet.SourceAddresses
		if len(addresses) != 1 || addresses[0].Address != "2001:db8:f00d::100" {
			t.Errorf("got %v, want the first shard by name", addresses)
		}
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
