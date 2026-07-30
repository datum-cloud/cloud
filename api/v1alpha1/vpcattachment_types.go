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

const VPCAttachmentAnnotation = "k8s.v1alpha1.cloud.datumapis.com/vpc-attachment"

// VPCAttachmentSpec defines the desired state of VPCAttachment
//
// +kubebuilder:validation:XValidation:rule="has(self.vpc) && self.vpc.name != ”",message="vpc reference is required"
type VPCAttachmentSpec struct {
	// VPC this attachment belongs to.
	// +required
	VPC VPCRef `json:"vpc"`

	// Interface defines the network interface configuration.
	// +required
	Interface VPCAttachmentInterface `json:"interface"`
}

// VPCRef references a VPC by name within the same namespace.
type VPCRef struct {
	// Name is the name of the VPC.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// IPAddress is an IPv4 or IPv6 address with CIDR notation.
// +kubebuilder:validation:MaxLength=64
type IPAddress string

// VPCAttachmentInterface defines the network interface details.
//
// +kubebuilder:validation:XValidation:rule="self.addresses.all(a, isCIDR(a))",message="each address must be a valid IPv4 or IPv6 CIDR"
type VPCAttachmentInterface struct {
	// Name of the interface (e.g., eth0).
	// +required
	// +default:value="eth0"
	Name string `json:"name"`

	// A list of IPv4 or IPv6 addresses associated with the interface.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +required
	Addresses []IPAddress `json:"addresses"`
}

// VPCAttachmentStatus defines the observed state of VPCAttachment.
type VPCAttachmentStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Base62-encoded VPC identifier.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16
	VPC string `json:"vpc"`

	// Base62-encoded VPCAttachment identifier.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16
	VPCAttachment string `json:"vpcAttachment"`

	// Kubernetes node name where the attachment lives.
	// +kubebuilder:validation:MinLength=1
	Node string `json:"node"`

	// Full container ID (46 hex characters).
	// +kubebuilder:validation:MinLength=46
	// +kubebuilder:validation:MaxLength=46
	ContainerID string `json:"containerID"`

	// Pod name.
	// +kubebuilder:validation:MinLength=1
	PodName string `json:"podName"`

	// Host-side veth device name (e.g., "G000000010010H").
	// +kubebuilder:validation:MinLength=1
	HostInterface string `json:"hostInterface"`

	// VRF device name (e.g., "G000000010010V").
	// +kubebuilder:validation:MinLength=1
	VRFInterface string `json:"vrfInterface"`

	// Guest-side veth device name (e.g., "G000000010010G").
	// +kubebuilder:validation:MinLength=1
	// +optional
	GuestInterface string `json:"guestInterface,omitempty"`

	// Allocated /80 subnet in CIDR notation (e.g., "fd00:10:ff01:0:1::/80").
	// +kubebuilder:validation:MinLength=1
	//
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="podSubnet must be a valid IPv6 CIDR"
	PodSubnet string `json:"podSubnet"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// VPCAttachment is the Schema for the vpcattachments API
type VPCAttachment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of VPCAttachment
	// +required
	Spec VPCAttachmentSpec `json:"spec"`

	// status defines the observed state of VPCAttachment
	// +optional
	Status VPCAttachmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VPCAttachmentList contains a list of VPCAttachments
type VPCAttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VPCAttachment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VPCAttachment{}, &VPCAttachmentList{})
}
