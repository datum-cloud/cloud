# API Reference

## Packages
- [cloud.datumapis.com/v1alpha1](#clouddatumapiscomv1alpha1)


## cloud.datumapis.com/v1alpha1

Package v1alpha1 contains API Schema definitions for the cloud.datumapis.com/v1alpha1 API group.

### Resource Types
- [EgressShardParameters](#egressshardparameters)
- [EgressShardPool](#egressshardpool)
- [NetworkFabricIdentity](#networkfabricidentity)
- [VPC](#vpc)
- [VPCAttachment](#vpcattachment)



#### EgressShardParameters



EgressShardParameters is the configuration this controller reads when an
InternetEgressClass names it, and it holds which egress shards serve the
networks that class places in this cell.

It is cluster-scoped because the reference that reaches it carries no
namespace: a class is cluster-scoped and its parametersRef states a group, a
kind and a name only, so a namespaced parameters object would be
unresolvable from the class that names it. The content is an operator's
statement about the cell's own data plane rather than anything belonging to
one tenant, and every tenant namespace resolves the same answer from it.

This object is written by an operator. No consumer reads or writes one, and
a consumer names a class, never these parameters.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cloud.datumapis.com/v1alpha1` | | |
| `kind` _string_ | `EgressShardParameters` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EgressShardParametersSpec](#egressshardparametersspec)_ | Spec is the whole of this object. There is no status: nothing reconciles<br />these parameters, and the result of applying them is reported on the<br />network context whose egress they served. |  |  |


#### EgressShardParametersSpec



EgressShardParametersSpec selects the egress shards serving a class.



_Appears in:_
- [EgressShardParameters](#egressshardparameters)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `shardNamespace` _string_ | ShardNamespace is the namespace holding the EgressShard objects this<br />selector may match.<br />It is required and there is no cluster-wide search. A selector evaluated<br />over every namespace would match an EgressShard a tenant created in a<br />namespace they write to, which is a tenant naming the node their own<br />traffic — and everyone else's on the same class — leaves the platform<br />through. Naming the one namespace an operator owns keeps that<br />unreachable.<br />It carries no default even though every deployment today answers<br />galactic-system, which is where the galactic data plane's own objects<br />live. The namespace names the nodes that every network on this class<br />leaves the platform through, and that is worth an operator stating. |  | MaxLength: 63 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `shardSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#labelselector-v1-meta)_ | ShardSelector selects the EgressShards a network on this class egresses<br />through, by the network.datumapis.com/egress-* labels an operator sets<br />on them.<br />An empty selector matches every shard in the namespace, which sends a<br />consumer's traffic out of an arbitrary cell. Egress is realized per<br />cell, so a selector is expected to pin a cell and a pool.<br />The selector runs one way, as the only binding between a class and the<br />shards serving it: a shard names nothing that selects it, which is what<br />keeps the data-plane API group independent of the consumer-facing one. |  | Required: \{\} <br /> |


#### EgressShardPool



EgressShardPool is an operator's declaration of the egress shards one cell
has. The builder expands it into one EgressShard per selected node, which is
the object a class's selector then matches.

It replaces a hand-written shard object per node and nothing more. It does
not provision on demand: a shard is unusable until its SRv6 identifier
exists, that identifier is still operator-supplied process configuration on
the node with no allocator behind it, and a shard created in response to a
network's demand would therefore attach, translate nothing, and be skipped by
the very selector meant to find it. Commissioning a node stays an operator's
act; this only removes the YAML that act used to require.

It is cluster-scoped like EgressShardParameters: the content is an operator's
statement about the cell's own data plane rather than anything belonging to
one tenant, and no consumer reads or writes one.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cloud.datumapis.com/v1alpha1` | | |
| `kind` _string_ | `EgressShardPool` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EgressShardPoolSpec](#egressshardpoolspec)_ | spec declares the shards this pool has |  |  |
| `status` _[EgressShardPoolStatus](#egressshardpoolstatus)_ | status reports what this pool built |  |  |


#### EgressShardPoolSpec



EgressShardPoolSpec declares the egress shards a cell has, by naming the
nodes that translate for it.



_Appears in:_
- [EgressShardPool](#egressshardpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `shardNamespace` _string_ | ShardNamespace is the namespace the shards this pool builds live in.<br />It is required, for the same reason EgressShardParameters requires one:<br />the namespace names the nodes that every network on a class leaves the<br />platform through, which is worth an operator stating rather than<br />defaulting to wherever the data plane happens to keep its objects. |  | MaxLength: 63 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `nodeSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#labelselector-v1-meta)_ | NodeSelector selects the nodes this pool builds a shard for. Reach for<br />LabelNodeEgressPool; its doc comment explains why the label a translating<br />node already carries cannot be used here.<br />An empty selector is refused rather than treated as "every node". A<br />selector that matched every node would build a shard per node, which<br />makes the egress address per-node and dissolves the shared address. |  | Required: \{\} <br /> |
| `shardLabels` _object (keys:string, values:string)_ | ShardLabels are stamped on each shard this pool builds, on top of the<br />pool label the builder always writes.<br />They are what a class's shard selector matches, so a pool that stamps no<br />cell is selected together with another cell's shards. The family labels<br />belong to whoever assigns the addresses and are deliberately not settable<br />here: they restate an assignment this pool does not make, and a pool that<br />claimed a family its shards hold no address for would be selected for<br />traffic that then translates nothing. |  | MaxProperties: 16 <br />Optional: \{\} <br /> |


#### EgressShardPoolStatus



EgressShardPoolStatus reports what this pool built.

It reports no shard names and no counts of the networks using them. Which
shards a pool built is answered by listing the pool label in the shard
namespace, and which networks a shard serves by listing the claims bound to
it — neither is a number stored here to fall out of step.



_Appears in:_
- [EgressShardPool](#egressshardpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ |  |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ |  |  |  |


#### IPAddress

_Underlying type:_ _string_

IPAddress is an IPv4 or IPv6 address with CIDR notation.

_Validation:_
- MaxLength: 64

_Appears in:_
- [VPCAttachmentInterface](#vpcattachmentinterface)



#### InternetEgressAddressFamily

_Underlying type:_ _string_

InternetEgressAddressFamily is the address family of an egress source
address.

Only IPv6 is reported. Reaching an IPv4 destination needs a resolver and a
translator sharing a prefix, which the platform pairs neither of, so the
value is withheld rather than reported and not delivered. An address written
today records IPv6, so accepting IPv4 later changes no attachment.

_Validation:_
- Enum: [IPv6]

_Appears in:_
- [InternetEgressSourceAddress](#internetegresssourceaddress)

| Field | Description |
| --- | --- |
| `IPv6` |  |


#### InternetEgressAddressStability

_Underlying type:_ _string_

InternetEgressAddressStability is how far a consumer may rely on an egress
source address. It is the consumer-side projection of the serving class's
sharing, derived here so a consumer never reads a class.

_Validation:_
- Enum: [None Network]

_Appears in:_
- [InternetEgressSourceAddress](#internetegresssourceaddress)

| Field | Description |
| --- | --- |
| `None` | InternetEgressAddressStabilityNone means the address may change and<br />other networks share it. Allow-listing it admits traffic from other<br />networks and loses access when the address changes.<br /> |
| `Network` | InternetEgressAddressStabilityNetwork means the address belongs to this<br />network and persists. Allow-listing it is safe.<br /> |


#### InternetEgressSourceAddress



InternetEgressSourceAddress is one address outbound traffic leaves on.



_Appears in:_
- [VPCAttachmentInternetEgressStatus](#vpcattachmentinternetegressstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `family` _[InternetEgressAddressFamily](#internetegressaddressfamily)_ | Family is the address family of this source address. |  | Enum: [IPv6] <br /> |
| `address` _string_ | Address is the source address translation writes, without a prefix<br />length. |  | MaxLength: 39 <br />MinLength: 1 <br /> |
| `stability` _[InternetEgressAddressStability](#internetegressaddressstability)_ | Stability states how far a consumer may rely on this address before<br />they act on it. |  | Enum: [None Network] <br /> |


#### Network

_Underlying type:_ _string_

Network is an IPv4 or IPv6 CIDR block (e.g., "10.0.0.0/24").

_Validation:_
- MaxLength: 64

_Appears in:_
- [VPCSpec](#vpcspec)



#### NetworkFabricIdentity



NetworkFabricIdentity tells a location what identity the fabric knows a
network by.

There is one per network, not one per location. A VPC is the network's
realization at a single location and takes its identity from here, which is
what makes the locations of one network the same network on the fabric
instead of unrelated ones that happen to share a name.

This is platform-internal. It is written centrally and carried to the cells
where the network is required; it never appears in a project control plane
and no consumer reads or writes one. The identity is a value the fabric acts
on directly, so it is kept to the platform rather than published beside the
network it belongs to.

This object is managed for you. It follows the Network it was allocated for.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cloud.datumapis.com/v1alpha1` | | |
| `kind` _string_ | `NetworkFabricIdentity` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[NetworkFabricIdentitySpec](#networkfabricidentityspec)_ | Spec is the whole of this object. There is no status: federation carries<br />configuration to a cell and deliberately does not carry status, so<br />anything a cell has to read has to be here. |  |  |


#### NetworkFabricIdentityNetworkRef



NetworkFabricIdentityNetworkRef identifies the network an identity was
allocated for.



_Appears in:_
- [NetworkFabricIdentitySpec](#networkfabricidentityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the network's name. |  | Required: \{\} <br /> |


#### NetworkFabricIdentitySpec



NetworkFabricIdentitySpec carries the identity the fabric knows one network
by.



_Appears in:_
- [NetworkFabricIdentity](#networkfabricidentity)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `identity` _integer_ | Identity is what the fabric knows the network by, the same in every<br />location the network reaches. The Route Target is derived from it, which<br />is what makes two locations of one network import each other's routes<br />rather than behave as two networks that share a name. The VRF device is<br />named from it for the same reason.<br />It is an integer rather than an encoded string because a consumer builds<br />`ASN:<identity>` from it and encodes it for its own use. It is 32 bits<br />wide because that is what survives into the Route Target: the fabric<br />truncates, so a wider value would be uniqueness the platform believes it<br />has and the fabric does not.<br />It is never zero and never changes. The fabric embeds it in import policy<br />in every location the network reaches, so a network that changed identity<br />would be a different network to everything already carrying its traffic. |  | Maximum: 4.294967295e+09 <br />Minimum: 1 <br />Required: \{\} <br /> |
| `networkRef` _[NetworkFabricIdentityNetworkRef](#networkfabricidentitynetworkref)_ | NetworkRef names the network this identity belongs to.<br />It carries a name and no UID, deliberately. The identity is a permanent<br />property of a name in a namespace, not of one object's lifetime: a<br />network deleted and recreated under the same name inherits it. A UID here<br />would document the opposite of the rule. |  | Required: \{\} <br /> |


#### NetworkInterfaceRef



NetworkInterfaceRef references a networking.datumapis.com NetworkInterface in
the same namespace.



_Appears in:_
- [VPCAttachmentSpec](#vpcattachmentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the NetworkInterface. |  | MinLength: 1 <br /> |


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


#### VPCAttachmentEgressStatus



VPCAttachmentEgressStatus reports what this attachment reaches outside the
platform.



_Appears in:_
- [VPCAttachmentStatus](#vpcattachmentstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `internet` _[VPCAttachmentInternetEgressStatus](#vpcattachmentinternetegressstatus)_ | Internet is the internet egress realized for this attachment. |  |  |


#### VPCAttachmentInterface



VPCAttachmentInterface defines the network interface details.



_Appears in:_
- [VPCAttachmentSpec](#vpcattachmentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the interface (e.g., eth0). |  |  |
| `mode` _[VPCAttachmentInterfaceMode](#vpcattachmentinterfacemode)_ | Mode is how the workload consumes the interface, resolved and written by<br />the attachment controller rather than by whoever runs the workload. | Netns | Enum: [Netns Hypervisor HypervisorDeclared] <br /> |
| `addresses` _[IPAddress](#ipaddress) array_ | A list of IPv4 or IPv6 addresses associated with the interface. Empty when<br />the guest manages its own addressing. |  | MaxItems: 16 <br />MaxLength: 64 <br /> |


#### VPCAttachmentInterfaceMode

_Underlying type:_ _string_

VPCAttachmentInterfaceMode is how the workload consumes the interface. It
describes the guest, not the data plane, so a change of implementation on the
data plane side does not move this API.

_Validation:_
- Enum: [Netns Hypervisor HypervisorDeclared]

_Appears in:_
- [VPCAttachmentInterface](#vpcattachmentinterface)

| Field | Description |
| --- | --- |
| `Netns` | VPCAttachmentInterfaceModeNetns moves the interface into the workload's<br />network namespace, which is what a container consumes.<br /> |
| `Hypervisor` | VPCAttachmentInterfaceModeHypervisor hands the interface to a hypervisor as<br />a device, which is what a virtual machine guest consumes.<br /> |
| `HypervisorDeclared` | VPCAttachmentInterfaceModeHypervisorDeclared also hands the interface to a<br />hypervisor as a device. It differs from Hypervisor in who tells the<br />hypervisor that the device exists. Under Hypervisor the hypervisor finds<br />the device from what the node publishes. Under HypervisorDeclared the data<br />plane states the device, its addresses, and its MTU to the hypervisor<br />directly, which is what a guest whose hypervisor reads no node state<br />needs.<br /> |


#### VPCAttachmentInternetEgressStatus



VPCAttachmentInternetEgressStatus reports the outbound path this attachment
leaves the platform on.



_Appears in:_
- [VPCAttachmentEgressStatus](#vpcattachmentegressstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `sourceAddresses` _[InternetEgressSourceAddress](#internetegresssourceaddress) array_ | SourceAddresses are the addresses translation writes for this<br />attachment, one per family reached.<br />Absent means this attachment reaches nothing outside the platform, or<br />that no address has been reported for a path that does. An absent list<br />is never a placeholder: a consumer that allow-listed a guessed address<br />would admit the wrong traffic and believe otherwise. |  | MaxItems: 2 <br /> |


#### VPCAttachmentSpec



VPCAttachmentSpec defines the desired state of VPCAttachment



_Appears in:_
- [VPCAttachment](#vpcattachment)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vpc` _[VPCRef](#vpcref)_ | VPC this attachment belongs to. |  |  |
| `interfaceRef` _[NetworkInterfaceRef](#networkinterfaceref)_ | NetworkInterface this attachment realizes. |  |  |
| `interface` _[VPCAttachmentInterface](#vpcattachmentinterface)_ | Interface defines the network interface configuration. |  |  |


#### VPCAttachmentStatus



VPCAttachmentStatus defines the observed state of VPCAttachment.

Every field but Conditions is optional: an identifier is recorded before a pod
attaches, and a guest managing its own addressing never reports a subnet.



_Appears in:_
- [VPCAttachment](#vpcattachment)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ |  |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ |  |  |  |
| `vpc` _string_ | Base62-encoded VPC identifier. |  | MaxLength: 16 <br />MinLength: 1 <br /> |
| `vpcAttachment` _string_ | Base62-encoded VPCAttachment identifier. |  | MaxLength: 16 <br />MinLength: 1 <br /> |
| `node` _string_ | Kubernetes node name where the attachment lives. |  | MinLength: 1 <br /> |
| `containerID` _string_ | Full container ID (46 hex characters). |  | MaxLength: 46 <br />MinLength: 46 <br /> |
| `podName` _string_ | Pod name. |  | MinLength: 1 <br /> |
| `hostInterface` _string_ | Host-side veth or tap device name (e.g., "G000000010013H"). |  | MinLength: 1 <br /> |
| `vrfInterface` _string_ | VRF device name, which is per-VPC (e.g., "G000000010V"). |  | MinLength: 1 <br /> |
| `guestInterface` _string_ | Guest-side veth device name (e.g., "G000000010013G"). |  | MinLength: 1 <br /> |
| `podSubnet` _string_ | Allocated subnet in CIDR notation (e.g., "fd00:10:ff01:0:1::/80"). |  | MinLength: 1 <br /> |
| `networkAttachmentDefinition` _string_ | NetworkAttachmentDefinition rendered for this attachment. |  | MinLength: 1 <br /> |
| `egress` _[VPCAttachmentEgressStatus](#vpcattachmentegressstatus)_ | Egress reports what this attachment reaches outside the platform.<br />It is reported per attachment rather than on the network, because the<br />interface is what a workload holds and what a consumer reads back<br />through. This controller is the only component that resolved which shard<br />the network bound to, so it is the only one that can report the answer. |  |  |


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
| `observedGeneration` _integer_ |  |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ |  |  |  |
| `vpc` _string_ | Base62-encoded VPC identifier. |  | MaxLength: 16 <br />MinLength: 1 <br /> |


