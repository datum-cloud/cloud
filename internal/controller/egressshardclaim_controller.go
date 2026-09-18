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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

// egressUnavailableMessage is what a consumer reads when this location
// provides no egress for their network. It names no node, no shard and no
// other consumer: a consumer cannot act on any of those, and each says where
// the platform runs their workload. The cause is logged and lives on the
// claim, which is an operator's object.
const egressUnavailableMessage = "No component in this location provides internet egress for this network"

// bindingRefusedError is why one binding may not be made, carrying the named
// reason it is reported under. A binding that cannot be made says which
// incompatibility stopped it; a generic failure would leave an operator to work
// that out from the objects.
type bindingRefusedError struct {
	reason  string
	message string
}

func (e *bindingRefusedError) Error() string { return e.message }

// EgressShardClaimReconciler binds a network's presence in this cell to one
// egress shard, once.
//
// It is the cell's single decision point for egress. The intent a location was
// instructed with says what the network needs; this decides which shard answers
// it, records that on the claim, and reports the result on the network context
// a consumer already reads. Everything downstream — the route a node installs,
// the address a consumer allow-lists — reads the binding rather than selecting
// again, because two selections made from the same inputs at different moments
// are two answers, and the datapath can hold one.
//
// Every claim binds a shared shard. Dedicated capacity is a hand-commissioned
// shard node and no controller can grow it while nothing allocates the
// identifier a shard is unusable without, so the platform withholds the value
// rather than accepting a request that would wait indefinitely — the same way
// it withholds reaching IPv4 destinations until a resolver and a translator
// share a prefix. The claim records the sharing it was created under and
// nothing here branches on it.
type EgressShardClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkcontexts,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkcontexts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=egressshardparameters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=egressshardclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=egressshardclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.datumapis.com,resources=egressshards,verbs=get;list;watch;update;patch

func (r *EgressShardClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// A claim carries its network context's name, so one key reads both: the
	// instruction this cell was given, and the binding made from it.
	var networkContext networkingv1alpha.NetworkContext
	contextFound := true
	if err := r.Get(ctx, req.NamespacedName, &networkContext); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get NetworkContext %s: %w", req.NamespacedName, err)
		}
		contextFound = false
	}

	var claim cloudv1alpha1.EgressShardClaim
	claimFound := true
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get EgressShardClaim %s: %w", req.NamespacedName, err)
		}
		claimFound = false
	}
	if claimFound && !claim.DeletionTimestamp.IsZero() {
		// The shard's finalizer is released by the reconciler watching shards,
		// which sees this claim leave the consumer set.
		return ctrl.Result{}, nil
	}

	if !contextFound || !networkContext.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.releaseClaim(ctx, &claim, claimFound,
			"the network is no longer present in this cell")
	}

	intent := internetEgressIntent(&networkContext)
	terms, err := r.claimTerms(ctx, &networkContext)
	if err != nil {
		var refused *bindingRefusedError
		if errors.As(err, &refused) {
			// Nothing can be bound and nothing is: no claim, so no route and no
			// address, which is what a consumer reads as no egress here. The
			// cause is logged rather than published, for the same reason the
			// condition's message never carries one.
			logf.FromContext(ctx).Info("nothing can be bound for this location's egress",
				"networkContext", networkContext.Name, "reason", refused.reason,
				"cause", refused.message)
			if err := r.releaseClaim(ctx, &claim, claimFound, refused.message); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.reportContext(ctx, &networkContext, metav1.ConditionFalse,
				networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
				egressUnavailableMessage)
		}
		return ctrl.Result{}, err
	}
	if terms == nil {
		// Either this location was told to reach nothing, or its class is
		// served by another implementation. Both are ordinary answers and
		// neither is this controller's to report on.
		return ctrl.Result{}, r.releaseClaim(ctx, &claim, claimFound,
			"this location provides no internet egress this controller serves")
	}

	if !claimFound {
		return ctrl.Result{}, r.createClaim(ctx, &networkContext, terms)
	}

	if !equality.Semantic.DeepEqual(claim.Spec, *terms) {
		// The spec is immutable, so the terms cannot be brought into line. An
		// unbound claim is discarded and rewritten; a bound one keeps
		// delivering what it was bound for and says that it no longer matches.
		if claim.Status.ShardRef == nil {
			return ctrl.Result{}, r.releaseClaim(ctx, &claim, claimFound,
				"the terms this location is instructed with changed before a shard was bound")
		}
		logf.FromContext(ctx).Info("the egress terms changed after a shard was bound; keeping the binding",
			"claim", client.ObjectKeyFromObject(&claim), "shard", claim.Status.ShardRef.Name)
		if err := r.publishClaimStatus(ctx, &claim, metav1.ConditionFalse,
			cloudv1alpha1.EgressShardClaimReasonTermsChanged,
			fmt.Sprintf("Network %q is bound to egress shard %q under terms this location no longer states; delete this claim to bind under the new ones",
				claim.Spec.Network.Name, claim.Status.ShardRef.Name)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.reportBoundContext(ctx, &networkContext, &claim)
	}

	if claim.Status.ShardRef != nil {
		// Decided once. Nothing here recomputes a binding: a rebinding moves a
		// live VPC's egress to a different source address, which is the value a
		// consumer allow-listed at their destination.
		return ctrl.Result{}, r.reportExistingBinding(ctx, &networkContext, &claim)
	}

	return ctrl.Result{}, r.bind(ctx, &networkContext, &claim, intent.ParametersRef.Name)
}

// claimTerms are the terms a claim would be written with for this location, or
// nil when this controller has nothing to bind.
//
// It reads the instruction and never the class: every field it copies was
// resolved upstream, so nothing here selects a class, picks a default, or
// interprets a parameters reference beyond recognizing whether it is this
// controller's to serve.
func (r *EgressShardClaimReconciler) claimTerms(
	ctx context.Context, networkContext *networkingv1alpha.NetworkContext,
) (*cloudv1alpha1.EgressShardClaimSpec, error) {
	intent := internetEgressIntent(networkContext)
	if intent == nil || intent.Mode != networkingv1alpha.NetworkInternetEgressEnabled {
		return nil, nil
	}
	ref := intent.ParametersRef
	if ref == nil {
		return nil, &bindingRefusedError{
			reason: networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
			message: fmt.Sprintf("Internet egress class %q names no parameters, so which shards serve it is unstated",
				intent.ClassName),
		}
	}
	if ref.Group != cloudv1alpha1.GroupVersion.Group || ref.Kind != cloudv1alpha1.KindEgressShardParameters {
		return nil, nil
	}

	sharing, err := claimSharing(intent.Sharing)
	if err != nil {
		return nil, err
	}
	families, err := claimFamilies(intent.Reach)
	if err != nil {
		return nil, err
	}
	if networkContext.Spec.Network.Name == "" {
		return nil, &bindingRefusedError{
			reason:  networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
			message: "This location names no network, so there is nothing to bind to a shard",
		}
	}
	// Read only to establish that the class this cell was pointed at exists
	// here. Which shards it selects is read at the moment of binding.
	if err := r.Get(ctx, client.ObjectKey{Name: ref.Name},
		&cloudv1alpha1.EgressShardParameters{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &bindingRefusedError{
				reason: cloudv1alpha1.EgressShardClaimReasonParametersUnavailable,
				message: fmt.Sprintf("Internet egress class %q is served by parameters %q, which do not exist in this location",
					intent.ClassName, ref.Name),
			}
		}
		return nil, fmt.Errorf("get EgressShardParameters %s: %w", ref.Name, err)
	}

	return &cloudv1alpha1.EgressShardClaimSpec{
		Network:        cloudv1alpha1.NetworkRef{Name: networkContext.Spec.Network.Name},
		NetworkContext: cloudv1alpha1.NetworkContextRef{Name: networkContext.Name},
		ClassName:      intent.ClassName,
		Sharing:        sharing,
		Families:       families,
	}, nil
}

// claimSharing carries the class's sharing onto the claim.
//
// An unprojected value is refused rather than defaulted. Sharing decides how
// many networks a shard may take, and there is no safe guess: reading it as
// Shared would place a network promised its own address alongside others, and
// reading it as Dedicated would hold a shard open for a network that asked for
// no such thing.
func claimSharing(sharing networkingv1alpha.InternetEgressSharing) (cloudv1alpha1.EgressSharing, error) {
	switch sharing {
	case networkingv1alpha.InternetEgressSharingShared:
		return cloudv1alpha1.EgressSharingShared, nil
	case networkingv1alpha.InternetEgressSharingDedicated:
		return cloudv1alpha1.EgressSharingDedicated, nil
	default:
		return "", &bindingRefusedError{
			reason:  networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
			message: "The serving class's sharing did not reach this location, so how many networks may share a shard is unknown here",
		}
	}
}

// claimFamilies carries the families this location was told to reach.
//
// Only IPv6 is accepted, because it is the only family the instruction can
// carry and the only one a shard is selected for. A family that arrives anyway
// is refused rather than dropped: silently binding a shard that translates
// nothing for it would report egress a consumer does not have.
func claimFamilies(
	reach []networkingv1alpha.IPFamily,
) ([]cloudv1alpha1.InternetEgressAddressFamily, error) {
	if len(reach) == 0 {
		return nil, &bindingRefusedError{
			reason:  networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
			message: "This location was told to reach no address family, so no shard can be selected for it",
		}
	}
	families := make([]cloudv1alpha1.InternetEgressAddressFamily, 0, len(reach))
	for _, family := range reach {
		if family != networkingv1alpha.IPv6Protocol {
			return nil, &bindingRefusedError{
				reason: networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
				message: fmt.Sprintf("This location was told to reach %s destinations, which no shard in it translates",
					family),
			}
		}
		families = append(families, cloudv1alpha1.InternetEgressAddressFamilyIPv6)
	}
	return families, nil
}

// createClaim writes the one claim this location's egress is bound through. It
// is owned by the network context, so a network withdrawn from the cell takes
// its claim with it and the shard it held is released.
func (r *EgressShardClaimReconciler) createClaim(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	terms *cloudv1alpha1.EgressShardClaimSpec,
) error {
	claim := &cloudv1alpha1.EgressShardClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: networkContext.Namespace,
			Name:      networkContext.Name,
		},
		Spec: *terms,
	}
	if err := controllerutil.SetControllerReference(networkContext, claim, r.Scheme); err != nil {
		return fmt.Errorf("set the owner on EgressShardClaim %s: %w", claim.Name, err)
	}
	if err := r.Create(ctx, claim); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create EgressShardClaim %s: %w", client.ObjectKeyFromObject(claim), err)
	}
	logf.FromContext(ctx).Info("claimed an egress shard for a location",
		"claim", client.ObjectKeyFromObject(claim), "network", terms.Network.Name,
		"class", terms.ClassName, "sharing", terms.Sharing)
	return nil
}

// releaseClaim deletes the claim for a location that no longer has egress this
// controller provides. Deleting it is what releases the shard: the consumer set
// is a list of claims, so leaving the shard, and takes the route and the
// address with it.
func (r *EgressShardClaimReconciler) releaseClaim(
	ctx context.Context, claim *cloudv1alpha1.EgressShardClaim, claimFound bool, why string,
) error {
	if !claimFound {
		return nil
	}
	if err := r.Delete(ctx, claim); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("delete EgressShardClaim %s: %w",
			client.ObjectKeyFromObject(claim), err))
	}
	logf.FromContext(ctx).Info("released an egress shard claim",
		"claim", client.ObjectKeyFromObject(claim), "reason", why)
	return nil
}

// bind chooses the shard this network egresses through and records it.
//
// The candidates are the shards the class selects, in name order, and the first
// usable one wins. A refused candidate carries the reason it was refused, so a
// claim that binds nothing says which incompatibility stopped it rather than
// reporting a generic failure. A claim that found no usable shard waits: it is
// not refused, and nothing provisions a shard for it, because a shard is
// unusable until an operator commissions its identifier.
func (r *EgressShardClaimReconciler) bind(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	claim *cloudv1alpha1.EgressShardClaim,
	parametersName string,
) error {
	shards, err := egressShards(ctx, r.Client, parametersName)
	if err != nil {
		return err
	}
	if len(shards) == 0 {
		return r.refuse(ctx, networkContext, claim, &bindingRefusedError{
			reason: cloudv1alpha1.EgressShardClaimReasonNoShardMatchesTheClass,
			message: fmt.Sprintf("No egress shard carries the labels internet egress class %q selects",
				claim.Spec.ClassName),
		})
	}

	var firstRefusal *bindingRefusedError
	for i := range shards {
		refusal := shardRefusal(&shards[i])
		if refusal == nil {
			return r.recordBinding(ctx, networkContext, claim, &shards[i])
		}
		if firstRefusal == nil {
			firstRefusal = refusal
		}
	}
	return r.refuse(ctx, networkContext, claim, firstRefusal)
}

// shardRefusal is why no claim may bind one shard, or nil if it may be bound.
//
// It reads the shard alone. Nothing about the claim asking narrows the set,
// because every claim takes a shared shard; when dedicated capacity is offered,
// this is where a claim's own terms would start to matter.
func shardRefusal(shard *bgpv1alpha1.EgressShard) *bindingRefusedError {
	if shard.Status.ShardSID == "" {
		return &bindingRefusedError{
			reason: cloudv1alpha1.EgressShardClaimReasonNoShardIdentifier,
			message: fmt.Sprintf("Egress shard %q has no identifier a node can route toward, so binding it would carry no packet",
				shard.Name),
		}
	}
	if !shard.DeletionTimestamp.IsZero() {
		return &bindingRefusedError{
			reason: cloudv1alpha1.EgressShardClaimReasonShardTerminating,
			message: fmt.Sprintf("Egress shard %q is being deleted, so it takes no further network",
				shard.Name),
		}
	}
	return nil
}

// recordBinding writes the binding: the shard's finalizer first, then the label
// that makes this claim part of the shard's consumer set, then the binding
// itself.
//
// The order is what keeps a crash between the writes harmless. A finalizer with
// no binding behind it is removed by the reconciler that watches shards, and a
// label with no binding behind it counts as no consumer and is overwritten by
// the next pass. A binding recorded before either would be a network egressing
// through a shard nothing holds open and nothing counts.
func (r *EgressShardClaimReconciler) recordBinding(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	claim *cloudv1alpha1.EgressShardClaim,
	shard *bgpv1alpha1.EgressShard,
) error {
	if err := holdShard(ctx, r.Client, shard); err != nil {
		return err
	}
	if err := r.labelClaim(ctx, claim, shard.Name); err != nil {
		return err
	}

	claim.Status.ShardRef = &cloudv1alpha1.EgressShardReference{
		Namespace: shard.Namespace,
		Name:      shard.Name,
	}
	logf.FromContext(ctx).Info("bound a network's egress to a shard",
		"claim", client.ObjectKeyFromObject(claim), "network", claim.Spec.Network.Name,
		"shard", client.ObjectKeyFromObject(shard), "sharing", claim.Spec.Sharing)

	if err := r.publishClaimStatus(ctx, claim, metav1.ConditionTrue,
		cloudv1alpha1.EgressShardClaimReasonBound,
		fmt.Sprintf("Network %q egresses through egress shard %q", claim.Spec.Network.Name, shard.Name)); err != nil {
		return err
	}
	return r.reportBinding(ctx, networkContext, shard)
}

// labelClaim stamps the shard a claim is bound to, so the shard's consumer set
// is a list query. It is re-asserted on every pass over a bound claim, because
// a label lost to an edit would hide a network from the count that keeps a
// dedicated shard exclusive.
func (r *EgressShardClaimReconciler) labelClaim(
	ctx context.Context, claim *cloudv1alpha1.EgressShardClaim, shardName string,
) error {
	if claim.Labels[cloudv1alpha1.LabelEgressShardClaimShard] == shardName {
		return nil
	}
	patch := client.MergeFrom(claim.DeepCopy())
	if claim.Labels == nil {
		claim.Labels = map[string]string{}
	}
	claim.Labels[cloudv1alpha1.LabelEgressShardClaimShard] = shardName
	if err := r.Patch(ctx, claim, patch); err != nil {
		return fmt.Errorf("label EgressShardClaim %s with its shard: %w",
			client.ObjectKeyFromObject(claim), err)
	}
	return nil
}

// reportExistingBinding says what a binding already made is delivering, on the
// claim and on the network context, and repairs the label the consumer set is
// counted by.
func (r *EgressShardClaimReconciler) reportExistingBinding(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	claim *cloudv1alpha1.EgressShardClaim,
) error {
	if err := r.labelClaim(ctx, claim, claim.Status.ShardRef.Name); err != nil {
		return err
	}
	return r.reportBoundContext(ctx, networkContext, claim)
}

// reportBoundContext reports a binding whose shard has to be read back for it.
func (r *EgressShardClaimReconciler) reportBoundContext(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	claim *cloudv1alpha1.EgressShardClaim,
) error {
	var shard bgpv1alpha1.EgressShard
	key := client.ObjectKey{
		Namespace: claim.Status.ShardRef.Namespace,
		Name:      claim.Status.ShardRef.Name,
	}
	if err := r.Get(ctx, key, &shard); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get the bound EgressShard %s: %w", key, err)
		}
		// Nothing rebinds, so this network has no egress and no second answer
		// coming. The finalizer exists to make this reachable only by someone
		// removing it.
		message := fmt.Sprintf("Egress shard %q no longer exists, and a binding is not remade", key.Name)
		if err := r.publishClaimStatus(ctx, claim, metav1.ConditionFalse,
			cloudv1alpha1.EgressShardClaimReasonShardMissing, message); err != nil {
			return err
		}
		return r.reportContext(ctx, networkContext, metav1.ConditionFalse,
			networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
			egressUnavailableMessage)
	}
	return r.reportBinding(ctx, networkContext, &shard)
}

// reportBinding projects a binding onto the network context condition a
// consumer reads.
//
// Degraded is deliberately never written. It means egress works for some
// declared families and not others, and only one family is accepted anywhere on
// this path, so no state can reach it.
func (r *EgressShardClaimReconciler) reportBinding(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	shard *bgpv1alpha1.EgressShard,
) error {
	if shard.Status.ShardAddressIPv6 == "" {
		return r.reportContext(ctx, networkContext, metav1.ConditionFalse,
			networkingv1alpha.NetworkContextInternetEgressReasonAddressUnavailable,
			"No egress address has been allocated for this location yet")
	}
	return r.reportContext(ctx, networkContext, metav1.ConditionTrue,
		networkingv1alpha.NetworkContextInternetEgressReasonReady,
		fmt.Sprintf("Instances in this location reach the internet, and %s is the address they reach it from",
			shard.Status.ShardAddressIPv6))
}

// refuse records that nothing was bound, and why. The claim stays, unbound,
// and takes the first usable shard that appears.
func (r *EgressShardClaimReconciler) refuse(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	claim *cloudv1alpha1.EgressShardClaim,
	refusal *bindingRefusedError,
) error {
	if err := r.publishClaimStatus(ctx, claim, metav1.ConditionFalse,
		refusal.reason, refusal.message); err != nil {
		return err
	}
	// The context carries the fact about the consumer's network. Which shard
	// refused it, and why, is on the claim, which is an operator's object.
	return r.reportContext(ctx, networkContext, metav1.ConditionFalse,
		networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
		egressUnavailableMessage)
}

func (r *EgressShardClaimReconciler) publishClaimStatus(
	ctx context.Context,
	claim *cloudv1alpha1.EgressShardClaim,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	claim.Status.ObservedGeneration = claim.Generation
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               cloudv1alpha1.ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: claim.Generation,
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return fmt.Errorf("update EgressShardClaim %s status: %w",
			client.ObjectKeyFromObject(claim), err)
	}
	return nil
}

// reportContext writes the readiness a consumer reads for this location.
//
// The reasons were defined with no writer; this is the writer of all of them
// but the class-resolution refusal, which is written upstream where the class
// is read. The message states a fact about the consumer's network and names no
// node, no shard and no other consumer: a consumer cannot act on those, and
// they describe where the platform runs their workload.
func (r *EgressShardClaimReconciler) reportContext(
	ctx context.Context,
	networkContext *networkingv1alpha.NetworkContext,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	condition := metav1.Condition{
		Type:               networkingv1alpha.NetworkContextInternetEgressReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: networkContext.Generation,
	}
	if !meta.SetStatusCondition(&networkContext.Status.Conditions, condition) {
		return nil
	}
	if err := r.Status().Update(ctx, networkContext); err != nil {
		return fmt.Errorf("update NetworkContext %s status: %w",
			client.ObjectKeyFromObject(networkContext), err)
	}
	return nil
}

// egressShards lists the shards a class's parameters select, in name order.
//
// It moved here from the interface controller with binding. The order used to
// be what made a candidate list a function of the matched set alone, so two
// attachments of one VPC could not compute different lists; now it is what
// makes the binding itself deterministic over the set of shards it saw.
func egressShards(
	ctx context.Context, reader client.Reader, parametersName string,
) ([]bgpv1alpha1.EgressShard, error) {
	var parameters cloudv1alpha1.EgressShardParameters
	if err := reader.Get(ctx, client.ObjectKey{Name: parametersName}, &parameters); err != nil {
		return nil, fmt.Errorf("get EgressShardParameters %s: %w", parametersName, err)
	}

	selector, err := metav1.LabelSelectorAsSelector(&parameters.Spec.ShardSelector)
	if err != nil {
		return nil, fmt.Errorf("parse the shard selector on EgressShardParameters %s: %w",
			parameters.Name, err)
	}
	// Only IPv6 is reached, so a shard that translates no IPv6 flow is no
	// candidate however an operator wrote the selector. The family label is
	// matched on presence: absence, not a false value, means the family is
	// unserved, so a shard predating the label never reads as serving one.
	servesIPv6, err := labels.NewRequirement(bgpv1alpha1.LabelEgressShardIPv6, selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("build the IPv6 shard requirement: %w", err)
	}

	var shards bgpv1alpha1.EgressShardList
	if err := reader.List(ctx, &shards,
		client.InNamespace(parameters.Spec.ShardNamespace),
		client.MatchingLabelsSelector{Selector: selector.Add(*servesIPv6)},
	); err != nil {
		return nil, fmt.Errorf("list egress shards for EgressShardParameters %s: %w",
			parameters.Name, err)
	}
	slices.SortFunc(shards.Items, func(a, b bgpv1alpha1.EgressShard) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return shards.Items, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *EgressShardClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.EgressShardClaim{},
			builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
				// A bound claim is never reconsidered on its own events, which
				// is what makes "decided once" a property of the controller
				// rather than a check inside it.
				claim, ok := object.(*cloudv1alpha1.EgressShardClaim)
				return ok && claim.Status.ShardRef == nil
			}))).
		Watches(&networkingv1alpha.NetworkContext{},
			handler.EnqueueRequestsFromMapFunc(claimForNetworkContext)).
		Watches(&bgpv1alpha1.EgressShard{},
			handler.EnqueueRequestsFromMapFunc(r.unboundClaimsForEgressShard)).
		Named("egressshardclaim").
		Complete(r)
}

// claimForNetworkContext maps a location to its one claim, which carries the
// same name.
func claimForNetworkContext(_ context.Context, object client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(object)}}
}

// unboundClaimsForEgressShard wakes the claims still waiting for a shard when
// one arrives, reports its identifier, or leaves.
//
// Only unbound claims are enqueued. A bound one has nothing to recompute, and a
// shard arriving is exactly the moment a claim that was told there was no free
// dedicated shard can stop waiting.
func (r *EgressShardClaimReconciler) unboundClaimsForEgressShard(
	ctx context.Context, _ client.Object,
) []reconcile.Request {
	var claims cloudv1alpha1.EgressShardClaimList
	if err := r.List(ctx, &claims); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(claims.Items))
	for i := range claims.Items {
		if claims.Items[i].Status.ShardRef != nil {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&claims.Items[i]),
		})
	}
	return requests
}
