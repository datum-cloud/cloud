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
	"time"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/galactic"
	"go.datum.net/cloud/internal/identifier"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

const (
	// LabelVPC records the base62 VPC identifier a NAD attaches to.
	LabelVPC = "cloud.datumapis.com/vpc"

	// LabelVPCAttachment records the base62 attachment identifier a NAD holds.
	// The NAD is the allocation record for that identifier.
	LabelVPCAttachment = "cloud.datumapis.com/vpc-attachment"

	// AnnotationHostInterface records the host device this attachment will get,
	// on the NAD, at the moment the NAD is written. The name is derived from the
	// VPC and the attachment identifier, so it is known here — well before the
	// CNI ADD that creates the device. A runtime that must name the device when
	// it asks for an interface, rather than learn it from the ADD result, has
	// nowhere else to read it in time. galactic writes the same key during ADD
	// from the same inputs, so the two always agree.
	AnnotationHostInterface = "k8s.v1.cni.cncf.io/host-interface"

	// ConditionTypePrepared reports that the data plane's pre-Pod artifacts exist.
	// Unlike Programmed, which only becomes true at CNI ADD, it is safe to gate
	// Pod creation on. network-services-operator is adding the type in parallel.
	ConditionTypePrepared = "Prepared"
)

// maxIdentifierAttempts bounds the retry loop that draws an unused identifier.
const maxIdentifierAttempts = 100

// NetworkInterfaceReconciler realizes a fulfilled NetworkInterface claim on the
// galactic data plane.
//
// It creates the VPCAttachment and the NetworkAttachmentDefinition in one pass,
// so it already holds every render input and never has to look sideways. Both
// objects are per-interface, which keeps the attachment identifier and the tap
// device name stable across instance replacement, and it publishes the
// annotations a workload must carry so no infrastructure provider has to know
// what a NAD is.
type NetworkInterfaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader bypasses the cache when listing allocated identifiers, so a NAD
	// written moments ago cannot be missed and its identifier reissued.
	APIReader client.Reader

	// AttachmentMode is how guests in this cell consume an interface. It is
	// required configuration standing in for a capability class that does not
	// exist yet, so a cell states what it is rather than defaulting. It covers
	// every interface that states no mode of its own.
	AttachmentMode cloudv1alpha1.VPCAttachmentInterfaceMode
}

// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaceclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaceclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkcontexts,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcs,verbs=get;list;watch
// +kubebuilder:rbac:groups=network.datumapis.com,resources=egressshards,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcattachments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcattachments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;create;update;patch;delete

func (r *NetworkInterfaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var networkInterface networkingv1alpha.NetworkInterface
	if err := r.Get(ctx, req.NamespacedName, &networkInterface); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !networkInterface.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if !claimFulfilled(&networkInterface) {
		return ctrl.Result{}, nil
	}

	// The VPC is named after the NetworkContext it realizes, so one key reads
	// both: the identity the fabric keys on, and the egress intent projected
	// onto this location.
	var vpc cloudv1alpha1.VPC
	vpcKey := types.NamespacedName{
		Namespace: networkInterface.Namespace,
		Name:      networkInterface.Status.NetworkContextRef.Name,
	}
	if err := r.Get(ctx, vpcKey, &vpc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, r.markPrepared(ctx, &networkInterface,
				metav1.ConditionFalse, "AwaitingVPC", fmt.Sprintf("VPC %s does not exist yet", vpcKey.Name))
		}
		return ctrl.Result{}, fmt.Errorf("get VPC %s: %w", vpcKey, err)
	}
	if vpc.Status.VPC == "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.markPrepared(ctx, &networkInterface,
			metav1.ConditionFalse, "AwaitingVPCIdentifier",
			fmt.Sprintf("VPC %s has no identifier yet", vpc.Name))
	}

	var networkContext networkingv1alpha.NetworkContext
	if err := r.Get(ctx, vpcKey, &networkContext); err != nil {
		if apierrors.IsNotFound(err) {
			// The VPC exists only because this context did, so a context that
			// is gone is a network being withdrawn from the cell rather than a
			// race worth rendering through.
			return ctrl.Result{RequeueAfter: 10 * time.Second}, r.markPrepared(ctx, &networkInterface,
				metav1.ConditionFalse, "AwaitingNetworkContext",
				fmt.Sprintf("NetworkContext %s does not exist yet", vpcKey.Name))
		}
		return ctrl.Result{}, fmt.Errorf("get NetworkContext %s: %w", vpcKey, err)
	}

	attachment, err := r.reconcileAttachment(ctx, &networkInterface, &vpc)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Resolved once for the whole pass, after the attachment exists, because
	// the node it reports is what the address is read for. The conflist the
	// node reads and the address a consumer reads back have to be the same
	// answer, and resolving twice could produce two.
	egress, err := r.resolveInternetEgress(ctx, &networkContext, attachment)
	if err != nil {
		return ctrl.Result{}, err
	}
	nad, err := r.reconcileNAD(ctx, attachment, &vpc, &networkInterface, egress)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.publishAttachmentStatus(ctx, attachment, &vpc, nad, egress); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.publishToInterface(ctx, &networkInterface, attachment, &vpc)
}

// claimFulfilled reports whether an interface is bound to a claim and holds
// every address it must carry. Nothing can be rendered before that.
func claimFulfilled(networkInterface *networkingv1alpha.NetworkInterface) bool {
	if networkInterface.Status.Phase != networkingv1alpha.NetworkInterfacePhaseBound {
		return false
	}
	if networkInterface.Status.NetworkContextRef == nil {
		return false
	}
	return meta.IsStatusConditionTrue(networkInterface.Status.Conditions,
		networkingv1alpha.NetworkInterfaceAllocated)
}

// reconcileAttachment creates the VPCAttachment for an interface. The controller
// owns this object, not the infrastructure provider: it is the only component
// that speaks both the workload vocabulary and the data plane's.
func (r *NetworkInterfaceReconciler) reconcileAttachment(
	ctx context.Context, networkInterface *networkingv1alpha.NetworkInterface, vpc *cloudv1alpha1.VPC,
) (*cloudv1alpha1.VPCAttachment, error) {
	attachment := &cloudv1alpha1.VPCAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: networkInterface.Name, Namespace: networkInterface.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, attachment, func() error {
		attachment.Spec.VPC = cloudv1alpha1.VPCRef{Name: vpc.Name}
		attachment.Spec.InterfaceRef = &cloudv1alpha1.NetworkInterfaceRef{Name: networkInterface.Name}
		attachment.Spec.Interface.Name = networkInterface.Spec.InterfaceName
		attachment.Spec.Interface.Mode = r.attachmentMode(networkInterface)
		attachment.Spec.Interface.Addresses = interfaceAddresses(networkInterface)
		return controllerutil.SetControllerReference(networkInterface, attachment, r.Scheme)
	}); err != nil {
		return nil, fmt.Errorf("reconcile VPC attachment %s: %w", attachment.Name, err)
	}
	return attachment, nil
}

// reconcileNAD creates or updates the NAD the attachment owns.
func (r *NetworkInterfaceReconciler) reconcileNAD(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	vpc *cloudv1alpha1.VPC,
	networkInterface *networkingv1alpha.NetworkInterface,
	egress *internetEgress,
) (*nadv1.NetworkAttachmentDefinition, error) {
	nad := &nadv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: attachment.Name, Namespace: attachment.Namespace},
	}

	attachmentID := ""
	if err := r.Get(ctx, client.ObjectKeyFromObject(nad), nad); err == nil {
		attachmentID = nad.Labels[LabelVPCAttachment]
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get NetworkAttachmentDefinition %s: %w", nad.Name, err)
	}
	if attachmentID == "" {
		allocated, err := r.allocateAttachmentIdentifier(ctx, vpc.Status.VPC)
		if err != nil {
			return nil, err
		}
		attachmentID = allocated
	}

	addresses := make([]galactic.Address, 0, len(networkInterface.Spec.Addresses))
	for _, address := range networkInterface.Spec.Addresses {
		addresses = append(addresses, galactic.Address{
			Address: address.Address,
			Gateway: address.Gateway,
		})
	}
	config, err := galactic.ConflistJSON(attachment.Name, masterPlugin(attachment.Spec.Interface.Mode),
		vpc.Status.VPC, attachmentID, networkInterface.Spec.MTU, addresses,
		declaresDevice(attachment.Spec.Interface.Mode), egress.conflist())
	if err != nil {
		return nil, err
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, nad, func() error {
		if nad.Labels == nil {
			nad.Labels = map[string]string{}
		}
		nad.Labels[LabelVPC] = vpc.Status.VPC
		nad.Labels[LabelVPCAttachment] = attachmentID
		if nad.Annotations == nil {
			nad.Annotations = map[string]string{}
		}
		nad.Annotations[AnnotationHostInterface] = galactic.HostInterfaceName(vpc.Status.VPC, attachmentID)
		nad.Spec.Config = config
		return controllerutil.SetControllerReference(attachment, nad, r.Scheme)
	}); err != nil {
		return nil, fmt.Errorf("reconcile NetworkAttachmentDefinition %s: %w", nad.Name, err)
	}
	return nad, nil
}

// attachmentMode resolves how one guest consumes its interface. An interface
// that states a mode carries the workload's own requirement, so it wins. The
// cell-wide mode covers everything else, which is every interface written
// before a workload could state one.
func (r *NetworkInterfaceReconciler) attachmentMode(
	networkInterface *networkingv1alpha.NetworkInterface,
) cloudv1alpha1.VPCAttachmentInterfaceMode {
	switch networkInterface.Spec.AttachmentMode {
	case networkingv1alpha.NetworkInterfaceAttachmentModeNetns:
		return cloudv1alpha1.VPCAttachmentInterfaceModeNetns
	case networkingv1alpha.NetworkInterfaceAttachmentModeHypervisor:
		return cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor
	case networkingv1alpha.NetworkInterfaceAttachmentModeHypervisorDeclared:
		return cloudv1alpha1.VPCAttachmentInterfaceModeHypervisorDeclared
	default:
		return r.AttachmentMode
	}
}

// masterPlugin translates how a guest consumes an interface into the galactic
// binary that realizes it. This is the only place the two vocabularies meet.
func masterPlugin(mode cloudv1alpha1.VPCAttachmentInterfaceMode) string {
	switch mode {
	case cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor,
		cloudv1alpha1.VPCAttachmentInterfaceModeHypervisorDeclared:
		return galactic.PluginTap
	default:
		return galactic.PluginVeth
	}
}

// declaresDevice reports whether the tap plugin describes the device to the
// hypervisor instead of leaving the hypervisor to discover it.
func declaresDevice(mode cloudv1alpha1.VPCAttachmentInterfaceMode) bool {
	return mode == cloudv1alpha1.VPCAttachmentInterfaceModeHypervisorDeclared
}

// interfaceAddresses copies the addresses NSO allocated onto the attachment, so
// the attachment describes itself without a second read.
func interfaceAddresses(networkInterface *networkingv1alpha.NetworkInterface) []cloudv1alpha1.IPAddress {
	addresses := make([]cloudv1alpha1.IPAddress, 0, len(networkInterface.Spec.Addresses))
	for _, address := range networkInterface.Spec.Addresses {
		addresses = append(addresses, cloudv1alpha1.IPAddress(address.Address))
	}
	return addresses
}

// internetEgress is what one attachment's egress intent resolved to: whether
// the node installs a route, and the address a consumer reads back.
//
// A nil internetEgress is a network that reaches nothing outside the platform.
// It is not an empty one: absence is the instruction, in the conflist and on
// the attachment alike.
type internetEgress struct {
	// sourceAddress is what translation writes, read from the shard on the
	// node this attachment landed on. Empty until the attachment reports its
	// node and that node's shard reports an address.
	sourceAddress *cloudv1alpha1.InternetEgressSourceAddress
}

// conflist renders the block the node reads, or nothing. It carries the
// declaration alone: the node routes toward its own shard, so no shard
// identity travels here.
func (e *internetEgress) conflist() *galactic.Egress {
	if e == nil {
		return nil
	}
	return &galactic.Egress{Internet: &galactic.InternetEgress{Mode: galactic.InternetEgressEnabled}}
}

// status renders what a consumer reads back, or nothing. An address the
// platform cannot state is reported as no egress rather than as a guess: a
// consumer allow-listing the wrong address admits the wrong traffic and has no
// way to tell.
func (e *internetEgress) status() *cloudv1alpha1.VPCAttachmentEgressStatus {
	if e == nil || e.sourceAddress == nil {
		return nil
	}
	return &cloudv1alpha1.VPCAttachmentEgressStatus{
		Internet: &cloudv1alpha1.VPCAttachmentInternetEgressStatus{
			SourceAddresses: []cloudv1alpha1.InternetEgressSourceAddress{*e.sourceAddress},
		},
	}
}

// resolveInternetEgress reads the egress this attachment provides: the
// declaration projected onto its network context, and the address of the shard
// on the node it landed on.
//
// Translation runs on the node the instance attached to, so the shard is the
// node's and nothing here selects one. The declaration is what the node reads.
// The address is what the consumer reads, and it is stated only once the
// attachment has reported its node and that node's shard has reported an
// address. A wrong address is worse than an absent one, because a consumer
// allow-lists it at their destination.
func (r *NetworkInterfaceReconciler) resolveInternetEgress(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	attachment *cloudv1alpha1.VPCAttachment,
) (*internetEgress, error) {
	log := logf.FromContext(ctx)

	intent := internetEgressIntent(networkContext)
	if intent == nil {
		// A context written before this field existed carries no intent, which
		// is not the same as a network that reaches nothing. Both render no
		// egress; only this one is worth saying out loud.
		log.V(1).Info("network context carries no projected egress intent",
			"networkContext", networkContext.Name)
		return nil, nil
	}
	if intent.Mode != networkingv1alpha.NetworkInternetEgressEnabled {
		return nil, nil
	}

	resolved := &internetEgress{}
	if attachment.Status.Node == "" {
		return resolved, nil
	}
	shard, err := egressShardOnNode(ctx, r.Client, attachment.Status.Node)
	if err != nil {
		return nil, err
	}
	if shard == nil {
		log.Info("internet egress is enabled but the node serving this attachment has no shard",
			"attachment", attachment.Name, "node", attachment.Status.Node)
		return resolved, nil
	}
	resolved.sourceAddress = sourceAddress(shard)
	if resolved.sourceAddress == nil {
		log.Info("internet egress is enabled but the node's shard reports no source address",
			"attachment", attachment.Name, "node", attachment.Status.Node, "shard", shard.Name)
	}
	return resolved, nil
}

// sourceAddress is what a consumer reads back for the shard on their node.
//
// The address itself is write-once and immutable upstream, so a reported value
// that changes means the instance moved nodes or the shard was replaced, not
// that the platform renumbered a live one. It is shared by every network on the
// node and follows the node, which is what stability None states.
func sourceAddress(shard *bgpv1alpha1.EgressShard) *cloudv1alpha1.InternetEgressSourceAddress {
	if shard == nil || shard.Status.ShardAddressIPv6 == "" {
		return nil
	}
	return &cloudv1alpha1.InternetEgressSourceAddress{
		Family:    cloudv1alpha1.InternetEgressAddressFamilyIPv6,
		Address:   shard.Status.ShardAddressIPv6,
		Stability: cloudv1alpha1.InternetEgressAddressStabilityNone,
	}
}

// internetEgressIntent reads the internet egress a location was instructed to
// provide, or nil when the context carries no instruction at all.
func internetEgressIntent(
	networkContext *networkingv1alpha.NetworkContext,
) *networkingv1alpha.NetworkContextInternetEgress {
	if networkContext.Spec.Egress == nil {
		return nil
	}
	return networkContext.Spec.Egress.Internet
}

// egressShardOnNode is the shard running on a node, or nil when the node has
// none. Shards are listed rather than named because a shard's name is the
// operator's to choose, and the node reference is what ties one to a node. Two
// shards naming one node is an operator error, and the first by name is taken
// so that every attachment on that node computes the same answer.
func egressShardOnNode(
	ctx context.Context, reader client.Reader, node string,
) (*bgpv1alpha1.EgressShard, error) {
	var shards bgpv1alpha1.EgressShardList
	if err := reader.List(ctx, &shards, client.InNamespace(galactic.SystemNamespace)); err != nil {
		return nil, fmt.Errorf("list egress shards: %w", err)
	}
	var found *bgpv1alpha1.EgressShard
	for i := range shards.Items {
		shard := &shards.Items[i]
		if shard.Spec.TargetRef.Name != node {
			continue
		}
		if found == nil || shard.Name < found.Name {
			found = shard
		}
	}
	return found, nil
}

// allocateAttachmentIdentifier draws a random identifier unused within the VPC.
// Random rather than lowest-free, so a freed identifier is not immediately
// reissued while its BGPAdvertisement is still being garbage collected.
func (r *NetworkInterfaceReconciler) allocateAttachmentIdentifier(ctx context.Context, vpc string) (string, error) {
	var nads nadv1.NetworkAttachmentDefinitionList
	if err := r.APIReader.List(ctx, &nads, client.MatchingLabels{LabelVPC: vpc}); err != nil {
		return "", fmt.Errorf("list NetworkAttachmentDefinitions for VPC %s: %w", vpc, err)
	}
	used := make(map[string]struct{}, len(nads.Items))
	for _, nad := range nads.Items {
		if id := nad.Labels[LabelVPCAttachment]; id != "" {
			used[id] = struct{}{}
		}
	}

	for range maxIdentifierAttempts {
		candidate, err := identifier.RandomVPCAttachmentBase62()
		if err != nil {
			return "", err
		}
		if _, taken := used[candidate]; !taken {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no unused attachment identifier found in VPC %s after %d attempts",
		vpc, maxIdentifierAttempts)
}

// publishAttachmentStatus records the allocated identifiers on the attachment,
// and the egress address a consumer reads back through the interface that holds
// it.
//
// Two reconcilers write this status, over disjoint field sets: this one writes
// the identifiers, the attachment definition and now the egress address, and
// the BGPAdvertisement reconciler writes what the node programmed. Both do a
// whole-object Status().Update, which carries the resourceVersion it was read
// at, so a writer working from a copy the other has since superseded is
// rejected with a conflict and retries — it does not overwrite fields it never
// set. Adding a field set to a reconciler that already writes here keeps the
// writer count at two and that property intact.
//
// Server-side apply was considered and rejected. It would have to convert both
// writers to be coherent: a status subresource written by SSA on one side and
// replaced wholesale on the other is worse than either alone, because the
// wholesale writer drops whatever it did not read. Converting both means
// generated apply configurations this repository does not produce, or the
// deprecated unstructured apply path whose single use here is a foreign object.
// That is a change to make deliberately, for the type as a whole, and not as a
// side effect of adding three fields.
func (r *NetworkInterfaceReconciler) publishAttachmentStatus(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	vpc *cloudv1alpha1.VPC,
	nad *nadv1.NetworkAttachmentDefinition,
	egress *internetEgress,
) error {
	attachment.Status.VPC = vpc.Status.VPC
	attachment.Status.VPCAttachment = nad.Labels[LabelVPCAttachment]
	attachment.Status.NetworkAttachmentDefinition = nad.Name
	attachment.Status.Egress = egress.status()
	attachment.Status.ObservedGeneration = attachment.Generation
	meta.SetStatusCondition(&attachment.Status.Conditions, metav1.Condition{
		Type:               cloudv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "AttachmentDefinitionReady",
		Message:            fmt.Sprintf("NetworkAttachmentDefinition %s is ready for use", nad.Name),
		ObservedGeneration: attachment.Generation,
	})
	if err := r.Status().Update(ctx, attachment); err != nil {
		return fmt.Errorf("update VPC attachment %s status: %w", client.ObjectKeyFromObject(attachment), err)
	}
	return nil
}

// publishToInterface records what realizes the interface and which VPC it landed
// in, then reports Prepared so compute can release the Pod.
func (r *NetworkInterfaceReconciler) publishToInterface(
	ctx context.Context,
	networkInterface *networkingv1alpha.NetworkInterface,
	attachment *cloudv1alpha1.VPCAttachment,
	vpc *cloudv1alpha1.VPC,
) error {
	ref := &networkingv1alpha.NetworkInterfaceAttachmentRef{
		APIGroup: cloudv1alpha1.GroupVersion.Group,
		Kind:     "VPCAttachment",
		Name:     attachment.Name,
	}
	networkInterface.Status.AttachmentRef = ref
	networkInterface.Status.VPC = vpc.Status.VPC
	return r.markPrepared(ctx, networkInterface, metav1.ConditionTrue, "AttachmentReady",
		fmt.Sprintf("VPCAttachment %s and its attachment definition exist", attachment.Name))
}

// markPrepared reports whether the pre-Pod artifacts exist, on the interface and
// on the claim holding it, which is what compute gates Pod creation on.
func (r *NetworkInterfaceReconciler) markPrepared(
	ctx context.Context,
	networkInterface *networkingv1alpha.NetworkInterface,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	condition := metav1.Condition{
		Type:               ConditionTypePrepared,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: networkInterface.Generation,
	}
	meta.SetStatusCondition(&networkInterface.Status.Conditions, condition)
	if err := r.Status().Update(ctx, networkInterface); err != nil {
		return fmt.Errorf("update network interface %s status: %w",
			client.ObjectKeyFromObject(networkInterface), err)
	}

	if networkInterface.Spec.ClaimRef == nil {
		return nil
	}
	var claim networkingv1alpha.NetworkInterfaceClaim
	key := types.NamespacedName{Namespace: networkInterface.Namespace, Name: networkInterface.Spec.ClaimRef.Name}
	if err := r.Get(ctx, key, &claim); err != nil {
		return client.IgnoreNotFound(err)
	}
	claimCondition := condition
	claimCondition.ObservedGeneration = claim.Generation
	meta.SetStatusCondition(&claim.Status.Conditions, claimCondition)
	if err := r.Status().Update(ctx, &claim); err != nil {
		return fmt.Errorf("update network interface claim %s status: %w", key, err)
	}
	return nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *NetworkInterfaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A NAD is owned by the attachment rather than the interface, but all three
	// share a name and namespace, so mapping one back is an identity.
	nadToInterface := func(_ context.Context, obj client.Object) []ctrl.Request {
		return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(obj)}}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha.NetworkInterface{}).
		Owns(&cloudv1alpha1.VPCAttachment{}).
		Watches(&nadv1.NetworkAttachmentDefinition{}, handler.EnqueueRequestsFromMapFunc(nadToInterface)).
		Watches(&networkingv1alpha.NetworkContext{},
			handler.EnqueueRequestsFromMapFunc(r.interfacesForNetworkContext)).
		Watches(&bgpv1alpha1.EgressShard{},
			handler.EnqueueRequestsFromMapFunc(r.interfacesForEgressShard)).
		Named("networkinterface").
		Complete(r)
}

// interfacesForNetworkContext re-renders a location's attachments when the
// egress a consumer declared reaches it, so a network that was enabled does not
// wait out a poll interval it does not have.
func (r *NetworkInterfaceReconciler) interfacesForNetworkContext(
	ctx context.Context, object client.Object,
) []reconcile.Request {
	var interfaces networkingv1alpha.NetworkInterfaceList
	if err := r.List(ctx, &interfaces, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(interfaces.Items))
	for i := range interfaces.Items {
		reference := interfaces.Items[i].Status.NetworkContextRef
		if reference == nil || reference.Name != object.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&interfaces.Items[i]),
		})
	}
	return requests
}

// interfacesForEgressShard re-renders every attachment in the cell when a shard
// arrives, reports its address, or leaves.
//
// It enqueues everything rather than working out which attachments sit on the
// shard's node: the sweep is affordable because a shard is an operator-written
// object in one namespace with one per node, and re-rendering an unaffected
// attachment writes nothing.
func (r *NetworkInterfaceReconciler) interfacesForEgressShard(
	ctx context.Context, _ client.Object,
) []reconcile.Request {
	var interfaces networkingv1alpha.NetworkInterfaceList
	if err := r.List(ctx, &interfaces); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(interfaces.Items))
	for i := range interfaces.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&interfaces.Items[i]),
		})
	}
	return requests
}
