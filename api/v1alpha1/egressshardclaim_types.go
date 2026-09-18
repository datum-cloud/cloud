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

// LabelEgressShardClaimShard names the shard a claim is bound to.
//
// It is what makes a shard's consumer set a list rather than a number anyone
// has to keep in step: a shard holds no list of the networks it serves, so
// "which networks does this shard serve" is answered by listing claims
// carrying this label, the same way the network and location labels make
// counting a presence's consumers a list query.
//
// The value is the shard's name. The binding itself lives on the claim's
// status, which is what a reader trusts; this label narrows the query that
// finds the claims to ask.
const LabelEgressShardClaimShard = "cloud.datumapis.com/egress-shard"

// FinalizerEgressShardBinding is the one piece of state a binder adds to a
// shard, held while any claim is bound to it.
//
// It exists so that decommissioning a shard is an act someone takes rather
// than an outcome networks discover. Deleting a shard that is translating
// strands the return traffic of every flow on it, and nothing rebinds a claim:
// a binding is decided once, so a network whose shard vanished has no egress
// and no second answer coming.
const FinalizerEgressShardBinding = "cloud.datumapis.com/egress-shard-binding"

// EgressSharing is how many networks may share one egress shard. It is the
// serving class's sharing, copied verbatim and recorded as the fact this
// binding was made under.
//
// Nothing branches on it. Every claim binds a shared shard, because dedicated
// capacity is a hand-commissioned shard node that no controller can grow while
// nothing allocates the identifier a shard is unusable without — a claim beyond
// that count would wait indefinitely. The value is carried because the
// projection writes it and a claim records what it was created from.
//
// +kubebuilder:validation:Enum=Shared;Dedicated
type EgressSharing string

const (
	// EgressSharingShared lets many networks bind one shard and therefore
	// leave the platform on one address.
	EgressSharingShared EgressSharing = "Shared"

	// EgressSharingDedicated would let exactly one network bind a shard, which
	// is what makes that shard's address the network's own. It is not offered
	// yet and nothing here enforces it; the value is defined so that a claim
	// written when it is offered means today what it will mean then.
	EgressSharingDedicated EgressSharing = "Dedicated"
)

// EgressShardClaimSpec is the network being bound to an egress shard, and the
// terms the binding has to satisfy.
//
// Every field is already resolved upstream and copied here verbatim. Nothing
// reading a claim selects a class, picks a default, or interprets a class's
// parameters.
//
// The whole spec is immutable. The binding is decided once from these facts
// and never recomputed, so a fact that moved underneath it would describe a
// binding that was never made under it. A consumer changing what they asked
// for is a claim deleted and a new one written, which is a crossing someone
// can see.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; a binding is decided once, so delete the claim to ask for different terms"
type EgressShardClaimSpec struct {
	// Network is the network reaching the internet.
	// +required
	Network NetworkRef `json:"network"`

	// NetworkContext is that network's presence in this cell, which is what
	// makes the claim one per location.
	// +required
	NetworkContext NetworkContextRef `json:"networkContext"`

	// ClassName is the InternetEgressClass resolved for this network. It is
	// recorded rather than read: the class is cluster-scoped upstream and no
	// copy of it reaches this cell.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	ClassName string `json:"className"`

	// Sharing is how many networks that class allows on one shard.
	// +required
	Sharing EgressSharing `json:"sharing"`

	// Families are the destination address families this binding has to reach,
	// so the shard it binds is one that translates them.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +required
	Families []InternetEgressAddressFamily `json:"families"`
}

// NetworkRef references a networking.datumapis.com Network by name.
type NetworkRef struct {
	// Name of the Network.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// NetworkContextRef references a networking.datumapis.com NetworkContext by
// name in the same namespace.
type NetworkContextRef struct {
	// Name of the NetworkContext.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// EgressShardReference names one egress shard.
type EgressShardReference struct {
	// Namespace holding the shard. It is stated rather than assumed: the
	// shards are in the namespace an operator gave the serving class, which is
	// not the namespace a claim lives in.
	// +kubebuilder:validation:MinLength=1
	// +required
	Namespace string `json:"namespace"`

	// Name of the shard.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// EgressShardClaimStatus is the binding.
//
// The binding is recorded here and nowhere else. The shard side carries no
// reference back, unlike the interface and subnet claims this follows in every
// other respect: both of those are strictly one-to-one and the reference on the
// provisioned object is what enforces it, whereas many networks share one
// shard, so a shard-side reference would have to be a list of the networks
// served — which is the state a shard deliberately does not hold.
type EgressShardClaimStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ShardRef is the shard this network egresses through.
	//
	// Absent means no shard is bound, which is what a location whose cell holds
	// no usable shard reads. Nothing publishes an address or a route in that
	// state: an address a consumer might allow-list is withheld until the
	// platform can state which one their packets leave on.
	//
	// Present is permanent for this claim's life. It is written once, and
	// nothing recomputes it: a rebinding would move a live VPC's egress to a
	// different source address, which is the value a consumer allow-listed at
	// their destination.
	// +optional
	ShardRef *EgressShardReference `json:"shardRef,omitempty"`
}

// Reasons reported on an EgressShardClaim's Ready condition.
const (
	// EgressShardClaimReasonBound means this network egresses through the
	// shard status names.
	EgressShardClaimReasonBound = "Bound"

	// EgressShardClaimReasonParametersUnavailable means the parameters the
	// serving class names do not exist in this cell, so which shards serve the
	// class is unknown here.
	EgressShardClaimReasonParametersUnavailable = "ParametersUnavailable"

	// EgressShardClaimReasonNoShardMatchesTheClass means no shard in the
	// namespace the class names carries the labels its selector requires.
	EgressShardClaimReasonNoShardMatchesTheClass = "NoShardMatchesTheClass"

	// EgressShardClaimReasonNoShardIdentifier means every shard the class
	// selects is still without the SRv6 identifier a node routes toward, so
	// there is nothing to bind that would carry a packet.
	EgressShardClaimReasonNoShardIdentifier = "NoShardIdentifier"

	// EgressShardClaimReasonShardMissing means the bound shard no longer
	// exists. Nothing rebinds a claim, so this network has no egress and no
	// second answer coming; the finalizer is what makes the state reachable
	// only by someone removing it.
	EgressShardClaimReasonShardMissing = "ShardMissing"

	// EgressShardClaimReasonShardTerminating means the bound shard is being
	// deleted. The binding stands, because nothing rebinds a claim, and the
	// shard is held until the claim is gone.
	EgressShardClaimReasonShardTerminating = "ShardTerminating"

	// EgressShardClaimReasonTermsChanged means the egress this location is
	// instructed to provide no longer matches the terms this binding was made
	// under. The binding stands and delivers what it always did; changing the
	// terms means deleting the claim.
	EgressShardClaimReasonTermsChanged = "TermsChanged"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Network",type="string",JSONPath=".spec.network.name"
// +kubebuilder:printcolumn:name="Sharing",type="string",JSONPath=".spec.sharing"
// +kubebuilder:printcolumn:name="Shard",type="string",JSONPath=".status.shardRef.name"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// EgressShardClaim is one network being bound to one egress shard in this cell.
//
// There is one claim per network context that declares egress — not one per
// interface and not one per attachment. The binding has to outlive the
// workloads using it: an instance is replaced routinely, and a binding that
// followed an attachment would move a network's source address every time that
// happened, which is the address a consumer allow-listed at their destination.
//
// The claim names no shard, no selector, no address and no pool. It states what
// the network needs and the cell answers with which shard serves it, the same
// division a subnet claim makes.
type EgressShardClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata. The name is the network context's
	// own, because there is exactly one claim per context.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the network being bound and the terms the binding satisfies
	// +required
	Spec EgressShardClaimSpec `json:"spec"`

	// status is the binding
	// +optional
	Status EgressShardClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EgressShardClaimList contains a list of EgressShardClaim.
type EgressShardClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressShardClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EgressShardClaim{}, &EgressShardClaimList{})
}
