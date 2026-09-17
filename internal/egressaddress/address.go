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

// Package egressaddress holds the public address an egress shard translates to.
//
// A shard's address is drawn from announceable public space shared by every
// shard in a location, not from a per-consumer prefix: one address serves every
// network the class places on the shard, because per-network blocks exhaust a
// public aggregate long before networks exhaust it.
//
// The claim is the record. Its name is derived from the shard, so the address a
// shard holds is a permanent property of that shard's name in that namespace
// for as long as the claim lives, and nothing here has to store a mapping of
// its own. Attribution rides on the name and on annotations because the service
// overwrites spec.ownerRef with the requesting project's identity, so a claim
// cannot record which shard it is held for that way.
package egressaddress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// AddressBits is the prefix length a shard's address is claimed at. A
	// shard translates to one address, and the datapath claims a reply by
	// exact match against it, so anything shorter would hand the shard space
	// it cannot use and would consume announceable public space at a multiple
	// of what a shard needs.
	AddressBits = 128

	// ScopeRoleLocation is the scope role naming the location a claim is made
	// for. The class holding the per-location pool names it in poolPer, so a
	// claim omitting it is refused rather than drawn from another location's
	// space.
	ScopeRoleLocation = "location"

	// locationAPIGroup and locationKind identify a Location the same way every
	// other claim in the platform does. A scope reference is compared, never
	// resolved, so the three fields only have to agree with what else claims
	// against this space -- and disagreeing would carve a second pool for one
	// location rather than return an error.
	locationAPIGroup = "networking.datumapis.com"
	locationKind     = "Location"

	// claimNamePrefix is what makes every claim of this kind recognisable to an
	// operator reading the platform project, where claims for several purposes
	// share one namespace.
	claimNamePrefix = "egress-shard-ipv6"

	// AnnotationShardNamespace and AnnotationShardName record the shard a claim
	// is held for.
	//
	// Annotations rather than labels: a shard is named after the node it runs
	// on and may exceed the 63 characters a label value allows, and nothing
	// selects these claims -- the name is derived from the shard, so every
	// lookup is a Get. A label would buy a selector nobody uses at the cost of
	// refusing to record the shards with the longest names.
	AnnotationShardNamespace = "cloud.datumapis.com/egress-shard-namespace"
	AnnotationShardName      = "cloud.datumapis.com/egress-shard-name"

	// maxClaimNameLength is the ceiling on an object name in Kubernetes, which
	// is all an IPClaim name is.
	maxClaimNameLength = 253
)

// Request names one shard's claim on the public address space.
type Request struct {
	// ClassName is the class that hands out shard addresses.
	ClassName string

	// Namespace is the namespace in the platform's own tenancy the claim is
	// written to.
	Namespace string

	// Location is the location whose shared public range the address comes
	// from. Shards in one location draw from one range, and two cells serving
	// the same location share it.
	Location string

	// ShardNamespace and ShardName identify the shard the address is for. The
	// claim is named from the pair, so a shard reconciled twice finds the
	// address it already holds rather than drawing a second one.
	ShardNamespace string
	ShardName      string
}

// ClaimName is the name the request's claim is held under.
//
// The delimiter is a dot rather than a dash because a namespace is a DNS label
// and cannot contain one, so the first dot after the prefix always ends the
// namespace. A dash would be ambiguous: namespace "a-b" with shard "c" and
// namespace "a" with shard "b-c" would collide, and a collision here is two
// shards sharing one address.
func ClaimName(shardNamespace, shardName string) string {
	name := fmt.Sprintf("%s.%s.%s", claimNamePrefix, shardNamespace, shardName)
	if len(name) <= maxClaimNameLength {
		return name
	}

	sum := sha256.Sum256([]byte(shardNamespace + "/" + shardName))
	suffix := "." + hex.EncodeToString(sum[:])[:16]
	return name[:maxClaimNameLength-len(suffix)] + suffix
}

// Claim holds the address this shard translates to.
//
// The service binds on create and refuses a duplicate name, so the read comes
// first. That is what makes the allocation idempotent without this recording
// anything of its own.
func Claim(ctx context.Context, ipamClient client.Client, request Request) (netip.Addr, error) {
	ipClaim := &ipamv1alpha1.IPClaim{}
	ipClaim.Namespace = request.Namespace
	ipClaim.Name = ClaimName(request.ShardNamespace, request.ShardName)
	ipClaim.Annotations = map[string]string{
		AnnotationShardNamespace: request.ShardNamespace,
		AnnotationShardName:      request.ShardName,
	}
	ipClaim.Spec = ipamv1alpha1.IPClaimSpec{
		ClassName: request.ClassName,

		// The class already fixes the family, but the server bounds a claim's
		// prefix length from the family on the claim alone, before it resolves
		// the class at all. Left unset, a /128 is read as an IPv4 length and
		// refused, so every allocation fails.
		IPFamily: ipamv1alpha1.IPv6,

		Target:       ipamv1alpha1.TargetBlock,
		PrefixLength: ptr.To(int32(AddressBits)),

		Scope: map[string]ipamv1alpha1.ScopeRef{
			ScopeRoleLocation: {
				APIGroup: locationAPIGroup,
				Kind:     locationKind,
				Name:     request.Location,
			},
		},

		// Stated here rather than left to the class, because the reason for it
		// is this controller's own. A shard holding the wrong address is fixed
		// by deleting and recreating the shard, and a retained allocation would
		// hand the replacement the same address back and silently defeat that
		// remedy. Announceable public space is also scarce enough that an
		// address held forever by a decommissioned node is a real loss, where
		// reissue costs nothing a consumer was promised: the class is shared,
		// so the address is reported as one nobody may rely on.
		ReclaimPolicy: ipamv1alpha1.ReclaimDelete,
	}

	existing := &ipamv1alpha1.IPClaim{}
	getErr := ipamClient.Get(ctx, client.ObjectKeyFromObject(ipClaim), existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return netip.Addr{}, fmt.Errorf("read the egress address claim %q: %w", ipClaim.Name, getErr)
	}

	if getErr == nil {
		ipClaim = existing
	} else if createErr := ipamClient.Create(ctx, ipClaim); createErr != nil {
		// An allocation retained by an earlier claim of this name is this
		// shard's own address. The service refuses the create and names the
		// allocation, so the address is one read away rather than lost: read it
		// rather than treating the refusal as a failure. Nothing here writes
		// Retain, so this is reached only where an operator set it on the class
		// or released a claim by hand.
		if allocationName, retained := ipamerrors.RetainedAllocation(createErr); retained {
			return adopt(ctx, ipamClient, request.Namespace, allocationName)
		}

		// The create can still lose a race with another writer, so ask again
		// before calling this a failure to allocate.
		raced := &ipamv1alpha1.IPClaim{}
		if err := ipamClient.Get(ctx, client.ObjectKeyFromObject(ipClaim), raced); err != nil {
			return netip.Addr{}, fmt.Errorf("claim an egress address: %w", createErr)
		}
		ipClaim = raced
	}

	if ipClaim.Status.AllocatedCIDR == "" {
		// Not an error about the address: the claim exists and holds nothing
		// yet. The caller must write nothing, because the field it would write
		// cannot be corrected afterwards.
		return netip.Addr{}, &UnboundError{
			claimName: ipClaim.Name,
			phase:     string(ipClaim.Status.Phase),
		}
	}

	return FromAllocatedCIDR(ipClaim.Status.AllocatedCIDR)
}

// Release gives the shard's address back.
//
// Deleting the claim is what frees it, because the claim is written with
// ReclaimPolicy Delete. It is called only for a shard that is gone: releasing
// while a shard still holds the address in its spec would put the address back
// in circulation for another shard to be handed while the first is still
// translating with it, and the two would split each other's return traffic.
func Release(ctx context.Context, ipamClient client.Client, namespace, shardNamespace, shardName string) error {
	ipClaim := &ipamv1alpha1.IPClaim{}
	ipClaim.Namespace = namespace
	ipClaim.Name = ClaimName(shardNamespace, shardName)

	if err := ipamClient.Delete(ctx, ipClaim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("release the egress address claim %q: %w", ipClaim.Name, err)
	}
	return nil
}

// adopt reads the address out of an allocation this shard already holds. The
// allocation outlives the claim that made it, which is what retention is for,
// so the address it names is the one this shard has always had.
func adopt(ctx context.Context, ipamClient client.Client, namespace, allocationName string) (netip.Addr, error) {
	allocation := &ipamv1alpha1.IPAllocation{}
	if err := ipamClient.Get(ctx,
		client.ObjectKey{Namespace: namespace, Name: allocationName}, allocation); err != nil {
		return netip.Addr{}, fmt.Errorf("read the retained allocation %q: %w", allocationName, err)
	}
	if allocation.Status.AllocatedCIDR == "" {
		return netip.Addr{}, &UnboundError{claimName: allocationName, phase: string(allocation.Status.Phase)}
	}
	return FromAllocatedCIDR(allocation.Status.AllocatedCIDR)
}

// FromAllocatedCIDR reads the shard's address out of what the service handed
// out.
//
// status.allocatedCIDR is the only field read. The API also carries a
// status.address holding the single-address form, and no released version of
// the service writes it, so a controller reading it treats every successful
// allocation as unbound.
func FromAllocatedCIDR(cidr string) (netip.Addr, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Addr{}, &UnusableError{message: fmt.Sprintf(
			"the address service answered with %q, which is not a prefix", cidr)}
	}

	address := prefix.Addr()
	if !address.Is6() || address.Is4In6() {
		return netip.Addr{}, &UnusableError{message: fmt.Sprintf(
			"the address service answered with %q; a shard's IPv6 address is read out of an IPv6 space", cidr)}
	}

	// Anything shorter is a block rather than an address. A shard given one
	// would translate to its first address while holding the rest out of
	// circulation, and nothing would report the difference.
	if prefix.Bits() != AddressBits {
		return netip.Addr{}, &UnusableError{message: fmt.Sprintf(
			"the address service answered with %q; a shard's address is read out of a /%d", cidr, AddressBits)}
	}

	return address, nil
}

// UnusableError says the address service answered, and its answer cannot be
// used as a shard address. Retrying reaches the same allocation, so this is a
// wait on an operator rather than on the service.
type UnusableError struct {
	message string
}

func (e *UnusableError) Error() string { return e.message }

// UnboundError says the claim exists and holds no address yet. Unlike
// UnusableError this resolves on its own, so it is worth retrying and is never
// worth writing anything on.
type UnboundError struct {
	claimName string
	phase     string
}

func (e *UnboundError) Error() string {
	return fmt.Sprintf("the address service has allocated nothing for claim %q yet (phase %q)",
		e.claimName, e.phase)
}
