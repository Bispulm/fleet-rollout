package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/db"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/sim"
)

// machine describes where a benchmark ran, which every result has to state.
type machine struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUs     int    `json:"cpus"`
	Model    string `json:"model,omitempty"`
	Go       string `json:"go"`
	LoadFrom string `json:"load_before"`
	LoadTo   string `json:"load_after,omitempty"`
}

func describeMachine() machine {
	return machine{
		OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Model: cpuModel(), Go: runtime.Version(),
		LoadFrom: loadAverage(),
	}
}

// loadAverage returns the 1, 5, and 15 minute load averages, which say whether anything else
// was competing for the machine.
func loadAverage() string {
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		return strings.Join(strings.Fields(string(data))[:3], " ")
	}
	out, err := exec.CommandContext(context.Background(), "sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return "unknown"
	}
	return strings.Trim(strings.TrimSpace(string(out)), "{} ")
}

func cpuModel() string {
	if out, err := exec.CommandContext(context.Background(), "sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if name, ok := strings.CutPrefix(line, "model name"); ok {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), ":"))
		}
	}
	return ""
}

func printMachine(w io.Writer, m machine) {
	model := m.Model
	if model == "" {
		model = "unknown CPU"
	}
	_, _ = fmt.Fprintf(w, "Machine: %s, %d CPUs, %s/%s, %s. Load average before: %s", model, m.CPUs, m.OS, m.Arch, m.Go, m.LoadFrom)
	if m.LoadTo != "" {
		_, _ = fmt.Fprintf(w, ", after: %s", m.LoadTo)
	}
	_, _ = fmt.Fprintln(w, ".")
}

// deterministic runs a scenario in process, in a scratch database of its own.
func deterministic(ctx context.Context, server string, sc sim.Scenario) (sim.Report, error) {
	url, drop, err := db.CreateScratch(ctx, server, "rollout_bench_")
	if err != nil {
		return sim.Report{}, err
	}
	report, err := func() (sim.Report, error) {
		pool, err := db.Open(ctx, url, 4)
		if err != nil {
			return sim.Report{}, err
		}
		defer pool.Close()
		if _, err := db.Migrate(ctx, pool); err != nil {
			return sim.Report{}, fmt.Errorf("migrate: %w", err)
		}
		return sim.Run(ctx, pool, sc)
	}()
	if dropErr := drop(); err == nil {
		err = dropErr
	}
	return report, err
}

func durationPtr(d time.Duration) *rollout.Duration {
	v := rollout.Duration(d)
	return &v
}

// quantile returns the q-th quantile of sorted values by the nearest-rank method, so it's
// always a value that was actually observed.
func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

func sortedCopy(values []time.Duration) []time.Duration {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

func parseFloats(list string) ([]float64, error) {
	var out []float64
	for part := range strings.SplitSeq(list, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", part)
		}
		out = append(out, v)
	}
	return out, nil
}

func parseInts(list string) ([]int, error) {
	var out []int
	for part := range strings.SplitSeq(list, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || v < 1 {
			return nil, fmt.Errorf("%q is not a positive whole number", part)
		}
		out = append(out, v)
	}
	return out, nil
}

// table prints a Markdown table, so results can go straight into the docs.
func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 1, ' ', 0)
	line := func(cells []string) { _, _ = fmt.Fprintln(tw, "| "+strings.Join(cells, "\t| ")+"\t|") }
	line(header)
	rule := make([]string, len(header))
	for i := range rule {
		rule[i] = "---"
	}
	line(rule)
	for _, row := range rows {
		line(row)
	}
	_ = tw.Flush()
}

// writeJSON saves the full results, if asked to.
func writeJSON(path string, v any) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func formatMillis(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64)
}

func formatSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 2, 64)
}

func percent(part, whole float64) string {
	if whole == 0 {
		return "-"
	}
	return strconv.FormatFloat(100*part/whole, 'f', 1, 64) + "%"
}

// maxRSSMegabytes reads peak memory from rusage, which macOS reports in bytes and Linux in
// kilobytes.
func maxRSSMegabytes(r *syscall.Rusage) float64 {
	if runtime.GOOS == "darwin" {
		return float64(r.Maxrss) / (1 << 20)
	}
	return float64(r.Maxrss) / (1 << 10)
}
