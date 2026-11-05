package rollout

import (
	"errors"
	"testing"
)

func TestValidTransitions(t *testing.T) {
	cases := []struct {
		from  State
		event Event
		to    State
	}{
		{StateDraft, EventStart, StateRunning},
		{StateDraft, EventAbort, StateAborted},
		{StateRunning, EventCoverageMet, StateSoaking},
		{StateSoaking, EventSoakPassed, StateRunning},
		{StateSoaking, EventFinalSoakPassed, StateCompleted},
		{StateRunning, EventPause, StatePaused},
		{StateSoaking, EventPause, StatePaused},
		{StateRunning, EventBreach, StateHalted},
		{StateSoaking, EventBreach, StateHalted},
		{StatePaused, EventBreach, StateHalted},
		{StateRunning, EventRollback, StateRollingBack},
		{StateSoaking, EventRollback, StateRollingBack},
		{StatePaused, EventRollback, StateRollingBack},
		{StateHalted, EventRollback, StateRollingBack},
		{StateRunning, EventAbort, StateAborted},
		{StateSoaking, EventAbort, StateAborted},
		{StatePaused, EventAbort, StateAborted},
		{StateHalted, EventAbort, StateAborted},
		{StateRollingBack, EventRollbackDone, StateRolledBack},
		{StateRollingBack, EventRollbackExpired, StateRollbackIncomplete},
		{StateRollbackIncomplete, EventRollback, StateRollingBack},
		{StateRollbackIncomplete, EventAbort, StateAborted},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"/"+string(tc.event), func(t *testing.T) {
			got, err := Next(tc.from, tc.event, "")
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got != tc.to {
				t.Fatalf("Next = %q, want %q", got, tc.to)
			}
		})
	}
}

func TestResumeReturnsToThePausedFromState(t *testing.T) {
	for _, from := range []State{StateRunning, StateSoaking} {
		got, err := Next(StatePaused, EventResume, from)
		if err != nil {
			t.Fatalf("resume to %q: %v", from, err)
		}
		if got != from {
			t.Fatalf("resume = %q, want %q", got, from)
		}
	}
}

func TestResumeRejectsAnUnknownOrigin(t *testing.T) {
	for _, from := range []State{"", StateDraft, StateHalted, StateCompleted} {
		if _, err := Next(StatePaused, EventResume, from); err == nil {
			t.Errorf("resume with origin %q succeeded", from)
		}
	}
}

// Every pair not in the table above must be rejected. This catches a new edge
// being added by accident, since the expected set is written out separately.
func TestAllOtherTransitionsAreRejected(t *testing.T) {
	allowed := map[State][]Event{
		StateDraft:              {EventStart, EventAbort},
		StateRunning:            {EventCoverageMet, EventPause, EventBreach, EventRollback, EventAbort},
		StateSoaking:            {EventSoakPassed, EventFinalSoakPassed, EventPause, EventBreach, EventRollback, EventAbort},
		StatePaused:             {EventResume, EventBreach, EventRollback, EventAbort},
		StateHalted:             {EventRollback, EventAbort},
		StateRollingBack:        {EventRollbackDone, EventRollbackExpired},
		StateRollbackIncomplete: {EventRollback, EventAbort},
		StateCompleted:          {},
		StateAborted:            {},
		StateRolledBack:         {},
	}
	allEvents := []Event{
		EventStart, EventCoverageMet, EventSoakPassed, EventFinalSoakPassed, EventPause, EventResume,
		EventBreach, EventRollback, EventRollbackDone, EventRollbackExpired, EventAbort,
	}
	for from, events := range allowed {
		permitted := make(map[Event]bool, len(events))
		for _, e := range events {
			permitted[e] = true
		}
		for _, event := range allEvents {
			if permitted[event] {
				continue
			}
			_, err := Next(from, event, StateRunning)
			var invalid *InvalidTransitionError
			if !errors.As(err, &invalid) {
				t.Errorf("Next(%q, %q) error = %v, want *InvalidTransitionError", from, event, err)
				continue
			}
			if invalid.From != from || invalid.Event != event {
				t.Errorf("error fields = %+v, want from %q event %q", invalid, from, event)
			}
		}
	}
}

func TestHaltedRolloutsDoNotResume(t *testing.T) {
	if _, err := Next(StateHalted, EventResume, StateRunning); err == nil {
		t.Fatal("a halted rollout resumed")
	}
}

func TestStateClassification(t *testing.T) {
	terminal := map[State]bool{StateCompleted: true, StateAborted: true, StateRolledBack: true}
	active := map[State]bool{
		StateRunning: true, StateSoaking: true, StatePaused: true, StateHalted: true,
		StateRollingBack: true, StateRollbackIncomplete: true,
	}
	for _, s := range []State{
		StateDraft, StateRunning, StateSoaking, StatePaused, StateHalted, StateRollingBack,
		StateRollbackIncomplete, StateCompleted, StateAborted, StateRolledBack,
	} {
		if s.IsTerminal() != terminal[s] {
			t.Errorf("%q IsTerminal = %v", s, s.IsTerminal())
		}
		if s.HoldsFleet() != active[s] {
			t.Errorf("%q HoldsFleet = %v", s, s.HoldsFleet())
		}
	}
}
