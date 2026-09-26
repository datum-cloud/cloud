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
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// egressUnavailableMessage is what a consumer reads when the node serving their
// instance provides no egress. It names no node and no shard: a consumer cannot
// act on either, and each says where the platform runs their workload. The
// cause lives on the claim, which is an operator's object.
const egressUnavailableMessage = "No component serving this instance provides internet egress"

// bindingRefusedError is why one shard may not be recorded, carrying the named
// reason it is reported under.
type bindingRefusedError struct {
	reason  string
	message string
}

func (e *bindingRefusedError) Error() string { return e.message }

// EgressShardClaimReconciler records which shard each attachment egresses
// through: the one on the node the attachment landed on.
//
// It decides nothing. The node installs its route from its own configuration
// the moment the attachment exists, and this runs after the attachment has
// reported its node. The record exists so the binding is readable, so a node
// without a usable shard produces a condition a consumer can see on the
// attachment, and so a later tier that does select among shards binds through
// the same object.
type EgressShardClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcattachments,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.datumapis.com,resources=vpcattachments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=networkcontexts,verbs=get;list;watch
// +kubebuilder:rbac:groups=network.datumapis.com,resources=egressshardclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=network.datumapis.com,resources=egressshardclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.datumapis.com,resources=egressshards,verbs=get;list;watch;update;patch

func (r *EgressShardClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// A claim carries its attachment's name, so one key reads both.
	var attachment cloudv1alpha1.VPCAttachment
	attachmentFound := true
	if err := r.Get(ctx, req.NamespacedName, &attachment); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get VPCAttachment %s: %w", req.NamespacedName, err)
		}
		attachmentFound = false
	}

	var claim bgpv1alpha1.EgressShardClaim
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

	if !attachmentFound || !attachment.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.releaseClaim(ctx, &claim, claimFound, "the attachment is gone")
	}

	terms, err := r.claimTerms(ctx, &attachment)
	if err != nil {
		var refused *bindingRefusedError
		if errors.As(err, &refused) {
			logf.FromContext(ctx).Info("nothing can be recorded for this attachment's egress",
				"attachment", client.ObjectKeyFromObject(&attachment), "reason", refused.reason,
				"cause", refused.message)
			if err := r.releaseClaim(ctx, &claim, claimFound, refused.message); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.reportAttachment(ctx, &attachment, metav1.ConditionFalse,
				networkingv1alpha.NetworkContextInternetEgressReasonUnavailable, egressUnavailableMessage)
		}
		return ctrl.Result{}, err
	}
	if terms == nil {
		// The network reaches nothing, or the attachment has not landed yet.
		// Both are ordinary and neither is this controller's to report on.
		return ctrl.Result{}, r.releaseClaim(ctx, &claim, claimFound,
			"this attachment declares no internet egress, or has not reported its node")
	}

	if !claimFound {
		return ctrl.Result{}, r.createClaim(ctx, &attachment, terms)
	}

	if !equality.Semantic.DeepEqual(claim.Spec, *terms) {
		// The spec is immutable and the attachment moved, most often to another
		// node. The old record describes a node this attachment is no longer on,
		// so it is replaced on the next pass.
		return ctrl.Result{}, r.releaseClaim(ctx, &claim, claimFound,
			"the attachment no longer matches the record")
	}

	if claim.Status.ShardRef != nil {
		return ctrl.Result{}, r.reportExistingBinding(ctx, &attachment, &claim)
	}

	return ctrl.Result{}, r.bind(ctx, &attachment, &claim)
}

// claimTerms are the terms a claim would be written with for this attachment,
// or nil when there is nothing to record yet.
func (r *EgressShardClaimReconciler) claimTerms(
	ctx context.Context, attachment *cloudv1alpha1.VPCAttachment,
) (*bgpv1alpha1.EgressShardClaimSpec, error) {
	if attachment.Status.Node == "" {
		return nil, nil
	}

	var networkContext networkingv1alpha.NetworkContext
	key := client.ObjectKey{Namespace: attachment.Namespace, Name: attachment.Spec.VPC.Name}
	if err := r.Get(ctx, key, &networkContext); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get NetworkContext %s: %w", key, err)
	}
	intent := internetEgressIntent(&networkContext)
	if intent == nil || intent.Mode != networkingv1alpha.NetworkInternetEgressEnabled {
		return nil, nil
	}

	families, err := claimFamilies(intent.Reach)
	if err != nil {
		return nil, err
	}
	return &bgpv1alpha1.EgressShardClaimSpec{
		Attachment: bgpv1alpha1.EgressShardClaimAttachmentRef{Name: attachment.Name},
		VPC:        bgpv1alpha1.EgressShardClaimVPCRef{Name: attachment.Spec.VPC.Name},
		NodeName:   attachment.Status.Node,
		Families:   families,
	}, nil
}

// claimFamilies carries the families the network declared, copied from the
// network rather than stated per attachment, so two attachments of one network
// on one node can never ask for different shards.
//
// Only IPv6 is accepted, because it is the only family the declaration can
// carry. A family that arrives anyway is refused rather than dropped: silently
// recording a shard that translates nothing for it would report egress a
// consumer does not have.
func claimFamilies(
	reach []networkingv1alpha.IPFamily,
) ([]bgpv1alpha1.EgressAddressFamily, error) {
	if len(reach) == 0 {
		return nil, &bindingRefusedError{
			reason:  networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
			message: "The network declares no address family to reach, so no shard can serve it",
		}
	}
	families := make([]bgpv1alpha1.EgressAddressFamily, 0, len(reach))
	for _, family := range reach {
		if family != networkingv1alpha.IPv6Protocol {
			return nil, &bindingRefusedError{
				reason: networkingv1alpha.NetworkContextInternetEgressReasonUnavailable,
				message: fmt.Sprintf("The network declares %s destinations, which no shard translates",
					family),
			}
		}
		families = append(families, bgpv1alpha1.EgressAddressFamilyIPv6)
	}
	return families, nil
}

// createClaim writes the one claim recording this attachment's egress. It is
// owned by the attachment, so an attachment that goes takes its record with it
// and the shard it held is released.
func (r *EgressShardClaimReconciler) createClaim(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	terms *bgpv1alpha1.EgressShardClaimSpec,
) error {
	claim := &bgpv1alpha1.EgressShardClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: attachment.Namespace,
			Name:      attachment.Name,
			Labels:    map[string]string{bgpv1alpha1.LabelEgressShardClaimNode: terms.NodeName},
		},
		Spec: *terms,
	}
	if err := controllerutil.SetControllerReference(attachment, claim, r.Scheme); err != nil {
		return fmt.Errorf("set the owner on EgressShardClaim %s: %w", claim.Name, err)
	}
	if err := r.Create(ctx, claim); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create EgressShardClaim %s: %w", client.ObjectKeyFromObject(claim), err)
	}
	logf.FromContext(ctx).Info("recorded an attachment's egress",
		"claim", client.ObjectKeyFromObject(claim), "node", terms.NodeName)
	return nil
}

// releaseClaim deletes the claim for an attachment that no longer has egress
// to record. Deleting it is what releases the shard: the consumer set is a
// list of claims.
func (r *EgressShardClaimReconciler) releaseClaim(
	ctx context.Context, claim *bgpv1alpha1.EgressShardClaim, claimFound bool, why string,
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

// bind records the shard on the attachment's node, or why none can be.
func (r *EgressShardClaimReconciler) bind(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	claim *bgpv1alpha1.EgressShardClaim,
) error {
	shard, err := egressShardOnNode(ctx, r.Client, claim.Spec.NodeName)
	if err != nil {
		return err
	}
	if shard == nil {
		return r.refuse(ctx, attachment, claim, &bindingRefusedError{
			reason: bgpv1alpha1.EgressShardClaimReasonNoShardOnNode,
			message: fmt.Sprintf("No egress shard names node %q, so nothing on it translates this attachment's traffic",
				claim.Spec.NodeName),
		})
	}
	if refusal := shardRefusal(shard, claim.Spec.Families); refusal != nil {
		return r.refuse(ctx, attachment, claim, refusal)
	}
	return r.recordBinding(ctx, attachment, claim, shard)
}

// shardRefusal is why a shard may not be recorded, or nil if it may be.
func shardRefusal(
	shard *bgpv1alpha1.EgressShard, families []bgpv1alpha1.EgressAddressFamily,
) *bindingRefusedError {
	if !shard.DeletionTimestamp.IsZero() {
		return &bindingRefusedError{
			reason: bgpv1alpha1.EgressShardClaimReasonShardTerminating,
			message: fmt.Sprintf("Egress shard %q is being deleted, so it takes no further attachment",
				shard.Name),
		}
	}
	if shard.Status.ShardSID == "" {
		return &bindingRefusedError{
			reason: bgpv1alpha1.EgressShardClaimReasonShardNotReady,
			message: fmt.Sprintf("Egress shard %q has reported no identifier a node can route toward",
				shard.Name),
		}
	}
	if (shard.Spec.ShardSID != "" && shard.Spec.ShardSID != shard.Status.ShardSID) ||
		(shard.Spec.ShardAddressIPv6 != "" && shard.Status.ShardAddressIPv6 != "" &&
			shard.Spec.ShardAddressIPv6 != shard.Status.ShardAddressIPv6) {
		return &bindingRefusedError{
			reason: bgpv1alpha1.EgressShardClaimReasonShardMismatch,
			message: fmt.Sprintf("Egress shard %q runs an identity other than the one its spec states, so which one serves this node is unknown",
				shard.Name),
		}
	}
	if slices.Contains(families, bgpv1alpha1.EgressAddressFamilyIPv4) &&
		shard.Status.ShardAddressIPv4 == "" {
		return &bindingRefusedError{
			reason: bgpv1alpha1.EgressShardClaimReasonFamilyUnsupported,
			message: fmt.Sprintf("Egress shard %q translates no IPv4 flow, which the network declares it reaches",
				shard.Name),
		}
	}
	return nil
}

// recordBinding writes the record: the shard's finalizer first, then the label
// that makes this claim part of the shard's consumer set, then the record
// itself, so a crash between the writes leaves nothing that reads as bound
// without being held.
func (r *EgressShardClaimReconciler) recordBinding(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	claim *bgpv1alpha1.EgressShardClaim,
	shard *bgpv1alpha1.EgressShard,
) error {
	if err := holdShard(ctx, r.Client, shard); err != nil {
		return err
	}
	if err := r.labelClaim(ctx, claim, shard.Name); err != nil {
		return err
	}

	claim.Status.ShardRef = &bgpv1alpha1.EgressShardClaimShardRef{
		Namespace: shard.Namespace,
		Name:      shard.Name,
	}
	logf.FromContext(ctx).Info("recorded an attachment's egress shard",
		"claim", client.ObjectKeyFromObject(claim), "node", claim.Spec.NodeName,
		"shard", client.ObjectKeyFromObject(shard))

	if err := r.publishClaimStatus(ctx, claim, metav1.ConditionTrue,
		bgpv1alpha1.EgressShardClaimReasonBound,
		fmt.Sprintf("Attachment %q egresses through egress shard %q", claim.Spec.Attachment.Name, shard.Name)); err != nil {
		return err
	}
	return r.reportBinding(ctx, attachment, shard)
}

// labelClaim stamps the shard a claim records, so the shard's consumer set is
// a list query. It is re-asserted on every pass over a bound claim, because a
// label lost to an edit would hide an attachment from the query that holds a
// shard open.
func (r *EgressShardClaimReconciler) labelClaim(
	ctx context.Context, claim *bgpv1alpha1.EgressShardClaim, shardName string,
) error {
	if claim.Labels[bgpv1alpha1.LabelEgressShardClaimShard] == shardName {
		return nil
	}
	patch := client.MergeFrom(claim.DeepCopy())
	if claim.Labels == nil {
		claim.Labels = map[string]string{}
	}
	claim.Labels[bgpv1alpha1.LabelEgressShardClaimShard] = shardName
	if err := r.Patch(ctx, claim, patch); err != nil {
		return fmt.Errorf("label EgressShardClaim %s with its shard: %w",
			client.ObjectKeyFromObject(claim), err)
	}
	return nil
}

// reportExistingBinding says what a record already made is delivering, on the
// claim and on the attachment, and repairs the label the consumer set is
// counted by.
func (r *EgressShardClaimReconciler) reportExistingBinding(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	claim *bgpv1alpha1.EgressShardClaim,
) error {
	if err := r.labelClaim(ctx, claim, claim.Status.ShardRef.Name); err != nil {
		return err
	}

	var shard bgpv1alpha1.EgressShard
	key := client.ObjectKey{
		Namespace: claim.Status.ShardRef.Namespace,
		Name:      claim.Status.ShardRef.Name,
	}
	if err := r.Get(ctx, key, &shard); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get the recorded EgressShard %s: %w", key, err)
		}
		message := fmt.Sprintf("Egress shard %q no longer exists", key.Name)
		if err := r.publishClaimStatus(ctx, claim, metav1.ConditionFalse,
			bgpv1alpha1.EgressShardClaimReasonShardMissing, message); err != nil {
			return err
		}
		return r.reportAttachment(ctx, attachment, metav1.ConditionFalse,
			networkingv1alpha.NetworkContextInternetEgressReasonUnavailable, egressUnavailableMessage)
	}
	return r.reportBinding(ctx, attachment, &shard)
}

// reportBinding projects a recorded shard onto the attachment condition a
// consumer reads.
//
// Degraded is deliberately never written. It means egress works for some
// declared families and not others, and only one family is accepted anywhere
// on this path, so no state can reach it.
func (r *EgressShardClaimReconciler) reportBinding(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	shard *bgpv1alpha1.EgressShard,
) error {
	if shard.Status.ShardAddressIPv6 == "" {
		return r.reportAttachment(ctx, attachment, metav1.ConditionFalse,
			networkingv1alpha.NetworkContextInternetEgressReasonAddressUnavailable,
			"No egress address has been allocated for the node serving this instance yet")
	}
	return r.reportAttachment(ctx, attachment, metav1.ConditionTrue,
		networkingv1alpha.NetworkContextInternetEgressReasonReady,
		fmt.Sprintf("This instance reaches the internet, and %s is the address it reaches it from",
			shard.Status.ShardAddressIPv6))
}

// refuse records that nothing was recorded, and why. The claim stays, unbound,
// and takes the shard the moment one names its node and is usable.
func (r *EgressShardClaimReconciler) refuse(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	claim *bgpv1alpha1.EgressShardClaim,
	refusal *bindingRefusedError,
) error {
	if err := r.publishClaimStatus(ctx, claim, metav1.ConditionFalse,
		refusal.reason, refusal.message); err != nil {
		return err
	}
	// The attachment carries the fact about the consumer's instance. Which
	// shard refused it, and why, is on the claim, which is an operator's object.
	return r.reportAttachment(ctx, attachment, metav1.ConditionFalse,
		networkingv1alpha.NetworkContextInternetEgressReasonUnavailable, egressUnavailableMessage)
}

func (r *EgressShardClaimReconciler) publishClaimStatus(
	ctx context.Context,
	claim *bgpv1alpha1.EgressShardClaim,
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

// reportAttachment writes the egress readiness a consumer reads for this
// instance, on the attachment the interface's status is read from.
//
// It is patched rather than updated. The attachment's status has another
// writer, the controller that renders it, and a whole-object update from a
// copy read before that write would put stale values back over its fields.
func (r *EgressShardClaimReconciler) reportAttachment(
	ctx context.Context,
	attachment *cloudv1alpha1.VPCAttachment,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	condition := metav1.Condition{
		Type:               cloudv1alpha1.ConditionTypeInternetEgressReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: attachment.Generation,
	}
	patch := client.MergeFrom(attachment.DeepCopy())
	if !meta.SetStatusCondition(&attachment.Status.Conditions, condition) {
		return nil
	}
	if err := r.Status().Patch(ctx, attachment, patch); err != nil {
		return fmt.Errorf("report internet egress on VPCAttachment %s: %w",
			client.ObjectKeyFromObject(attachment), err)
	}
	return nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *EgressShardClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bgpv1alpha1.EgressShardClaim{},
			builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
				// A bound claim is never reconsidered on its own events. It is
				// re-read when its attachment or its shard changes.
				claim, ok := object.(*bgpv1alpha1.EgressShardClaim)
				return ok && claim.Status.ShardRef == nil
			}))).
		Watches(&cloudv1alpha1.VPCAttachment{},
			handler.EnqueueRequestsFromMapFunc(claimForAttachment)).
		Watches(&networkingv1alpha.NetworkContext{},
			handler.EnqueueRequestsFromMapFunc(r.claimsForNetworkContext)).
		Watches(&bgpv1alpha1.EgressShard{},
			handler.EnqueueRequestsFromMapFunc(r.claimsForEgressShard)).
		Named("egressshardclaim").
		Complete(r)
}

// claimForAttachment maps an attachment to its one claim, which carries the
// same name.
func claimForAttachment(_ context.Context, object client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(object)}}
}

// claimsForNetworkContext wakes every attachment of a network when its
// declaration changes, so a network disabled releases its records.
func (r *EgressShardClaimReconciler) claimsForNetworkContext(
	ctx context.Context, object client.Object,
) []reconcile.Request {
	var attachments cloudv1alpha1.VPCAttachmentList
	if err := r.List(ctx, &attachments, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(attachments.Items))
	for i := range attachments.Items {
		if attachments.Items[i].Spec.VPC.Name != object.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&attachments.Items[i]),
		})
	}
	return requests
}

// claimsForEgressShard wakes the claims on a shard's node when it arrives,
// reports its identity, or leaves, and the claims recorded against it so a
// missing or draining shard is reported.
func (r *EgressShardClaimReconciler) claimsForEgressShard(
	ctx context.Context, object client.Object,
) []reconcile.Request {
	shard, ok := object.(*bgpv1alpha1.EgressShard)
	if !ok {
		return nil
	}
	var claims bgpv1alpha1.EgressShardClaimList
	if err := r.List(ctx, &claims); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		onNode := claim.Spec.NodeName == shard.Spec.TargetRef.Name
		recorded := claim.Status.ShardRef != nil && claim.Status.ShardRef.Name == shard.Name
		if !onNode && !recorded {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
	}
	return requests
}
