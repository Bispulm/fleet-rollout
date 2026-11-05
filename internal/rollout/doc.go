// Package rollout holds the pure rules of a staged rollout: cohort ranking, policy
// validation, and the state machine. It has no database or network dependencies.
// docs/rollout-semantics.md is the contract these rules implement.
package rollout
