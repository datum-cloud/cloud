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

// egressContextName is the network context every egress test binds, and
// therefore the name of the one claim that binds it.
const egressContextName = "default-us-central-1"

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
		WithStatusSubresource(&cloudv1alpha1.EgressShardClaim{}, &networkingv1alpha.NetworkContext{}).
		Build()
	return &EgressShardClaimReconciler{Client: fakeClient, Scheme: scheme}, fakeClient
}

// newBoundContext is the location the binder works from: the projected intent,
// with the network it belongs to named.
func newBoundContext(mode networkingv1alpha.NetworkInternetEgressMode) *networkingv1alpha.NetworkContext {
	networkContext := newEgressContext(mode)
	networkContext.Spec.Network = networkingv1alpha.LocalNetworkRef{Name: "default"}
	return networkContext
}

func reconcileBinding(t *testing.T, r *EgressShardClaimReconciler) {
	t.Helper()
	key := client.ObjectKey{Namespace: egressTestNamespace, Name: egressContextName}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile the claim: %v", err)
	}
}

func readClaim(t *testing.T, cl client.Client) *cloudv1alpha1.EgressShardClaim {
	t.Helper()
	var claim cloudv1alpha1.EgressShardClaim
	key := client.ObjectKey{Namespace: egressTestNamespace, Name: egressContextName}
	if err := cl.Get(t.Context(), key, &claim); err != nil {
		t.Fatalf("get the claim: %v", err)
	}
	return &claim
}

func claimExists(t *testing.T, cl client.Client) bool {
	t.Helper()
	var claim cloudv1alpha1.EgressShardClaim
	key := client.ObjectKey{Namespace: egressTestNamespace, Name: egressContextName}
	err := cl.Get(t.Context(), key, &claim)
	return err == nil
}

// One claim per network context that declares egress, carrying the terms the
// projection resolved and nothing this controller invented.
func TestBinderClaimsOncePerNetworkContext(t *testing.T) {
	r, cl := newBinder(t, newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newEgressParameters(),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

	reconcileBinding(t, r)

	claim := readClaim(t, cl)
	if claim.Spec.Network.Name != "default" {
		t.Errorf("network: got %q, want default", claim.Spec.Network.Name)
	}
	if claim.Spec.NetworkContext.Name != egressContextName {
		t.Errorf("network context: got %q, want %q", claim.Spec.NetworkContext.Name, egressContextName)
	}
	if claim.Spec.ClassName != "shared" {
		t.Errorf("class: got %q, want shared", claim.Spec.ClassName)
	}
	if claim.Spec.Sharing != cloudv1alpha1.EgressSharingShared {
		t.Errorf("sharing: got %q, want Shared", claim.Spec.Sharing)
	}
	if len(claim.Spec.Families) != 1 ||
		claim.Spec.Families[0] != cloudv1alpha1.InternetEgressAddressFamilyIPv6 {
		t.Errorf("families: got %v, want [IPv6]", claim.Spec.Families)
	}
	// The claim names no shard, no selector, no address and no pool: the cell
	// answers with the shard, and it answers on status.
	if claim.Status.ShardRef != nil {
		t.Errorf("the claim bound %v in the pass that wrote it", claim.Status.ShardRef)
	}
}

func TestBinderBindsTheClaimToAShard(t *testing.T) {
	r, cl := newBinder(t, newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newEgressParameters(),
		newEgressShard("shard-b", "2001:db8:ff02::", "2001:db8:f00d::200", poolLabels()),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

	reconcileBinding(t, r)
	reconcileBinding(t, r)

	claim := readClaim(t, cl)
	if claim.Status.ShardRef == nil {
		t.Fatal("two usable shards bound nothing")
	}
	// Name order, moved here with the selection it belongs to: the binding has
	// to be deterministic over the set of shards it saw.
	if claim.Status.ShardRef.Name != "shard-a" {
		t.Errorf("shard: got %q, want shard-a", claim.Status.ShardRef.Name)
	}
	if claim.Status.ShardRef.Namespace != egressShardNamespace {
		t.Errorf("shard namespace: got %q, want %q", claim.Status.ShardRef.Namespace, egressShardNamespace)
	}
	// The label is what makes the shard's consumer set a list query, which is
	// what stands in for the list of networks a shard does not hold.
	if got := claim.Labels[cloudv1alpha1.LabelEgressShardClaimShard]; got != "shard-a" {
		t.Errorf("shard label: got %q, want shard-a", got)
	}
	assertClaimCondition(t, cl, metav1.ConditionTrue, cloudv1alpha1.EgressShardClaimReasonBound)
	assertContextCondition(t, cl, metav1.ConditionTrue,
		networkingv1alpha.NetworkContextInternetEgressReasonReady)

	// The finalizer is the only state a binder puts on a shard, written before
	// the binding so a recorded binding is never held by nothing.
	var shard bgpv1alpha1.EgressShard
	key := client.ObjectKey{Namespace: egressShardNamespace, Name: "shard-a"}
	if err := cl.Get(t.Context(), key, &shard); err != nil {
		t.Fatalf("get the bound shard: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&shard, cloudv1alpha1.FinalizerEgressShardBinding) {
		t.Error("the bound shard is not held open")
	}
	// Nothing else is written to it. A shard holds no list of the networks it
	// serves and no count of them.
	if len(shard.Labels) != len(poolLabels()) {
		t.Errorf("the binder wrote labels onto the shard: %v", shard.Labels)
	}
	if shard.Spec.ShardAddressIPv6 != "2001:db8:f00d::100" {
		t.Errorf("the binder rewrote the shard's address: %q", shard.Spec.ShardAddressIPv6)
	}
}

// Many networks bind one shard. Nothing branches on the sharing a claim
// records, because dedicated capacity is not offered.
func TestBinderBindsManyNetworksToOneShard(t *testing.T) {
	first := newEgressClaim("shard-a")
	first.Name = "other-us-central-1"
	first.Spec.Network.Name = "other"
	first.Spec.NetworkContext.Name = "other-us-central-1"

	r, cl := newBinder(t, newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newEgressParameters(), first,
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

	reconcileBinding(t, r)
	reconcileBinding(t, r)

	claim := readClaim(t, cl)
	if claim.Status.ShardRef == nil || claim.Status.ShardRef.Name != "shard-a" {
		t.Fatalf("got %v, want the shard another network already holds", claim.Status.ShardRef)
	}
}

// A shard with no identifier has nothing a node can route toward, so binding it
// would report egress that carries no packet. The claim waits, and the address
// stays unpublished.
func TestBinderWaitsForAShardItCanUse(t *testing.T) {
	tests := []struct {
		name    string
		objects []client.Object
		reason  string
	}{
		{
			name:    "no shard carries the class's labels",
			objects: []client.Object{newEgressParameters()},
			reason:  cloudv1alpha1.EgressShardClaimReasonNoShardMatchesTheClass,
		},
		{
			name: "the only shard reports no identifier",
			objects: []client.Object{newEgressParameters(),
				newEgressShard("unprogrammed", "", "2001:db8:f00d::100", poolLabels())},
			reason: cloudv1alpha1.EgressShardClaimReasonNoShardIdentifier,
		},
		{
			name: "the only shard translates no IPv6",
			objects: []client.Object{newEgressParameters(),
				newEgressShard("ipv4-only", "2001:db8:ff01::", "", map[string]string{
					bgpv1alpha1.LabelEgressShardPool: "shared",
					bgpv1alpha1.LabelEgressShardCell: "us-central-1",
					bgpv1alpha1.LabelEgressShardIPv4: bgpv1alpha1.LabelValueEgressFamilyServed,
				})},
			reason: cloudv1alpha1.EgressShardClaimReasonNoShardMatchesTheClass,
		},
		{
			name: "the only shard is being deleted",
			objects: []client.Object{newEgressParameters(),
				terminatingShard(newEgressShard("draining", "2001:db8:ff01::",
					"2001:db8:f00d::100", poolLabels()))},
			reason: cloudv1alpha1.EgressShardClaimReasonShardTerminating,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := append([]client.Object{
				newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled)}, test.objects...)
			r, cl := newBinder(t, objects...)

			reconcileBinding(t, r)
			reconcileBinding(t, r)

			claim := readClaim(t, cl)
			if claim.Status.ShardRef != nil {
				t.Fatalf("bound %v, want nothing", claim.Status.ShardRef)
			}
			assertClaimCondition(t, cl, metav1.ConditionFalse, test.reason)
			// The consumer reads a fact about their own network. Which shard
			// refused it is on the claim, which is an operator's object.
			assertContextCondition(t, cl, metav1.ConditionFalse,
				networkingv1alpha.NetworkContextInternetEgressReasonUnavailable)
		})
	}
}

// Egress that works and an address that cannot yet be stated are different
// facts, and the condition says which.
func TestBinderReportsThatNoAddressIsAllocatedYet(t *testing.T) {
	r, cl := newBinder(t, newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newEgressParameters(),
		newEgressShard("shard-a", "2001:db8:ff01::", "", poolLabels()))

	reconcileBinding(t, r)
	reconcileBinding(t, r)

	claim := readClaim(t, cl)
	if claim.Status.ShardRef == nil {
		t.Fatal("a shard with an identifier and no address bound nothing")
	}
	assertClaimCondition(t, cl, metav1.ConditionTrue, cloudv1alpha1.EgressShardClaimReasonBound)
	assertContextCondition(t, cl, metav1.ConditionFalse,
		networkingv1alpha.NetworkContextInternetEgressReasonAddressUnavailable)
}

// Decided once. A shard that would sort ahead of the bound one arriving later
// does not move a live network's egress, which is the address a consumer
// allow-listed at their destination.
func TestBinderNeverRebinds(t *testing.T) {
	r, cl := newBinder(t, newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newEgressParameters(),
		newEgressShard("shard-b", "2001:db8:ff02::", "2001:db8:f00d::200", poolLabels()))

	reconcileBinding(t, r)
	reconcileBinding(t, r)
	if got := readClaim(t, cl).Status.ShardRef; got == nil || got.Name != "shard-b" {
		t.Fatalf("got %v, want shard-b", got)
	}

	earlier := newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels())
	if err := cl.Create(t.Context(), earlier); err != nil {
		t.Fatalf("commission a shard sorting earlier: %v", err)
	}
	reconcileBinding(t, r)

	if got := readClaim(t, cl).Status.ShardRef; got == nil || got.Name != "shard-b" {
		t.Errorf("got %v, want the shard it was already bound to", got)
	}
}

// Egress withdrawn is a claim released, which is what takes the route and the
// address away and lets the shard go.
func TestBinderReleasesTheClaimWhenEgressIsWithdrawn(t *testing.T) {
	tests := []struct {
		name           string
		networkContext func() *networkingv1alpha.NetworkContext
	}{
		{
			name: "egress disabled",
			networkContext: func() *networkingv1alpha.NetworkContext {
				return newBoundContext(networkingv1alpha.NetworkInternetEgressDisabled)
			},
		},
		{
			name: "class served by another implementation",
			networkContext: func() *networkingv1alpha.NetworkContext {
				networkContext := newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled)
				networkContext.Spec.Egress.Internet.ParametersRef.Kind = "SomeOtherParameters"
				return networkContext
			},
		},
		{
			name: "intent never projected",
			networkContext: func() *networkingv1alpha.NetworkContext {
				networkContext := newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled)
				networkContext.Spec.Egress = nil
				return networkContext
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, cl := newBinder(t, test.networkContext(), newEgressParameters(),
				newEgressClaim("shard-a"),
				newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

			reconcileBinding(t, r)

			if claimExists(t, cl) {
				t.Error("the claim survived the egress that justified it")
			}
		})
	}
}

// A network no longer present in the cell takes its claim with it, rather than
// holding a shard open for a location that does not exist.
func TestBinderReleasesTheClaimWhenTheNetworkLeaves(t *testing.T) {
	r, cl := newBinder(t, newEgressParameters(), newEgressClaim("shard-a"),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

	reconcileBinding(t, r)

	if claimExists(t, cl) {
		t.Error("the claim survived its network context")
	}
}

// Parameters an operator has not written in this cell are an answer, not a
// silent nothing: the class this cell was pointed at does not exist here.
func TestBinderReportsAbsentParameters(t *testing.T) {
	r, cl := newBinder(t, newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled))

	reconcileBinding(t, r)

	if claimExists(t, cl) {
		t.Error("a claim was written for a class this cell cannot serve")
	}
	assertContextCondition(t, cl, metav1.ConditionFalse,
		networkingv1alpha.NetworkContextInternetEgressReasonUnavailable)
}

// Sharing decides nothing here, but an unprojected value still means the claim
// cannot record what it was created under, and a claim is refused rather than
// written with a guess.
func TestBinderRefusesTermsItCannotRecord(t *testing.T) {
	tests := []struct {
		name  string
		amend func(*networkingv1alpha.NetworkContext)
	}{
		{
			name: "sharing never projected",
			amend: func(networkContext *networkingv1alpha.NetworkContext) {
				networkContext.Spec.Egress.Internet.Sharing = ""
			},
		},
		{
			name: "no address family to reach",
			amend: func(networkContext *networkingv1alpha.NetworkContext) {
				networkContext.Spec.Egress.Internet.Reach = nil
			},
		},
		{
			name: "location names no network",
			amend: func(networkContext *networkingv1alpha.NetworkContext) {
				networkContext.Spec.Network = networkingv1alpha.LocalNetworkRef{}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			networkContext := newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled)
			test.amend(networkContext)
			r, cl := newBinder(t, networkContext, newEgressParameters(),
				newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

			reconcileBinding(t, r)

			if claimExists(t, cl) {
				t.Error("a claim was written from terms it could not record")
			}
			assertContextCondition(t, cl, metav1.ConditionFalse,
				networkingv1alpha.NetworkContextInternetEgressReasonUnavailable)
		})
	}
}

// The terms are immutable, so a class change reaching a bound location cannot
// be applied to the binding. The binding keeps delivering what it was made for
// and says it no longer matches.
func TestBinderKeepsABindingWhoseTermsChanged(t *testing.T) {
	networkContext := newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled)
	networkContext.Spec.Egress.Internet.ClassName = "some-other-class"

	r, cl := newBinder(t, networkContext, newEgressParameters(), newEgressClaim("shard-a"),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

	reconcileBinding(t, r)

	claim := readClaim(t, cl)
	if claim.Status.ShardRef == nil || claim.Status.ShardRef.Name != "shard-a" {
		t.Fatalf("got %v, want the binding it already had", claim.Status.ShardRef)
	}
	assertClaimCondition(t, cl, metav1.ConditionFalse,
		cloudv1alpha1.EgressShardClaimReasonTermsChanged)
}

// An unbound claim whose terms changed is discarded rather than kept, because
// nothing is bound to protect and the next pass writes one that matches.
func TestBinderDiscardsAnUnboundClaimWhoseTermsChanged(t *testing.T) {
	networkContext := newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled)
	networkContext.Spec.Egress.Internet.ClassName = "some-other-class"

	r, cl := newBinder(t, networkContext, newEgressParameters(), newEgressClaim(""),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

	reconcileBinding(t, r)

	if claimExists(t, cl) {
		t.Error("an unbound claim with stale terms was kept")
	}
}

// A label lost to an edit would hide a network from the query a shard's
// consumer set is counted by, so it is re-asserted on every pass.
func TestBinderRepairsTheShardLabel(t *testing.T) {
	claim := newEgressClaim("shard-a")
	claim.Labels = nil

	r, cl := newBinder(t, newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newEgressParameters(), claim,
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

	reconcileBinding(t, r)

	if got := readClaim(t, cl).Labels[cloudv1alpha1.LabelEgressShardClaimShard]; got != "shard-a" {
		t.Errorf("shard label: got %q, want shard-a", got)
	}
}

// Degraded means egress works for some declared families and not others. Only
// one family is accepted anywhere on this path, so nothing may write it — the
// reason stays defined and unreachable rather than being given a fabricated
// path to reach it.
func TestBinderNeverReportsDegraded(t *testing.T) {
	r, cl := newBinder(t, newBoundContext(networkingv1alpha.NetworkInternetEgressEnabled),
		newEgressParameters(),
		newEgressShard("shard-a", "2001:db8:ff01::", "2001:db8:f00d::100", poolLabels()))

	reconcileBinding(t, r)
	reconcileBinding(t, r)

	var networkContext networkingv1alpha.NetworkContext
	key := client.ObjectKey{Namespace: egressTestNamespace, Name: egressContextName}
	if err := cl.Get(t.Context(), key, &networkContext); err != nil {
		t.Fatalf("get the network context: %v", err)
	}
	condition := meta.FindStatusCondition(networkContext.Status.Conditions,
		networkingv1alpha.NetworkContextInternetEgressReady)
	if condition == nil {
		t.Fatal("the location reports no egress readiness")
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

func assertContextCondition(
	t *testing.T, cl client.Client, status metav1.ConditionStatus, reason string,
) {
	t.Helper()
	var networkContext networkingv1alpha.NetworkContext
	key := client.ObjectKey{Namespace: egressTestNamespace, Name: egressContextName}
	if err := cl.Get(t.Context(), key, &networkContext); err != nil {
		t.Fatalf("get the network context: %v", err)
	}
	condition := meta.FindStatusCondition(networkContext.Status.Conditions,
		networkingv1alpha.NetworkContextInternetEgressReady)
	if condition == nil {
		t.Fatal("the location reports no egress readiness")
	}
	if condition.Status != status || condition.Reason != reason {
		t.Errorf("InternetEgressReady: got %s/%s, want %s/%s",
			condition.Status, condition.Reason, status, reason)
	}
}
