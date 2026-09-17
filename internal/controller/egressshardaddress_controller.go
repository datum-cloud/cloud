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
	"net/netip"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"go.datum.net/cloud/internal/egressaddress"
	"go.datum.net/cloud/internal/ipam"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

// EgressShardAddressReconciler gives each egress shard in this cell the public
// IPv6 address it translates to.
//
// It runs in the cell, beside the shards it writes. A shard names the Node it
// executes on, so it exists only where that Node does, and a controller reading
// it from anywhere else would be reading a federated copy of an object whose
// whole purpose is local. That also keeps the write and the object it lands on
// in one cluster, which is what makes the write-once field below safe to
// attempt: there is no copy of the shard that could be carrying a different
// value.
//
// The shard does not claim its own address. A shard runs on every translating
// node, including hardware at the edge of the network, and the process that
// would make the claim is the one serving the datapath -- so an
// address-service credential would sit on every such node, reachable from the
// process that also handles tenant packets, and the allocation request would
// sit beside the path that attaches a workload. One controller per cell moves
// the credential count from the number of translating nodes to the number of
// cells and takes the allocation off that path entirely: an address is claimed
// when a shard object appears, which is when a node is commissioned, not when a
// workload arrives.
type EgressShardAddressReconciler struct {
	// Shards reads and writes the EgressShards in this cell.
	Client client.Client

	// IPAM reaches the address service.
	IPAM ipam.ClientFactory

	// AddressClassIPv6 is the class that hands out shard addresses.
	AddressClassIPv6 string

	// ClaimNamespace is the namespace in the platform's own tenancy that
	// address claims are written to.
	ClaimNamespace string

	// Location is the location this cell serves. It selects the shared public
	// range the address comes from, and two cells serving one location draw
	// from the same range.
	Location string
}

// Reconcile assigns the shard its address, once.
//
// Reading the shard is what separates "deleted" from "not addressed yet". Only
// a shard that is actually gone releases its claim.
func (r *EgressShardAddressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	shard := &bgpv1alpha1.EgressShard{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, shard)
	switch {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, r.release(ctx, req.Namespace, req.Name)
	case err != nil:
		// A read that failed says nothing about whether the shard is still
		// there, and releasing on it would put a live shard's address back in
		// circulation.
		return ctrl.Result{}, err
	}

	// A shard on its way out is not given an address it would never program,
	// and keeps the one it has until it is actually gone: the flows it is
	// translating are still there while it drains.
	if !shard.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if shard.Spec.ShardAddressIPv6 != "" {
		// Already addressed. The family label restates the assignment for the
		// selectors that place traffic on this shard, and cannot be written
		// with the address itself: a shard whose label write failed would
		// otherwise stay unselectable forever, because the address it would be
		// rewritten with cannot be written twice.
		return ctrl.Result{}, r.markFamilyServed(ctx, shard)
	}

	address, err := r.claim(ctx, shard)
	if err != nil {
		return ctrl.Result{}, err
	}

	// The value comes from a bound claim and from nowhere else. Every path that
	// could not produce one returned above, because spec.shardAddressIPv6 is
	// write-once: a placeholder written here is not correctable, and the shard
	// would have to be deleted and recreated to be rid of it.
	shard.Spec.ShardAddressIPv6 = address.String()
	if shard.Labels == nil {
		shard.Labels = map[string]string{}
	}
	shard.Labels[bgpv1alpha1.LabelEgressShardIPv6] = bgpv1alpha1.LabelValueEgressFamilyServed

	// An ordinary update on the object just read, not a server-side apply with
	// forced ownership. The field's own validation compares against the stored
	// value, so a conflicting write has to be refused rather than won: forcing
	// ownership of a field that cannot be reassigned is the one thing that must
	// not happen quietly here.
	if err := r.Client.Update(ctx, shard); err != nil {
		return ctrl.Result{}, fmt.Errorf("assign egress shard %q the address %s: %w",
			shard.Name, address, err)
	}

	log.FromContext(ctx).Info("assigned an egress shard its public IPv6 address",
		"shard", shard.Name, "address", address.String(), "location", r.Location)
	return ctrl.Result{}, nil
}

// markFamilyServed records that this shard translates IPv6, for the selectors
// that place traffic on it. A selector matches labels and cannot read a spec
// field, so whoever assigns the address states it here too.
func (r *EgressShardAddressReconciler) markFamilyServed(ctx context.Context, shard *bgpv1alpha1.EgressShard) error {
	if shard.Labels[bgpv1alpha1.LabelEgressShardIPv6] == bgpv1alpha1.LabelValueEgressFamilyServed {
		return nil
	}
	if shard.Labels == nil {
		shard.Labels = map[string]string{}
	}
	shard.Labels[bgpv1alpha1.LabelEgressShardIPv6] = bgpv1alpha1.LabelValueEgressFamilyServed
	if err := r.Client.Update(ctx, shard); err != nil {
		return fmt.Errorf("mark egress shard %q as serving IPv6: %w", shard.Name, err)
	}
	return nil
}

func (r *EgressShardAddressReconciler) claim(
	ctx context.Context,
	shard *bgpv1alpha1.EgressShard,
) (netip.Addr, error) {
	ipamClient, err := r.IPAM.ClientForPlatform()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("reach the public address space: %w", err)
	}

	address, err := egressaddress.Claim(ctx, ipamClient, egressaddress.Request{
		ClassName:      r.AddressClassIPv6,
		Namespace:      r.ClaimNamespace,
		Location:       r.Location,
		ShardNamespace: shard.Namespace,
		ShardName:      shard.Name,
	})
	if err != nil {
		// An unusable answer is a wait on an operator, not on the service:
		// retrying reaches the same allocation. Fail closed either way -- a
		// shard given an address it cannot translate to is worse than one given
		// none, because the assignment cannot be taken back.
		var unusable *egressaddress.UnusableError
		if errors.As(err, &unusable) {
			log.FromContext(ctx).Error(err, "the public address space handed out something no shard address can be read from",
				"shard", shard.Name, "location", r.Location)
		}
		return netip.Addr{}, fmt.Errorf("claim a public IPv6 address for egress shard %q: %w", shard.Name, err)
	}
	return address, nil
}

// release gives back the address of a shard that is gone.
//
// Triggered by the shard's absence rather than by a finalizer. A finalizer would
// close the window in which a missed delete leaks a claim, and would open a
// larger one: a shard whose address could not be released would refuse to
// finish deleting, which is how a node is kept from being decommissioned. A
// leaked claim is an operator deleting one object; a wedged shard is a node
// nobody can retire.
func (r *EgressShardAddressReconciler) release(ctx context.Context, namespace, name string) error {
	ipamClient, err := r.IPAM.ClientForPlatform()
	if err != nil {
		return fmt.Errorf("reach the public address space: %w", err)
	}
	if err := egressaddress.Release(ctx, ipamClient, r.ClaimNamespace, namespace, name); err != nil {
		return err
	}
	log.FromContext(ctx).Info("released the public IPv6 address of a shard that is gone",
		"shard", name, "location", r.Location)
	return nil
}

// This controller writes EgressShards in its own cell and claims addresses on
// the platform's behalf in IPAM, which it reaches under a separate credential
// and which no marker here covers.
//
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=network.datumapis.com,resources=egressshards,verbs=get;list;patch;update;watch

// SetupWithManager registers the reconciler.
func (r *EgressShardAddressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.AddressClassIPv6 == "" {
		return errors.New("an address class is required")
	}
	if r.ClaimNamespace == "" {
		return errors.New("a namespace to write claims in is required")
	}
	if r.Location == "" {
		// A claim carrying no location is refused by the service, and one
		// carrying the wrong location draws from another location's range and
		// hands this cell an address nothing routes to it.
		return errors.New("the location this cell serves is required")
	}
	if r.IPAM == nil {
		return errors.New("an address space is required")
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("egressshardaddress").
		For(&bgpv1alpha1.EgressShard{}).
		Complete(r)
}
