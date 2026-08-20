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
	"encoding/json"
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

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/galactic"
	"go.datum.net/cloud/internal/identifier"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	// LabelVPC records the base62 VPC identifier a NAD attaches to.
	LabelVPC = "cloud.datumapis.com/vpc"

	// LabelVPCAttachment records the base62 attachment identifier a NAD holds.
	// The NAD is the allocation record for that identifier.
	LabelVPCAttachment = "cloud.datumapis.com/vpc-attachment"

	// MultusNetworksAnnotation is the annotation Multus resolves at sandbox
	// creation to attach a workload to a NAD.
	MultusNetworksAnnotation = "k8s.v1.cni.cncf.io/networks"
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
	// exist yet, so a cell states what it is rather than defaulting.
	AttachmentMode cloudv1alpha1.VPCAttachmentInterfaceMode
}

// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcs,verbs=get;list;watch
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

	var vpc cloudv1alpha1.VPC
	vpcKey := types.NamespacedName{
		Namespace: networkInterface.Namespace,
		Name:      networkInterface.Status.NetworkContextRef.Name,
	}
	if err := r.Get(ctx, vpcKey, &vpc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get VPC %s: %w", vpcKey, err)
	}
	if vpc.Status.VPC == "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	attachment, err := r.reconcileAttachment(ctx, &networkInterface, &vpc)
	if err != nil {
		return ctrl.Result{}, err
	}
	nad, err := r.reconcileNAD(ctx, attachment, &vpc, &networkInterface)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.publishAttachmentStatus(ctx, attachment, &vpc, nad); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.publishToInterface(ctx, &networkInterface, attachment, &vpc, nad)
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
		attachment.Spec.InterfaceRef = &cloudv1alpha1.NetworkInterfaceRef{
			Name: networkInterface.Name,
			UID:  string(networkInterface.UID),
		}
		attachment.Spec.Interface.Name = networkInterface.Spec.InterfaceName
		attachment.Spec.Interface.Mode = r.AttachmentMode
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

	addresses := make([]string, 0, len(networkInterface.Spec.Addresses))
	for _, address := range networkInterface.Spec.Addresses {
		addresses = append(addresses, address.Address)
	}
	config, err := galactic.ConflistJSON(attachment.Name, masterPlugin(attachment.Spec.Interface.Mode),
		vpc.Status.VPC, attachmentID, networkInterface.Spec.MTU, addresses)
	if err != nil {
		return nil, err
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, nad, func() error {
		if nad.Labels == nil {
			nad.Labels = map[string]string{}
		}
		nad.Labels[LabelVPC] = vpc.Status.VPC
		nad.Labels[LabelVPCAttachment] = attachmentID
		nad.Spec.Config = config
		return controllerutil.SetControllerReference(attachment, nad, r.Scheme)
	}); err != nil {
		return nil, fmt.Errorf("reconcile NetworkAttachmentDefinition %s: %w", nad.Name, err)
	}
	return nad, nil
}

// masterPlugin translates how a guest consumes an interface into the galactic
// binary that realizes it. This is the only place the two vocabularies meet.
func masterPlugin(mode cloudv1alpha1.VPCAttachmentInterfaceMode) string {
	if mode == cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor {
		return galactic.PluginTap
	}
	return galactic.PluginVeth
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

// publishAttachmentStatus records the allocated identifiers on the attachment.
func (r *NetworkInterfaceReconciler) publishAttachmentStatus(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	vpc *cloudv1alpha1.VPC,
	nad *nadv1.NetworkAttachmentDefinition,
) error {
	attachment.Status.VPC = vpc.Status.VPC
	attachment.Status.VPCAttachment = nad.Labels[LabelVPCAttachment]
	attachment.Status.NetworkAttachmentDefinition = nad.Name
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

// publishToInterface hands the consumer everything it needs: what realizes the
// interface, which VPC it landed in, and the annotations to copy onto the
// workload object. The annotations are opaque to whoever applies them.
func (r *NetworkInterfaceReconciler) publishToInterface(
	ctx context.Context,
	networkInterface *networkingv1alpha.NetworkInterface,
	attachment *cloudv1alpha1.VPCAttachment,
	vpc *cloudv1alpha1.VPC,
	nad *nadv1.NetworkAttachmentDefinition,
) error {
	ref := &networkingv1alpha.NetworkInterfaceAttachmentRef{
		APIGroup: cloudv1alpha1.GroupVersion.Group,
		Kind:     "VPCAttachment",
		Name:     attachment.Name,
	}
	if current := networkInterface.Status.AttachmentRef; current == nil || *current != *ref ||
		networkInterface.Status.VPC != vpc.Status.VPC {
		networkInterface.Status.AttachmentRef = ref
		networkInterface.Status.VPC = vpc.Status.VPC
		if err := r.Status().Update(ctx, networkInterface); err != nil {
			return fmt.Errorf("publish attachment onto network interface %s: %w",
				client.ObjectKeyFromObject(networkInterface), err)
		}
	}

	return r.publishConsumerAnnotations(ctx, networkInterface, consumerAnnotations(nad))
}

// consumerAnnotations is what a workload must carry to be delivered onto this
// attachment.
func consumerAnnotations(nad *nadv1.NetworkAttachmentDefinition) map[string]string {
	return map[string]string{
		MultusNetworksAnnotation: fmt.Sprintf("%s/%s", nad.Namespace, nad.Name),
	}
}

// publishConsumerAnnotations writes status.consumerAnnotations as a raw merge
// patch, because the field is landing in network-services-operator in parallel
// and this repo cannot type against it yet.
func (r *NetworkInterfaceReconciler) publishConsumerAnnotations(
	ctx context.Context, networkInterface *networkingv1alpha.NetworkInterface, annotations map[string]string,
) error {
	patch, err := json.Marshal(map[string]any{"status": map[string]any{"consumerAnnotations": annotations}})
	if err != nil {
		return fmt.Errorf("marshal consumer annotations patch: %w", err)
	}
	if err := r.Status().Patch(ctx, networkInterface, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("publish consumer annotations onto network interface %s: %w",
			client.ObjectKeyFromObject(networkInterface), err)
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
		Named("networkinterface").
		Complete(r)
}
