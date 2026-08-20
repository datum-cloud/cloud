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
	"sigs.k8s.io/controller-runtime/pkg/handler"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// IndexVPCAttachmentIdentity indexes a VPCAttachment by the "<vpc>-<attachment>"
// pair galactic names its BGPAdvertisement after.
const IndexVPCAttachmentIdentity = "status.identity"

// VPCAttachmentReconciler joins an attachment to the identifiers allocated for
// the NetworkInterface it realizes, and points the interface back at it.
type VPCAttachmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcattachments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcattachments/status,verbs=get;update;patch

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

	// The NAD is the allocation record, and it is named after the interface.
	var nad nadv1.NetworkAttachmentDefinition
	nadKey := types.NamespacedName{Namespace: attachment.Namespace, Name: attachment.Spec.InterfaceRef.Name}
	if err := r.Get(ctx, nadKey, &nad); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.markNotReady(ctx, &attachment,
				"AwaitingAttachmentIdentifier", "waiting for the interface's NetworkAttachmentDefinition")
		}
		return ctrl.Result{}, fmt.Errorf("get NetworkAttachmentDefinition %s: %w", nadKey, err)
	}
	attachmentID := nad.Labels[LabelVPCAttachment]
	if attachmentID == "" {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.markNotReady(ctx, &attachment,
			"AwaitingAttachmentIdentifier", "waiting for the interface's attachment identifier")
	}

	attachment.Status.VPC = vpc.Status.VPC
	attachment.Status.VPCAttachment = attachmentID
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

	return ctrl.Result{}, r.recordAttachmentRef(ctx, &attachment)
}

// recordAttachmentRef points the NetworkInterface at the attachment realizing it.
func (r *VPCAttachmentReconciler) recordAttachmentRef(
	ctx context.Context, attachment *cloudv1alpha1.VPCAttachment,
) error {
	var networkInterface networkingv1alpha.NetworkInterface
	key := types.NamespacedName{Namespace: attachment.Namespace, Name: attachment.Spec.InterfaceRef.Name}
	if err := r.Get(ctx, key, &networkInterface); err != nil {
		return client.IgnoreNotFound(err)
	}

	ref := &networkingv1alpha.NetworkInterfaceAttachmentRef{
		APIGroup: cloudv1alpha1.GroupVersion.Group,
		Kind:     "VPCAttachment",
		Name:     attachment.Name,
	}
	if current := networkInterface.Status.AttachmentRef; current != nil && *current == *ref {
		return nil
	}
	networkInterface.Status.AttachmentRef = ref
	if err := r.Status().Update(ctx, &networkInterface); err != nil {
		return fmt.Errorf("record attachment ref on network interface %s: %w", key, err)
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
	nadToAttachments := func(ctx context.Context, obj client.Object) []ctrl.Request {
		var attachments cloudv1alpha1.VPCAttachmentList
		if err := mgr.GetClient().List(ctx, &attachments, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		var requests []ctrl.Request
		for _, attachment := range attachments.Items {
			if attachment.Spec.InterfaceRef != nil && attachment.Spec.InterfaceRef.Name == obj.GetName() {
				requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&attachment)})
			}
		}
		return requests
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.VPCAttachment{}).
		Watches(&nadv1.NetworkAttachmentDefinition{}, handler.EnqueueRequestsFromMapFunc(nadToAttachments)).
		Named("vpcattachment").
		Complete(r)
}
