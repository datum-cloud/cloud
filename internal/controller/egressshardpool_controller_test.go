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
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

const egressPoolName = "shared"

// newPoolReconciler builds a builder over a cell holding the pools and nodes
// given, so a test states only what it is about.
func newPoolReconciler(t *testing.T, objects ...client.Object) (*EgressShardPoolReconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the core scheme: %v", err)
	}
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the cloud scheme: %v", err)
	}
	if err := bgpv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("build the fabric scheme: %v", err)
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&cloudv1alpha1.EgressShardPool{}).Build()
	return &EgressShardPoolReconciler{Client: fakeClient, Scheme: scheme}, fakeClient
}

// newEgressPool is a cell's pool declaration: the namespace its shards live in,
// the nodes an operator commissioned, and the labels a class selects on.
func newEgressPool() *cloudv1alpha1.EgressShardPool {
	pool := &cloudv1alpha1.EgressShardPool{}
	pool.Name = egressPoolName
	pool.Spec.ShardNamespace = egressShardNamespace
	pool.Spec.NodeSelector = metav1.LabelSelector{MatchLabels: map[string]string{
		cloudv1alpha1.LabelNodeEgressPool: egressPoolName,
	}}
	pool.Spec.ShardLabels = map[string]string{
		bgpv1alpha1.LabelEgressShardCell: "us-central-1",
	}
	return pool
}

// newNode is a node carrying whatever labels a test gives it.
func newNode(name string, nodeLabels map[string]string) *corev1.Node {
	node := &corev1.Node{}
	node.Name = name
	node.Labels = nodeLabels
	return node
}

// commissionedNode is a node an operator opted into the pool.
func commissionedNode(name string) *corev1.Node {
	return newNode(name, map[string]string{
		cloudv1alpha1.LabelNodeEgressPool: egressPoolName,
	})
}

func reconcilePool(t *testing.T, r *EgressShardPoolReconciler) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(),
		ctrl.Request{NamespacedName: client.ObjectKey{Name: egressPoolName}}); err != nil {
		t.Fatalf("reconcile the pool: %v", err)
	}
}

func shardNames(t *testing.T, cl client.Client) []string {
	t.Helper()
	var shards bgpv1alpha1.EgressShardList
	if err := cl.List(t.Context(), &shards); err != nil {
		t.Fatalf("list shards: %v", err)
	}
	names := make([]string, 0, len(shards.Items))
	for i := range shards.Items {
		names = append(names, shards.Items[i].Name)
	}
	slices.Sort(names)
	return names
}

func TestEgressShardPoolBuildsAShardPerCommissionedNode(t *testing.T) {
	r, cl := newPoolReconciler(t, newEgressPool(),
		commissionedNode("node-b"), commissionedNode("node-a"))

	reconcilePool(t, r)

	want := []string{"shared-node-a", "shared-node-b"}
	if got := shardNames(t, cl); !slices.Equal(got, want) {
		t.Fatalf("shards: got %v, want %v", got, want)
	}

	var shard bgpv1alpha1.EgressShard
	key := client.ObjectKey{Namespace: egressShardNamespace, Name: "shared-node-a"}
	if err := cl.Get(t.Context(), key, &shard); err != nil {
		t.Fatalf("get the built shard: %v", err)
	}
	if shard.Spec.TargetRef.Kind != "Node" || shard.Spec.TargetRef.Name != "node-a" {
		t.Errorf("target: got %v, want Node/node-a", shard.Spec.TargetRef)
	}
	if got := shard.Labels[bgpv1alpha1.LabelEgressShardPool]; got != egressPoolName {
		t.Errorf("pool label: got %q, want %q", got, egressPoolName)
	}
	if got := shard.Labels[bgpv1alpha1.LabelEgressShardCell]; got != "us-central-1" {
		t.Errorf("cell label: got %q, want us-central-1", got)
	}
	// The address is claimed by the controller holding the addressing-service
	// credential. A pool that wrote one would be a second writer for a
	// write-once field.
	if shard.Spec.ShardAddressIPv6 != "" {
		t.Errorf("built shard carries address %q, want none", shard.Spec.ShardAddressIPv6)
	}
	// A family label restates an address assignment this pool does not make.
	if _, set := shard.Labels[bgpv1alpha1.LabelEgressShardIPv6]; set {
		t.Error("built shard claims to serve IPv6 before it holds an address")
	}
}

// The label every translating node already carries is compute's own role label,
// so selecting on it would build a shard per compute node and make the egress
// address per-node. A node that did not opt in gets nothing.
func TestEgressShardPoolIgnoresAnUncommissionedComputeNode(t *testing.T) {
	r, cl := newPoolReconciler(t, newEgressPool(),
		newNode("compute-1", map[string]string{"galactic.datumapis.com/node": "compute"}),
		commissionedNode("shard-1"),
	)

	reconcilePool(t, r)

	if got := shardNames(t, cl); !slices.Equal(got, []string{"shared-shard-1"}) {
		t.Errorf("shards: got %v, want only the commissioned node's", got)
	}
}

// An object written before the schema refused an empty selector still selects
// every node in the cell, which is the shard-per-node outcome the whole model
// rests on not happening.
func TestEgressShardPoolRefusesAnEmptySelector(t *testing.T) {
	pool := newEgressPool()
	pool.Spec.NodeSelector = metav1.LabelSelector{}
	r, cl := newPoolReconciler(t, pool, commissionedNode("node-a"))

	reconcilePool(t, r)

	if got := shardNames(t, cl); len(got) != 0 {
		t.Errorf("shards: got %v, want none", got)
	}
	assertPoolCondition(t, cl, metav1.ConditionFalse, cloudv1alpha1.EgressShardPoolReasonNoNodeSelected)
}

// A pool whose label an operator never set on a node looks exactly like a
// working pool until a network asks to egress through it.
func TestEgressShardPoolReportsThatItSelectedNoNode(t *testing.T) {
	r, cl := newPoolReconciler(t, newEgressPool())

	reconcilePool(t, r)

	assertPoolCondition(t, cl, metav1.ConditionFalse, cloudv1alpha1.EgressShardPoolReasonNoNodeSelected)
}

func TestEgressShardPoolReportsTheShardsItBuilt(t *testing.T) {
	r, cl := newPoolReconciler(t, newEgressPool(), commissionedNode("node-a"))

	reconcilePool(t, r)

	assertPoolCondition(t, cl, metav1.ConditionTrue, cloudv1alpha1.EgressShardPoolReasonShardsBuilt)
}

// An existing shard is left exactly as it was found. Its address is write-once
// because the datapath claims a reply by exact match against it, and its labels
// are what a class's selector matches, so relabelling one moves live traffic.
func TestEgressShardPoolNeverRewritesAnExistingShard(t *testing.T) {
	existing := &bgpv1alpha1.EgressShard{}
	existing.Namespace = egressShardNamespace
	existing.Name = "shared-node-a"
	existing.Labels = map[string]string{
		bgpv1alpha1.LabelEgressShardPool: "some-other-pool",
		bgpv1alpha1.LabelEgressShardIPv6: bgpv1alpha1.LabelValueEgressFamilyServed,
	}
	existing.Spec.TargetRef = bgpv1alpha1.TargetRef{Kind: "Node", Name: "node-a"}
	existing.Spec.ShardAddressIPv6 = "2001:db8:f00d::100"

	r, cl := newPoolReconciler(t, newEgressPool(), commissionedNode("node-a"), existing)

	reconcilePool(t, r)

	var shard bgpv1alpha1.EgressShard
	key := client.ObjectKey{Namespace: egressShardNamespace, Name: "shared-node-a"}
	if err := cl.Get(t.Context(), key, &shard); err != nil {
		t.Fatalf("get the pre-existing shard: %v", err)
	}
	if shard.Spec.ShardAddressIPv6 != "2001:db8:f00d::100" {
		t.Errorf("address: got %q, want the one it already held", shard.Spec.ShardAddressIPv6)
	}
	if got := shard.Labels[bgpv1alpha1.LabelEgressShardPool]; got != "some-other-pool" {
		t.Errorf("pool label: got %q, want the one it already held", got)
	}
}

// The pool label's value is the pool's own name, so it cannot drift from the
// object that stamped it, and cannot be pointed elsewhere by hand.
func TestEgressShardPoolLabelIsNotOverridable(t *testing.T) {
	pool := newEgressPool()
	pool.Spec.ShardLabels[bgpv1alpha1.LabelEgressShardPool] = "somewhere-else"
	r, cl := newPoolReconciler(t, pool, commissionedNode("node-a"))

	reconcilePool(t, r)

	var shard bgpv1alpha1.EgressShard
	key := client.ObjectKey{Namespace: egressShardNamespace, Name: "shared-node-a"}
	if err := cl.Get(t.Context(), key, &shard); err != nil {
		t.Fatalf("get the built shard: %v", err)
	}
	if got := shard.Labels[bgpv1alpha1.LabelEgressShardPool]; got != egressPoolName {
		t.Errorf("pool label: got %q, want %q", got, egressPoolName)
	}
}

// The shards outlive the declaration that built them: a pool deleted by an
// operator reorganizing their cell must not take the translation of every
// network on it away.
func TestEgressShardPoolBuildsNothingWhileTerminating(t *testing.T) {
	pool := newEgressPool()
	pool.Finalizers = []string{"test.datumapis.com/hold"}
	deletion := metav1.Now()
	pool.DeletionTimestamp = &deletion
	r, cl := newPoolReconciler(t, pool, commissionedNode("node-a"))

	reconcilePool(t, r)

	if got := shardNames(t, cl); len(got) != 0 {
		t.Errorf("shards: got %v, want none", got)
	}
}

func assertPoolCondition(
	t *testing.T, cl client.Client, status metav1.ConditionStatus, reason string,
) {
	t.Helper()
	var pool cloudv1alpha1.EgressShardPool
	if err := cl.Get(t.Context(), client.ObjectKey{Name: egressPoolName}, &pool); err != nil {
		t.Fatalf("get the pool: %v", err)
	}
	condition := meta.FindStatusCondition(pool.Status.Conditions, cloudv1alpha1.ConditionTypeReady)
	if condition == nil {
		t.Fatal("the pool reports no Ready condition")
	}
	if condition.Status != status || condition.Reason != reason {
		t.Errorf("Ready: got %s/%s, want %s/%s", condition.Status, condition.Reason, status, reason)
	}
}
