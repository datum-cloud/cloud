# API Reference

## Packages
- [cloud.datumapis.com/v1alpha1](#clouddatumapiscomv1alpha1)


## cloud.datumapis.com/v1alpha1

Package v1alpha1 contains API Schema definitions for the cloud.datumapis.com/v1alpha1 API group.

### Resource Types
- [VPC](#vpc)
- [VPCAttachment](#vpcattachment)



#### IPAddress

_Underlying type:_ _string_

IPAddress is an IPv4 or IPv6 address with CIDR notation.

_Validation:_
- MaxLength: 64

_Appears in:_
- [VPCAttachmentInterface](#vpcattachmentinterface)



#### Network

_Underlying type:_ _string_

Network is an IPv4 or IPv6 CIDR block (e.g., "10.0.0.0/24").

_Validation:_
- MaxLength: 64

_Appears in:_
- [VPCSpec](#vpcspec)



#### VPC



VPC represents a virtual private cloud — an isolated Layer 2 domain backed
by one or more CIDR blocks.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cloud.datumapis.com/v1alpha1` | | |
| `kind` _string_ | `VPC` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VPCSpec](#vpcspec)_ | Desired CIDR address space. |  |  |
| `status` _[VPCStatus](#vpcstatus)_ | Controller-observed state. |  |  |


#### VPCAttachment



VPCAttachment is the Schema for the vpcattachments API





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cloud.datumapis.com/v1alpha1` | | |
| `kind` _string_ | `VPCAttachment` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VPCAttachmentSpec](#vpcattachmentspec)_ | spec defines the desired state of VPCAttachment |  |  |
| `status` _[VPCAttachmentStatus](#vpcattachmentstatus)_ | status defines the observed state of VPCAttachment |  |  |


#### VPCAttachmentInterface



VPCAttachmentInterface defines the network interface details.



_Appears in:_
- [VPCAttachmentSpec](#vpcattachmentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the interface (e.g., eth0). |  |  |
| `addresses` _[IPAddress](#ipaddress) array_ | A list of IPv4 or IPv6 addresses associated with the interface. |  | MaxItems: 16 <br />MaxLength: 64 <br />MinItems: 1 <br /> |


#### VPCAttachmentSpec



VPCAttachmentSpec defines the desired state of VPCAttachment



_Appears in:_
- [VPCAttachment](#vpcattachment)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vpc` _[VPCRef](#vpcref)_ | VPC this attachment belongs to. |  |  |
| `interface` _[VPCAttachmentInterface](#vpcattachmentinterface)_ | Interface defines the network interface configuration. |  |  |


#### VPCAttachmentStatus



VPCAttachmentStatus defines the observed state of VPCAttachment.



_Appears in:_
- [VPCAttachment](#vpcattachment)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ready` _boolean_ | Indicates whether the VPCAttachment is ready for use |  |  |
| `identifier` _string_ | A unique identifier assigned to this VPCAttachment |  |  |


#### VPCRef



VPCRef references a VPC by name within the same namespace.



_Appears in:_
- [VPCAttachmentSpec](#vpcattachmentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the VPC. |  | MinLength: 1 <br /> |


#### VPCSpec



VPCSpec defines the desired state of a VPC. It specifies the CIDR address space.



_Appears in:_
- [VPC](#vpc)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `networks` _[Network](#network) array_ | CIDR blocks that form the VPC address space. |  | MaxItems: 64 <br />MaxLength: 64 <br />MinItems: 1 <br /> |


#### VPCStatus



VPCStatus defines the observed state of a VPC, populated by the controller.



_Appears in:_
- [VPC](#vpc)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ready` _boolean_ | True when the VPC is provisioned and ready for attachments. |  |  |
| `identifier` _string_ | Opaque controller-assigned identifier for this VPC. |  |  |


