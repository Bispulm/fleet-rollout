package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/metrics"
)

const requestIDHeader = "X-Request-Id"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type requestIDKey struct{}

// requestID returns the ID assigned to the current request, if any.
func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// statusRecorder captures the status and size of a response for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observe assigns a request ID, recovers from panics, and writes the access log.
// Panics are logged in full but never reach the client.
func observe(logger *slog.Logger, m *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		rec := &statusRecorder{ResponseWriter: w}

		defer func() {
			if v := recover(); v != nil {
				// http.ErrAbortHandler is net/http's signal to abort quietly.
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared as a panic value
					panic(v)
				}
				logger.LogAttrs(r.Context(), slog.LevelError, "handler panic",
					slog.String("request_id", id),
					slog.Any("panic", v),
					slog.String("stack", string(debug.Stack())))
				if rec.status == 0 {
					writeProblem(rec, http.StatusInternalServerError, "")
				}
			}
			m.Request(r.Pattern, r.Method, rec.status, time.Since(start))
			logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("request_id", id),
				slog.String("method", r.Method),
				slog.String("route", r.Pattern),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.bytes),
				slog.Duration("duration", time.Since(start)))
		}()

		next.ServeHTTP(rec, r)
	})
}

func newRequestID() string {
	b := make([]byte, 16)
	// crypto/rand.Read never returns an error on supported platforms.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
