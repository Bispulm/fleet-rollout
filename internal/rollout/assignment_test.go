package rollout

import "testing"

func TestAssignmentProgression(t *testing.T) {
	allowed := []struct{ from, to AssignmentState }{
		{AssignmentAssigned, AssignmentDownloading},
		{AssignmentDownloading, AssignmentDownloading}, // Progress updates, and retries after a resume.
		{AssignmentDownloading, AssignmentVerifying},
		{AssignmentVerifying, AssignmentInstalling},
		{AssignmentInstalling, AssignmentHealthCheck},
		{AssignmentHealthCheck, AssignmentSucceeded},
		{AssignmentInstalling, AssignmentReverting},
		{AssignmentHealthCheck, AssignmentReverting},
		{AssignmentReverting, AssignmentFailed},
		{AssignmentAssigned, AssignmentFailed},
		{AssignmentDownloading, AssignmentFailed},
		{AssignmentVerifying, AssignmentFailed},
		{AssignmentInstalling, AssignmentFailed},
		{AssignmentHealthCheck, AssignmentFailed},
		{AssignmentAssigned, AssignmentInstalling}, // Steps may be skipped going forward.
	}
	for _, tc := range allowed {
		if !CanProgress(tc.from, tc.to) {
			t.Errorf("%s to %s should be allowed", tc.from, tc.to)
		}
	}

	rejected := []struct{ from, to AssignmentState }{
		{AssignmentInstalling, AssignmentDownloading}, // Backward.
		{AssignmentHealthCheck, AssignmentInstalling},
		{AssignmentVerifying, AssignmentDownloading},
		{AssignmentReverting, AssignmentHealthCheck},
		{AssignmentReverting, AssignmentSucceeded}, // A revert can't end in success.
		{AssignmentAssigned, AssignmentSucceeded},  // Success requires a health check.
		{AssignmentDownloading, AssignmentSucceeded},
		{AssignmentInstalling, AssignmentSucceeded},
		{AssignmentAssigned, AssignmentReverting}, // Nothing was installed to revert.
		{AssignmentDownloading, AssignmentReverting},
		{AssignmentSucceeded, AssignmentFailed}, // Terminal states stay terminal.
		{AssignmentFailed, AssignmentSucceeded},
		{AssignmentSuperseded, AssignmentDownloading},
		{AssignmentDownloading, AssignmentSuperseded}, // Only the server supersedes.
		{AssignmentDownloading, AssignmentAssigned},   // Only the server assigns.
	}
	for _, tc := range rejected {
		if CanProgress(tc.from, tc.to) {
			t.Errorf("%s to %s should be rejected", tc.from, tc.to)
		}
	}
}

func TestReportableStates(t *testing.T) {
	for _, s := range []AssignmentState{
		AssignmentDownloading, AssignmentVerifying, AssignmentInstalling,
		AssignmentHealthCheck, AssignmentReverting, AssignmentSucceeded, AssignmentFailed,
	} {
		if !s.Reportable() {
			t.Errorf("%s should be reportable", s)
		}
	}
	for _, s := range []AssignmentState{AssignmentAssigned, AssignmentSuperseded, "nonsense", ""} {
		if s.Reportable() {
			t.Errorf("%q should not be reportable", s)
		}
	}
}

func TestDirectives(t *testing.T) {
	cases := []struct {
		rollout    State
		assignment AssignmentState
		want       Directive
	}{
		{StateRunning, AssignmentAssigned, DirectiveProceed},
		{StateSoaking, AssignmentDownloading, DirectiveProceed},
		{StateCompleted, AssignmentInstalling, DirectiveProceed},
		{StatePaused, AssignmentAssigned, DirectiveHold},
		{StatePaused, AssignmentDownloading, DirectiveHold},
		{StatePaused, AssignmentVerifying, DirectiveHold},
		{StatePaused, AssignmentInstalling, DirectiveProceed},
		{StatePaused, AssignmentHealthCheck, DirectiveProceed},
		{StatePaused, AssignmentReverting, DirectiveProceed},
		{StateHalted, AssignmentInstalling, DirectiveProceed},
		{StateAborted, AssignmentHealthCheck, DirectiveProceed},
	}
	for _, tc := range cases {
		if got := DirectiveFor(tc.rollout, tc.assignment); got != tc.want {
			t.Errorf("DirectiveFor(%s, %s) = %s, want %s", tc.rollout, tc.assignment, got, tc.want)
		}
	}
}

func TestInFlightStates(t *testing.T) {
	for _, s := range []AssignmentState{AssignmentInstalling, AssignmentHealthCheck, AssignmentReverting} {
		if !s.InFlight() {
			t.Errorf("%s should be in flight", s)
		}
	}
	for _, s := range []AssignmentState{AssignmentAssigned, AssignmentDownloading, AssignmentVerifying, AssignmentSucceeded, AssignmentFailed, AssignmentSuperseded} {
		if s.InFlight() {
			t.Errorf("%s should not be in flight", s)
		}
	}
}
