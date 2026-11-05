package rollout

// AssignmentState is how far a device has got with an assignment. The values match
// a check constraint in the database schema.
type AssignmentState string

// The assignment states. Superseded is set only by the server.
const (
	AssignmentAssigned    AssignmentState = "assigned"
	AssignmentDownloading AssignmentState = "downloading"
	AssignmentVerifying   AssignmentState = "verifying"
	AssignmentInstalling  AssignmentState = "installing"
	AssignmentHealthCheck AssignmentState = "health_check"
	AssignmentReverting   AssignmentState = "reverting"
	AssignmentSucceeded   AssignmentState = "succeeded"
	AssignmentFailed      AssignmentState = "failed"
	AssignmentSuperseded  AssignmentState = "superseded"
)

// IsTerminal reports whether an assignment in this state is finished.
func (s AssignmentState) IsTerminal() bool {
	return s == AssignmentSucceeded || s == AssignmentFailed || s == AssignmentSuperseded
}

// InFlight reports whether a device in this state has started changing itself, and
// so can't be recalled. It has to run to the end and report.
func (s AssignmentState) InFlight() bool {
	return s == AssignmentInstalling || s == AssignmentHealthCheck || s == AssignmentReverting
}

// Reportable reports whether a device may report this state.
func (s AssignmentState) Reportable() bool {
	switch s {
	case AssignmentDownloading, AssignmentVerifying, AssignmentInstalling,
		AssignmentHealthCheck, AssignmentReverting, AssignmentSucceeded, AssignmentFailed:
		return true
	default:
		return false
	}
}

// forwardRank orders the steps a healthy assignment goes through.
var forwardRank = map[AssignmentState]int{
	AssignmentAssigned:    0,
	AssignmentDownloading: 1,
	AssignmentVerifying:   2,
	AssignmentInstalling:  3,
	AssignmentHealthCheck: 4,
}

// CanProgress reports whether a report of state to applies to an assignment in
// state from. Progress only moves forward, though it may skip steps, since a device
// can finish a step before the report of an earlier one gets through.
func CanProgress(from, to AssignmentState) bool {
	if from.IsTerminal() || !to.Reportable() {
		return false
	}
	switch to {
	case AssignmentFailed:
		return true
	case AssignmentSucceeded:
		// A device can't claim success without having checked its health.
		return from == AssignmentHealthCheck
	case AssignmentReverting:
		// Reverting needs something installed to revert from.
		return from == AssignmentInstalling || from == AssignmentHealthCheck || from == AssignmentReverting
	}
	if from == AssignmentReverting {
		return false
	}
	return forwardRank[to] >= forwardRank[from]
}

// Why the server recorded a report without applying it.
const (
	IgnoredAssignmentFinished = "assignment_finished"
	IgnoredOutOfOrder         = "out_of_order"
	IgnoredInvalidProgression = "invalid_progression"
	// IgnoredOnHold refuses to let a device start installing while its assignment is on
	// hold. The assignment stays open, and the device may try again once it's released.
	IgnoredOnHold = "on_hold"
)

// Directive tells a device what to do with its open assignment.
type Directive string

// The directives.
const (
	DirectiveProceed Directive = "proceed"
	DirectiveHold    Directive = "hold"
)

// DirectiveFor returns the directive for an open assignment. Only a pause holds
// anything: halting, aborting, and completing all supersede assignments that haven't
// reached installation in the same transaction, so any still open are in flight and
// must finish.
func DirectiveFor(rollout State, assignment AssignmentState) Directive {
	if rollout == StatePaused && !assignment.InFlight() {
		return DirectiveHold
	}
	return DirectiveProceed
}
