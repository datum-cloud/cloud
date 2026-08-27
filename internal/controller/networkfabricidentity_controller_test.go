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
	"errors"
	"sort"
	"strings"
	"testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/fabricidentity"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	testNetworkName = "prod"
	testNamespace   = "ns-project"
	testNetworkUID  = "11111111-1111-1111-1111-111111111111"
	testClass       = "datum-fabric-identity"
)

// fakeIdentityIPAM stands in for the address service. Allocation is synchronous
// there, so the create response already carries the block.
type fakeIdentityIPAM struct {
	client client.Client
	// next is the index the pool hands out. A correctly provisioned identifier
	// pool holds its own first block back, because a network handed index zero
	// reads as one holding no identity at all.
	next int
	// created records every claim name the factory was asked to bind.
	created []string
}

func newFakeIdentityIPAM(t *testing.T) *fakeIdentityIPAM {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ipamv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the IPAM scheme: %v", err)
	}

	f := &fakeIdentityIPAM{next: 1}
	f.client = fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			ipClaim, ok := obj.(*ipamv1alpha1.IPClaim)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			f.created = append(f.created, ipClaim.Name)
			index := f.next
			f.next++
			ipClaim.Status.Phase = ipamv1alpha1.ClaimBound
			ipClaim.Status.AllocatedCIDR = blockForIndex(index)
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	return f
}

// blockForIndex renders the block a pool rooted at fc00::/32 hands out for an
// index, which is what the identity is read back out of.
func blockForIndex(index int) string {
	return "fc00:0:" + hex16(index>>16) + ":" + hex16(index&0xffff) + "::/64"
}

func hex16(v int) string {
	const digits = "0123456789abcdef"
	if v == 0 {
		return "0"
	}
	out := ""
	for v > 0 {
		out = string(digits[v&0xf]) + out
		v >>= 4
	}
	return out
}

func (f *fakeIdentityIPAM) ClientForPlatform() (client.Client, error) { return f.client, nil }
func (f *fakeIdentityIPAM) ClientForProject(string) (client.Client, error) {
	return nil, errors.New("a fabric identity is never drawn from a consumer's project")
}

type identityFixture struct {
	t          *testing.T
	ctx        context.Context
	networks   client.Client
	hub        client.Client
	ipam       *fakeIdentityIPAM
	reconciler *NetworkFabricIdentityReconciler
	network    *networkingv1alpha.Network
}

func newIdentityFixture(t *testing.T, presences ...string) *identityFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := networkingv1alpha.AddToScheme(scheme); err != nil {
		t.Fatalf("build the networking scheme: %v", err)
	}
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the cloud scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(clusterPropagationPolicyGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(clusterPropagationPolicyGVK.GroupVersion().WithKind("ClusterPropagationPolicyList"),
		&unstructured.UnstructuredList{})

	network := &networkingv1alpha.Network{}
	network.Namespace = testNamespace
	network.Name = testNetworkName
	network.UID = types.UID(testNetworkUID)

	objects := []client.Object{network}
	for _, location := range presences {
		presence := &networkingv1alpha.NetworkContext{}
		presence.Namespace = testNamespace
		presence.Name = testNetworkName + "-" + location
		presence.Labels = map[string]string{networkingv1alpha.NetworkUIDLabel: testNetworkUID}
		presence.Spec.Network = networkingv1alpha.LocalNetworkRef{Name: testNetworkName}
		presence.Spec.Location = networkingv1alpha.LocationReference{Name: location}
		objects = append(objects, presence)
	}

	networks := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	hub := fake.NewClientBuilder().WithScheme(scheme).Build()
	ipam := newFakeIdentityIPAM(t)

	return &identityFixture{
		t:        t,
		ctx:      context.Background(),
		networks: networks,
		hub:      hub,
		ipam:     ipam,
		reconciler: &NetworkFabricIdentityReconciler{
			Networks:          networks,
			Hub:               hub,
			IPAM:              ipam,
			IdentityClass:     testClass,
			IdentityNamespace: "default",
		},
		network: network,
	}
}

func (f *identityFixture) reconcile() {
	f.t.Helper()
	_, err := f.reconciler.Reconcile(f.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testNetworkName},
	})
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
}

func (f *identityFixture) published() (*cloudv1alpha1.NetworkFabricIdentity, bool) {
	f.t.Helper()
	var identity cloudv1alpha1.NetworkFabricIdentity
	err := f.hub.Get(f.ctx, client.ObjectKey{Namespace: testNamespace, Name: testNetworkName}, &identity)
	if err != nil {
		return nil, false
	}
	return &identity, true
}

// placement reads the locations off the identity itself, which is what the
// per-location policies select on.
func (f *identityFixture) placement() ([]string, bool) {
	f.t.Helper()
	identity, ok := f.published()
	if !ok {
		return nil, false
	}
	locations := make([]string, 0, len(identity.Labels))
	for key, value := range identity.Labels {
		if strings.HasPrefix(key, fabricIdentityLocationLabelPrefix) && value == "true" {
			locations = append(locations, strings.TrimPrefix(key, fabricIdentityLocationLabelPrefix))
		}
	}
	sort.Strings(locations)
	return locations, len(locations) > 0
}

// policyFor reads the one policy that carries every identity required at a
// location. There is one of these per location, not per network.
func (f *identityFixture) policyFor(location string) (*unstructured.Unstructured, bool) {
	f.t.Helper()
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(clusterPropagationPolicyGVK)
	if err := f.hub.Get(f.ctx, client.ObjectKey{Name: FabricIdentityPolicyName(location)}, policy); err != nil {
		return nil, false
	}
	return policy, true
}

// The identity is published on a cloud object, not on the Network. Nothing a
// consumer reads carries it.
func TestIdentityIsPublishedOnItsOwnObject(t *testing.T) {
	f := newIdentityFixture(t, "us-central-1")
	f.reconcile()

	identity, ok := f.published()
	if !ok {
		t.Fatal("the identity must be published")
	}
	if identity.Spec.Identity == 0 {
		t.Fatal("a published identity is never zero")
	}
	if identity.Spec.NetworkRef.Name != testNetworkName || identity.Spec.NetworkRef.UID != testNetworkUID {
		t.Fatalf("the identity must name the network it belongs to, got %+v", identity.Spec.NetworkRef)
	}
	if len(f.ipam.created) != 1 || f.ipam.created[0] != fabricidentity.ClaimName(testNetworkUID) {
		t.Fatalf("the identity must be claimed from the platform tenancy under the network's UID, got %v", f.ipam.created)
	}
}

// One network, one identity, however many times it is reconciled.
func TestIdentityIsAllocatedOnlyOnce(t *testing.T) {
	f := newIdentityFixture(t, "us-central-1")
	f.reconcile()

	first, _ := f.published()
	for range 3 {
		f.reconcile()
	}
	again, _ := f.published()

	if first.Spec.Identity != again.Spec.Identity {
		t.Fatalf("the identity moved from %d to %d", first.Spec.Identity, again.Spec.Identity)
	}
	if len(f.ipam.created) != 1 {
		t.Fatalf("a network already holding an identity must not ask for another, got %v", f.ipam.created)
	}
}

// Two networks are two identities. One shared between them would make them one
// network on the fabric: each would import the other's routes.
func TestEachNetworkGetsItsOwnIdentity(t *testing.T) {
	f := newIdentityFixture(t, "us-central-1")
	f.reconcile()
	first, _ := f.published()

	presence := &networkingv1alpha.NetworkContext{}
	presence.Namespace = testNamespace
	presence.Name = "staging-us-central-1"
	presence.Labels = map[string]string{
		networkingv1alpha.NetworkUIDLabel: "22222222-2222-2222-2222-222222222222",
	}
	presence.Spec.Network = networkingv1alpha.LocalNetworkRef{Name: "staging"}
	presence.Spec.Location = networkingv1alpha.LocationReference{Name: "us-central-1"}
	if err := f.networks.Create(f.ctx, presence); err != nil {
		t.Fatalf("declare the second network's presence: %v", err)
	}
	if _, err := f.reconciler.Reconcile(f.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "staging"},
	}); err != nil {
		t.Fatalf("reconcile the second network: %v", err)
	}

	var second cloudv1alpha1.NetworkFabricIdentity
	if err := f.hub.Get(f.ctx, client.ObjectKey{Namespace: testNamespace, Name: "staging"}, &second); err != nil {
		t.Fatalf("the second network must be published too: %v", err)
	}
	if first.Spec.Identity == second.Spec.Identity {
		t.Fatal("two networks must not share an identity")
	}
}

// Placement follows the presences: the identity reaches exactly the cells
// backing the network's NetworkContexts, and no others.
func TestIdentityIsPlacedOnTheCellsTheNetworkIsRequiredIn(t *testing.T) {
	f := newIdentityFixture(t, "us-central-1", "us-east-1")
	f.reconcile()

	locations, placed := f.placement()
	if !placed {
		t.Fatal("a network required somewhere must be placed there")
	}
	if len(locations) != 2 || locations[0] != "us-central-1" || locations[1] != "us-east-1" {
		t.Fatalf("expected both locations, got %v", locations)
	}
}

// A network nothing declares a presence for needs no identity anywhere, so
// nothing is published for it. That must not error and must not churn.
func TestIdentityWithNoPresencesPublishesNothing(t *testing.T) {
	f := newIdentityFixture(t)
	f.reconcile()

	if _, ok := f.published(); ok {
		t.Fatal("nothing requires the identity, so nothing should carry it")
	}
	if len(f.ipam.created) != 0 {
		t.Fatalf("nothing should be claimed for a network required nowhere, got %v", f.ipam.created)
	}

	// Again, to prove it settles rather than churning.
	f.reconcile()
	if _, ok := f.published(); ok {
		t.Fatal("a second pass must not publish one either")
	}
}

// The last presence going takes the object and its placement with it, so a
// policy is not matched forever for a network that may no longer exist. The
// allocation itself is retained: a Route Target still installed in a remote
// location would merge a new network into a dead one's routes.
func TestLastPresenceCollectsTheObjectButNotTheAllocation(t *testing.T) {
	f := newIdentityFixture(t, "us-central-1")
	f.reconcile()

	before, ok := f.published()
	if !ok {
		t.Fatal("expected an identity to start from")
	}

	presence := &networkingv1alpha.NetworkContext{}
	presence.Namespace = testNamespace
	presence.Name = testNetworkName + "-us-central-1"
	if err := f.networks.Delete(f.ctx, presence); err != nil {
		t.Fatalf("delete the presence: %v", err)
	}
	f.reconcile()

	if _, ok := f.published(); ok {
		t.Fatal("nothing requires the identity any more, so the object should be collected")
	}

	// The network comes back. It must come back as itself, with the identity it
	// always had, because the claim was retained and is named from its UID.
	if err := f.networks.Create(f.ctx, newPresence("us-central-1")); err != nil {
		t.Fatalf("declare the presence again: %v", err)
	}
	f.reconcile()

	after, ok := f.published()
	if !ok {
		t.Fatal("the identity must be republished when the network is required again")
	}
	if after.Spec.Identity != before.Spec.Identity {
		t.Fatalf("a retained allocation must give back the same identity, got %d then %d",
			before.Spec.Identity, after.Spec.Identity)
	}
}

func newPresence(location string) *networkingv1alpha.NetworkContext {
	presence := &networkingv1alpha.NetworkContext{}
	presence.Namespace = testNamespace
	presence.Name = testNetworkName + "-" + location
	presence.Labels = map[string]string{networkingv1alpha.NetworkUIDLabel: testNetworkUID}
	presence.Spec.Network = networkingv1alpha.LocalNetworkRef{Name: testNetworkName}
	presence.Spec.Location = networkingv1alpha.LocationReference{Name: location}
	return presence
}

// One policy per location, selecting every identity required there. The policy
// count is the number of locations, not the number of networks.
func TestOnePolicyPerLocationCarriesEveryIdentityRequiredThere(t *testing.T) {
	f := newIdentityFixture(t, "us-central-1", "us-east-1")
	f.reconcile()

	for _, location := range []string{"us-central-1", "us-east-1"} {
		policy, ok := f.policyFor(location)
		if !ok {
			t.Fatalf("expected a policy for %q", location)
		}

		selectors, _, err := unstructured.NestedSlice(policy.Object, "spec", "resourceSelectors")
		if err != nil || len(selectors) != 1 {
			t.Fatalf("expected one resource selector, got %v (%v)", selectors, err)
		}
		entry, _ := selectors[0].(map[string]any)
		labels, _, _ := unstructured.NestedStringMap(entry, "labelSelector", "matchLabels")
		if labels[LocationLabel(location)] != "true" {
			t.Fatalf("the policy for %q must select identities required there, got %v", location, labels)
		}

		placement, _, _ := unstructured.NestedStringMap(policy.Object,
			"spec", "placement", "clusterAffinity", "labelSelector", "matchLabels")
		if placement[servingLocationTopologyLabel] != location {
			t.Fatalf("the policy for %q must place on the cell serving it, got %v", location, placement)
		}
	}

	// A second network in the same location reuses the same policy rather than
	// adding one.
	if err := f.networks.Create(f.ctx, &networkingv1alpha.NetworkContext{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      "staging-us-central-1",
			Labels:    map[string]string{networkingv1alpha.NetworkUIDLabel: "33333333-3333-3333-3333-333333333333"},
		},
		Spec: networkingv1alpha.NetworkContextSpec{
			Network:  networkingv1alpha.LocalNetworkRef{Name: "staging"},
			Location: networkingv1alpha.LocationReference{Name: "us-central-1"},
		},
	}); err != nil {
		t.Fatalf("declare a second network in the same location: %v", err)
	}
	if _, err := f.reconciler.Reconcile(f.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "staging"},
	}); err != nil {
		t.Fatalf("reconcile the second network: %v", err)
	}

	var policies unstructured.UnstructuredList
	policies.SetGroupVersionKind(clusterPropagationPolicyGVK.GroupVersion().WithKind("ClusterPropagationPolicyList"))
	if err := f.hub.List(f.ctx, &policies); err != nil {
		t.Fatalf("list policies: %v", err)
	}
	if len(policies.Items) != 2 {
		t.Fatalf("two locations must need two policies however many networks there are, got %d", len(policies.Items))
	}
}

// Withdrawal is what tears a data plane down, so it happens only when a
// presence is positively observed to be gone.
func TestPlacementShrinksOnlyOnAnObservedDeletion(t *testing.T) {
	f := newIdentityFixture(t, "us-central-1", "us-east-1")
	f.reconcile()

	east := &networkingv1alpha.NetworkContext{}
	east.Namespace = testNamespace
	east.Name = testNetworkName + "-us-east-1"
	if err := f.networks.Delete(f.ctx, east); err != nil {
		t.Fatalf("delete the presence: %v", err)
	}
	f.reconcile()

	locations, placed := f.placement()
	if !placed {
		t.Fatal("the placement must survive one presence going")
	}
	if len(locations) != 1 || locations[0] != "us-central-1" {
		t.Fatalf("a presence that is actually gone withdraws only that cell, got %v", locations)
	}
}

// A read that failed says nothing about where the network is required. The
// placement already in force must survive it untouched.
func TestPlacementSurvivesAnUnreadablePresence(t *testing.T) {
	f := newIdentityFixture(t, "us-central-1", "us-east-1")
	f.reconcile()

	before, placed := f.placement()
	if !placed {
		t.Fatal("expected a placement to start from")
	}

	f.reconciler.Networks = failingLister{Client: f.networks}
	if _, err := f.reconciler.Reconcile(f.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testNetworkName},
	}); err == nil {
		t.Fatal("an unreadable presence must be an error, never a withdrawal")
	}

	f.reconciler.Networks = f.networks
	after, placed := f.placement()
	if !placed {
		t.Fatal("the placement must not be withdrawn by a failed read")
	}
	if len(after) != len(before) || after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("the placement must be left exactly as it was: %v became %v", before, after)
	}
}

// failingLister stands in for a control plane that cannot answer. Every list is
// an error, which is the case a placement must never mistake for "the network
// is required nowhere".
type failingLister struct {
	client.Client
}

func (failingLister) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("the control plane is unreachable")
}
