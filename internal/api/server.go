// Package api implements the rolloutd HTTP API.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"

	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	"github.com/Bispulm/fleet-rollout/internal/auth"
	"github.com/Bispulm/fleet-rollout/internal/metrics"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

const readinessTimeout = 2 * time.Second

// ReadinessCheck reports whether the server can do useful work.
type ReadinessCheck func(context.Context) error

// Options configures a Server. Admin routes are registered only when Store is set,
// and AdminToken is required with it.
type Options struct {
	Logger     *slog.Logger
	Ready      ReadinessCheck
	Store      *store.Store
	AdminToken string
	// PollInterval is how often devices are told to poll.
	PollInterval time.Duration
	// Trust is who may sign artifacts.
	Trust signing.TrustSet
	// Artifacts holds artifact bytes.
	Artifacts artifactstore.Store
	// MaxArtifactBytes is the largest artifact accepted.
	MaxArtifactBytes int64
	// MaxDeviceRequests bounds how many device polls, reports, and manifest fetches are
	// served at once. More than that are turned away with a time to retry, rather than
	// queued.
	MaxDeviceRequests int
	// MaxDeviceDownloads bounds artifact downloads in the same way, separately, so that slow
	// downloads can't crowd out the reports of devices already installing.
	MaxDeviceDownloads int
	// Metrics, if set, counts requests, shed requests, and reports.
	Metrics *metrics.Metrics
	// Tracer, if set, records a span for each request.
	Tracer trace.TracerProvider
}

// Server is the HTTP API. It is safe for concurrent use.
type Server struct {
	logger    *slog.Logger
	ready     ReadinessCheck
	store     *store.Store
	adminAuth auth.AdminVerifier
	mux       *http.ServeMux
	handler   http.Handler
	draining  atomic.Bool

	pollInterval     time.Duration
	trust            signing.TrustSet
	artifacts        artifactstore.Store
	maxArtifactBytes int64
	deviceSlots      chan struct{}
	downloadSlots    chan struct{}
	metrics          *metrics.Metrics
}

// New builds a Server with its routes registered.
func New(opts Options) *Server {
	s := &Server{
		logger: opts.Logger,
		ready:  opts.Ready,
		store:  opts.Store,
		mux:    http.NewServeMux(),

		pollInterval:     opts.PollInterval,
		trust:            opts.Trust,
		artifacts:        opts.Artifacts,
		maxArtifactBytes: opts.MaxArtifactBytes,
		metrics:          opts.Metrics,
	}
	if s.maxArtifactBytes == 0 {
		s.maxArtifactBytes = defaultMaxArtifactBytes
	}
	slots := opts.MaxDeviceRequests
	if slots == 0 {
		slots = defaultMaxDeviceRequests
	}
	s.deviceSlots = make(chan struct{}, slots)
	downloads := opts.MaxDeviceDownloads
	if downloads == 0 {
		downloads = defaultMaxDeviceDownloads
	}
	s.downloadSlots = make(chan struct{}, downloads)
	if s.pollInterval == 0 {
		s.pollInterval = defaultPollInterval
	}
	s.handler = observe(s.logger, s.metrics, http.HandlerFunc(s.route))
	if opts.Tracer != nil {
		// Spans start before routing, so observe renames each one after its route.
		s.handler = otelhttp.NewHandler(s.handler, "request", otelhttp.WithTracerProvider(opts.Tracer))
	}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	if opts.Store != nil {
		s.adminAuth = auth.NewAdminVerifier(opts.AdminToken)
		s.registerAdminRoutes()
	}
	return s
}

// Handle registers an additional route.
func (s *Server) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// Drain makes readiness fail so that load balancers stop sending new requests.
// Requests already routed here continue to be served.
func (s *Server) Drain() {
	s.draining.Store(true)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// route dispatches through the mux, replacing its plain-text 404 and 405
// responses with problem documents.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	fallback, pattern := s.mux.Handler(r)
	if pattern != "" {
		// Dispatching through the mux, not the returned handler, is what sets the
		// request's pattern and path values.
		s.mux.ServeHTTP(w, r)
		return
	}
	probe := &fallbackProbe{header: make(http.Header)}
	fallback.ServeHTTP(probe, r)
	if allow := probe.header.Get("Allow"); allow != "" {
		w.Header().Set("Allow", allow)
	}
	writeProblem(w, probe.status, "")
}

// fallbackProbe records what the mux's fallback handler would have sent.
type fallbackProbe struct {
	header http.Header
	status int
}

func (p *fallbackProbe) Header() http.Header         { return p.header }
func (p *fallbackProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *fallbackProbe) WriteHeader(status int)      { p.status = status }

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	if err := s.ready(ctx); err != nil {
		// The reason can name infrastructure, so it goes to the log only.
		s.logger.LogAttrs(ctx, slog.LevelWarn, "not ready",
			slog.String("request_id", requestID(ctx)), slog.Any("error", err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// Serve accepts connections until ctx is canceled, then drains: readiness fails,
// the listener closes, and in-flight requests get shutdownTimeout to finish.
func (s *Server) Serve(ctx context.Context, listener net.Listener, shutdownTimeout time.Duration) error {
	httpServer := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	s.Drain()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// Abandon whatever did not finish so the serve goroutine can exit.
		_ = httpServer.Close()
		if endErr := <-serveErr; !errors.Is(endErr, http.ErrServerClosed) {
			err = errors.Join(err, endErr)
		}
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
