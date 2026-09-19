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
	"fmt"
	"testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.datum.net/cloud/internal/egressaddress"
	"go.datum.net/cloud/internal/ipam"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

const (
	testShardNamespace  = "galactic-system"
	testShardName       = "worker-8b4e1647-dfw"
	testLocation        = "us-central-1"
	testAddressClass    = "datum-egress-shard-address-ipv6"
	testClaimNamespace  = "default"
	testPlatformProject = "datum-cloud"
)

// fakeAddressIPAM stands in for the address service. Allocation is synchronous
// there, so the create response already carries the address.
type fakeAddressIPAM struct {
	client client.Client
	// next is the host index the location's range hands out.
	next int
	// created records every claim name the service was asked to bind, so a
	// second claim for one shard is visible rather than merely harmless.
	created []string
	// deleted records every claim name released.
	deleted []string
	// retained maps an allocation name to the address it still holds after its
	// claim was deleted under Retain. It is what makes a second claim of the
	// same name a conflict rather than a fresh allocation.
	retained map[string]string
	// unbound holds the allocation back, which is what a claim looks like
	// between being accepted and being bound.
	unbound bool
}

func allocationNameForClaim(claimName string) string { return "alloc-" + claimName }

// refuseWhatTheAddressServerWouldRefuse mirrors the parts of the service's admission
// this depends on. The fake would otherwise bind anything, which is how a claim
// no real server has ever accepted passes every test here.
func refuseWhatTheAddressServerWouldRefuse(ipClaim *ipamv1alpha1.IPClaim) error {
	invalid := func(detail string) error {
		return apierrors.NewInvalid(
			ipamv1alpha1.SchemeGroupVersion.WithKind("IPClaim").GroupKind(), ipClaim.Name,
			field.ErrorList{field.Required(field.NewPath("spec"), detail)})
	}
	// The server bounds a claim's prefix length by the family stated on the
	// claim, before it looks at the class at all.
	if p := ipClaim.Spec.PrefixLength; p != nil {
		maxLen := int32(32)
		if ipClaim.Spec.IPFamily == ipamv1alpha1.IPv6 {
			maxLen = 128
		}
		if *p > maxLen {
			return invalid(fmt.Sprintf("prefixLength %d exceeds %d for family %q",
				*p, maxLen, ipClaim.Spec.IPFamily))
		}
	}
	// The class holding the per-location range names "location" in poolPer, so
	// a claim omitting it cannot be resolved to a pool.
	if _, ok := ipClaim.Spec.Scope[egressaddress.ScopeRoleLocation]; !ok {
		return invalid("scope is missing role \"location\"")
	}
	return nil
}

func newFakeAddressIPAM(t *testing.T) *fakeAddressIPAM {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := ipamv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the IPAM scheme: %v", err)
	}

	service := &fakeAddressIPAM{retained: map[string]string{}}
	service.client = fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
				ipClaim, ok := object.(*ipamv1alpha1.IPClaim)
				if !ok {
					return c.Create(ctx, object, opts...)
				}
				if err := refuseWhatTheAddressServerWouldRefuse(ipClaim); err != nil {
					return err
				}
				allocationName := allocationNameForClaim(ipClaim.Name)
				if cidr, held := service.retained[allocationName]; held {
					_ = cidr
					return newRetainedAllocationConflict(ipClaim.Name, allocationName)
				}
				service.created = append(service.created, ipClaim.Name)
				if !service.unbound {
					service.next++
					ipClaim.Status.Phase = ipamv1alpha1.ClaimPhase("Bound")
					ipClaim.Status.AllocatedCIDR = fmt.Sprintf("2001:db8:100::%x/128", service.next)
				} else {
					ipClaim.Status.Phase = ipamv1alpha1.ClaimPhase("Pending")
				}
				return c.Create(ctx, ipClaim, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
				if ipClaim, ok := object.(*ipamv1alpha1.IPClaim); ok {
					service.deleted = append(service.deleted, ipClaim.Name)
				}
				return c.Delete(ctx, object, opts...)
			},
		}).
		Build()
	return service
}

func (f *fakeAddressIPAM) ClientForPlatform() (client.Client, error) { return f.client, nil }

func (f *fakeAddressIPAM) ClientForProject(string) (client.Client, error) {
	return nil, errors.New("a shard's address is never drawn from a consumer's project")
}

var _ ipam.ClientFactory = (*fakeAddressIPAM)(nil)

// newRetainedAllocationConflict is the refusal the service answers a claim with
// when an allocation under the same identity is still held by a released claim.
// It is built with the service's own constructor so the classifier the
// controller depends on is the one under test, rather than a status this test
// invented and only this test can read.
func newRetainedAllocationConflict(claimName, allocationName string) error {
	return ipamerrors.NewRetainedAllocation(
		ipamv1alpha1.Resource("ipclaims"), claimName, allocationName,
		fmt.Sprintf("an allocation under this identity already exists: IPAllocation %q, retained by an earlier claim of the same name", allocationName))
}

func newShardCell(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := bgpv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the cell scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func shard(name string) *bgpv1alpha1.EgressShard {
	return &bgpv1alpha1.EgressShard{
		ObjectMeta: metav1.ObjectMeta{Namespace: testShardNamespace, Name: name},
		Spec: bgpv1alpha1.EgressShardSpec{
			TargetRef: bgpv1alpha1.TargetRef{Kind: "Node", Name: name},
		},
	}
}

func reconcilerFor(cell client.Client, service *fakeAddressIPAM) *EgressShardAddressReconciler {
	return &EgressShardAddressReconciler{
		Client:           cell,
		IPAM:             service,
		AddressClassIPv6: testAddressClass,
		ClaimNamespace:   testClaimNamespace,
		PlatformProject:  testPlatformProject,
		Location:         testLocation,
	}
}

func requestFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testShardNamespace, Name: name}}
}

func readShard(t *testing.T, cell client.Client, name string) *bgpv1alpha1.EgressShard {
	t.Helper()
	got := &bgpv1alpha1.EgressShard{}
	if err := cell.Get(context.Background(), client.ObjectKey{Namespace: testShardNamespace, Name: name}, got); err != nil {
		t.Fatalf("read the shard back: %v", err)
	}
	return got
}

// The whole point: a shard an operator never gave an address to gets one, in
// spec, where galactic reads it.
func TestAShardWithoutAnAddressIsGivenOne(t *testing.T) {
	cell := newShardCell(t, shard(testShardName))
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := readShard(t, cell, testShardName)
	if got.Spec.ShardAddressIPv6 == "" {
		t.Fatal("the shard was left with no address to translate to")
	}
	if got.Labels[bgpv1alpha1.LabelEgressShardIPv6] != bgpv1alpha1.LabelValueEgressFamilyServed {
		t.Errorf("the shard is not selectable as serving IPv6: labels = %v", got.Labels)
	}
}

// The claim is named for the shard, which is what makes a second reconcile find
// the address already held rather than draw another one out of a shared public
// range.
func TestReconcilingTwiceClaimsOneAddress(t *testing.T) {
	cell := newShardCell(t, shard(testShardName))
	service := newFakeAddressIPAM(t)
	reconciler := reconcilerFor(cell, service)

	for i := range 3 {
		if _, err := reconciler.Reconcile(context.Background(),
			requestFor(testShardName)); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	if len(service.created) != 1 {
		t.Fatalf("the service was asked to bind %v; one shard holds one address", service.created)
	}
	want := egressaddress.ClaimName(testShardNamespace, testShardName)
	if service.created[0] != want {
		t.Errorf("claim name = %q, want the name derived from the shard %q", service.created[0], want)
	}
}

// spec.shardAddressIPv6 cannot be corrected once written, so a claim that holds
// no address yet must leave the field alone rather than write a blank or a
// guess to be replaced later.
func TestAnUnboundClaimWritesNothing(t *testing.T) {
	cell := newShardCell(t, shard(testShardName))
	service := newFakeAddressIPAM(t)
	service.unbound = true

	_, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName))
	if err == nil {
		t.Fatal("a claim holding no address reconciled successfully")
	}

	got := readShard(t, cell, testShardName)
	if got.Spec.ShardAddressIPv6 != "" {
		t.Fatalf("a write-once field was written with %q before an address existed", got.Spec.ShardAddressIPv6)
	}
	if _, marked := got.Labels[bgpv1alpha1.LabelEgressShardIPv6]; marked {
		t.Error("the shard was marked as serving IPv6 while holding no address")
	}
}

// An address an operator assigned by hand is what every shard carries today.
// Nothing may claim a second one for it, and nothing may try to rewrite it.
func TestAnOperatorAssignedAddressIsLeftAlone(t *testing.T) {
	existing := shard(testShardName)
	existing.Spec.ShardAddressIPv6 = "2001:db8:100::dead"
	cell := newShardCell(t, existing)
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(service.created) != 0 {
		t.Errorf("an addressed shard drew %v from the public range", service.created)
	}
	got := readShard(t, cell, testShardName)
	if got.Spec.ShardAddressIPv6 != "2001:db8:100::dead" {
		t.Errorf("address = %q, want the operator's own value untouched", got.Spec.ShardAddressIPv6)
	}
	// The label still has to be caught up: an operator writing the address by
	// hand is exactly the case where it is missing, and a shard carrying the
	// address without the label is selected by nothing.
	if got.Labels[bgpv1alpha1.LabelEgressShardIPv6] != bgpv1alpha1.LabelValueEgressFamilyServed {
		t.Errorf("an addressed shard was left unselectable: labels = %v", got.Labels)
	}
}

// A shard draining still carries flows the address is translating, and giving
// one an address it will never program consumes public space for nothing.
func TestAShardOnItsWayOutIsNotAddressed(t *testing.T) {
	leaving := shard(testShardName)
	leaving.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	leaving.Finalizers = []string{"test.datumapis.com/hold"}
	cell := newShardCell(t, leaving)
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(service.created) != 0 {
		t.Errorf("a shard being deleted drew %v from the public range", service.created)
	}
	if len(service.deleted) != 0 {
		t.Errorf("a shard still draining released %v while still translating", service.deleted)
	}
}

// Announceable public space is scarce, so a shard that is actually gone gives
// its address back. The claim carries ReclaimPolicy Delete, so removing it is
// what frees the address.
func TestAShardThatIsGoneReleasesItsAddress(t *testing.T) {
	cell := newShardCell(t)
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	want := egressaddress.ClaimName(testShardNamespace, testShardName)
	if len(service.deleted) != 1 || service.deleted[0] != want {
		t.Fatalf("released %v, want the claim named for the departed shard %q", service.deleted, want)
	}
}

// A read that failed says nothing about whether the shard is still there.
// Releasing on it would put a live shard's address back in circulation for
// another shard to be handed while the first is still translating with it.
func TestAFailedReadReleasesNothing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := bgpv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the cell scheme: %v", err)
	}
	cell := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(shard(testShardName)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("the cell's API server is unreachable")
			},
		}).
		Build()
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err == nil {
		t.Fatal("an unreadable shard reconciled successfully")
	}
	if len(service.deleted) != 0 {
		t.Errorf("an unreadable shard released %v", service.deleted)
	}
}

// Two shards in one cell are two addresses. Sharing one would split each
// other's return traffic, because the datapath claims a reply by exact match
// against the address it translates to.
func TestTwoShardsGetTwoAddresses(t *testing.T) {
	cell := newShardCell(t,
		shard("worker-a"),
		shard("worker-b"))
	service := newFakeAddressIPAM(t)
	reconciler := reconcilerFor(cell, service)

	for _, name := range []string{"worker-a", "worker-b"} {
		if _, err := reconciler.Reconcile(context.Background(),
			requestFor(name)); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}

	first := readShard(t, cell, "worker-a").Spec.ShardAddressIPv6
	second := readShard(t, cell, "worker-b").Spec.ShardAddressIPv6
	if first == "" || second == "" {
		t.Fatalf("a shard was left unaddressed: %q and %q", first, second)
	}
	if first == second {
		t.Fatalf("two shards were given one address %q", first)
	}
}

// The claim has to carry the location, because the class holding the shared
// per-location range names it in poolPer. A claim without it is refused, and
// one with the wrong value succeeds and hands this cell an address another
// location's fabric attracts.
func TestTheClaimCarriesTheLocationAndTheFamily(t *testing.T) {
	cell := newShardCell(t, shard(testShardName))
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	stored := &ipamv1alpha1.IPClaim{}
	if err := service.client.Get(context.Background(), client.ObjectKey{
		Namespace: testClaimNamespace,
		Name:      egressaddress.ClaimName(testShardNamespace, testShardName),
	}, stored); err != nil {
		t.Fatalf("read the claim back: %v", err)
	}

	if got := stored.Spec.Scope[egressaddress.ScopeRoleLocation].Name; got != testLocation {
		t.Errorf("claim location = %q, want %q", got, testLocation)
	}
	if stored.Spec.IPFamily != ipamv1alpha1.IPv6 {
		t.Errorf("claim family = %q; without it the server reads a /128 as an IPv4 length", stored.Spec.IPFamily)
	}
	if stored.Spec.ReclaimPolicy != ipamv1alpha1.ReclaimDelete {
		t.Errorf("reclaimPolicy = %q; Retain would hand a recreated shard the same address back and defeat the only remedy for a wrong one",
			stored.Spec.ReclaimPolicy)
	}
	// spec.ownerRef is overwritten by the server with the requesting project's
	// identity, so the shard a claim is held for can only be recorded here.
	if stored.Annotations[egressaddress.AnnotationShardName] != testShardName {
		t.Errorf("the claim records no shard: annotations = %v", stored.Annotations)
	}
}

// A deployment that cannot say which location it serves must not reach a shard
// at all: the address it would write cannot be taken back.
func TestSetupRefusesADeploymentThatCannotClaimCorrectly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reconciler *EgressShardAddressReconciler
	}{
		{"no class", &EgressShardAddressReconciler{ClaimNamespace: "default", PlatformProject: testPlatformProject, Location: testLocation, IPAM: &fakeAddressIPAM{}}},
		{"no location", &EgressShardAddressReconciler{AddressClassIPv6: testAddressClass, ClaimNamespace: "default", PlatformProject: testPlatformProject, IPAM: &fakeAddressIPAM{}}},
		{"no namespace", &EgressShardAddressReconciler{AddressClassIPv6: testAddressClass, PlatformProject: testPlatformProject, Location: testLocation, IPAM: &fakeAddressIPAM{}}},
		{"no project", &EgressShardAddressReconciler{AddressClassIPv6: testAddressClass, ClaimNamespace: "default", Location: testLocation, IPAM: &fakeAddressIPAM{}}},
		{"no address space", &EgressShardAddressReconciler{AddressClassIPv6: testAddressClass, ClaimNamespace: "default", PlatformProject: testPlatformProject, Location: testLocation}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.reconciler.SetupWithManager(nil); err == nil {
				t.Fatal("a deployment that cannot claim correctly started anyway")
			}
		})
	}
}

// Nothing here writes Retain, but an operator can set it on the class or
// release a claim by hand, and the service then refuses a claim of the same
// name with a 409 rather than handing the address back (milo-os/ipam #107 --
// no lease expiry and no replacement matching). The refusal names the
// allocation, so the address is one read away: take it rather than leaving the
// shard unaddressed forever behind a conflict that will never clear.
func TestARetainedAddressIsAdoptedRatherThanLost(t *testing.T) {
	claimName := egressaddress.ClaimName(testShardNamespace, testShardName)
	allocationName := allocationNameForClaim(claimName)
	const held = "2001:db8:100::abcd/128"

	retained := &ipamv1alpha1.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{Namespace: testClaimNamespace, Name: allocationName},
		Status:     ipamv1alpha1.IPAllocationStatus{AllocatedCIDR: held},
	}

	cell := newShardCell(t, shard(testShardName))
	service := newFakeAddressIPAM(t)
	service.retained[allocationName] = held
	if err := service.client.Create(context.Background(), retained); err != nil {
		t.Fatalf("seed the retained allocation: %v", err)
	}

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := readShard(t, cell, testShardName).Spec.ShardAddressIPv6
	if got != "2001:db8:100::abcd" {
		t.Fatalf("address = %q, want the retained address %q read out of the allocation the refusal named",
			got, "2001:db8:100::abcd")
	}
}

// The address is read out of status.allocatedCIDR and nowhere else. The API
// also carries a status.address holding the single-address form, and no
// released version of the service writes it, so a controller reading that
// instead treats every successful allocation as unbound.
func TestTheAddressIsReadFromAllocatedCIDRNotStatusAddress(t *testing.T) {
	cell := newShardCell(t, shard(testShardName))
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	stored := &ipamv1alpha1.IPClaim{}
	if err := service.client.Get(context.Background(), client.ObjectKey{
		Namespace: testClaimNamespace,
		Name:      egressaddress.ClaimName(testShardNamespace, testShardName),
	}, stored); err != nil {
		t.Fatalf("read the claim back: %v", err)
	}
	if stored.Status.Address != "" {
		t.Fatal("the fake set status.address, so this no longer proves the controller ignores it")
	}
	if readShard(t, cell, testShardName).Spec.ShardAddressIPv6 == "" {
		t.Fatal("the shard was left unaddressed by a claim whose allocatedCIDR was set")
	}
}

// The address alone is unattributable: the service overwrites a claim's
// ownerRef with the requesting project's identity, so the only trail from a
// translating address to the allocation accountable for it is this reference.
func TestAFreshlyClaimedAddressRecordsItsClaim(t *testing.T) {
	cell := newShardCell(t, shard(testShardName))
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := readShard(t, cell, testShardName)
	ref := got.Spec.ShardAddressIPv6ClaimRef
	if ref == nil {
		t.Fatal("the address was assigned with no trail back to what holds it")
	}
	if ref.Kind != egressaddress.KindIPClaim {
		t.Errorf("kind = %q, want %q for an address a live claim holds", ref.Kind, egressaddress.KindIPClaim)
	}
	if want := egressaddress.ClaimName(testShardNamespace, testShardName); ref.Name != want {
		t.Errorf("name = %q, want the claim named for the shard %q", ref.Name, want)
	}
	if ref.Namespace != testClaimNamespace {
		t.Errorf("namespace = %q, want %q", ref.Namespace, testClaimNamespace)
	}
	// Required by the API, and a reference without it resolves nowhere.
	if ref.Project != testPlatformProject {
		t.Errorf("project = %q, want %q", ref.Project, testPlatformProject)
	}
	if ref.APIGroup != ipamv1alpha1.GroupName {
		t.Errorf("apiGroup = %q, want %q", ref.APIGroup, ipamv1alpha1.GroupName)
	}
	// The reference must name the claim the address actually came from, which
	// is the one the service was asked to bind.
	if len(service.created) != 1 || service.created[0] != ref.Name {
		t.Errorf("the reference names %q but the claims created were %v", ref.Name, service.created)
	}
}

// The case most likely to record something that does not exist. Adopting an
// address held by a retained allocation means no claim was ever stored, so the
// reference has to name the allocation. Both fields are write-once, so naming
// the refused claim would be permanent for this shard's lifetime.
func TestAnAdoptedAddressRecordsTheAllocationItCameFrom(t *testing.T) {
	claimName := egressaddress.ClaimName(testShardNamespace, testShardName)
	allocationName := allocationNameForClaim(claimName)
	const held = "2001:db8:100::abcd/128"

	retained := &ipamv1alpha1.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{Namespace: testClaimNamespace, Name: allocationName},
		Status:     ipamv1alpha1.IPAllocationStatus{AllocatedCIDR: held},
	}

	cell := newShardCell(t, shard(testShardName))
	service := newFakeAddressIPAM(t)
	service.retained[allocationName] = held
	if err := service.client.Create(context.Background(), retained); err != nil {
		t.Fatalf("seed the retained allocation: %v", err)
	}

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := readShard(t, cell, testShardName)
	ref := got.Spec.ShardAddressIPv6ClaimRef
	if ref == nil {
		t.Fatal("an adopted address was assigned with no trail back to what holds it")
	}
	if ref.Name == claimName {
		t.Fatal("the reference names the claim the service refused and never stored")
	}
	if ref.Name != allocationName {
		t.Errorf("name = %q, want the allocation the refusal named %q", ref.Name, allocationName)
	}
	if ref.Kind != egressaddress.KindIPAllocation {
		t.Errorf("kind = %q, want %q; no claim exists to point at", ref.Kind, egressaddress.KindIPAllocation)
	}
	if got.Spec.ShardAddressIPv6 != "2001:db8:100::abcd" {
		t.Errorf("address = %q, want the retained address", got.Spec.ShardAddressIPv6)
	}
}

// Both fields are write-once, so a shard already carrying them is read and left
// exactly as it is. Reconciling one must not draw a second address, and must
// not attempt a rewrite the API would refuse.
func TestAShardCarryingBothIsLeftUntouched(t *testing.T) {
	existing := shard(testShardName)
	existing.Spec.ShardAddressIPv6 = "2001:db8:100::dead"
	existing.Spec.ShardAddressIPv6ClaimRef = &bgpv1alpha1.AddressClaimRef{
		APIGroup:  ipamv1alpha1.GroupName,
		Kind:      egressaddress.KindIPClaim,
		Project:   testPlatformProject,
		Namespace: testClaimNamespace,
		Name:      "a-claim-someone-else-made",
	}
	existing.Labels = map[string]string{
		bgpv1alpha1.LabelEgressShardIPv6: bgpv1alpha1.LabelValueEgressFamilyServed,
	}
	cell := newShardCell(t, existing)
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(service.created) != 0 {
		t.Errorf("an addressed shard drew %v from the public range", service.created)
	}
	got := readShard(t, cell, testShardName)
	if got.Spec.ShardAddressIPv6 != "2001:db8:100::dead" {
		t.Errorf("address = %q, want it untouched", got.Spec.ShardAddressIPv6)
	}
	if got.Spec.ShardAddressIPv6ClaimRef == nil || got.Spec.ShardAddressIPv6ClaimRef.Name != "a-claim-someone-else-made" {
		t.Errorf("reference = %+v, want it untouched", got.Spec.ShardAddressIPv6ClaimRef)
	}
}

// An address an operator assigned by hand has no claim behind it, so there is
// nothing truthful to reference. It stays unattributable rather than gaining a
// reference this controller invented for an allocation it never made -- which
// would be permanent, and would name a claim that never existed.
func TestAnOperatorAssignedAddressGainsNoInventedReference(t *testing.T) {
	existing := shard(testShardName)
	existing.Spec.ShardAddressIPv6 = "2001:db8:100::dead"
	cell := newShardCell(t, existing)
	service := newFakeAddressIPAM(t)

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := readShard(t, cell, testShardName)
	if got.Spec.ShardAddressIPv6ClaimRef != nil {
		t.Fatalf("a hand-assigned address gained the invented reference %+v",
			got.Spec.ShardAddressIPv6ClaimRef)
	}
	if len(service.created) != 0 {
		t.Errorf("a hand-assigned address caused %v to be claimed for the sake of a reference", service.created)
	}
}

// Nothing is written at all while the claim holds no address, so a shard never
// gains a reference whose address is still missing -- both fields are
// write-once and a half-written pair cannot be completed.
func TestAnUnboundClaimWritesNeitherAddressNorReference(t *testing.T) {
	cell := newShardCell(t, shard(testShardName))
	service := newFakeAddressIPAM(t)
	service.unbound = true

	if _, err := reconcilerFor(cell, service).Reconcile(context.Background(),
		requestFor(testShardName)); err == nil {
		t.Fatal("a claim holding no address reconciled successfully")
	}

	got := readShard(t, cell, testShardName)
	if got.Spec.ShardAddressIPv6 != "" {
		t.Errorf("address = %q, want nothing written", got.Spec.ShardAddressIPv6)
	}
	if got.Spec.ShardAddressIPv6ClaimRef != nil {
		t.Errorf("reference = %+v, want nothing written", got.Spec.ShardAddressIPv6ClaimRef)
	}
}
