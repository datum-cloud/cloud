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
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/fabricidentity"
	"go.datum.net/cloud/internal/ipam"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	// fabricIdentityFieldManager owns every placement policy this writes.
	fabricIdentityFieldManager = "cloud-fabric-identity"

	// FabricIdentityPolicyLabel marks the placement policies this controller
	// owns, so nothing else is ever rewritten or removed by it.
	FabricIdentityPolicyLabel = "cloud.datumapis.com/fabric-identity-policy"

	// servingLocationTopologyLabel is the cluster label a cell carries to claim
	// the location it serves. Placement selects on it.
	servingLocationTopologyLabel = "topology.datum.net/location"

	// fabricIdentityLocationLabelPrefix marks an identity as required at one
	// location. It follows the per-location convention the existing policies
	// already select on, with the location in the key rather than the value: a
	// label key holds one value, and one network is required in several
	// locations at once.
	fabricIdentityLocationLabelPrefix = "cloud.datumapis.com/location-"
)

// LocationLabel is the label marking an identity as required at one location.
func LocationLabel(location string) string {
	return fabricIdentityLocationLabelPrefix + location
}

var clusterPropagationPolicyGVK = schema.GroupVersionKind{
	Group:   "policy.karmada.io",
	Version: "v1alpha1",
	Kind:    "ClusterPropagationPolicy",
}

// NetworkFabricIdentityReconciler gives each network the identity the fabric
// knows it by, once, and carries it to the locations that need it.
//
// It runs centrally, not in a cell. A network spans locations, so the one thing
// that must be the same in all of them cannot be decided in any one of them —
// which is exactly the defect this replaces, where each location drew its own
// value and two locations of one network were two networks on the fabric.
type NetworkFabricIdentityReconciler struct {
	// Networks reads the Networks identities are allocated for.
	//
	// Separate from Hub because they are not the same plane in production: a
	// Network lives in its consumer's project control plane, and the identity
	// is published where cells can be reached. They are the same client only in
	// a deployment where one cluster holds both.
	Networks client.Client

	// Hub is where the identity and its placement are written, and from where
	// federation carries them to the cells.
	Hub client.Client

	// IPAM reaches the identifier space.
	IPAM ipam.ClientFactory

	// IdentityClass is the IPClass that hands out identities.
	IdentityClass string

	// IdentityNamespace is the namespace in the platform's own tenancy that
	// identity claims are written to.
	IdentityNamespace string
}

// Reconcile is keyed by a network, and reads everything it needs from that
// network's NetworkContexts. A context is the declaration that the network is
// required at a location, so the set of them names the network, identifies it,
// and gives the placement all at once.
func (r *NetworkFabricIdentityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	presences, err := r.presences(ctx, req.Namespace, req.Name)
	if err != nil {
		// A list that failed says nothing about where the network is required,
		// or whether it still exists. Changing nothing is the only safe answer.
		return ctrl.Result{}, err
	}

	// No presence anywhere means nothing needs the identity. Collect the object
	// and its placement rather than leave them to be matched forever for a
	// network that may no longer exist.
	//
	// This is safe to undo. The IPAM claim is retained and named from the
	// network's namespace and name, so a network that comes back to a location
	// is republished with the identity it always had, never a new one.
	if len(presences) == 0 {
		return ctrl.Result{}, r.collect(ctx, req.Namespace, req.Name)
	}

	locations := locationsFrom(presences)

	if err := r.publish(ctx, req.Namespace, req.Name, locations); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.placeLocations(ctx, locations)
}

// presences reads the contexts declaring the network is required somewhere.
func (r *NetworkFabricIdentityReconciler) presences(
	ctx context.Context,
	namespace string,
	networkName string,
) ([]networkingv1alpha.NetworkContext, error) {
	var contexts networkingv1alpha.NetworkContextList
	if err := r.Networks.List(ctx, &contexts, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("read the presences of network %q: %w", networkName, err)
	}

	matching := make([]networkingv1alpha.NetworkContext, 0, len(contexts.Items))
	for i := range contexts.Items {
		presence := &contexts.Items[i]
		if presence.Spec.Network.Name != networkName {
			continue
		}
		// A presence on its way out is still a presence. The location keeps the
		// identity until the context is actually gone, because the traffic it
		// carries is still there while it drains.
		matching = append(matching, *presence)
	}
	return matching, nil
}

func locationsFrom(presences []networkingv1alpha.NetworkContext) []string {
	seen := map[string]struct{}{}
	locations := make([]string, 0, len(presences))
	for i := range presences {
		location := presences[i].Spec.Location.Name
		if location == "" {
			continue
		}
		if _, ok := seen[location]; ok {
			continue
		}
		seen[location] = struct{}{}
		locations = append(locations, location)
	}
	sort.Strings(locations)
	return locations
}

// publish allocates the identity if it does not have one yet, and marks it as
// required at each location the network reaches.
func (r *NetworkFabricIdentityReconciler) publish(
	ctx context.Context,
	namespace string,
	networkName string,
	locations []string,
) error {
	key := client.ObjectKey{Namespace: namespace, Name: networkName}

	published := &cloudv1alpha1.NetworkFabricIdentity{}
	err := r.Hub.Get(ctx, key, published)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read the published identity for network %q: %w", networkName, err)
	}

	identity := published.Spec.Identity
	if identity == 0 {
		identity, err = r.claim(ctx, namespace, networkName)
		if err != nil {
			return err
		}
	}

	object := &cloudv1alpha1.NetworkFabricIdentity{}
	object.Namespace = key.Namespace
	object.Name = key.Name

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Hub, object, func() error {
		// Never moved once set. A network that changed identity would be a
		// different network to every location already carrying its traffic, and
		// the API refuses the write in any case.
		if object.Spec.Identity == 0 {
			object.Spec.Identity = identity
		}
		object.Spec.NetworkRef = cloudv1alpha1.NetworkFabricIdentityNetworkRef{Name: networkName}
		setLocationLabels(object, locations)
		return nil
	}); err != nil {
		return fmt.Errorf("publish the fabric identity for network %q: %w", networkName, err)
	}
	return nil
}

// setLocationLabels marks the identity as required at exactly these locations.
//
// A label is dropped only because a location is absent from a set built from a
// successful read of the presences. Every failure returns before reaching here,
// so a context that was briefly unreadable, or a cell that went quiet, never
// withdraws an identity. That matters because the fabric keys a VRF on it:
// withdrawing it under live traffic tears the data plane down at that location.
func setLocationLabels(object *cloudv1alpha1.NetworkFabricIdentity, locations []string) {
	if object.Labels == nil {
		object.Labels = map[string]string{}
	}
	required := make(map[string]struct{}, len(locations))
	for _, location := range locations {
		key := LocationLabel(location)
		required[key] = struct{}{}
		object.Labels[key] = "true"
	}
	for key := range object.Labels {
		if !strings.HasPrefix(key, fabricIdentityLocationLabelPrefix) {
			continue
		}
		if _, ok := required[key]; !ok {
			delete(object.Labels, key)
		}
	}
}

func (r *NetworkFabricIdentityReconciler) claim(
	ctx context.Context,
	networkNamespace string,
	networkName string,
) (int64, error) {
	ipamClient, err := r.IPAM.ClientForPlatform()
	if err != nil {
		return 0, fmt.Errorf("reach the platform identifier space: %w", err)
	}

	identity, err := fabricidentity.Claim(ctx, ipamClient, fabricidentity.Request{
		ClassName:        r.IdentityClass,
		Namespace:        r.IdentityNamespace,
		NetworkNamespace: networkNamespace,
		NetworkName:      networkName,
	})
	if err != nil {
		// An unusable answer is a wait on an operator, not on the service:
		// retrying reaches the same block. Fail closed either way — a network
		// given an ambiguous identity is worse than one given none.
		var unusable *fabricidentity.UnusableError
		if errors.As(err, &unusable) {
			log.FromContext(ctx).Error(err, "the identifier space handed out a block no identity can be read from",
				"network", networkName)
		}
		return 0, fmt.Errorf("allocate a fabric identity for network %q: %w", networkName, err)
	}
	return identity, nil
}

// collect removes the published identity when nothing requires it any more.
//
// The IPAM claim is deliberately not released. A Route Target still installed
// in a remote location's import policy would silently merge a new network into
// a dead one's routes, so the allocation is retained forever and the identity
// is never reissued. Only the object and its placement go.
func (r *NetworkFabricIdentityReconciler) collect(
	ctx context.Context,
	namespace string,
	networkName string,
) error {
	object := &cloudv1alpha1.NetworkFabricIdentity{}
	object.Namespace = namespace
	object.Name = networkName

	if err := r.Hub.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("collect the fabric identity for network %q: %w", networkName, err)
	}
	return nil
}

// placeLocations keeps one policy per location, not one per network.
//
// A policy per network would put the policy count on the order of the number of
// networks, and federation evaluates its policy set against candidate
// resources, so that cost is paid by everything else propagating through the
// same hub rather than by this alone. One policy per location selects every
// identity required there by label, so the object count stays per network while
// the policy count falls to the number of locations.
//
// Placing it fleet-wide is not an option: the identity is capability-like, and
// what holds it can name a network's forwarding state.
func (r *NetworkFabricIdentityReconciler) placeLocations(ctx context.Context, locations []string) error {
	for _, location := range locations {
		if err := r.placeLocation(ctx, location); err != nil {
			return err
		}
	}
	return nil
}

func (r *NetworkFabricIdentityReconciler) placeLocation(ctx context.Context, location string) error {
	policy := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"conflictResolution": "Overwrite",
			"resourceSelectors": []any{
				map[string]any{
					"apiVersion": cloudv1alpha1.GroupVersion.String(),
					"kind":       "NetworkFabricIdentity",
					"labelSelector": map[string]any{
						"matchLabels": map[string]any{
							LocationLabel(location): "true",
						},
					},
				},
			},
			"placement": map[string]any{
				"clusterAffinity": map[string]any{
					"labelSelector": map[string]any{
						"matchLabels": map[string]any{
							servingLocationTopologyLabel: location,
						},
					},
				},
			},
		},
	}}
	policy.SetGroupVersionKind(clusterPropagationPolicyGVK)
	policy.SetName(FabricIdentityPolicyName(location))
	policy.SetLabels(map[string]string{FabricIdentityPolicyLabel: "true"})

	if err := r.Hub.Patch(ctx, policy, client.Apply, //nolint:staticcheck // SA1019: the typed Apply API needs a generated ApplyConfiguration this unstructured policy has none of
		client.FieldOwner(fabricIdentityFieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("place fabric identities for location %q: %w", location, err)
	}
	return nil
}

// FabricIdentityPolicyName names the placement for one location.
func FabricIdentityPolicyName(location string) string {
	return "cloud-fabric-identity-" + location
}

// SetupWithManager registers the reconciler. Placement follows the network's
// presences, so a presence appearing or going away has to wake the network it
// belongs to; nothing else would carry the identity to a location the network
// has just reached.
func (r *NetworkFabricIdentityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.IdentityClass == "" {
		return errors.New("an identifier class is required")
	}
	if r.IPAM == nil {
		return errors.New("an identifier space is required")
	}
	// Driven entirely by NetworkContexts. A context names its network, carries
	// its UID, and gives the location, so nothing here has to reach the plane a
	// Network lives in.
	// Every request is keyed by a network, never by a context, because one
	// network's identity is decided from all of its contexts at once.
	return ctrl.NewControllerManagedBy(mgr).
		Named("networkfabricidentity").
		Watches(&networkingv1alpha.NetworkContext{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				presence, ok := obj.(*networkingv1alpha.NetworkContext)
				if !ok || presence.Spec.Network.Name == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: presence.Namespace,
					Name:      presence.Spec.Network.Name,
				}}}
			})).
		Complete(r)
}
