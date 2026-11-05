package rollout

import "fmt"

// State is a rollout's lifecycle state. The values match a check constraint in
// the database schema.
type State string

// The rollout states. See docs/rollout-semantics.md for what each one means.
const (
	StateDraft              State = "draft"
	StateRunning            State = "running"
	StateSoaking            State = "soaking"
	StatePaused             State = "paused"
	StateHalted             State = "halted"
	StateRollingBack        State = "rolling_back"
	StateRollbackIncomplete State = "rollback_incomplete"
	StateRolledBack         State = "rolled_back"
	StateCompleted          State = "completed"
	StateAborted            State = "aborted"
)

// IsTerminal reports whether a rollout in this state can never change again.
func (s State) IsTerminal() bool {
	return s == StateCompleted || s == StateAborted || s == StateRolledBack
}

// HoldsFleet reports whether a rollout in this state blocks another from starting
// on the same fleet. Drafts don't, since they have exposed nobody.
func (s State) HoldsFleet() bool {
	return s != StateDraft && !s.IsTerminal()
}

// Event is something that can move a rollout between states.
type Event string

// The rollout events.
const (
	EventStart           Event = "start"
	EventCoverageMet     Event = "coverage_met"
	EventSoakPassed      Event = "soak_passed"
	EventFinalSoakPassed Event = "final_soak_passed"
	EventPause           Event = "pause"
	EventResume          Event = "resume"
	EventBreach          Event = "breach"
	EventRollback        Event = "rollback"
	EventRollbackDone    Event = "rollback_done"
	EventRollbackExpired Event = "rollback_expired"
	EventAbort           Event = "abort"
)

// InvalidTransitionError reports an event that isn't allowed in a state.
type InvalidTransitionError struct {
	From  State
	Event Event
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("event %q is not allowed in state %q", e.Event, e.From)
}

var transitions = map[State]map[Event]State{
	StateDraft: {
		EventStart: StateRunning,
		EventAbort: StateAborted,
	},
	StateRunning: {
		EventCoverageMet: StateSoaking,
		EventPause:       StatePaused,
		EventBreach:      StateHalted,
		EventRollback:    StateRollingBack,
		EventAbort:       StateAborted,
	},
	StateSoaking: {
		EventSoakPassed:      StateRunning,
		EventFinalSoakPassed: StateCompleted,
		EventPause:           StatePaused,
		EventBreach:          StateHalted,
		EventRollback:        StateRollingBack,
		EventAbort:           StateAborted,
	},
	StatePaused: {
		EventBreach:   StateHalted,
		EventRollback: StateRollingBack,
		EventAbort:    StateAborted,
	},
	StateHalted: {
		EventRollback: StateRollingBack,
		EventAbort:    StateAborted,
	},
	StateRollingBack: {
		EventRollbackDone:    StateRolledBack,
		EventRollbackExpired: StateRollbackIncomplete,
	},
	StateRollbackIncomplete: {
		EventRollback: StateRollingBack,
		EventAbort:    StateAborted,
	},
}

// Next returns the state an event leads to. pausedFrom is the state a paused
// rollout came from, and it only matters for EventResume.
func Next(from State, event Event, pausedFrom State) (State, error) {
	if from == StatePaused && event == EventResume {
		if pausedFrom != StateRunning && pausedFrom != StateSoaking {
			return "", fmt.Errorf("rollout paused from unexpected state %q", pausedFrom)
		}
		return pausedFrom, nil
	}
	to, ok := transitions[from][event]
	if !ok {
		return "", &InvalidTransitionError{From: from, Event: event}
	}
	return to, nil
}
