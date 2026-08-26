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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/identifier"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// NetworkContextReconciler gives a network's presence in one location its
// data-plane identity: one VPC per NetworkContext, carrying the base62 VPC
// identifier the whole galactic fabric keys on.
type NetworkContextReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	APIReader client.Reader
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
		allocated, err := r.vpcIdentifier(ctx, &networkContext)
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
		start, prefixLength, ok := subnetRange(&subnet)
		if !ok {
			continue
		}
		networks = append(networks, cloudv1alpha1.Network(
			fmt.Sprintf("%s/%d", start, prefixLength)))
	}
	slices.Sort(networks)
	return networks, nil
}

// subnetRange reads a subnet's allocated range, preferring status and falling
// back to spec. A location's copy arrives by propagation, which carries spec
// and never status.
func subnetRange(subnet *networkingv1alpha.Subnet) (string, int32, bool) {
	if subnet.Status.StartAddress != nil && subnet.Status.PrefixLength != nil {
		return *subnet.Status.StartAddress, *subnet.Status.PrefixLength, true
	}
	if subnet.Spec.StartAddress != "" && subnet.Spec.PrefixLength != 0 {
		return subnet.Spec.StartAddress, subnet.Spec.PrefixLength, true
	}
	return "", 0, false
}

// vpcIdentifier resolves the identifier for a VPC that does not have one yet.
// A network whose fabric identity has been allocated for it derives its
// identifier from that value, so every location holding the same network
// arrives at the same one. A network with no allocated identity keeps the
// original behaviour and draws a random identifier for this cell.
func (r *NetworkContextReconciler) vpcIdentifier(
	ctx context.Context, networkContext *networkingv1alpha.NetworkContext,
) (string, error) {
	fabricIdentity, err := r.fabricIdentity(ctx, networkContext)
	if err != nil {
		return "", err
	}
	if fabricIdentity == 0 {
		return r.allocateVPCIdentifier(ctx)
	}
	return vpcIdentifierFor(fabricIdentity)
}

// vpcIdentifierFor renders an allocated fabric identity as the base62 VPC
// identifier the galactic data plane keys on. The rendering is total and
// deterministic: the same identity yields the same identifier in every cell.
func vpcIdentifierFor(fabricIdentity int64) (string, error) {
	if fabricIdentity < 0 {
		return "", fmt.Errorf("fabric identity %d is negative", fabricIdentity)
	}
	rendered, err := identifier.VPCBase62(uint64(fabricIdentity))
	if err != nil {
		return "", fmt.Errorf("render fabric identity %d: %w", fabricIdentity, err)
	}
	return rendered, nil
}

// fabricIdentity reads the identity allocated for the network this context
// belongs to, or zero when none has been allocated.
//
// The field is read untyped because the Go type in the pinned
// network-services-operator release does not carry it yet, and a typed client
// discards fields its struct does not know: the value would read as absent
// every time. Reading it directly means this cell picks the identity up as
// soon as it is published, with no release ordering between the two repos.
// Once a release carrying the field is pinned, this collapses to a field read.
func (r *NetworkContextReconciler) fabricIdentity(
	ctx context.Context, networkContext *networkingv1alpha.NetworkContext,
) (int64, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	raw := &unstructured.Unstructured{}
	raw.SetGroupVersionKind(networkingv1alpha.GroupVersion.WithKind("NetworkContext"))
	if err := reader.Get(ctx, client.ObjectKeyFromObject(networkContext), raw); err != nil {
		return 0, fmt.Errorf("read NetworkContext %s: %w", networkContext.Name, err)
	}
	return fabricIdentityFrom(raw.Object)
}

// fabricIdentityFrom extracts spec.fabricIdentity from an untyped
// NetworkContext, treating an absent field as no allocated identity.
func fabricIdentityFrom(object map[string]any) (int64, error) {
	value, found, err := unstructured.NestedInt64(object, "spec", "fabricIdentity")
	if err != nil {
		return 0, fmt.Errorf("read spec.fabricIdentity: %w", err)
	}
	if !found {
		return 0, nil
	}
	return value, nil
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
