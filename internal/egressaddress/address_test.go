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

package egressaddress

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A claim name collision is two shards sharing one address, so the split
// between namespace and name has to be unambiguous. A dash would not be: the
// two cases below would produce the same name.
func TestClaimNameCannotCollideAcrossTheNamespaceBoundary(t *testing.T) {
	first := ClaimName("a-b", "c")
	second := ClaimName("a", "b-c")
	if first == second {
		t.Fatalf("two different shards share the claim name %q", first)
	}
}

func TestClaimNameNamesTheShard(t *testing.T) {
	got := ClaimName("galactic-system", "worker-8b4e1647-dfw")
	if !strings.Contains(got, "worker-8b4e1647-dfw") {
		t.Errorf("claim name %q does not name the shard it is held for", got)
	}
	if !strings.HasPrefix(got, claimNamePrefix) {
		t.Errorf("claim name %q is not recognisable as an egress shard address claim", got)
	}
}

// A shard is named after its node, and the name it produces still has to be a
// name the API server will accept.
func TestALongShardNameStillYieldsAValidClaimName(t *testing.T) {
	long := strings.Repeat("n", 300)
	got := ClaimName("galactic-system", long)
	if len(got) > maxClaimNameLength {
		t.Fatalf("claim name is %d characters, over the %d the API server allows", len(got), maxClaimNameLength)
	}
	// Two long names that share a prefix must not truncate to one claim.
	other := ClaimName("galactic-system", long+"x")
	if got == other {
		t.Fatal("two shards with long names share one claim name")
	}
}

func TestFromAllocatedCIDRReadsTheAddress(t *testing.T) {
	got, err := FromAllocatedCIDR("2607:ed40:70::1/128")
	if err != nil {
		t.Fatalf("read the address: %v", err)
	}
	if got.String() != "2607:ed40:70::1" {
		t.Errorf("address = %q, want the host address without its prefix length", got.String())
	}
}

// Every one of these is an answer the service gave that cannot be used as a
// shard address. Reading one anyway would write it into a field that cannot be
// corrected.
func TestFromAllocatedCIDRRefusesWhatIsNotAShardAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		cidr string
	}{
		{"not a prefix", "2607:ed40:70::1"},
		{"empty", ""},
		{"a block rather than an address", "2607:ed40:70::/64"},
		{"the wrong family", "198.51.100.7/32"},
		{"an IPv4 address in IPv6 clothing", "::ffff:198.51.100.7/128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromAllocatedCIDR(tc.cidr)
			if err == nil {
				t.Fatalf("%q was accepted as a shard address (%s)", tc.cidr, got)
			}
			var unusable *UnusableError
			if !errors.As(err, &unusable) {
				t.Errorf("error = %v; an answer that cannot be used is a wait on an operator, not a retry", err)
			}
		})
	}
}

const (
	probeNamespace      = "default"
	probeShardNamespace = "galactic-system"
	probeShardName      = "worker-8b4e1647-dfw"
)

func probeRequest() Request {
	return Request{
		ClassName:      "datum-egress-shard-address-ipv6",
		Namespace:      probeNamespace,
		Location:       "us-central-1",
		ShardNamespace: probeShardNamespace,
		ShardName:      probeShardName,
	}
}

func ipamScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ipamv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the IPAM scheme: %v", err)
	}
	return scheme
}

// A fresh claim reports itself as the record holding the address, under the
// name it was actually stored as.
func TestAFreshClaimIsReportedAsTheHolder(t *testing.T) {
	bind := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			ipClaim, ok := object.(*ipamv1alpha1.IPClaim)
			if !ok {
				return c.Create(ctx, object, opts...)
			}
			ipClaim.Status.Phase = ipamv1alpha1.ClaimPhase("Bound")
			ipClaim.Status.AllocatedCIDR = "2001:db8:100::7/128"
			return c.Create(ctx, ipClaim, opts...)
		},
	}
	ipamClient := fake.NewClientBuilder().WithScheme(ipamScheme(t)).WithInterceptorFuncs(bind).Build()

	holding, err := Claim(context.Background(), ipamClient, probeRequest())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !holding.HeldByClaim() {
		t.Errorf("kind = %q, want the address held by a claim", holding.Kind)
	}
	if want := ClaimName(probeShardNamespace, probeShardName); holding.Name != want {
		t.Errorf("holder name = %q, want the claim named for the shard %q", holding.Name, want)
	}
	if holding.Namespace != probeNamespace {
		t.Errorf("holder namespace = %q, want %q", holding.Namespace, probeNamespace)
	}
	if holding.Address != netip.MustParseAddr("2001:db8:100::7") {
		t.Errorf("address = %s, want the allocated address", holding.Address)
	}
}

// The case most likely to record a reference to something that does not exist.
//
// The service rolls its transaction back before refusing a claim whose identity
// a retained allocation already occupies, so the claim is NEVER stored. The
// holder is the allocation, named as the refusal named it -- and the allocation
// name is a hash of the claim's namespace and name, so it is not the claim name
// and cannot be derived from the shard.
func TestAnAdoptedAddressIsReportedAsHeldByTheAllocation(t *testing.T) {
	claimName := ClaimName(probeShardNamespace, probeShardName)
	// A hash, as the service computes it -- deliberately unlike the claim name.
	const allocationName = "alloc-3f2a1b0c9d8e7f60"

	retained := &ipamv1alpha1.IPAllocation{
		ObjectMeta: metav1.ObjectMeta{Namespace: probeNamespace, Name: allocationName},
		Status:     ipamv1alpha1.IPAllocationStatus{AllocatedCIDR: "2001:db8:100::abcd/128"},
	}

	refuse := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if _, ok := object.(*ipamv1alpha1.IPClaim); ok {
				return ipamerrors.NewRetainedAllocation(
					ipamv1alpha1.Resource("ipclaims"), claimName, allocationName,
					fmt.Sprintf("an allocation under this identity already exists: IPAllocation %q", allocationName))
			}
			return c.Create(ctx, object, opts...)
		},
	}
	ipamClient := fake.NewClientBuilder().
		WithScheme(ipamScheme(t)).
		WithObjects(retained).
		WithInterceptorFuncs(refuse).
		Build()

	holding, err := Claim(context.Background(), ipamClient, probeRequest())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if holding.HeldByClaim() {
		t.Fatalf("kind = %q; no claim was stored, so naming one names an object that does not exist", holding.Kind)
	}
	if holding.Kind != KindIPAllocation {
		t.Errorf("kind = %q, want %q", holding.Kind, KindIPAllocation)
	}
	if holding.Name != allocationName {
		t.Fatalf("holder name = %q, want the allocation the refusal named %q", holding.Name, allocationName)
	}
	if holding.Name == claimName {
		t.Fatal("the adopted address was attributed to the claim that was refused and never stored")
	}
	if holding.Address != netip.MustParseAddr("2001:db8:100::abcd") {
		t.Errorf("address = %s, want the retained address", holding.Address)
	}
}

// An unbound claim reports no holder at all, because there is nothing to
// attribute yet and the fields it would be written into cannot be rewritten.
func TestAnUnboundClaimReportsNoHolder(t *testing.T) {
	hold := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if ipClaim, ok := object.(*ipamv1alpha1.IPClaim); ok {
				ipClaim.Status.Phase = ipamv1alpha1.ClaimPhase("Pending")
				return c.Create(ctx, ipClaim, opts...)
			}
			return c.Create(ctx, object, opts...)
		},
	}
	ipamClient := fake.NewClientBuilder().WithScheme(ipamScheme(t)).WithInterceptorFuncs(hold).Build()

	holding, err := Claim(context.Background(), ipamClient, probeRequest())
	if err == nil {
		t.Fatal("an unbound claim was reported as holding an address")
	}
	var unbound *UnboundError
	if !errors.As(err, &unbound) {
		t.Errorf("error = %v, want an unbound claim worth retrying", err)
	}
	if holding != (Holding{}) {
		t.Errorf("holding = %+v, want nothing to attribute", holding)
	}
}
