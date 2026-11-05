// Package metrics holds rolloutd's Prometheus metrics. Events are counted as they happen, and
// rollout state is read from the store when Prometheus scrapes, so every instance reports the
// same thing whether or not it leads the engine (ADR 0012). A nil *Metrics records nothing, so
// callers don't have to check.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

const namespace = "fleet_rollout"

// Metrics is one process's metrics.
type Metrics struct {
	registry        *prometheus.Registry
	httpRequests    *prometheus.CounterVec
	httpDuration    *prometheus.HistogramVec
	shed            *prometheus.CounterVec
	reports         *prometheus.CounterVec
	engineDecisions *prometheus.CounterVec
	halts           *prometheus.CounterVec
	engineLeader    prometheus.Gauge
}

// New makes the metrics, with the Go runtime's and the process's included.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "http_requests_total",
			Help: "HTTP requests, by route pattern, method, and status code.",
		}, []string{"route", "method", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "http_request_duration_seconds",
			Help:    "How long HTTP requests took, by route pattern.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route"}),
		shed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "device_requests_shed_total",
			Help: "Device requests turned away because their pool was full, by pool.",
		}, []string{"pool"}),
		reports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "device_reports_total",
			Help: "Device reports, by the state reported and what became of it.",
		}, []string{"state", "result"}),
		engineDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "engine_decisions_total",
			Help: "Rollout state changes the engine made, by event.",
		}, []string{"event"}),
		halts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "rollout_halts_total",
			Help: "Rollouts the engine halted, by reason.",
		}, []string{"reason"}),
		engineLeader: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "engine_leader",
			Help: "1 while this instance holds the engine lease, and 0 otherwise.",
		}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpRequests, m.httpDuration, m.shed, m.reports, m.engineDecisions, m.halts, m.engineLeader,
	)
	// A labeled counter appears only once it's first counted, so its first sample is already 1, and
	// increase() never sees that first event. Starting the known ones at zero fixes that, so the very
	// first halt on a new server shows up.
	for _, pool := range []string{"requests", "downloads"} {
		m.shed.WithLabelValues(pool)
	}
	for _, reason := range []rollout.HaltReason{
		rollout.HaltStageBreach, rollout.HaltCertainBreach, rollout.HaltRolloutBreach, rollout.HaltLateRegression,
		rollout.HaltDeadline,
	} {
		m.halts.WithLabelValues(string(reason))
	}
	for _, event := range []rollout.Event{
		rollout.EventCoverageMet, rollout.EventSoakPassed, rollout.EventFinalSoakPassed, rollout.EventBreach,
		rollout.EventRollback, rollout.EventRollbackDone, rollout.EventRollbackExpired,
	} {
		m.engineDecisions.WithLabelValues(string(event))
	}
	return m
}

// Handler serves the metrics in Prometheus's format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveRollouts reports rollout state read from the store at each scrape.
func (m *Metrics) ObserveRollouts(s *store.Store, logger *slog.Logger) {
	m.registry.MustRegister(&rolloutCollector{store: s, logger: logger})
}

// Request counts an HTTP request. route is the pattern it matched, so paths can't add series.
func (m *Metrics) Request(route, method string, code int, took time.Duration) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	m.httpRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	m.httpDuration.WithLabelValues(route).Observe(took.Seconds())
}

// Shed counts a device request turned away from a full pool.
func (m *Metrics) Shed(pool string) {
	if m != nil {
		m.shed.WithLabelValues(pool).Inc()
	}
}

// Report counts a device report and what became of it: applied, duplicate, or the reason it was
// ignored.
func (m *Metrics) Report(state, result string) {
	if m != nil {
		m.reports.WithLabelValues(state, result).Inc()
	}
}

// Decision counts a state change the engine made, and a halt by its reason.
func (m *Metrics) Decision(event, haltReason string) {
	if m == nil {
		return
	}
	m.engineDecisions.WithLabelValues(event).Inc()
	if haltReason != "" {
		m.halts.WithLabelValues(haltReason).Inc()
	}
}

// Leading records whether this instance holds the engine lease.
func (m *Metrics) Leading(leading bool) {
	if m == nil {
		return
	}
	if leading {
		m.engineLeader.Set(1)
	} else {
		m.engineLeader.Set(0)
	}
}

// scrapeTimeout bounds the store reads behind one scrape.
const scrapeTimeout = 5 * time.Second

var (
	rolloutInfo = prometheus.NewDesc(namespace+"_rollout_info",
		"1 for each recent rollout, labeled with its state.", []string{"rollout_id", "fleet", "rollout", "state"}, nil)
	rolloutHalt = prometheus.NewDesc(namespace+"_rollout_halt_info",
		"1 for each recent rollout that halted, labeled with why. Unlike the halts counter, it doesn't depend on a scrape "+
			"having happened before the halt.", []string{"rollout_id", "fleet", "rollout", "reason"}, nil)
	rolloutStage = prometheus.NewDesc(namespace+"_rollout_current_stage",
		"The stage a rollout is on.", []string{"rollout_id", "fleet", "rollout"}, nil)
	stageDevices = prometheus.NewDesc(namespace+"_stage_devices",
		"Devices in each stage of a rollout, by outcome.", []string{"rollout_id", "fleet", "rollout", "stage", "outcome"}, nil)
	rollbackDevices = prometheus.NewDesc(namespace+"_rollback_devices",
		"A rollback's devices: needed, restored, failed, and unresolved.", []string{"rollout_id", "fleet", "rollout", "outcome"}, nil)
	scrapeErrors = prometheus.NewDesc(namespace+"_rollout_scrape_errors",
		"1 if the last read of rollout state failed.", nil, nil)
)

type rolloutCollector struct {
	store  *store.Store
	logger *slog.Logger
}

func (c *rolloutCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{rolloutInfo, rolloutHalt, rolloutStage, stageDevices, rollbackDevices, scrapeErrors} {
		ch <- d
	}
}

func (c *rolloutCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	rollouts, err := c.store.RecentRollouts(ctx, time.Hour)
	if err != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "read rollouts for metrics", slog.Any("error", err))
		ch <- prometheus.MustNewConstMetric(scrapeErrors, prometheus.GaugeValue, 1)
		return
	}
	ch <- prometheus.MustNewConstMetric(scrapeErrors, prometheus.GaugeValue, 0)
	for _, r := range rollouts {
		labels := []string{r.ID, r.FleetName, r.Name}
		ch <- prometheus.MustNewConstMetric(rolloutInfo, prometheus.GaugeValue, 1, append(labels, string(r.State))...)
		if r.HaltReason != nil {
			ch <- prometheus.MustNewConstMetric(rolloutHalt, prometheus.GaugeValue, 1, append(labels, *r.HaltReason)...)
		}
		if r.CurrentStage != nil {
			ch <- prometheus.MustNewConstMetric(rolloutStage, prometheus.GaugeValue, float64(*r.CurrentStage), labels...)
		}
		for _, stage := range r.Stages {
			ordinal := strconv.Itoa(stage.Ordinal)
			for outcome, count := range map[string]int{
				"succeeded": stage.Succeeded, "failed": stage.Failed, "superseded": stage.Superseded,
				"in_flight": stage.InFlight, "unknown": stage.Unknown, "pending": stage.Pending,
			} {
				ch <- prometheus.MustNewConstMetric(stageDevices, prometheus.GaugeValue, float64(count),
					append(labels, ordinal, outcome)...)
			}
		}
		if rb := r.Rollback; rb != nil {
			for outcome, count := range map[string]int{
				"needed": rb.Needed, "restored": rb.Restored, "failed": rb.Failed, "unresolved": rb.Unresolved,
			} {
				ch <- prometheus.MustNewConstMetric(rollbackDevices, prometheus.GaugeValue, float64(count), append(labels, outcome)...)
			}
		}
	}
}
