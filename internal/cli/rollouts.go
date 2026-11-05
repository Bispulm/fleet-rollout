package cli

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

const timeLayout = "2006-01-02 15:04:05Z07:00"

func formatBPS(bps int) string {
	return fmt.Sprintf("%d.%02d%%", bps/100, bps%100)
}

func parseIntList(flag, s string) ([]int, error) {
	parts := strings.Split(s, ",")
	values := make([]int, len(parts))
	for i, part := range parts {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("--%s: %q is not an integer", flag, part)
		}
		values[i] = value
	}
	return values, nil
}

func (a *app) rolloutCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "rollout", Short: "Create, inspect, and control rollouts"}
	cmd.AddCommand(
		a.rolloutCreateCommand(),
		a.rolloutStatusCommand(),
		a.rolloutActionCommand(client.ActionStart, "Freeze membership and activate the first stage"),
		a.rolloutActionCommand(client.ActionPause, "Stop forward progress and the soak and deadline clocks"),
		a.rolloutActionCommand(client.ActionResume, "Return a paused rollout to where it was"),
		a.rolloutActionCommand(client.ActionAbort, "Finish a rollout without rolling back"),
		a.rolloutActionCommand(client.ActionRollback, "Restore what each updated device ran before, or try again after an incomplete rollback"),
		a.rolloutCohortCommand(),
		a.rolloutHistoryCommand(),
	)
	return cmd
}

type createFlags struct {
	fleet, name, artifact, id, stages, minObservations string
	soak, deadline, staleAfter, rollbackDeadline       time.Duration
	maxFailure, rolloutMaxFailure                      string
	coverage, maxUnknown, onBreach                     string
	selector, exclude                                  []string
}

func (a *app) rolloutCreateCommand() *cobra.Command {
	var f createFlags
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a draft rollout",
		Long: `Create a draft rollout. Settings you leave out use defaults that are stored with
the rollout, so a later change to the defaults can't affect it.

The request is rejected if the fleet's eligible devices could never satisfy the
policy. Repeating a request with an identical definition returns the existing
rollout, so it's safe to retry.`,
		Example: `  rolloutctl rollout create --fleet lab-a --name demo-1-4-0 --artifact <id> \
    --stages 1,10,50,100 --soak 10m --max-failure-rate 2%`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := f.request(cmd)
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			created, err := c.CreateRollout(cmd.Context(), f.fleet, req)
			if err != nil {
				return err
			}
			return a.printRollout(created)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.fleet, "fleet", "", "fleet name")
	flags.StringVar(&f.name, "name", "", "rollout name, unique within the fleet")
	flags.StringVar(&f.artifact, "artifact", "", "artifact ID")
	flags.StringVar(&f.id, "id", "", "rollout UUID; supplying it makes the cohort reproducible across databases")
	flags.StringVar(&f.stages, "stages", "", "cumulative percentages, ending at 100, such as 1,10,50,100")
	flags.DurationVar(&f.soak, "soak", 0, "time each stage must stay healthy after reaching coverage (default 10m)")
	flags.DurationVar(&f.deadline, "deadline", 0, "time each stage has to reach coverage (default 1h)")
	flags.StringVar(&f.minObservations, "min-observations", "", "minimum outcomes per stage: one value, or one for each stage (default 1)")
	flags.StringVar(&f.maxFailure, "max-failure-rate", "", "highest allowed failure rate within a stage's own cohort, such as 2%")
	flags.StringVar(&f.rolloutMaxFailure, "rollout-max-failure-rate", "", "highest allowed failure rate across all exposed devices (default: same as --max-failure-rate)")
	flags.StringVar(&f.coverage, "required-coverage", "", "share of a cohort that must report an outcome (default 90%)")
	flags.StringVar(&f.maxUnknown, "max-unknown", "", "largest share of a cohort allowed to be unknown at promotion (default 5%)")
	flags.DurationVar(&f.staleAfter, "stale-after", 0, "silence after which a device counts as unknown (default 15m)")
	flags.StringVar(&f.onBreach, "on-breach", "", "action when a threshold is crossed: halt, or rollback to halt and restore what devices ran before (default halt)")
	flags.DurationVar(&f.rollbackDeadline, "rollback-deadline", 0, "how long a rollback may take before it's marked incomplete (default 1h)")
	flags.StringArrayVar(&f.selector, "selector", nil, "only include devices with this label, as key=value; repeatable")
	flags.StringArrayVar(&f.exclude, "exclude-device", nil, "leave this device out; repeatable")
	mustMarkRequired(cmd, "fleet", "name", "artifact", "stages", "max-failure-rate")
	return cmd
}

// request turns flags into an API request. Flags left unset stay unset, so the
// server's defaults apply. Only values that can't even be parsed are rejected here.
func (f createFlags) request(cmd *cobra.Command) (apiv1.CreateRolloutRequest, error) {
	percents, err := parseIntList("stages", f.stages)
	if err != nil {
		return apiv1.CreateRolloutRequest{}, err
	}
	stages := make([]rollout.StageSpec, len(percents))
	for i, percent := range percents {
		stages[i].Percent = percent
	}

	changed := cmd.Flags().Changed
	if changed("soak") {
		soak := rollout.Duration(f.soak)
		for i := range stages {
			stages[i].Soak = &soak
		}
	}
	if changed("deadline") {
		deadline := rollout.Duration(f.deadline)
		for i := range stages {
			stages[i].Deadline = &deadline
		}
	}
	if changed("min-observations") {
		minimums, err := parseIntList("min-observations", f.minObservations)
		if err != nil {
			return apiv1.CreateRolloutRequest{}, err
		}
		switch len(minimums) {
		case 1:
			for i := range stages {
				stages[i].MinObservations = &minimums[0]
			}
		case len(stages):
			for i := range stages {
				stages[i].MinObservations = &minimums[i]
			}
		default:
			return apiv1.CreateRolloutRequest{}, fmt.Errorf(
				"--min-observations: got %d values for %d stages; give one value, or one for each stage", len(minimums), len(stages))
		}
	}

	safety := rollout.SafetySpec{OnBreach: rollout.OnBreach(f.onBreach)}
	maxFailure, err := rollout.ParseRate(f.maxFailure)
	if err != nil {
		return apiv1.CreateRolloutRequest{}, fmt.Errorf("--max-failure-rate: %w", err)
	}
	safety.MaxFailureBPS = &maxFailure
	optionalRates := []struct {
		flag  string
		value string
		dst   **int
	}{
		{"rollout-max-failure-rate", f.rolloutMaxFailure, &safety.RolloutMaxFailureBPS},
		{"required-coverage", f.coverage, &safety.RequiredCoverageBPS},
		{"max-unknown", f.maxUnknown, &safety.MaxUnknownBPS},
	}
	for _, rate := range optionalRates {
		if !changed(rate.flag) {
			continue
		}
		bps, err := rollout.ParseRate(rate.value)
		if err != nil {
			return apiv1.CreateRolloutRequest{}, fmt.Errorf("--%s: %w", rate.flag, err)
		}
		*rate.dst = &bps
	}
	if changed("stale-after") {
		staleAfter := rollout.Duration(f.staleAfter)
		safety.StaleAfter = &staleAfter
	}
	if changed("rollback-deadline") {
		deadline := rollout.Duration(f.rollbackDeadline)
		safety.RollbackDeadline = &deadline
	}

	selector, err := parseLabels("selector", f.selector)
	if err != nil {
		return apiv1.CreateRolloutRequest{}, err
	}
	return apiv1.CreateRolloutRequest{
		ID:         f.id,
		Name:       f.name,
		ArtifactID: f.artifact,
		Policy: rollout.PolicySpec{
			Stages:         stages,
			Safety:         safety,
			Selector:       selector,
			ExcludeDevices: f.exclude,
		},
	}, nil
}

func (a *app) rolloutStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status <id>",
		Short: "Show a rollout's state, policy, and stages",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			found, err := c.GetRollout(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.printRollout(found)
		},
	}
}

func (a *app) rolloutActionCommand(action, short string) *cobra.Command {
	var ifRevision int64
	var reason string
	cmd := &cobra.Command{
		Use:   action + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			req := apiv1.TransitionRequest{Reason: reason}
			if cmd.Flags().Changed("if-revision") {
				req.ExpectedRevision = &ifRevision
			}
			updated, err := c.Transition(cmd.Context(), args[0], action, req)
			if err != nil {
				return err
			}
			return a.printRollout(updated)
		},
	}
	cmd.Flags().Int64Var(&ifRevision, "if-revision", 0, "apply only if the rollout is still at this revision")
	cmd.Flags().StringVar(&reason, "reason", "", "why, recorded in the audit log")
	return cmd
}

func (a *app) rolloutCohortCommand() *cobra.Command {
	var stage, after, limit int
	cmd := &cobra.Command{
		Use:   "cohort <id>",
		Short: "List a started rollout's frozen membership in exposure order",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			page, err := c.ListCohort(cmd.Context(), args[0], stage, after, limit)
			if client.IsCode(err, apiv1.CodeMembershipNotReady) {
				return errors.New("this rollout has no membership yet; it's frozen when you start the rollout")
			}
			if err != nil {
				return err
			}
			if a.output == outputJSON {
				return a.printJSON(page)
			}
			table := a.table()
			_, _ = fmt.Fprintln(table, "POSITION\tSTAGE\tDEVICE\tRANK KEY")
			for _, entry := range page.Entries {
				_, _ = fmt.Fprintf(table, "%d\t%d\t%s\t%s\n", entry.Position, entry.Stage, entry.DeviceID, entry.RankKey[:16])
			}
			if err := table.Flush(); err != nil {
				return err
			}
			if page.NextAfter != 0 {
				_, err = fmt.Fprintf(a.stdout, "More members follow; continue with --after %d\n", page.NextAfter)
			}
			return err
		},
	}
	cmd.Flags().IntVar(&stage, "stage", 0, "show only the cohort first exposed by this stage")
	cmd.Flags().IntVar(&after, "after", 0, "continue after this position")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum members to show, up to 1000")
	return cmd
}

func (a *app) rolloutHistoryCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "history <id>",
		Short: "Show a rollout's audit log of state changes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			log, err := c.ListTransitions(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.output == outputJSON {
				return a.printJSON(apiv1.TransitionLog{Transitions: log})
			}
			table := a.table()
			_, _ = fmt.Fprintln(table, "REVISION\tTIME\tEVENT\tFROM\tTO\tACTOR\tREASON")
			for _, entry := range log {
				_, _ = fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", entry.Revision, entry.OccurredAt.Format(timeLayout),
					entry.Event, entry.From, entry.To, entry.Actor, entry.Reason)
			}
			return table.Flush()
		},
	}
}

func (a *app) printRollout(r apiv1.Rollout) error {
	if a.output == outputJSON {
		return a.printJSON(r)
	}
	w := a.table()
	row := func(label, value string) { _, _ = fmt.Fprintf(w, "%s:\t%s\n", label, value) }

	row("Rollout", r.Name+" ("+r.ID+")")
	row("Fleet", r.Fleet)
	row("Artifact", r.ArtifactID)
	row("State", string(r.State))
	if r.Waiting != "" {
		row("Waiting", r.Waiting)
	}
	if r.HaltReason != "" {
		row("Halted", r.HaltReason+": "+r.HaltDetail)
	}
	if rb := r.Rollback; rb != nil {
		line := fmt.Sprintf("attempt %d: %d of %d devices restored, %d failed, %d unresolved",
			rb.Attempt, rb.Restored, rb.Needed, rb.Failed, rb.Unresolved)
		if rb.DeadlineAt != nil && r.State == rollout.StateRollingBack {
			line += ", deadline " + rb.DeadlineAt.Format(timeLayout)
		}
		row("Rollback", line)
	}
	row("Revision", strconv.FormatInt(r.Revision, 10))
	if r.MemberCount != nil {
		row("Members", strconv.Itoa(*r.MemberCount))
	}
	if len(r.Exclusions) > 0 {
		row("Excluded", formatExclusions(r.Exclusions))
	}
	safety := r.Policy.Safety
	row("Failure limit", formatBPS(safety.MaxFailureBPS)+" per stage, "+formatBPS(safety.RolloutMaxFailureBPS)+" across the rollout")
	row("Coverage", formatBPS(safety.RequiredCoverageBPS)+" required, at most "+formatBPS(safety.MaxUnknownBPS)+" unknown")
	row("Stale after", safety.StaleAfter.Std().String())
	row("On breach", string(safety.OnBreach))
	row("Policy digest", r.PolicyDigest)
	if err := w.Flush(); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(a.stdout); err != nil {
		return err
	}
	stages := a.table()
	_, _ = fmt.Fprintln(stages, "STAGE\tPERCENT\tTARGET\tNEW\tMIN OBS\tSOAK\tSTATE\tOK\tFAILED\tIN FLIGHT\tPENDING\tUNKNOWN\tDEADLINE")
	for i, st := range r.Stages {
		spec := r.Policy.Stages[i]
		deadline := "-"
		if st.DeadlineAt != nil {
			deadline = st.DeadlineAt.Format(timeLayout)
		}
		_, _ = fmt.Fprintf(stages, "%d\t%d%%\t%s\t%s\t%d\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n", st.Ordinal, st.Percent,
			optionalInt(st.TargetCount), optionalInt(st.CohortSize), spec.MinObservations, spec.Soak.Std(), st.State,
			st.Succeeded, st.Failed, st.InFlight, st.Pending, st.Unknown, deadline)
	}
	return stages.Flush()
}

func optionalInt(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

func formatExclusions(counts map[rollout.ExclusionReason]int) string {
	reasons := slices.Sorted(maps.Keys(counts))
	parts := make([]string, len(reasons))
	for i, reason := range reasons {
		parts[i] = fmt.Sprintf("%s=%d", reason, counts[reason])
	}
	return strings.Join(parts, ", ")
}
