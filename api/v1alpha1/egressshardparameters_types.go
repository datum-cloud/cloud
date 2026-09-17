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

// KindEgressShardParameters is the kind an InternetEgressClass names in its
// parametersRef to be served by this controller.
//
// The reference is opaque to everything that carries it: the class states a
// group, a kind and a name, and no component between the class and this
// controller reads them. This controller answers only for its own group and
// this kind, and ignores a class whose parameters some other implementation
// owns, so two implementations can serve two classes in the same cell.
const KindEgressShardParameters = "EgressShardParameters"

// EgressShardParametersSpec selects the egress shards serving a class.
type EgressShardParametersSpec struct {
	// ShardNamespace is the namespace holding the EgressShard objects this
	// selector may match.
	//
	// It is required and there is no cluster-wide search. A selector evaluated
	// over every namespace would match an EgressShard a tenant created in a
	// namespace they write to, which is a tenant naming the node their own
	// traffic — and everyone else's on the same class — leaves the platform
	// through. Naming the one namespace an operator owns keeps that
	// unreachable.
	//
	// It carries no default even though every deployment today answers
	// galactic-system, which is where the galactic data plane's own objects
	// live. The namespace names the nodes that every network on this class
	// leaves the platform through, and that is worth an operator stating.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ShardNamespace string `json:"shardNamespace"`

	// ShardSelector selects the EgressShards a network on this class egresses
	// through, by the network.datumapis.com/egress-* labels an operator sets
	// on them.
	//
	// An empty selector matches every shard in the namespace, which sends a
	// consumer's traffic out of an arbitrary cell. Egress is realized per
	// cell, so a selector is expected to pin a cell and a pool.
	//
	// The selector runs one way, as the only binding between a class and the
	// shards serving it: a shard names nothing that selects it, which is what
	// keeps the data-plane API group independent of the consumer-facing one.
	//
	// +kubebuilder:validation:Required
	ShardSelector metav1.LabelSelector `json:"shardSelector"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Shard Namespace",type="string",JSONPath=".spec.shardNamespace"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// EgressShardParameters is the configuration this controller reads when an
// InternetEgressClass names it, and it holds which egress shards serve the
// networks that class places in this cell.
//
// It is cluster-scoped because the reference that reaches it carries no
// namespace: a class is cluster-scoped and its parametersRef states a group, a
// kind and a name only, so a namespaced parameters object would be
// unresolvable from the class that names it. The content is an operator's
// statement about the cell's own data plane rather than anything belonging to
// one tenant, and every tenant namespace resolves the same answer from it.
//
// This object is written by an operator. No consumer reads or writes one, and
// a consumer names a class, never these parameters.
type EgressShardParameters struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the whole of this object. There is no status: nothing reconciles
	// these parameters, and the result of applying them is reported on the
	// network context whose egress they served.
	Spec EgressShardParametersSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// EgressShardParametersList contains a list of EgressShardParameters.
type EgressShardParametersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressShardParameters `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EgressShardParameters{}, &EgressShardParametersList{})
}
