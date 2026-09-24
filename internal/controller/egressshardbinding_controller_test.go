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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

// egressTestShard is the one shard these tests hold open or release.
const egressTestShard = "shard-a"

func newShardBinder(t *testing.T, objects ...client.Object) (*EgressShardBindingReconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the cloud scheme: %v", err)
	}
	if err := bgpv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the fabric scheme: %v", err)
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &EgressShardBindingReconciler{Client: fakeClient, Scheme: scheme}, fakeClient
}

func reconcileShard(t *testing.T, r *EgressShardBindingReconciler) {
	t.Helper()
	key := client.ObjectKey{Namespace: egressShardNamespace, Name: egressTestShard}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile the shard: %v", err)
	}
}

func heldOpen(t *testing.T, cl client.Client) bool {
	t.Helper()
	var shard bgpv1alpha1.EgressShard
	key := client.ObjectKey{Namespace: egressShardNamespace, Name: egressTestShard}
	if err := cl.Get(t.Context(), key, &shard); err != nil {
		t.Fatalf("get the shard: %v", err)
	}
	return controllerutil.ContainsFinalizer(&shard, cloudv1alpha1.FinalizerEgressShardBinding)
}

// The finalizer is the only state a binder adds to a shard, and it holds while
// a network is bound: deleting a translating shard strands the return traffic of
// every flow on it, and nothing rebinds a claim.
func TestShardIsHeldOpenWhileANetworkIsBound(t *testing.T) {
	r, cl := newShardBinder(t, newEgressClaim("shard-a"),
		newEgressShard("shard-a", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100"))

	reconcileShard(t, r)

	if !heldOpen(t, cl) {
		t.Error("a shard with a network bound to it is not held open")
	}
}

// A drained shard is released, which is what lets an operator decommission the
// node it runs on.
func TestShardIsReleasedWhenNoNetworkIsBound(t *testing.T) {
	shard := newEgressShard("shard-a", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100")
	shard.Finalizers = []string{cloudv1alpha1.FinalizerEgressShardBinding}
	r, cl := newShardBinder(t, shard)

	reconcileShard(t, r)

	if heldOpen(t, cl) {
		t.Error("a shard no network is bound to is still held open")
	}
}

// A label with no binding behind it is no consumer. A claim that was labelled
// by a pass that then failed must not hold a shard open forever.
func TestShardIsReleasedWhenAClaimHoldsOnlyTheLabel(t *testing.T) {
	claim := newEgressClaim("shard-a")
	claim.Status.ShardRef = nil
	shard := newEgressShard("shard-a", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100")
	shard.Finalizers = []string{cloudv1alpha1.FinalizerEgressShardBinding}
	r, cl := newShardBinder(t, claim, shard)

	reconcileShard(t, r)

	if heldOpen(t, cl) {
		t.Error("a label with no binding behind it held a shard open")
	}
}

// A claim naming a shard of the same name in another namespace is another
// cell's business, not a consumer of this one.
func TestShardIgnoresAClaimBoundElsewhere(t *testing.T) {
	claim := newEgressClaim("shard-a")
	claim.Status.ShardRef.Namespace = "some-other-namespace"
	shard := newEgressShard("shard-a", egressTestNode, "2001:db8:ff01::", "2001:db8:f00d::100")
	shard.Finalizers = []string{cloudv1alpha1.FinalizerEgressShardBinding}
	r, cl := newShardBinder(t, claim, shard)

	reconcileShard(t, r)

	if heldOpen(t, cl) {
		t.Error("a claim bound to another namespace's shard held this one open")
	}
}

// The mapping that makes the last claim's deletion release its shard. A claim
// with no binding maps to nothing, because it was in no consumer set.
func TestEgressShardForClaim(t *testing.T) {
	bound := newEgressClaim("shard-a")
	requests := egressShardForClaim(t.Context(), bound)
	if len(requests) != 1 {
		t.Fatalf("requests: got %d, want 1", len(requests))
	}
	if requests[0].Name != "shard-a" || requests[0].Namespace != egressShardNamespace {
		t.Errorf("got %v, want the bound shard's key", requests[0].NamespacedName)
	}

	if requests := egressShardForClaim(t.Context(), newEgressClaim("")); len(requests) != 0 {
		t.Errorf("an unbound claim mapped to %v", requests)
	}
}
