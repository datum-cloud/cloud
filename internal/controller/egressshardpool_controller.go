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
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

// EgressShardPoolReconciler builds the egress shard objects a cell has, one per
// node an operator commissioned into a pool.
//
// It creates and never rewrites. A shard's addresses are write-once because the
// datapath claims a reply by exact match against the address it translates to,
// so a shard holding the wrong value is deleted and recreated at a moment
// someone chose rather than edited underneath live flows. The same reasoning
// covers its labels, which is what a class's selector matches: moving a shard
// between pools by relabelling it moves traffic silently. This reconciler
// therefore leaves an existing shard exactly as it found it, and its shard
// access is get, list, watch and create.
//
// Decommissioning is likewise not its job. A node that stops matching keeps its
// shard, because deleting one strands the return traffic of every flow it is
// translating and nothing here knows whether that is what an operator meant.
type EgressShardPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=egressshardpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=egressshardpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=network.datumapis.com,resources=egressshards,verbs=get;list;watch;create

func (r *EgressShardPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool cloudv1alpha1.EgressShardPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		// The shards outlive the declaration that built them. A pool deleted by
		// an operator who is reorganizing their cell must not take the
		// translation of every network on it with it.
		return ctrl.Result{}, nil
	}

	nodes, err := r.selectedNodes(ctx, &pool)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(nodes) == 0 {
		return ctrl.Result{}, r.publishPoolStatus(ctx, &pool, metav1.ConditionFalse,
			cloudv1alpha1.EgressShardPoolReasonNoNodeSelected,
			"No node carries the labels this pool selects, so it builds no shard")
	}

	for _, node := range nodes {
		name := shardNameForNode(&pool, node)
		if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
			return ctrl.Result{}, r.publishPoolStatus(ctx, &pool, metav1.ConditionFalse,
				"ShardNameInvalid",
				fmt.Sprintf("Pool %q and node %q derive shard name %q, which is not a valid object name: %s",
					pool.Name, node, name, problems[0]))
		}
		if err := r.buildShard(ctx, &pool, node, name); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, r.publishPoolStatus(ctx, &pool, metav1.ConditionTrue,
		cloudv1alpha1.EgressShardPoolReasonShardsBuilt,
		fmt.Sprintf("Every one of the %d nodes this pool selects has a shard in namespace %s",
			len(nodes), pool.Spec.ShardNamespace))
}

// selectedNodes are the nodes this pool commissions, in name order so the
// shards are built in a stable order and a partial pass resumes where it left
// off rather than somewhere else.
func (r *EgressShardPoolReconciler) selectedNodes(
	ctx context.Context, pool *cloudv1alpha1.EgressShardPool,
) ([]string, error) {
	selector, err := metav1.LabelSelectorAsSelector(&pool.Spec.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("parse the node selector on EgressShardPool %s: %w", pool.Name, err)
	}
	// An empty selector is refused by the schema, but a selector parsed from an
	// object written before that rule reaches everything selects every node in
	// the cell, which is a shard per compute node. Refuse it here too.
	if selector.Empty() {
		return nil, nil
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list the nodes EgressShardPool %s selects: %w", pool.Name, err)
	}

	names := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		names = append(names, nodes.Items[i].Name)
	}
	slices.Sort(names)
	return names, nil
}

// buildShard creates the shard for one node if it does not exist yet.
//
// Nothing here writes an address. The controller holding the addressing-service
// credential claims one and writes it into this spec afterwards, which is what
// keeps that credential off every translating node — so a shard is born with a
// target and nothing else, and says so as Programmed=False/AddressUnassigned
// until the claim lands.
func (r *EgressShardPoolReconciler) buildShard(
	ctx context.Context, pool *cloudv1alpha1.EgressShardPool, node, name string,
) error {
	key := client.ObjectKey{Namespace: pool.Spec.ShardNamespace, Name: name}
	var existing bgpv1alpha1.EgressShard
	switch err := r.Get(ctx, key, &existing); {
	case err == nil:
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("get EgressShard %s: %w", key, err)
	}

	shard := &bgpv1alpha1.EgressShard{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace,
			Name:      key.Name,
			Labels:    shardLabels(pool),
		},
		Spec: bgpv1alpha1.EgressShardSpec{
			TargetRef: bgpv1alpha1.TargetRef{Kind: "Node", Name: node},
		},
	}
	// No owner reference: the pool is cluster-scoped and the shard is not, and
	// a shard must outlive the declaration that built it in any case.
	if err := r.Create(ctx, shard); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create EgressShard %s for node %s: %w", key, node, err)
	}
	logf.FromContext(ctx).Info("built an egress shard for a commissioned node",
		"pool", pool.Name, "shard", key, "node", node)
	return nil
}

// shardLabels are what a class's shard selector matches on the built shard. The
// pool label carries the pool's own name, so it cannot drift from the object
// that stamped it, and an operator's own labels cannot overwrite it.
func shardLabels(pool *cloudv1alpha1.EgressShardPool) map[string]string {
	labels := make(map[string]string, len(pool.Spec.ShardLabels)+1)
	maps.Copy(labels, pool.Spec.ShardLabels)
	labels[bgpv1alpha1.LabelEgressShardPool] = pool.Name
	return labels
}

// shardNameForNode derives a shard's name from its pool and its node, so the
// same pass finds the shard it built last time without reading a reference
// anyone has to keep in step.
func shardNameForNode(pool *cloudv1alpha1.EgressShardPool, node string) string {
	return pool.Name + "-" + node
}

func (r *EgressShardPoolReconciler) publishPoolStatus(
	ctx context.Context,
	pool *cloudv1alpha1.EgressShardPool,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	pool.Status.ObservedGeneration = pool.Generation
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               cloudv1alpha1.ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: pool.Generation,
	})
	if err := r.Status().Update(ctx, pool); err != nil {
		return fmt.Errorf("update EgressShardPool %s status: %w", pool.Name, err)
	}
	return nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *EgressShardPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.EgressShardPool{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.poolsForNode)).
		Named("egressshardpool").
		Complete(r)
}

// poolsForNode wakes every pool when a node is labelled, so commissioning a
// node builds its shard immediately instead of waiting out a poll interval that
// does not exist. Which pool a node joined is a label the pool selects on
// rather than a field, so every pool is asked rather than one being looked up.
func (r *EgressShardPoolReconciler) poolsForNode(ctx context.Context, _ client.Object) []reconcile.Request {
	var pools cloudv1alpha1.EgressShardPoolList
	if err := r.List(ctx, &pools); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(pools.Items))
	for i := range pools.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&pools.Items[i]),
		})
	}
	return requests
}
