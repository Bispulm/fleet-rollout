package operator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Bispulm/fleet-rollout/api/v1alpha1"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	apiclient "github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

const (
	// finalizer makes deleting a Rollout abort the rollout it started, if it's unfinished.
	finalizer = "rollouts.bispulm.github.io/abort-on-delete"

	// conditionReady says whether the resource exists in the control plane, and is in sync.
	conditionReady = "Ready"

	// activeResync is how often a rollout in progress is checked. The control plane moves it
	// along without telling the operator.
	activeResync = 10 * time.Second
	// waitResync is how often something a person has to fix is checked again.
	waitResync = 30 * time.Second
)

// The reasons a resource isn't ready.
const (
	reasonInSync              = "InSync"
	reasonArtifactNotFound    = "ArtifactNotFound"
	reasonFleetNotFound       = "FleetNotFound"
	reasonInvalid             = "Invalid"
	reasonBlocked             = "Blocked"
	reasonLabelsDiffer        = "LabelsDiffer"
	reasonControlPlaneFailing = "ControlPlaneUnavailable"
)

// FleetReconciler makes sure each Fleet exists in the control plane.
type FleetReconciler struct {
	Client client.Client
	API    *apiclient.Client
}

// Reconcile creates the fleet if it's missing, and reports its ID.
func (r *FleetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var fleet v1alpha1.Fleet
	if err := r.Client.Get(ctx, req.NamespacedName, &fleet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !fleet.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	original := fleet.DeepCopy()

	got, err := r.API.GetFleet(ctx, fleet.Name)
	if apiclient.IsStatus(err, http.StatusNotFound) {
		got, err = r.API.CreateFleet(ctx, apiv1.CreateFleetRequest{Name: fleet.Name, Labels: fleet.Spec.Labels})
	}
	if err != nil {
		return r.fail(ctx, &fleet, original, err)
	}

	fleet.Status.ID = got.ID
	fleet.Status.ObservedGeneration = fleet.Generation
	if labelsEqual(got.Labels, fleet.Spec.Labels) {
		setReady(&fleet.Status.Conditions, fleet.Generation, metav1.ConditionTrue, reasonInSync, "The fleet exists in the control plane.")
	} else {
		setReady(&fleet.Status.Conditions, fleet.Generation, metav1.ConditionFalse, reasonLabelsDiffer,
			"The fleet already exists in the control plane with different labels, which can't be changed.")
	}
	return ctrl.Result{}, r.Client.Status().Patch(ctx, &fleet, client.MergeFrom(original))
}

func (r *FleetReconciler) fail(ctx context.Context, fleet, original *v1alpha1.Fleet, err error) (ctrl.Result, error) {
	reason, retry := classify(err)
	setReady(&fleet.Status.Conditions, fleet.Generation, metav1.ConditionFalse, reason, err.Error())
	if patchErr := r.Client.Status().Patch(ctx, fleet, client.MergeFrom(original)); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	if retry {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: waitResync}, nil
}

// SetupWithManager registers the reconciler.
func (r *FleetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Fleet{}).Named("fleet").Complete(r)
}

// RolloutReconciler drives each Rollout through the control plane: it creates and starts the
// rollout, pauses and resumes it, aborts it when the resource is deleted, and keeps the status
// current.
type RolloutReconciler struct {
	Client client.Client
	API    *apiclient.Client
}

// Reconcile moves the control plane toward what the resource asks for.
func (r *RolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var res v1alpha1.Rollout
	if err := r.Client.Get(ctx, req.NamespacedName, &res); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !res.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &res)
	}
	if controllerutil.AddFinalizer(&res, finalizer) {
		if err := r.Client.Update(ctx, &res); err != nil {
			return ctrl.Result{}, err
		}
	}
	original := res.DeepCopy()

	// The resource's UID is the rollout's ID, so creating is idempotent across retries and
	// restarts, and a resource always finds its own rollout.
	id := string(res.UID)
	current, err := r.API.GetRollout(ctx, id)
	if apiclient.IsStatus(err, http.StatusNotFound) {
		var result *ctrl.Result
		current, result, err = r.create(ctx, &res, original)
		if result != nil || err != nil {
			return derefResult(result), err
		}
	}
	if err != nil {
		return r.fail(ctx, &res, original, err)
	}

	current, err = r.steer(ctx, &res, current)
	if err != nil {
		return r.fail(ctx, &res, original, err)
	}

	conditions := res.Status.Conditions
	res.Status = statusFrom(current)
	res.Status.Conditions = conditions
	res.Status.ObservedGeneration = res.Generation
	setReady(&res.Status.Conditions, res.Generation, metav1.ConditionTrue, reasonInSync, "The rollout exists in the control plane.")
	if err := r.Client.Status().Patch(ctx, &res, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, err
	}
	if current.State.IsTerminal() {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: activeResync}, nil
}

// create makes the rollout in the control plane. A non-nil result means it couldn't, for a
// reason the status now explains, and that's what to return.
func (r *RolloutReconciler) create(ctx context.Context, res, original *v1alpha1.Rollout) (apiv1.Rollout, *ctrl.Result, error) {
	artifact, found, err := r.API.FindArtifact(ctx, res.Spec.Artifact.Name, res.Spec.Artifact.Platform, res.Spec.Artifact.Version)
	if err != nil {
		result, err := r.fail(ctx, res, original, err)
		return apiv1.Rollout{}, &result, err
	}
	if !found {
		result, err := r.wait(ctx, res, original, reasonArtifactNotFound, fmt.Sprintf(
			"No artifact %s %s for %s has been pushed.", res.Spec.Artifact.Name, res.Spec.Artifact.Version, res.Spec.Artifact.Platform))
		return apiv1.Rollout{}, &result, err
	}
	policy, err := policyFor(res.Spec)
	if err != nil {
		result, err := r.wait(ctx, res, original, reasonInvalid, err.Error())
		return apiv1.Rollout{}, &result, err
	}
	created, err := r.API.CreateRollout(ctx, res.Spec.FleetRef, apiv1.CreateRolloutRequest{
		ID: string(res.UID), Name: controlPlaneName(res.Name, res.UID), ArtifactID: artifact.ID, Policy: policy,
	})
	if err != nil {
		result, err := r.fail(ctx, res, original, err)
		return apiv1.Rollout{}, &result, err
	}
	return created, nil, nil
}

// steer starts the rollout, and pauses or resumes it to match the resource.
func (r *RolloutReconciler) steer(ctx context.Context, res *v1alpha1.Rollout, current apiv1.Rollout) (apiv1.Rollout, error) {
	var action, reason string
	switch {
	case current.State == rollout.StateDraft:
		action, reason = apiclient.ActionStart, "started by the Rollout resource"
	case res.Spec.Paused && (current.State == rollout.StateRunning || current.State == rollout.StateSoaking):
		action, reason = apiclient.ActionPause, "paused by the Rollout resource"
	case !res.Spec.Paused && current.State == rollout.StatePaused:
		action, reason = apiclient.ActionResume, "resumed by the Rollout resource"
	default:
		return current, nil
	}
	// The revision makes the change conditional on nothing else having moved the rollout.
	revision := current.Revision
	return r.API.Transition(ctx, current.ID, action, apiv1.TransitionRequest{ExpectedRevision: &revision, Reason: reason})
}

// finalize aborts the rollout if it's unfinished, and lets the resource go.
func (r *RolloutReconciler) finalize(ctx context.Context, res *v1alpha1.Rollout) error {
	if !controllerutil.ContainsFinalizer(res, finalizer) {
		return nil
	}
	current, err := r.API.GetRollout(ctx, string(res.UID))
	switch {
	case apiclient.IsStatus(err, http.StatusNotFound):
	case err != nil:
		return err
	case !current.State.IsTerminal():
		_, err := r.API.Transition(ctx, current.ID, apiclient.ActionAbort,
			apiv1.TransitionRequest{Reason: "the Rollout resource was deleted"})
		if err != nil && !apiclient.IsCode(err, apiv1.CodeInvalidTransition) {
			return err
		}
	}
	controllerutil.RemoveFinalizer(res, finalizer)
	return r.Client.Update(ctx, res)
}

// wait records something a person has to fix, and checks again later.
func (r *RolloutReconciler) wait(ctx context.Context, res, original *v1alpha1.Rollout, reason, message string) (ctrl.Result, error) {
	res.Status.ObservedGeneration = res.Generation
	setReady(&res.Status.Conditions, res.Generation, metav1.ConditionFalse, reason, message)
	if err := r.Client.Status().Patch(ctx, res, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: waitResync}, nil
}

func (r *RolloutReconciler) fail(ctx context.Context, res, original *v1alpha1.Rollout, err error) (ctrl.Result, error) {
	reason, retry := classify(err)
	result, patchErr := r.wait(ctx, res, original, reason, err.Error())
	if patchErr != nil {
		return result, patchErr
	}
	if retry {
		return ctrl.Result{}, err
	}
	return result, nil
}

// SetupWithManager registers the reconciler.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Rollout{}).Named("rollout").Complete(r)
}

// classify turns a control plane error into a condition reason, and whether retrying soon could
// help.
func classify(err error) (reason string, retry bool) {
	var apiErr *apiclient.APIError
	switch {
	case !errors.As(err, &apiErr), apiclient.IsRetryable(err):
		return reasonControlPlaneFailing, true
	case apiclient.IsCode(err, apiv1.CodeRevisionMismatch), apiclient.IsCode(err, apiv1.CodeInvalidTransition):
		// Something else moved the rollout first. Looking again sorts it out.
		return reasonBlocked, true
	case apiErr.Problem.Status == http.StatusNotFound:
		return reasonFleetNotFound, false
	case apiErr.Problem.Status == http.StatusConflict:
		return reasonBlocked, false
	default:
		return reasonInvalid, false
	}
}

func setReady(conditions *[]metav1.Condition, generation int64, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type: conditionReady, Status: status, Reason: reason, Message: message, ObservedGeneration: generation,
	})
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func derefResult(r *ctrl.Result) ctrl.Result {
	if r == nil {
		return ctrl.Result{}
	}
	return *r
}
