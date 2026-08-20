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
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/identifier"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// maxIdentifierAttempts bounds the retry loop that draws an unused identifier.
const maxIdentifierAttempts = 100

// NetworkContextReconciler gives a network's presence in one location its
// data-plane identity: one VPC per NetworkContext, carrying the base62 VPC
// identifier the whole galactic fabric keys on.
type NetworkContextReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkcontexts,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=subnets,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcs/status,verbs=get;update;patch

func (r *NetworkContextReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var networkContext networkingv1alpha.NetworkContext
	if err := r.Get(ctx, req.NamespacedName, &networkContext); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !networkContext.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	networks, err := r.networksForContext(ctx, &networkContext)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(networks) == 0 {
		// The VPC address space comes from the Subnets IPAM allocated for this
		// location, and VPCSpec is immutable once written.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	vpc := &cloudv1alpha1.VPC{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkContext.Name,
			Namespace: networkContext.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, vpc, func() error {
		if vpc.CreationTimestamp.IsZero() {
			vpc.Spec.Networks = networks
		}
		return controllerutil.SetControllerReference(&networkContext, vpc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile VPC %s: %w", vpc.Name, err)
	}

	if vpc.Status.VPC == "" {
		allocated, err := r.allocateVPCIdentifier(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		vpc.Status.VPC = allocated
	}
	vpc.Status.ObservedGeneration = vpc.Generation
	meta.SetStatusCondition(&vpc.Status.Conditions, metav1.Condition{
		Type:               cloudv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "IdentifierAllocated",
		Message:            fmt.Sprintf("VPC identifier %s allocated", vpc.Status.VPC),
		ObservedGeneration: vpc.Generation,
	})
	if err := r.Status().Update(ctx, vpc); err != nil {
		return ctrl.Result{}, fmt.Errorf("update VPC %s status: %w", vpc.Name, err)
	}

	return ctrl.Result{}, nil
}

// networksForContext collects the CIDRs IPAM allocated for this location.
func (r *NetworkContextReconciler) networksForContext(
	ctx context.Context, networkContext *networkingv1alpha.NetworkContext,
) ([]cloudv1alpha1.Network, error) {
	var subnets networkingv1alpha.SubnetList
	if err := r.List(ctx, &subnets, client.InNamespace(networkContext.Namespace)); err != nil {
		return nil, fmt.Errorf("list subnets: %w", err)
	}

	networks := make([]cloudv1alpha1.Network, 0, len(subnets.Items))
	for _, subnet := range subnets.Items {
		if subnet.Spec.NetworkContext.Name != networkContext.Name {
			continue
		}
		if subnet.Status.StartAddress == nil || subnet.Status.PrefixLength == nil {
			continue
		}
		networks = append(networks, cloudv1alpha1.Network(
			fmt.Sprintf("%s/%d", *subnet.Status.StartAddress, *subnet.Status.PrefixLength)))
	}
	slices.Sort(networks)
	return networks, nil
}

// allocateVPCIdentifier draws a random 48-bit identifier not already in use.
// A single leader-elected controller is the only writer, so a list plus a
// collision check serializes correctly.
func (r *NetworkContextReconciler) allocateVPCIdentifier(ctx context.Context) (string, error) {
	var vpcs cloudv1alpha1.VPCList
	if err := r.List(ctx, &vpcs); err != nil {
		return "", fmt.Errorf("list VPCs: %w", err)
	}
	used := make(map[string]struct{}, len(vpcs.Items))
	for _, vpc := range vpcs.Items {
		if vpc.Status.VPC != "" {
			used[vpc.Status.VPC] = struct{}{}
		}
	}

	for range maxIdentifierAttempts {
		candidate, err := identifier.RandomVPCBase62()
		if err != nil {
			return "", err
		}
		if _, taken := used[candidate]; !taken {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no unused VPC identifier found after %d attempts", maxIdentifierAttempts)
}

// SetupWithManager registers the reconciler with the manager.
func (r *NetworkContextReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha.NetworkContext{}).
		Owns(&cloudv1alpha1.VPC{}).
		Named("networkcontext").
		Complete(r)
}
