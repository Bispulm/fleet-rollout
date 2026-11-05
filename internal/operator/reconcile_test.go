package operator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Bispulm/fleet-rollout/api/v1alpha1"
	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	apiclient "github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/signing/signingtest"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

const adminToken = "operator-test-admin-token-0123456789"

// world is a real control plane over HTTP, and a fake Kubernetes API.
type world struct {
	t        *testing.T
	ctx      context.Context
	api      *apiclient.Client
	signer   ed25519.PrivateKey
	kube     client.Client
	fleets   *FleetReconciler
	rollouts *RolloutReconciler
}

func newWorld(t *testing.T) *world {
	t.Helper()
	t.Parallel()
	signer := signingtest.NewKey(t)
	server := httptest.NewServer(api.New(api.Options{
		Logger:     slog.New(slog.DiscardHandler),
		Ready:      func(context.Context) error { return nil },
		Store:      store.New(testdb.New(t)),
		AdminToken: adminToken,
		Trust:      signingtest.Trust(t, signer),
		Artifacts:  artifactstore.NewMemory(),
	}))
	t.Cleanup(server.Close)

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Fleet{}, &v1alpha1.Rollout{}).Build()
	admin := apiclient.New(server.URL, adminToken, nil)
	return &world{
		t: t, ctx: t.Context(), api: admin, signer: signer, kube: kube,
		fleets:   &FleetReconciler{Client: kube, API: admin},
		rollouts: &RolloutReconciler{Client: kube, API: admin},
	}
}

// fleetWithDevices creates a fleet in the control plane, with devices registered.
func (w *world) fleetWithDevices(name string, devices int) {
	w.t.Helper()
	if _, err := w.api.CreateFleet(w.ctx, apiv1.CreateFleetRequest{Name: name}); err != nil {
		w.t.Fatal(err)
	}
	specs := make([]apiv1.DeviceSpec, devices)
	for i := range specs {
		specs[i] = apiv1.DeviceSpec{ID: fmt.Sprintf("%s-%03d", name, i), Platform: "linux/arm64"}
	}
	if _, err := w.api.RegisterDevices(w.ctx, name, specs); err != nil {
		w.t.Fatal(err)
	}
}

// push signs and uploads an artifact.
func (w *world) push(version string, release int64) apiv1.Artifact {
	w.t.Helper()
	content := bytes.Repeat([]byte(version), 512)
	_, envelope := signingtest.Signed(w.t, w.signer, signing.Meta{
		Name: "demo-app", Version: version, Platform: "linux/arm64", ReleaseCounter: release, ChunkSize: 1024,
	}, content)
	artifact, err := w.api.CreateArtifact(w.ctx, envelope)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.api.UploadArtifactContent(w.ctx, artifact.ID, bytes.NewReader(content), int64(len(content))); err != nil {
		w.t.Fatal(err)
	}
	return artifact
}

func rolloutResource(name, fleet, version string) *v1alpha1.Rollout {
	return &v1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(newUUID(name)), Generation: 1},
		Spec: v1alpha1.RolloutSpec{
			FleetRef: fleet,
			Artifact: v1alpha1.ArtifactRef{Name: "demo-app", Version: version, Platform: "linux/arm64"},
			Stages:   []v1alpha1.Stage{{Percent: 50}, {Percent: 100}},
			Safety:   v1alpha1.Safety{MaxFailureRate: "10%"},
		},
	}
}

// newUUID makes a stable, valid UUID from a name, standing in for the UID Kubernetes assigns.
func newUUID(name string) string {
	b := []byte(fmt.Sprintf("%-16.16s", name))
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (w *world) create(obj client.Object) {
	w.t.Helper()
	if err := w.kube.Create(w.ctx, obj); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) reconcileRollout(name string) ctrl.Result {
	w.t.Helper()
	result, err := w.rollouts.Reconcile(w.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		w.t.Fatalf("Reconcile %s: %v", name, err)
	}
	return result
}

func (w *world) getRollout(name string) *v1alpha1.Rollout {
	w.t.Helper()
	var out v1alpha1.Rollout
	if err := w.kube.Get(w.ctx, types.NamespacedName{Name: name}, &out); err != nil {
		w.t.Fatal(err)
	}
	return &out
}

// editSpec changes a Rollout's spec, bumping its generation as the API server would.
func (w *world) editSpec(name string, edit func(*v1alpha1.RolloutSpec)) {
	w.t.Helper()
	got := w.getRollout(name)
	edit(&got.Spec)
	got.Generation++
	if err := w.kube.Update(w.ctx, got); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) controlPlaneState(res *v1alpha1.Rollout) rollout.State {
	w.t.Helper()
	got, err := w.api.GetRollout(w.ctx, string(res.UID))
	if err != nil {
		w.t.Fatal(err)
	}
	return got.State
}

func ready(conditions []metav1.Condition) *metav1.Condition {
	return apimeta.FindStatusCondition(conditions, conditionReady)
}

func TestAFleetIsCreatedInTheControlPlane(t *testing.T) {
	w := newWorld(t)
	w.create(&v1alpha1.Fleet{ObjectMeta: metav1.ObjectMeta{Name: "lab-a", Generation: 1},
		Spec: v1alpha1.FleetSpec{Labels: map[string]string{"site": "lab"}}})
	for range 2 { // The second pass finds what the first made.
		if _, err := w.fleets.Reconcile(w.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "lab-a"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	got, err := w.api.GetFleet(w.ctx, "lab-a")
	if err != nil || got.Labels["site"] != "lab" {
		t.Fatalf("control plane fleet = %+v, %v", got, err)
	}
	var fleet v1alpha1.Fleet
	if err := w.kube.Get(w.ctx, types.NamespacedName{Name: "lab-a"}, &fleet); err != nil {
		t.Fatal(err)
	}
	if fleet.Status.ID != got.ID || ready(fleet.Status.Conditions).Status != metav1.ConditionTrue {
		t.Fatalf("status = %+v", fleet.Status)
	}
}

func TestAFleetThatAlreadyExistsDifferentlyIsReported(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 1)
	w.create(&v1alpha1.Fleet{ObjectMeta: metav1.ObjectMeta{Name: "lab-a", Generation: 1},
		Spec: v1alpha1.FleetSpec{Labels: map[string]string{"site": "lab"}}})
	if _, err := w.fleets.Reconcile(w.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "lab-a"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var fleet v1alpha1.Fleet
	if err := w.kube.Get(w.ctx, types.NamespacedName{Name: "lab-a"}, &fleet); err != nil {
		t.Fatal(err)
	}
	if c := ready(fleet.Status.Conditions); c.Status != metav1.ConditionFalse || c.Reason != reasonLabelsDiffer {
		t.Fatalf("condition = %+v", c)
	}
}

func TestARolloutIsCreatedAndStartedOnce(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 10)
	artifact := w.push("1.4.0", 14)
	res := rolloutResource("demo-app.1.4.0", "lab-a", "1.4.0")
	w.create(res)

	first := w.reconcileRollout(res.Name)
	second := w.reconcileRollout(res.Name)
	if first.RequeueAfter == 0 || second.RequeueAfter == 0 {
		t.Fatalf("results = %+v, %+v; a rollout in progress has to be checked again", first, second)
	}
	got := w.getRollout(res.Name)
	if got.Status.RolloutID != string(res.UID) || got.Status.ArtifactID != artifact.ID || got.Status.Phase != "running" {
		t.Fatalf("status = %+v", got.Status)
	}
	if c := ready(got.Status.Conditions); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("ready = %+v", c)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != finalizer {
		t.Fatalf("finalizers = %v", got.Finalizers)
	}
	cp, err := w.api.GetRollout(w.ctx, string(res.UID))
	if err != nil || cp.Name != "demo-app-1-4-0-"+string(res.UID)[:8] || cp.Revision != 2 {
		t.Fatalf("control plane rollout = %+v, %v; want it created and started exactly once", cp, err)
	}
}

func TestARolloutWaitsForItsArtifact(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 10)
	res := rolloutResource("demo", "lab-a", "1.4.0")
	w.create(res)

	if result := w.reconcileRollout(res.Name); result.RequeueAfter == 0 {
		t.Fatal("a missing artifact has to be checked for again")
	}
	if c := ready(w.getRollout(res.Name).Status.Conditions); c.Status != metav1.ConditionFalse || c.Reason != reasonArtifactNotFound {
		t.Fatalf("condition = %+v", c)
	}

	w.push("1.4.0", 14)
	w.reconcileRollout(res.Name)
	if phase := w.getRollout(res.Name).Status.Phase; phase != "running" {
		t.Fatalf("phase = %q after the artifact arrived", phase)
	}
}

func TestAnUnsatisfiablePolicyIsReportedAndNothingIsCreated(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 10)
	w.push("1.4.0", 14)
	res := rolloutResource("demo", "lab-a", "1.4.0")
	res.Spec.Stages[0].MinObservations = ptr(50) // The first stage only has 5 devices.
	w.create(res)

	w.reconcileRollout(res.Name)
	if c := ready(w.getRollout(res.Name).Status.Conditions); c.Status != metav1.ConditionFalse || c.Reason != reasonInvalid {
		t.Fatalf("condition = %+v", c)
	}
	if _, err := w.api.GetRollout(w.ctx, string(res.UID)); !apiclient.IsStatus(err, 404) {
		t.Fatalf("GetRollout error = %v, want nothing created", err)
	}
}

func TestPausedPausesAndResumes(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 10)
	w.push("1.4.0", 14)
	res := rolloutResource("demo", "lab-a", "1.4.0")
	w.create(res)
	w.reconcileRollout(res.Name)

	for _, tc := range []struct {
		paused bool
		want   rollout.State
	}{{true, rollout.StatePaused}, {false, rollout.StateRunning}} {
		w.editSpec(res.Name, func(spec *v1alpha1.RolloutSpec) { spec.Paused = tc.paused })
		w.reconcileRollout(res.Name)
		if got := w.controlPlaneState(res); got != tc.want {
			t.Fatalf("paused = %v: control plane state = %s", tc.paused, got)
		}
		if phase := w.getRollout(res.Name).Status.Phase; phase != string(tc.want) {
			t.Fatalf("paused = %v: phase = %q", tc.paused, phase)
		}
	}
}

func TestARolloutCreatedPausedWaitsToStart(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 10)
	w.push("1.4.0", 14)
	res := rolloutResource("demo", "lab-a", "1.4.0")
	res.Spec.Paused = true
	w.create(res)

	w.reconcileRollout(res.Name)
	w.reconcileRollout(res.Name)
	if got := w.controlPlaneState(res); got != rollout.StateDraft {
		t.Fatalf("control plane state = %s, want a draft that exposes nobody", got)
	}
	w.editSpec(res.Name, func(spec *v1alpha1.RolloutSpec) { spec.Paused = false })
	w.reconcileRollout(res.Name)
	if got := w.controlPlaneState(res); got != rollout.StateRunning {
		t.Fatalf("control plane state = %s after unpausing", got)
	}
}

// A pause made during an incident, through rolloutctl or the API, mustn't be undone by the
// operator because the resource still says to run. Only a change to the resource acts.
func TestAPauseOrResumeMadeElsewhereStands(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 10)
	w.push("1.4.0", 14)
	res := rolloutResource("demo", "lab-a", "1.4.0")
	w.create(res)
	w.reconcileRollout(res.Name)

	transition := func(action string) {
		t.Helper()
		if _, err := w.api.Transition(w.ctx, string(res.UID), action, apiv1.TransitionRequest{Reason: "by hand"}); err != nil {
			t.Fatal(err)
		}
	}
	transition(apiclient.ActionPause)
	w.reconcileRollout(res.Name)
	if got := w.controlPlaneState(res); got != rollout.StatePaused {
		t.Fatalf("control plane state = %s; the operator undid a pause made elsewhere", got)
	}
	if phase := w.getRollout(res.Name).Status.Phase; phase != "paused" {
		t.Fatalf("phase = %q, want the status to show the pause", phase)
	}

	w.editSpec(res.Name, func(spec *v1alpha1.RolloutSpec) { spec.Paused = true })
	w.reconcileRollout(res.Name)
	transition(apiclient.ActionResume)
	w.reconcileRollout(res.Name)
	if got := w.controlPlaneState(res); got != rollout.StateRunning {
		t.Fatalf("control plane state = %s; the operator undid a resume made elsewhere", got)
	}
}

func TestDeletingARolloutAbortsItAndLetsGo(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 10)
	w.push("1.4.0", 14)
	res := rolloutResource("demo", "lab-a", "1.4.0")
	w.create(res)
	w.reconcileRollout(res.Name)

	if err := w.kube.Delete(w.ctx, w.getRollout(res.Name)); err != nil {
		t.Fatal(err)
	}
	w.reconcileRollout(res.Name)
	cp, err := w.api.GetRollout(w.ctx, string(res.UID))
	if err != nil || cp.State != rollout.StateAborted {
		t.Fatalf("control plane state = %s, %v; want aborted", cp.State, err)
	}
	var gone v1alpha1.Rollout
	if err := w.kube.Get(w.ctx, types.NamespacedName{Name: res.Name}, &gone); err == nil {
		t.Fatalf("the resource is still there, with finalizers %v", gone.Finalizers)
	}
}

func TestASecondRolloutOnABusyFleetWaitsItsTurn(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 10)
	w.push("1.4.0", 14)
	w.push("1.5.0", 15)
	first := rolloutResource("first", "lab-a", "1.4.0")
	second := rolloutResource("second", "lab-a", "1.5.0")
	w.create(first)
	w.create(second)
	w.reconcileRollout(first.Name)

	if result := w.reconcileRollout(second.Name); result.RequeueAfter == 0 {
		t.Fatal("a blocked rollout has to be tried again")
	}
	if c := ready(w.getRollout(second.Name).Status.Conditions); c.Status != metav1.ConditionFalse || c.Reason != reasonBlocked {
		t.Fatalf("condition = %+v", c)
	}

	// Once the first is out of the way, the second starts.
	if err := w.kube.Delete(w.ctx, w.getRollout(first.Name)); err != nil {
		t.Fatal(err)
	}
	w.reconcileRollout(first.Name)
	w.reconcileRollout(second.Name)
	if phase := w.getRollout(second.Name).Status.Phase; phase != "running" {
		t.Fatalf("phase = %q", phase)
	}
}

func TestAControlPlaneThatCantBeReachedIsRetried(t *testing.T) {
	w := newWorld(t)
	w.rollouts.API = apiclient.New("http://127.0.0.1:1", adminToken, nil)
	res := rolloutResource("demo", "lab-a", "1.4.0")
	w.create(res)
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	if _, err := w.rollouts.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: res.Name}}); err == nil {
		t.Fatal("Reconcile succeeded without a control plane")
	}
	if c := ready(w.getRollout(res.Name).Status.Conditions); c.Status != metav1.ConditionFalse || c.Reason != reasonControlPlaneFailing {
		t.Fatalf("condition = %+v", c)
	}
}

func TestARolloutCanRollBackOnBreach(t *testing.T) {
	w := newWorld(t)
	w.fleetWithDevices("lab-a", 10)
	w.push("1.4.0", 14)
	res := rolloutResource("demo", "lab-a", "1.4.0")
	res.Spec.Safety.OnBreach = "Rollback"
	w.create(res)
	w.reconcileRollout(res.Name)

	cp, err := w.api.GetRollout(w.ctx, string(res.UID))
	if err != nil || cp.Policy.Safety.OnBreach != rollout.OnBreachRollback {
		t.Fatalf("control plane policy = %+v, %v", cp.Policy.Safety, err)
	}
}
