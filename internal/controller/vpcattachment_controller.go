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

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/galactic"
	"go.datum.net/cloud/internal/identifier"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	// IndexVPCAttachmentIdentity indexes a VPCAttachment by the
	// "<vpc>-<attachment>" pair galactic names its BGPAdvertisement after.
	IndexVPCAttachmentIdentity = "status.identity"

	// IndexVPCAttachmentInterface indexes a VPCAttachment by the NetworkInterface
	// it realizes.
	IndexVPCAttachmentInterface = "spec.interfaceRef.name"

	// LabelVPC records the base62 VPC identifier a NAD attaches to.
	LabelVPC = "cloud.datumapis.com/vpc"

	// LabelVPCAttachment records the base62 attachment identifier a NAD holds.
	// The NAD is the allocation record for that identifier.
	LabelVPCAttachment = "cloud.datumapis.com/vpc-attachment"
)

// maxIdentifierAttempts bounds the retry loop that draws an unused identifier.
const maxIdentifierAttempts = 100

// VPCAttachmentReconciler renders the NetworkAttachmentDefinition the galactic
// CNI chain reads.
//
// The attachment is the one object that knows both ends: it names the VPC, it
// carries how the guest consumes the interface, and it forward-references the
// NetworkInterface holding the addresses. So it owns the NAD, the NAD is named
// after it, and every render input is reachable from it without a reverse
// lookup.
type VPCAttachmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader bypasses the cache when listing allocated identifiers, so a NAD
	// written moments ago cannot be missed and its identifier reissued.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcattachments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcattachments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcs,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;create;update;patch;delete

func (r *VPCAttachmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var attachment cloudv1alpha1.VPCAttachment
	if err := r.Get(ctx, req.NamespacedName, &attachment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !attachment.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if attachment.Spec.InterfaceRef == nil {
		return ctrl.Result{}, r.markNotReady(ctx, &attachment, "InterfaceRefMissing",
			"spec.interfaceRef must name the NetworkInterface this attachment realizes")
	}

	var vpc cloudv1alpha1.VPC
	vpcKey := types.NamespacedName{Namespace: attachment.Namespace, Name: attachment.Spec.VPC.Name}
	if err := r.Get(ctx, vpcKey, &vpc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, r.markNotReady(ctx, &attachment,
				"VPCNotFound", fmt.Sprintf("VPC %s does not exist", attachment.Spec.VPC.Name))
		}
		return ctrl.Result{}, fmt.Errorf("get VPC %s: %w", vpcKey, err)
	}
	if vpc.Status.VPC == "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.markNotReady(ctx, &attachment,
			"VPCIdentifierPending", fmt.Sprintf("VPC %s has no identifier yet", vpc.Name))
	}

	networkInterface, err := r.dereferenceInterface(ctx, &attachment)
	if err != nil {
		return ctrl.Result{}, err
	}
	if networkInterface == nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.markNotReady(ctx, &attachment,
			"InterfaceUnresolved", fmt.Sprintf(
				"NetworkInterface %s does not exist, or was recreated under a different UID",
				attachment.Spec.InterfaceRef.Name))
	}

	nad, err := r.reconcileNAD(ctx, &attachment, &vpc, networkInterface)
	if err != nil {
		return ctrl.Result{}, err
	}

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
	if err := r.Status().Update(ctx, &attachment); err != nil {
		return ctrl.Result{}, fmt.Errorf("update VPC attachment %s status: %w", req.NamespacedName, err)
	}

	return ctrl.Result{}, r.publishToInterface(ctx, &attachment, networkInterface, vpc.Status.VPC)
}

// dereferenceInterface resolves spec.interfaceRef. A UID that no longer matches
// means the interface was recreated and this attachment is stale, so it must not
// bind to the new one and render its addresses.
func (r *VPCAttachmentReconciler) dereferenceInterface(
	ctx context.Context, attachment *cloudv1alpha1.VPCAttachment,
) (*networkingv1alpha.NetworkInterface, error) {
	var networkInterface networkingv1alpha.NetworkInterface
	key := types.NamespacedName{Namespace: attachment.Namespace, Name: attachment.Spec.InterfaceRef.Name}
	if err := r.Get(ctx, key, &networkInterface); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get network interface %s: %w", key, err)
	}
	if uid := attachment.Spec.InterfaceRef.UID; uid != "" && uid != string(networkInterface.UID) {
		return nil, nil
	}
	return &networkInterface, nil
}

// reconcileNAD creates or updates the NAD this attachment owns.
func (r *VPCAttachmentReconciler) reconcileNAD(
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

// allocateAttachmentIdentifier draws a random identifier unused within the VPC.
// Random rather than lowest-free, so a freed identifier is not immediately
// reissued while its BGPAdvertisement is still being garbage collected.
func (r *VPCAttachmentReconciler) allocateAttachmentIdentifier(ctx context.Context, vpc string) (string, error) {
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

// publishToInterface points the NetworkInterface at the attachment realizing it
// and records the VPC it landed in. Programmed is left to the data plane.
func (r *VPCAttachmentReconciler) publishToInterface(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	networkInterface *networkingv1alpha.NetworkInterface,
	vpc string,
) error {
	ref := &networkingv1alpha.NetworkInterfaceAttachmentRef{
		APIGroup: cloudv1alpha1.GroupVersion.Group,
		Kind:     "VPCAttachment",
		Name:     attachment.Name,
	}
	current := networkInterface.Status.AttachmentRef
	if current != nil && *current == *ref && networkInterface.Status.VPC == vpc {
		return nil
	}
	networkInterface.Status.AttachmentRef = ref
	networkInterface.Status.VPC = vpc
	if err := r.Status().Update(ctx, networkInterface); err != nil {
		return fmt.Errorf("publish attachment onto network interface %s: %w",
			client.ObjectKeyFromObject(networkInterface), err)
	}
	return nil
}

func (r *VPCAttachmentReconciler) markNotReady(
	ctx context.Context, attachment *cloudv1alpha1.VPCAttachment, reason, message string,
) error {
	meta.SetStatusCondition(&attachment.Status.Conditions, metav1.Condition{
		Type:               cloudv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: attachment.Generation,
	})
	attachment.Status.ObservedGeneration = attachment.Generation
	if err := r.Status().Update(ctx, attachment); err != nil {
		return fmt.Errorf("update VPC attachment %s status: %w", client.ObjectKeyFromObject(attachment), err)
	}
	return nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *VPCAttachmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	interfaceToAttachments := func(ctx context.Context, obj client.Object) []ctrl.Request {
		var attachments cloudv1alpha1.VPCAttachmentList
		if err := mgr.GetClient().List(ctx, &attachments,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{IndexVPCAttachmentInterface: obj.GetName()}); err != nil {
			return nil
		}
		requests := make([]ctrl.Request, 0, len(attachments.Items))
		for i := range attachments.Items {
			requests = append(requests, ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(&attachments.Items[i]),
			})
		}
		return requests
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.VPCAttachment{}).
		Owns(&nadv1.NetworkAttachmentDefinition{}).
		Watches(&networkingv1alpha.NetworkInterface{}, handler.EnqueueRequestsFromMapFunc(interfaceToAttachments)).
		Named("vpcattachment").
		Complete(r)
}
