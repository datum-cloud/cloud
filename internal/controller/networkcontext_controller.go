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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/identifier"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	// fabricIdentityGracePeriod bounds how long a new VPC waits for the identity
	// the fabric knows its network by before falling back to drawing its own.
	fabricIdentityGracePeriod = 5 * time.Minute

	// fabricIdentityPollInterval is how often a VPC still waiting looks again.
	fabricIdentityPollInterval = 10 * time.Second
)

// NetworkContextReconciler gives a network's presence in one location its
// data-plane identity: one VPC per NetworkContext, carrying the base62 VPC
// identifier the whole galactic fabric keys on.
type NetworkContextReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkcontexts,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=networkfabricidentities,verbs=get;list;watch
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

	// Everything below is conditioned on the VPC not having an identifier yet. A
	// VPC that already has one keeps it: the VRF device is named from it and the
	// Route Target derives from it, so renumbering a live VPC would rename its
	// interface and change its routes under running traffic.
	if vpc.Status.VPC == "" {
		allocated, waiting, err := r.vpcIdentifier(ctx, &networkContext, vpc)
		if err != nil {
			return ctrl.Result{}, err
		}
		if waiting {
			return ctrl.Result{RequeueAfter: fabricIdentityPollInterval},
				r.markAwaitingFabricIdentity(ctx, vpc)
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

// vpcIdentifier resolves the identifier this VPC carries for the rest of its
// life, and reports whether it is still worth waiting for a better answer.
//
// The identity the fabric knows a network by is allocated centrally, once for
// the whole network, and carried to each cell the network reaches. Deriving the
// VPC identifier from it is what makes two locations of one network the same
// network on the fabric; drawing a random value per location, which is what
// this used to do unconditionally, made them two.
//
// The identity may not have landed in this cell yet when the VPC is first
// reconciled, and the identifier is immutable once written, so a fallback taken
// too eagerly is permanent. The wait is bounded rather than indefinite, because
// a network that will never have an identity — one predating the allocator, or
// one whose central allocation is stuck — has to end up with a working VPC
// rather than none at all. Past the grace period the old random draw still
// happens and nothing regresses.
//
// The window is measured from the VPC's own creation timestamp, so it survives
// a controller restart or a change of leader rather than resetting each time.
func (r *NetworkContextReconciler) vpcIdentifier(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	vpc *cloudv1alpha1.VPC,
) (string, bool, error) {
	identity, found, err := r.fabricIdentity(ctx, networkContext)
	if err != nil {
		return "", false, err
	}
	if found {
		encoded, err := identifier.VPCBase62(uint64(identity))
		if err != nil {
			return "", false, fmt.Errorf("encode fabric identity %d for VPC %s: %w", identity, vpc.Name, err)
		}
		return encoded, false, nil
	}

	// An age that cannot be read reads as new. The fallback is permanent, so the
	// only safe way to be wrong about it is to wait longer.
	if vpc.CreationTimestamp.IsZero() ||
		time.Since(vpc.CreationTimestamp.Time) < fabricIdentityGracePeriod {
		return "", true, nil
	}

	allocated, err := r.allocateVPCIdentifier(ctx)
	return allocated, false, err
}

// fabricIdentity reads the identity carried to this cell for the context's
// network. It is one object per network, named after the network, in the
// network's namespace. A missing one is an ordinary answer, not a failure.
func (r *NetworkContextReconciler) fabricIdentity(
	ctx context.Context, networkContext *networkingv1alpha.NetworkContext,
) (int64, bool, error) {
	networkName := networkContext.Spec.Network.Name
	if networkName == "" {
		return 0, false, nil
	}

	var identity cloudv1alpha1.NetworkFabricIdentity
	key := client.ObjectKey{Namespace: networkContext.Namespace, Name: networkName}
	if err := r.Get(ctx, key, &identity); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read the fabric identity for network %q: %w", networkName, err)
	}
	if identity.Spec.Identity == 0 {
		return 0, false, nil
	}
	return identity.Spec.Identity, true, nil
}

// markAwaitingFabricIdentity says out loud that the VPC has no identifier yet
// and why, so a network stuck waiting on a central allocation is visible as
// that rather than as a VPC that silently never became ready.
func (r *NetworkContextReconciler) markAwaitingFabricIdentity(
	ctx context.Context, vpc *cloudv1alpha1.VPC,
) error {
	vpc.Status.ObservedGeneration = vpc.Generation
	meta.SetStatusCondition(&vpc.Status.Conditions, metav1.Condition{
		Type:               cloudv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             "AwaitingFabricIdentity",
		Message:            "Waiting for the identity the fabric knows this network by",
		ObservedGeneration: vpc.Generation,
	})
	if err := r.Status().Update(ctx, vpc); err != nil {
		return fmt.Errorf("update VPC %s status: %w", vpc.Name, err)
	}
	return nil
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
		Watches(&cloudv1alpha1.NetworkFabricIdentity{},
			handler.EnqueueRequestsFromMapFunc(r.contextsForFabricIdentity)).
		Named("networkcontext").
		Complete(r)
}

// contextsForFabricIdentity wakes a network's presences the moment its identity
// reaches this cell, so a VPC waiting on one takes it immediately instead of
// sitting out its poll interval.
func (r *NetworkContextReconciler) contextsForFabricIdentity(
	ctx context.Context, object client.Object,
) []reconcile.Request {
	var contexts networkingv1alpha.NetworkContextList
	if err := r.List(ctx, &contexts, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(contexts.Items))
	for i := range contexts.Items {
		if contexts.Items[i].Spec.Network.Name != object.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&contexts.Items[i]),
		})
	}
	return requests
}
