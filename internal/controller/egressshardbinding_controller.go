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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

// EgressShardBindingReconciler keeps a shard's finalizer in step with its
// consumer set.
//
// The finalizer is the only state a binder adds to a shard. It holds while any
// claim is bound, so decommissioning a shard is an act someone takes rather
// than an outcome networks discover: deleting one strands the return traffic of
// every flow it is translating, and nothing rebinds a claim, so a network whose
// shard vanished has no egress and no second answer coming.
//
// The consumer set is a list query over the claims, never a field on the shard.
// That is the whole point: a shard that recorded its own consumers would be
// holding the list of served networks the model refuses it, and the list would
// have to be kept in step by whoever binds.
type EgressShardBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=network.datumapis.com,resources=egressshards,verbs=get;list;watch;update;patch
// finalizerEgressShardBinding is held on a shard while any claim is bound to it,
// so decommissioning a shard is an act someone takes rather than an outcome
// instances discover.
const finalizerEgressShardBinding = "cloud.datumapis.com/egress-shard-binding"

// +kubebuilder:rbac:groups=network.datumapis.com,resources=egressshardclaims,verbs=get;list;watch

func (r *EgressShardBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var shard bgpv1alpha1.EgressShard
	if err := r.Get(ctx, req.NamespacedName, &shard); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	consumers, err := boundEgressShardClaims(ctx, r.Client, &shard)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(consumers) == 0 {
		return ctrl.Result{}, releaseShard(ctx, r.Client, &shard)
	}
	if !shard.DeletionTimestamp.IsZero() {
		// Said out loud, because the alternative to a stuck object here is a
		// network losing the internet with nothing to point at. Deleting the
		// claims is what lets the shard go.
		logf.FromContext(ctx).Info("holding an egress shard open while networks are bound to it",
			"shard", client.ObjectKeyFromObject(&shard), "consumers", len(consumers))
	}
	return ctrl.Result{}, holdShard(ctx, r.Client, &shard)
}

// holdShard adds the binder's finalizer, so the shard cannot go while a network
// is bound to it.
func holdShard(ctx context.Context, cl client.Client, shard *bgpv1alpha1.EgressShard) error {
	if controllerutil.ContainsFinalizer(shard, finalizerEgressShardBinding) {
		return nil
	}
	// Patched rather than updated. The controller holding the addressing-service
	// credential writes this spec's addresses, and a whole-object update from a
	// copy read before that write would put the old value back over a field that
	// is write-once.
	patch := client.MergeFrom(shard.DeepCopy())
	controllerutil.AddFinalizer(shard, finalizerEgressShardBinding)
	if err := cl.Patch(ctx, shard, patch); err != nil {
		return fmt.Errorf("hold egress shard %s open for the networks bound to it: %w",
			client.ObjectKeyFromObject(shard), err)
	}
	return nil
}

// releaseShard removes the binder's finalizer from a shard no network is bound
// to, which is what lets an operator decommission a drained node.
func releaseShard(ctx context.Context, cl client.Client, shard *bgpv1alpha1.EgressShard) error {
	if !controllerutil.ContainsFinalizer(shard, finalizerEgressShardBinding) {
		return nil
	}
	patch := client.MergeFrom(shard.DeepCopy())
	controllerutil.RemoveFinalizer(shard, finalizerEgressShardBinding)
	if err := cl.Patch(ctx, shard, patch); err != nil {
		return fmt.Errorf("release egress shard %s: %w", client.ObjectKeyFromObject(shard), err)
	}
	logf.FromContext(ctx).Info("released an egress shard no network is bound to",
		"shard", client.ObjectKeyFromObject(shard))
	return nil
}

// boundEgressShardClaims is a shard's consumer set: the networks bound to it.
//
// It is a list query over the label the binder stamps, which is what stands in
// for the list of served networks a shard does not hold. The label narrows the
// query and each claim's own status settles it, so a label left behind by a
// binding that never completed counts as nothing.
func boundEgressShardClaims(
	ctx context.Context, reader client.Reader, shard *bgpv1alpha1.EgressShard,
) ([]bgpv1alpha1.EgressShardClaim, error) {
	var claims bgpv1alpha1.EgressShardClaimList
	if err := reader.List(ctx, &claims, client.MatchingLabels{
		bgpv1alpha1.LabelEgressShardClaimShard: shard.Name,
	}); err != nil {
		return nil, fmt.Errorf("list the claims bound to egress shard %s: %w", shard.Name, err)
	}

	bound := make([]bgpv1alpha1.EgressShardClaim, 0, len(claims.Items))
	for i := range claims.Items {
		claim := claims.Items[i]
		held := claim.Status.ShardRef
		if held == nil || held.Name != shard.Name || held.Namespace != shard.Namespace {
			continue
		}
		if !claim.DeletionTimestamp.IsZero() {
			continue
		}
		bound = append(bound, claim)
	}
	return bound, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *EgressShardBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bgpv1alpha1.EgressShard{}).
		Watches(&bgpv1alpha1.EgressShardClaim{},
			handler.EnqueueRequestsFromMapFunc(egressShardForClaim)).
		Named("egressshardbinding").
		Complete(r)
}

// egressShardForClaim maps a claim to the shard it holds, including the last
// state of one being deleted — which is the event that lets the final claim on
// a shard release it.
//
// A claim holding no binding maps to nothing, and needs to: it was never part
// of any shard's consumer set, which is counted from this same field.
func egressShardForClaim(_ context.Context, object client.Object) []reconcile.Request {
	claim, ok := object.(*bgpv1alpha1.EgressShardClaim)
	if !ok || claim.Status.ShardRef == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: claim.Status.ShardRef.Namespace,
		Name:      claim.Status.ShardRef.Name,
	}}}
}
