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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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

	// AnnotationInterfaceType overrides the master plugin chosen for an
	// interface when no VPCAttachment has been created for it yet.
	AnnotationInterfaceType = "cloud.datumapis.com/interface-type"
)

// NetworkInterfaceReconciler allocates an attachment identifier per
// NetworkInterface and renders the NetworkAttachmentDefinition the galactic CNI
// chain reads.
//
// The NAD is bound to the interface, not to an instance: a NetworkInterface is
// slot-stable and outlives instance replacement under a Retain reclaim policy,
// so the attachment identifier, the tap device name and the BGPAdvertisement
// all survive a replacement, and the NAD's single host-interface annotation
// stays unambiguous.
type NetworkInterfaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader bypasses the cache when listing allocated identifiers, so a NAD
	// written moments ago cannot be missed and its identifier reissued.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkinterfaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;create;update;patch;delete

func (r *NetworkInterfaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var networkInterface networkingv1alpha.NetworkInterface
	if err := r.Get(ctx, req.NamespacedName, &networkInterface); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !networkInterface.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if networkInterface.Status.NetworkContextRef == nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
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

	attachmentID, err := r.reconcileNAD(ctx, &networkInterface, &vpc)
	if err != nil {
		return ctrl.Result{}, err
	}

	if networkInterface.Status.VPC != vpc.Status.VPC {
		networkInterface.Status.VPC = vpc.Status.VPC
		if err := r.Status().Update(ctx, &networkInterface); err != nil {
			return ctrl.Result{}, fmt.Errorf("record VPC on network interface %s: %w", req.NamespacedName, err)
		}
	}

	logf := ctrl.LoggerFrom(ctx)
	logf.V(1).Info("attachment realized", "vpc", vpc.Status.VPC, "vpcAttachment", attachmentID)
	return ctrl.Result{}, nil
}

// reconcileNAD creates or updates the NAD for an interface and returns the
// attachment identifier it carries.
func (r *NetworkInterfaceReconciler) reconcileNAD(
	ctx context.Context, networkInterface *networkingv1alpha.NetworkInterface, vpc *cloudv1alpha1.VPC,
) (string, error) {
	nad := &nadv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkInterface.Name,
			Namespace: networkInterface.Namespace,
		},
	}

	attachmentID := ""
	if err := r.Get(ctx, client.ObjectKeyFromObject(nad), nad); err == nil {
		attachmentID = nad.Labels[LabelVPCAttachment]
	} else if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("get NetworkAttachmentDefinition %s: %w", nad.Name, err)
	}
	if attachmentID == "" {
		allocated, err := r.allocateAttachmentIdentifier(ctx, vpc.Status.VPC)
		if err != nil {
			return "", err
		}
		attachmentID = allocated
	}

	plugin := galactic.PluginVeth
	interfaceType, err := r.resolveInterfaceType(ctx, networkInterface)
	if err != nil {
		return "", err
	}
	if interfaceType == cloudv1alpha1.VPCAttachmentInterfaceTypeTap {
		plugin = galactic.PluginTap
	}

	addresses := make([]string, 0, len(networkInterface.Spec.Addresses))
	for _, address := range networkInterface.Spec.Addresses {
		addresses = append(addresses, address.Address)
	}

	config, err := galactic.ConflistJSON(
		networkInterface.Name, plugin, vpc.Status.VPC, attachmentID, networkInterface.Spec.MTU, addresses)
	if err != nil {
		return "", err
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, nad, func() error {
		if nad.Labels == nil {
			nad.Labels = map[string]string{}
		}
		nad.Labels[LabelVPC] = vpc.Status.VPC
		nad.Labels[LabelVPCAttachment] = attachmentID
		nad.Spec.Config = config
		return controllerutil.SetControllerReference(networkInterface, nad, r.Scheme)
	}); err != nil {
		return "", fmt.Errorf("reconcile NetworkAttachmentDefinition %s: %w", nad.Name, err)
	}

	return attachmentID, nil
}

// resolveInterfaceType decides which master plugin realizes an interface. A
// VPCAttachment naming the interface is authoritative, since only the workload
// provider knows whether the guest is a container or a virtual machine.
func (r *NetworkInterfaceReconciler) resolveInterfaceType(
	ctx context.Context, networkInterface *networkingv1alpha.NetworkInterface,
) (cloudv1alpha1.VPCAttachmentInterfaceType, error) {
	var attachments cloudv1alpha1.VPCAttachmentList
	if err := r.List(ctx, &attachments, client.InNamespace(networkInterface.Namespace)); err != nil {
		return "", fmt.Errorf("list VPC attachments: %w", err)
	}
	for _, attachment := range attachments.Items {
		if attachment.Spec.InterfaceRef == nil || attachment.Spec.InterfaceRef.Name != networkInterface.Name {
			continue
		}
		if attachment.Spec.Interface.Type != "" {
			return attachment.Spec.Interface.Type, nil
		}
	}
	if declared := networkInterface.Annotations[AnnotationInterfaceType]; declared != "" {
		return cloudv1alpha1.VPCAttachmentInterfaceType(declared), nil
	}
	return cloudv1alpha1.VPCAttachmentInterfaceTypeVeth, nil
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

// SetupWithManager registers the reconciler with the manager.
func (r *NetworkInterfaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha.NetworkInterface{}).
		Owns(&nadv1.NetworkAttachmentDefinition{}).
		Named("networkinterface").
		Complete(r)
}
