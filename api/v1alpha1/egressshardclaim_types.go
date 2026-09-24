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
// has to keep in step: a shard holds no list of the attachments it serves, so
// "what does this shard serve" is answered by listing claims carrying this
// label. The value is the shard's name. The binding itself lives on the claim's
// status, which is what a reader trusts; this label narrows the query that
// finds the claims to ask.
const LabelEgressShardClaimShard = "cloud.datumapis.com/egress-shard"

// FinalizerEgressShardBinding is the one piece of state a binder adds to a
// shard, held while any claim is bound to it.
//
// It exists so that decommissioning a shard is an act someone takes rather
// than an outcome instances discover. Deleting a shard that is translating
// strands the return traffic of every flow on it.
const FinalizerEgressShardBinding = "cloud.datumapis.com/egress-shard-binding"

// EgressShardClaimSpec is one attachment being recorded against the egress
// shard on its node.
//
// The claim decides nothing. The node already routes toward its own shard from
// the moment the attachment exists; the claim records which shard that is, so
// the binding is readable, so a node without a usable shard produces a
// condition a consumer can see, and so a later tier that does select among
// shards binds through the same object.
//
// The whole spec is immutable. An attachment that lands on a different node is
// a different record, so the claim is replaced rather than edited.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; an attachment that moved nodes gets a new claim"
type EgressShardClaimSpec struct {
	// Attachment is the attachment this claim records egress for. The claim
	// carries the attachment's name and namespace, so the two are read by one
	// key.
	// +required
	Attachment AttachmentRef `json:"attachment"`

	// NodeName is the node the attachment landed on, and therefore the node
	// whose shard serves it.
	// +kubebuilder:validation:MinLength=1
	// +required
	NodeName string `json:"nodeName"`

	// Families are the destination address families the network declared,
	// so the shard on the node is one that translates them.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +required
	Families []InternetEgressAddressFamily `json:"families"`
}

// AttachmentRef references a VPCAttachment by name.
type AttachmentRef struct {
	// Name of the VPCAttachment.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// EgressShardReference names the shard an attachment egresses through.
type EgressShardReference struct {
	// Namespace of the EgressShard.
	// +kubebuilder:validation:MinLength=1
	// +required
	Namespace string `json:"namespace"`

	// Name of the EgressShard.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// EgressShardClaimStatus is the shard an attachment was recorded against.
type EgressShardClaimStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ShardRef is the shard on the attachment's node.
	//
	// Absent means the node holds no shard this claim can record, which is
	// what an attachment on a node an operator has not commissioned reads.
	// +optional
	ShardRef *EgressShardReference `json:"shardRef,omitempty"`
}

// Reasons reported on an EgressShardClaim's Ready condition.
const (
	// EgressShardClaimReasonBound means this attachment egresses through the
	// shard status names.
	EgressShardClaimReasonBound = "Bound"

	// EgressShardClaimReasonNoShardOnNode means no shard names the node the
	// attachment landed on.
	EgressShardClaimReasonNoShardOnNode = "NoShardOnNode"

	// EgressShardClaimReasonShardNotReady means the shard on the node has not
	// reported the identifier a node routes toward.
	EgressShardClaimReasonShardNotReady = "ShardNotReady"

	// EgressShardClaimReasonShardMismatch means the shard's spec and the
	// identity its process reported disagree, so which one the node runs is
	// unknown and nothing is recorded against it.
	EgressShardClaimReasonShardMismatch = "ShardMismatch"

	// EgressShardClaimReasonFamilyUnsupported means the shard on the node
	// translates none of a family the network declared.
	EgressShardClaimReasonFamilyUnsupported = "FamilyUnsupported"

	// EgressShardClaimReasonShardMissing means the recorded shard no longer
	// exists. The node's instances lost their egress with it.
	EgressShardClaimReasonShardMissing = "ShardMissing"

	// EgressShardClaimReasonShardTerminating means the recorded shard is being
	// deleted. The record stands, and the shard is held until the claim goes.
	EgressShardClaimReasonShardTerminating = "ShardTerminating"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Attachment",type="string",JSONPath=".spec.attachment.name"
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Shard",type="string",JSONPath=".status.shardRef.name"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// EgressShardClaim records one attachment's egress shard: the shard on the
// node the attachment landed on.
//
// There is one claim per attachment, owned by it, so an attachment that goes
// takes its record with it. The claim names no selector, no address and no
// pool: the node is the binding, and the claim writes it down.
type EgressShardClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec EgressShardClaimSpec `json:"spec"`

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
