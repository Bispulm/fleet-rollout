package rollout

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Basis-point limits and policy bounds. 10,000 basis points is 100%.
const (
	FullBPS = 10000

	maxStages         = 20
	maxPolicyDuration = 30 * 24 * time.Hour
	minStaleAfter     = time.Second
	maxExcludeDevices = 10000
)

// Defaults used when a spec leaves a field out. Normalize copies them into the
// frozen policy, so changing them later can't alter an existing rollout.
const (
	defaultSoak                = 10 * time.Minute
	defaultDeadline            = time.Hour
	defaultMinObservations     = 1
	defaultRequiredCoverageBPS = 9000
	defaultMaxUnknownBPS       = 500
	defaultStaleAfter          = 15 * time.Minute
)

// OnBreach is what the engine does when a safety threshold is crossed.
type OnBreach string

// The supported breach actions.
const (
	OnBreachHalt     OnBreach = "halt"
	OnBreachRollback OnBreach = "rollback"
)

// Duration is a time.Duration that encodes as a string such as "10m0s".
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"10m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// PolicySpec is a policy as a client submits it. Pointer fields distinguish an
// omitted value from an explicit zero, which is meaningful for limits and soak.
type PolicySpec struct {
	Stages         []StageSpec       `json:"stages"`
	Safety         SafetySpec        `json:"safety"`
	Selector       map[string]string `json:"selector,omitempty"`
	ExcludeDevices []string          `json:"exclude_devices,omitempty"`
}

// StageSpec is one stage as a client submits it.
type StageSpec struct {
	Percent         int       `json:"percent"`
	Soak            *Duration `json:"soak,omitempty"`
	Deadline        *Duration `json:"deadline,omitempty"`
	MinObservations *int      `json:"min_observations,omitempty"`
}

// SafetySpec holds the safety limits as a client submits them. MaxFailureBPS has
// no default, because a rollout must state what failure rate it tolerates.
type SafetySpec struct {
	MaxFailureBPS        *int      `json:"max_failure_bps"`
	RolloutMaxFailureBPS *int      `json:"rollout_max_failure_bps,omitempty"`
	RequiredCoverageBPS  *int      `json:"required_coverage_bps,omitempty"`
	MaxUnknownBPS        *int      `json:"max_unknown_bps,omitempty"`
	StaleAfter           *Duration `json:"stale_after,omitempty"`
	OnBreach             OnBreach  `json:"on_breach,omitempty"`
}

// Policy is the fully resolved, immutable policy stored with a rollout. Its JSON
// encoding is canonical: struct fields keep their order, and maps sort by key.
type Policy struct {
	Stages         []Stage           `json:"stages"`
	Safety         Safety            `json:"safety"`
	Selector       map[string]string `json:"selector"`
	ExcludeDevices []string          `json:"exclude_devices"`
}

// Stage is one resolved stage.
type Stage struct {
	Percent         int      `json:"percent"`
	Soak            Duration `json:"soak"`
	Deadline        Duration `json:"deadline"`
	MinObservations int      `json:"min_observations"`
}

// Safety holds the resolved limits.
type Safety struct {
	MaxFailureBPS        int      `json:"max_failure_bps"`
	RolloutMaxFailureBPS int      `json:"rollout_max_failure_bps"`
	RequiredCoverageBPS  int      `json:"required_coverage_bps"`
	MaxUnknownBPS        int      `json:"max_unknown_bps"`
	StaleAfter           Duration `json:"stale_after"`
	OnBreach             OnBreach `json:"on_breach"`
}

// Violation is one reason a policy can't be accepted.
type Violation struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// PolicyError lists every reason a policy was rejected.
type PolicyError struct {
	Violations []Violation
}

func (e *PolicyError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = v.Field + ": " + v.Message
	}
	return "invalid rollout policy: " + strings.Join(parts, "; ")
}

// violations collects problems found during validation.
type violations []Violation

func (v *violations) addf(field, format string, args ...any) {
	*v = append(*v, Violation{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (v violations) err() error {
	if len(v) == 0 {
		return nil
	}
	return &PolicyError{Violations: v}
}

// Normalize resolves defaults and validates everything that doesn't depend on
// fleet size. Use Plan to check a policy against a specific membership.
func (s PolicySpec) Normalize() (Policy, error) {
	var problems violations

	safety := s.Safety.resolve(&problems)
	stages := s.resolveStages(safety.StaleAfter.Std(), &problems)

	if !ValidLabels(s.Selector) {
		problems.addf("selector", "must have at most %d entries of non-empty letters, digits, '.', '_', '/', and '-'", MaxLabels)
	}
	selector := map[string]string{}
	for k, v := range s.Selector {
		selector[k] = v
	}

	if len(s.ExcludeDevices) > maxExcludeDevices {
		problems.addf("exclude_devices", "must have at most %d entries", maxExcludeDevices)
	}
	seen := make(map[string]bool, len(s.ExcludeDevices))
	exclude := make([]string, 0, len(s.ExcludeDevices))
	for _, id := range s.ExcludeDevices {
		switch {
		case !ValidDeviceID(id):
			problems.addf("exclude_devices", "%q is not a valid device ID", id)
		case seen[id]:
			problems.addf("exclude_devices", "%q is listed twice", id)
		default:
			seen[id] = true
			exclude = append(exclude, id)
		}
	}

	if err := problems.err(); err != nil {
		return Policy{}, err
	}
	return Policy{Stages: stages, Safety: safety, Selector: selector, ExcludeDevices: exclude}, nil
}

func (s SafetySpec) resolve(problems *violations) Safety {
	checkBPS := func(field string, value, minimum int) {
		if value < minimum || value > FullBPS {
			problems.addf(field, "must be between %d and %d basis points, got %d", minimum, FullBPS, value)
		}
	}

	var out Safety
	if s.MaxFailureBPS == nil {
		problems.addf("safety.max_failure_bps", "is required")
	} else {
		out.MaxFailureBPS = *s.MaxFailureBPS
		checkBPS("safety.max_failure_bps", out.MaxFailureBPS, 0)
	}

	out.RolloutMaxFailureBPS = out.MaxFailureBPS
	if s.RolloutMaxFailureBPS != nil {
		out.RolloutMaxFailureBPS = *s.RolloutMaxFailureBPS
		checkBPS("safety.rollout_max_failure_bps", out.RolloutMaxFailureBPS, 0)
	}

	out.RequiredCoverageBPS = defaultRequiredCoverageBPS
	if s.RequiredCoverageBPS != nil {
		out.RequiredCoverageBPS = *s.RequiredCoverageBPS
		checkBPS("safety.required_coverage_bps", out.RequiredCoverageBPS, 1)
	}

	out.MaxUnknownBPS = defaultMaxUnknownBPS
	if s.MaxUnknownBPS != nil {
		out.MaxUnknownBPS = *s.MaxUnknownBPS
		checkBPS("safety.max_unknown_bps", out.MaxUnknownBPS, 0)
	}

	out.StaleAfter = Duration(defaultStaleAfter)
	if s.StaleAfter != nil {
		out.StaleAfter = *s.StaleAfter
		if d := out.StaleAfter.Std(); d < minStaleAfter || d > maxPolicyDuration {
			problems.addf("safety.stale_after", "must be between %s and %s, got %s", minStaleAfter, maxPolicyDuration, d)
		}
	}

	out.OnBreach = OnBreachHalt
	if s.OnBreach != "" {
		out.OnBreach = s.OnBreach
		if out.OnBreach != OnBreachHalt && out.OnBreach != OnBreachRollback {
			problems.addf("safety.on_breach", "must be %q or %q, got %q", OnBreachHalt, OnBreachRollback, out.OnBreach)
		}
	}
	return out
}

func (s PolicySpec) resolveStages(staleAfter time.Duration, problems *violations) []Stage {
	if len(s.Stages) == 0 {
		problems.addf("stages", "at least one stage is required")
		return nil
	}
	if len(s.Stages) > maxStages {
		problems.addf("stages", "at most %d stages are allowed, got %d", maxStages, len(s.Stages))
		return nil
	}

	stages := make([]Stage, len(s.Stages))
	for i, spec := range s.Stages {
		field := fmt.Sprintf("stages[%d]", i)
		stage := Stage{
			Percent:         spec.Percent,
			Soak:            Duration(defaultSoak),
			Deadline:        Duration(defaultDeadline),
			MinObservations: defaultMinObservations,
		}
		if spec.Soak != nil {
			stage.Soak = *spec.Soak
		}
		if spec.Deadline != nil {
			stage.Deadline = *spec.Deadline
		}
		if spec.MinObservations != nil {
			stage.MinObservations = *spec.MinObservations
		}
		stages[i] = stage

		if stage.Percent < 1 || stage.Percent > 100 {
			problems.addf(field+".percent", "must be between 1 and 100, got %d", stage.Percent)
		}
		if i > 0 && stage.Percent <= s.Stages[i-1].Percent {
			problems.addf(field+".percent", "must exceed the previous stage's %d, got %d", s.Stages[i-1].Percent, stage.Percent)
		}
		if d := stage.Soak.Std(); d < 0 || d > maxPolicyDuration {
			problems.addf(field+".soak", "must be between 0 and %s, got %s", maxPolicyDuration, d)
		}
		if d := stage.Deadline.Std(); d <= 0 || d > maxPolicyDuration {
			problems.addf(field+".deadline", "must be above 0 and at most %s, got %s", maxPolicyDuration, d)
		} else if d <= staleAfter {
			problems.addf(field+".deadline", "must exceed safety.stale_after (%s), or devices can't be classified as unknown before it passes; got %s", staleAfter, d)
		}
		if stage.MinObservations < 1 {
			problems.addf(field+".min_observations", "must be at least 1, got %d", stage.MinObservations)
		}
	}
	if last := len(stages) - 1; stages[last].Percent != 100 && stages[last].Percent >= 1 && stages[last].Percent <= 100 {
		problems.addf(fmt.Sprintf("stages[%d].percent", last), "the final stage must be 100, got %d", stages[last].Percent)
	}
	return stages
}

// Canonical returns the policy's canonical JSON encoding and its SHA-256 digest.
func (p Policy) Canonical() ([]byte, [sha256.Size]byte, error) {
	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("encode policy: %w", err)
	}
	return encoded, sha256.Sum256(encoded), nil
}

// StagePlan is a stage's size for a specific membership.
type StagePlan struct {
	// Ordinal is 1-based.
	Ordinal int
	Percent int
	// Target is the cumulative number of exposed devices when the stage is complete.
	Target int
	// CohortSize is how many devices this stage newly exposes. Zero means the
	// stage is skipped, since rounding left it with nobody to expose.
	CohortSize int
}

// Plan sizes each stage for memberCount devices. It rejects a membership the
// policy can never complete, rather than lowering the policy's minimums to fit.
func (p Policy) Plan(memberCount int) ([]StagePlan, error) {
	var problems violations
	if memberCount < 1 {
		problems.addf("membership", "no eligible devices")
		return nil, problems.err()
	}

	percents := make([]int, len(p.Stages))
	for i, stage := range p.Stages {
		percents[i] = stage.Percent
	}
	targets := StageTargets(memberCount, percents)

	plan := make([]StagePlan, len(p.Stages))
	previous := 0
	for i, stage := range p.Stages {
		cohort := targets[i] - previous
		previous = targets[i]
		plan[i] = StagePlan{Ordinal: i + 1, Percent: stage.Percent, Target: targets[i], CohortSize: cohort}
		if cohort > 0 && cohort < stage.MinObservations {
			problems.addf(fmt.Sprintf("stages[%d].min_observations", i),
				"stage %d exposes %d new devices out of %d, below its minimum of %d observations; raise the percentage or lower the minimum",
				i+1, cohort, memberCount, stage.MinObservations)
		}
	}
	if err := problems.err(); err != nil {
		return nil, err
	}
	return plan, nil
}
