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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelNodeEgressPool is the label a node carries to be commissioned into an
// egress shard pool, set to the pool's own name.
//
// It exists because the label a translating node already carries is not usable
// for this. galactic-nat is compute's role-differentiating component: every
// compute node runs it unconditionally under
// galactic.datumapis.com/node=compute, and its DaemonSet says in as many words
// that no separate opt-in capability label exists. Selecting on that label
// would build one shard per compute node, which makes the egress address
// per-node and dissolves the shared address the whole model rests on.
//
// A pool's node selector is an ordinary selector and an operator may write any
// other requirement into it. This key is the one to reach for, and the reason
// it has to be a new one.
const LabelNodeEgressPool = "cloud.datumapis.com/egress-pool"

// EgressShardPoolSpec declares the egress shards a cell has, by naming the
// nodes that translate for it.
type EgressShardPoolSpec struct {
	// ShardNamespace is the namespace the shards this pool builds live in.
	//
	// It is required, for the same reason EgressShardParameters requires one:
	// the namespace names the nodes that every network on a class leaves the
	// platform through, which is worth an operator stating rather than
	// defaulting to wherever the data plane happens to keep its objects.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ShardNamespace string `json:"shardNamespace"`

	// NodeSelector selects the nodes this pool builds a shard for. Reach for
	// LabelNodeEgressPool; its doc comment explains why the label a translating
	// node already carries cannot be used here.
	//
	// An empty selector is refused rather than treated as "every node". A
	// selector that matched every node would build a shard per node, which
	// makes the egress address per-node and dissolves the shared address.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="(has(self.matchLabels) && size(self.matchLabels) > 0) || (has(self.matchExpressions) && size(self.matchExpressions) > 0)",message="nodeSelector must state at least one requirement; an empty selector would build a shard on every node"
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// ShardLabels are stamped on each shard this pool builds, on top of the
	// pool label the builder always writes.
	//
	// They are what a class's shard selector matches, so a pool that stamps no
	// cell is selected together with another cell's shards. The family labels
	// belong to whoever assigns the addresses and are deliberately not settable
	// here: they restate an assignment this pool does not make, and a pool that
	// claimed a family its shards hold no address for would be selected for
	// traffic that then translates nothing.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k.startsWith('network.datumapis.com/egress-ipv'))",message="the egress family labels are set by whoever assigns a shard's addresses, not by a pool"
	ShardLabels map[string]string `json:"shardLabels,omitempty"`
}

// EgressShardPoolStatus reports what this pool built.
//
// It reports no shard names and no counts of the networks using them. Which
// shards a pool built is answered by listing the pool label in the shard
// namespace, and which networks a shard serves by listing the claims bound to
// it — neither is a number stored here to fall out of step.
type EgressShardPoolStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	// EgressShardPoolReasonShardsBuilt means every node this pool selects has a
	// shard object.
	EgressShardPoolReasonShardsBuilt = "ShardsBuilt"

	// EgressShardPoolReasonNoNodeSelected means the selector matched no node,
	// so this pool builds nothing. It is reported rather than logged: a pool
	// whose label an operator never set on a node looks exactly like a pool
	// that is working until a network asks to egress through it.
	EgressShardPoolReasonNoNodeSelected = "NoNodeSelected"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Shard Namespace",type="string",JSONPath=".spec.shardNamespace"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// EgressShardPool is an operator's declaration of the egress shards one cell
// has. The builder expands it into one EgressShard per selected node, which is
// the object a class's selector then matches.
//
// It replaces a hand-written shard object per node and nothing more. It does
// not provision on demand: a shard is unusable until its SRv6 identifier
// exists, that identifier is still operator-supplied process configuration on
// the node with no allocator behind it, and a shard created in response to a
// network's demand would therefore attach, translate nothing, and be skipped by
// the very selector meant to find it. Commissioning a node stays an operator's
// act; this only removes the YAML that act used to require.
//
// It is cluster-scoped like EgressShardParameters: the content is an operator's
// statement about the cell's own data plane rather than anything belonging to
// one tenant, and no consumer reads or writes one.
type EgressShardPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata. The name is stamped on every
	// shard this pool builds as the pool label's value, so it is what a class's
	// selector names.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec declares the shards this pool has
	// +required
	Spec EgressShardPoolSpec `json:"spec"`

	// status reports what this pool built
	// +optional
	Status EgressShardPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EgressShardPoolList contains a list of EgressShardPool.
type EgressShardPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressShardPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EgressShardPool{}, &EgressShardPoolList{})
}
