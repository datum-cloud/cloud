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

// Package fabricidentity allocates the identity the fabric knows a network by.
//
// The platform's address service allocates prefixes, not integers. Rather than
// build a second allocator with the same uniqueness and concurrency problems
// already solved there, an identity is allocated as a block from a pool that is
// never routed, and the integer is the block's index within that pool. A /32
// root handing out /64s yields exactly 2^32 allocations whose distinguishing
// bits are exactly the 32 the fabric uses.
//
// That buys uniqueness, exhaustion accounting, quota and an audit trail, and
// costs address space that is never routed and never reachable.
package fabricidentity

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// BlockBits is the size of the block an identity is read out of. The pool
	// roots a /32 and hands out /64s, so the bits between them are the block's
	// index within the pool, and there are exactly 2^32 of them.
	BlockBits = 64

	// RootBits is where the pool's own prefix stops and the index begins.
	// Reading the index from a fixed offset rather than from the pool's CIDR
	// means the identity does not depend on an object this would otherwise have
	// to read on every allocation. A pool rooted longer than a /32 leaves the
	// leading bits of every index at zero, which is still unique within the one
	// pool the platform allocates from.
	RootBits = 32
)

// Request names one network's claim on the identifier space.
type Request struct {
	// ClassName is the IPClass that hands out identities.
	ClassName string

	// Namespace is the namespace in the platform's own tenancy the claim is
	// written to.
	Namespace string

	// NetworkUID names the network. The claim is named from it, so a reconcile
	// that lost its answer finds the same block again instead of taking a
	// second one, and a network deleted and recreated under the same name is a
	// different network with a different identity.
	NetworkUID string
}

// ClaimName is the name the request's claim is held under.
func ClaimName(networkUID string) string {
	return "fabric-identity-" + networkUID
}

// Claim holds one block of the identifier space and reads the identity out of
// it.
//
// IPAM binds on create and refuses a duplicate name, so the read comes first.
// That is what makes the allocation idempotent without this recording anything
// of its own: the claim is the record.
func Claim(ctx context.Context, ipamClient client.Client, request Request) (int64, error) {
	ipClaim := &ipamv1alpha1.IPClaim{}
	ipClaim.Namespace = request.Namespace
	ipClaim.Name = ClaimName(request.NetworkUID)
	ipClaim.Spec = ipamv1alpha1.IPClaimSpec{
		ClassName:    request.ClassName,
		Target:       ipamv1alpha1.TargetBlock,
		PrefixLength: ptr.To(int32(BlockBits)),

		// An identity is never given back. A Route Target still installed in a
		// remote location's import policy would silently merge a new network
		// into a dead one's routes, so holding the block forever is the safe
		// failure and reissuing it is not a failure anything can see.
		ReclaimPolicy: ipamv1alpha1.ReclaimRetain,
	}

	existing := &ipamv1alpha1.IPClaim{}
	getErr := ipamClient.Get(ctx, client.ObjectKeyFromObject(ipClaim), existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return 0, fmt.Errorf("read the identity claim %q: %w", ipClaim.Name, getErr)
	}

	if getErr == nil {
		ipClaim = existing
	} else if createErr := ipamClient.Create(ctx, ipClaim); createErr != nil {
		// The create can still lose a race with another writer, so ask again
		// before calling this a failure to allocate.
		raced := &ipamv1alpha1.IPClaim{}
		if err := ipamClient.Get(ctx, client.ObjectKeyFromObject(ipClaim), raced); err != nil {
			return 0, fmt.Errorf("claim a fabric identity: %w", createErr)
		}
		ipClaim = raced
	}

	if ipClaim.Status.AllocatedCIDR == "" {
		return 0, fmt.Errorf("the identity space allocated nothing for this network (phase %q)", ipClaim.Status.Phase)
	}

	return FromBlock(ipClaim.Status.AllocatedCIDR)
}

// FromBlock reads the identity out of the block the identifier space handed
// out. The block's index within the pool is the identity, and the index is the
// 32 bits between the pool's root and the block, which is exactly the width
// that survives into the Route Target.
func FromBlock(cidr string) (int64, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return 0, &UnusableError{message: fmt.Sprintf(
			"the identity space answered with %q, which is not a prefix", cidr)}
	}

	address := prefix.Addr()
	if !address.Is6() || address.Is4In6() {
		return 0, &UnusableError{message: fmt.Sprintf(
			"the identity space answered with %q; identifiers are read out of an IPv6 space", cidr)}
	}

	// Shorter than a /64 and two networks could be handed blocks that share an
	// index; longer and one block's index is not the whole of it.
	if prefix.Bits() != BlockBits {
		return 0, &UnusableError{message: fmt.Sprintf(
			"the identity space answered with %q; identifiers are read out of a /%d", cidr, BlockBits)}
	}

	octets := address.As16()
	identity := int64(binary.BigEndian.Uint32(octets[RootBits/8 : BlockBits/8]))
	if identity == 0 {
		return 0, &UnusableError{message: fmt.Sprintf(
			"the identity space answered with %q, whose index is zero; zero is what a network holding no identity reads as, so the pool must not hand out its first block", cidr)}
	}
	return identity, nil
}

// UnusableError says the identity space answered, and its answer cannot be
// turned into an identity. Retrying reaches the same block, so this is a wait
// on an operator rather than on the service.
type UnusableError struct {
	message string
}

func (e *UnusableError) Error() string { return e.message }
