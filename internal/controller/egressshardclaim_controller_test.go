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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

// egressAttachmentName is the attachment every egress test records, and
// therefore the name of the one claim recording it.
const egressAttachmentName = "web-eth0"

func newBinder(t *testing.T, objects ...client.Object) (*EgressShardClaimReconciler, client.Client) {
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

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&cloudv1alpha1.EgressShardClaim{}, &cloudv1alpha1.VPCAttachment{}).
		Build()
	return &EgressShardClaimReconciler{Client: fakeClient, Scheme: scheme}, fakeClient
}

// newLandedAttachment is an attachment of the egress test network that has
// reported the node it landed on.
func newLandedAttachment(node string) *cloudv1alpha1.VPCAttachment {
	attachment := &cloudv1alpha1.VPCAttachment{}
	attachment.Namespace = egressTestNamespace
	attachment.Name = egressAttachmentName
	attachment.Spec.VPC = cloudv1alpha1.VPCRef{Name: "default-us-central-1"}
	attachment.Spec.Interface.Name = "eth0"
	attachment.Status.Node = node
	return attachment
}

// newEgressClaim is a record already written for the test attachment, bound
// to shardName or, with an empty name, still unbound.
func newEgressClaim(shardName string) *cloudv1alpha1.EgressShardClaim {
	claim := &cloudv1alpha1.EgressShardClaim{}
	claim.Namespace = egressTestNamespace
	claim.Name = egressAttachmentName
	claim.Spec = cloudv1alpha1.EgressShardClaimSpec{
		Attachment: cloudv1alpha1.AttachmentRef{Name: egressAttachmentName},
		NodeName:   egressTestNode,
		Families:   []cloudv1alpha1.InternetEgressAddressFamily{cloudv1alpha1.InternetEgressAddressFamilyIPv6},
	}
	if shardName != "" {
		claim.Labels = map[string]string{cloudv1alpha1.LabelEgressShardClaimShard: shardName}
		claim.Status.ShardRef = &cloudv1alpha1.EgressShardReference{
			Namespace: egressShardNamespace,
			Name:      shardName,
		}
	}
	return claim
}

func reconcileBinding(t *testing.T, r *EgressShardClaimReconciler) {
	t.Helper()
	key := client.ObjectKey{Namespace: egressTestNamespace, Name: egressAttachmentName}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile the claim: %v", err)
	}
}

func readClaim(t *testing.T, cl client.Client) *cloudv1alpha1.EgressShardClaim {
	t.Helper()
	var claim cloudv1alpha1.EgressShardClaim
	key := client.ObjectKey{Namespace: egressTestNamespace, Name: egressAttachmentName}
	if err := cl.Get(t.Context(), key, &claim); err != nil {
		t.Fatalf("get the claim: %v", err)
	}
	return &claim
}

func claimExists(t *testing.T, cl client.Client) bool {
	t.Helper()
	var claim cloudv1alpha1.EgressShardClaim
	key := client.ObjectKey{Namespace: egressTestNamespace, Name: egressAttachmentName}
	return cl.Get(t.Context(), key, &claim) == nil
}

// One claim per attachment that has landed on a node, carrying the node and
// the families the network declared and nothing this controller invented.
func TestBinderRecordsOncePerAttachment(t *testing.T) {
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newLandedAttachment(egressTestNode),
		newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

	reconcileBinding(t, r)

	claim := readClaim(t, cl)
	if claim.Spec.Attachment.Name != egressAttachmentName {
		t.Errorf("attachment: got %q, want %q", claim.Spec.Attachment.Name, egressAttachmentName)
	}
	if claim.Spec.NodeName != egressTestNode {
		t.Errorf("node: got %q, want %q", claim.Spec.NodeName, egressTestNode)
	}
	if len(claim.Spec.Families) != 1 ||
		claim.Spec.Families[0] != cloudv1alpha1.InternetEgressAddressFamilyIPv6 {
		t.Errorf("families: got %v, want [IPv6]", claim.Spec.Families)
	}
	if claim.Status.ShardRef != nil {
		t.Errorf("the claim recorded %v in the pass that wrote it", claim.Status.ShardRef)
	}
	if !metav1.IsControlledBy(claim, newLandedAttachment(egressTestNode)) &&
		len(claim.OwnerReferences) == 0 {
		t.Error("the claim is not owned by its attachment")
	}
}

// Nothing is recorded before the attachment reports where it landed. The node
// is the whole content of the record.
func TestBinderWaitsForTheAttachmentsNode(t *testing.T) {
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newLandedAttachment(""),
		newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

	reconcileBinding(t, r)

	if claimExists(t, cl) {
		t.Error("a claim was written for an attachment on no known node")
	}
}

// The shard recorded is the one on the attachment's node and no other.
func TestBinderRecordsTheShardOnTheNode(t *testing.T) {
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newLandedAttachment(egressTestNode),
		newEgressShard("worker-2-egress", "worker-2", "2001:db8:ff02::", "2001:db8:f00d::200"),
		newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

	reconcileBinding(t, r)
	reconcileBinding(t, r)

	claim := readClaim(t, cl)
	if claim.Status.ShardRef == nil {
		t.Fatal("a shard on the node recorded nothing")
	}
	if claim.Status.ShardRef.Name != "worker-3-egress" {
		t.Errorf("shard: got %q, want worker-3-egress", claim.Status.ShardRef.Name)
	}
	if claim.Status.ShardRef.Namespace != egressShardNamespace {
		t.Errorf("shard namespace: got %q, want %q", claim.Status.ShardRef.Namespace, egressShardNamespace)
	}
	if got := claim.Labels[cloudv1alpha1.LabelEgressShardClaimShard]; got != "worker-3-egress" {
		t.Errorf("shard label: got %q, want worker-3-egress", got)
	}
	assertClaimCondition(t, cl, metav1.ConditionTrue, cloudv1alpha1.EgressShardClaimReasonBound)
	assertAttachmentCondition(t, cl, metav1.ConditionTrue,
		networkingv1alpha.NetworkContextInternetEgressReasonReady)

	// The finalizer is the only state a binder puts on a shard, written before
	// the record so a recorded shard is never held by nothing.
	var shard bgpv1alpha1.EgressShard
	key := client.ObjectKey{Namespace: egressShardNamespace, Name: "worker-3-egress"}
	if err := cl.Get(t.Context(), key, &shard); err != nil {
		t.Fatalf("get the recorded shard: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&shard, cloudv1alpha1.FinalizerEgressShardBinding) {
		t.Error("the recorded shard is not held open")
	}
	if shard.Spec.ShardAddressIPv6 != "2001:db8:f00d::100" {
		t.Errorf("the binder rewrote the shard's address: %q", shard.Spec.ShardAddressIPv6)
	}
}

// A node whose shard cannot be recorded leaves the claim unbound with the
// reason, and the consumer reads a fact about their own instance.
func TestBinderWaitsForAShardItCanUse(t *testing.T) {
	mismatched := newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100")
	mismatched.Status.ShardSID = "2001:db8:ffff::"

	tests := []struct {
		name    string
		objects []client.Object
		reason  string
	}{
		{
			name:   "no shard names the node",
			reason: cloudv1alpha1.EgressShardClaimReasonNoShardOnNode,
		},
		{
			name: "the shard reports no identifier",
			objects: []client.Object{
				newEgressShard("worker-3-egress", egressTestNode, "", "2001:db8:f00d::100")},
			reason: cloudv1alpha1.EgressShardClaimReasonShardNotReady,
		},
		{
			name:    "the shard runs an identity its spec does not state",
			objects: []client.Object{mismatched},
			reason:  cloudv1alpha1.EgressShardClaimReasonShardMismatch,
		},
		{
			name: "the shard is being deleted",
			objects: []client.Object{terminatingShard(
				newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))},
			reason: cloudv1alpha1.EgressShardClaimReasonShardTerminating,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := append([]client.Object{
				newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
				newLandedAttachment(egressTestNode)}, test.objects...)
			r, cl := newBinder(t, objects...)

			reconcileBinding(t, r)
			reconcileBinding(t, r)

			claim := readClaim(t, cl)
			if claim.Status.ShardRef != nil {
				t.Fatalf("recorded %v, want nothing", claim.Status.ShardRef)
			}
			assertClaimCondition(t, cl, metav1.ConditionFalse, test.reason)
			assertAttachmentCondition(t, cl, metav1.ConditionFalse,
				networkingv1alpha.NetworkContextInternetEgressReasonUnavailable)
		})
	}
}

// Egress that works and an address that cannot yet be stated are different
// facts, and the condition says which.
func TestBinderReportsThatNoAddressIsAllocatedYet(t *testing.T) {
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newLandedAttachment(egressTestNode),
		newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", ""))

	reconcileBinding(t, r)
	reconcileBinding(t, r)

	if readClaim(t, cl).Status.ShardRef == nil {
		t.Fatal("a shard with an identifier and no address recorded nothing")
	}
	assertClaimCondition(t, cl, metav1.ConditionTrue, cloudv1alpha1.EgressShardClaimReasonBound)
	assertAttachmentCondition(t, cl, metav1.ConditionFalse,
		networkingv1alpha.NetworkContextInternetEgressReasonAddressUnavailable)
}

// A record is not remade while the attachment stays where it is. A second
// shard naming the node is an operator error, and the one already recorded
// stands.
func TestBinderNeverRebinds(t *testing.T) {
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newLandedAttachment(egressTestNode),
		newEgressShard("worker-3-egress-b", egressTestNode, "2001:db8:ff02::", "2001:db8:f00d::200"))

	reconcileBinding(t, r)
	reconcileBinding(t, r)
	if got := readClaim(t, cl).Status.ShardRef; got == nil || got.Name != "worker-3-egress-b" {
		t.Fatalf("got %v, want worker-3-egress-b", got)
	}

	earlier := newEgressShard("worker-3-egress-a", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100")
	if err := cl.Create(t.Context(), earlier); err != nil {
		t.Fatalf("commission a shard sorting earlier: %v", err)
	}
	reconcileBinding(t, r)
	if got := readClaim(t, cl).Status.ShardRef; got == nil || got.Name != "worker-3-egress-b" {
		t.Errorf("got %v, want the shard already recorded", got)
	}
}

// Egress withdrawn is a claim released, which lets the shard go.
func TestBinderReleasesTheClaimWhenEgressIsWithdrawn(t *testing.T) {
	unprojected := newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled)
	unprojected.Spec.Egress = nil

	tests := []struct {
		name           string
		networkContext *networkingv1alpha.NetworkContext
	}{
		{"egress disabled", newEgressContext(networkingv1alpha.NetworkInternetEgressDisabled)},
		{"intent never projected", unprojected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, cl := newBinder(t, test.networkContext, newLandedAttachment(egressTestNode),
				newEgressClaim("worker-3-egress"),
				newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

			reconcileBinding(t, r)

			if claimExists(t, cl) {
				t.Error("the claim survived the egress that justified it")
			}
		})
	}
}

// An attachment that is gone takes its record with it, rather than holding a
// shard open for an instance that no longer exists.
func TestBinderReleasesTheClaimWhenTheAttachmentLeaves(t *testing.T) {
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newEgressClaim("worker-3-egress"),
		newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

	reconcileBinding(t, r)

	if claimExists(t, cl) {
		t.Error("the claim survived its attachment")
	}
}

// An attachment that moved nodes is a different record. The spec is immutable,
// so the stale one is released and the next pass writes the new one.
func TestBinderReplacesTheRecordWhenTheAttachmentMovesNodes(t *testing.T) {
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newLandedAttachment("worker-4"),
		newEgressClaim("worker-3-egress"),
		newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"),
		newEgressShard("worker-4-egress", "worker-4", "2001:db8:ff04::", "2001:db8:f00d::400"))

	reconcileBinding(t, r)
	if claimExists(t, cl) {
		t.Fatal("a record for the node the attachment left was kept")
	}

	reconcileBinding(t, r)
	reconcileBinding(t, r)
	claim := readClaim(t, cl)
	if claim.Spec.NodeName != "worker-4" {
		t.Errorf("node: got %q, want worker-4", claim.Spec.NodeName)
	}
	if claim.Status.ShardRef == nil || claim.Status.ShardRef.Name != "worker-4-egress" {
		t.Errorf("shard: got %v, want worker-4-egress", claim.Status.ShardRef)
	}
}

// A label lost to an edit would hide an attachment from the query a shard's
// consumer set is counted by, so it is re-asserted on every pass.
func TestBinderRepairsTheShardLabel(t *testing.T) {
	claim := newEgressClaim("worker-3-egress")
	claim.Labels = nil
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newLandedAttachment(egressTestNode), claim,
		newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

	reconcileBinding(t, r)

	if got := readClaim(t, cl).Labels[cloudv1alpha1.LabelEgressShardClaimShard]; got != "worker-3-egress" {
		t.Errorf("shard label: got %q, want worker-3-egress", got)
	}
}

// A recorded shard that vanished is said on both objects. Nothing rebinds.
func TestBinderReportsAMissingShard(t *testing.T) {
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newLandedAttachment(egressTestNode),
		newEgressClaim("worker-3-egress"))

	reconcileBinding(t, r)

	assertClaimCondition(t, cl, metav1.ConditionFalse, cloudv1alpha1.EgressShardClaimReasonShardMissing)
	assertAttachmentCondition(t, cl, metav1.ConditionFalse,
		networkingv1alpha.NetworkContextInternetEgressReasonUnavailable)
}

// Degraded means egress works for some declared families and not others. Only
// one family is accepted anywhere on this path, so nothing may write it.
func TestBinderNeverReportsDegraded(t *testing.T) {
	r, cl := newBinder(t, newEgressContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newLandedAttachment(egressTestNode),
		newEgressShard("worker-3-egress", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

	reconcileBinding(t, r)
	reconcileBinding(t, r)

	condition := attachmentEgressCondition(t, cl)
	if condition == nil {
		t.Fatal("the attachment reports no egress readiness")
	}
	if condition.Reason == networkingv1alpha.NetworkContextInternetEgressReasonDegraded {
		t.Error("Degraded was reported for a path that accepts one address family")
	}
}

func terminatingShard(shard *bgpv1alpha1.EgressShard) *bgpv1alpha1.EgressShard {
	shard.Finalizers = []string{cloudv1alpha1.FinalizerEgressShardBinding}
	deletion := metav1.Now()
	shard.DeletionTimestamp = &deletion
	return shard
}

func assertClaimCondition(
	t *testing.T, cl client.Client, status metav1.ConditionStatus, reason string,
) {
	t.Helper()
	condition := meta.FindStatusCondition(readClaim(t, cl).Status.Conditions,
		cloudv1alpha1.ConditionTypeReady)
	if condition == nil {
		t.Fatal("the claim reports no Ready condition")
	}
	if condition.Status != status || condition.Reason != reason {
		t.Errorf("claim Ready: got %s/%s, want %s/%s",
			condition.Status, condition.Reason, status, reason)
	}
}

func attachmentEgressCondition(t *testing.T, cl client.Client) *metav1.Condition {
	t.Helper()
	var attachment cloudv1alpha1.VPCAttachment
	key := client.ObjectKey{Namespace: egressTestNamespace, Name: egressAttachmentName}
	if err := cl.Get(t.Context(), key, &attachment); err != nil {
		t.Fatalf("get the attachment: %v", err)
	}
	return meta.FindStatusCondition(attachment.Status.Conditions,
		cloudv1alpha1.ConditionTypeInternetEgressReady)
}

func assertAttachmentCondition(
	t *testing.T, cl client.Client, status metav1.ConditionStatus, reason string,
) {
	t.Helper()
	condition := attachmentEgressCondition(t, cl)
	if condition == nil {
		t.Fatal("the attachment reports no InternetEgressReady condition")
	}
	if condition.Status != status || condition.Reason != reason {
		t.Errorf("attachment InternetEgressReady: got %s/%s, want %s/%s",
			condition.Status, condition.Reason, status, reason)
	}
}
