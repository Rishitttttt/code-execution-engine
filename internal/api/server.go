// Package api exposes the public REST surface: submit code, poll for a
// result, list submissions, and health checks.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rishitttttt/code-execution-engine/internal/config"
	"github.com/Rishitttttt/code-execution-engine/internal/db"
)

// Server holds the API's dependencies.
type Server struct {
	cfg     *config.Config
	q       *db.Queries
	pool    *pgxpool.Pool
	asynq   *asynq.Client
	inspect *asynq.Inspector
	log     *slog.Logger
}

// NewServer wires the API handlers to their dependencies.
func NewServer(cfg *config.Config, pool *pgxpool.Pool, ac *asynq.Client, in *asynq.Inspector, log *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		q:       db.New(pool),
		pool:    pool,
		asynq:   ac,
		inspect: in,
		log:     log,
	}
}

// Routes builds the HTTP handler.
//
// Routing is plain net/http: Go 1.22's method-and-wildcard patterns cover
// everything this API needs, so there is no third-party router.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/submissions", s.handleCreateSubmission)
	mux.HandleFunc("GET /v1/submissions/{id}", s.handleGetSubmission)
	mux.HandleFunc("GET /v1/submissions", s.handleListSubmissions)
	mux.HandleFunc("GET /v1/languages", s.handleListLanguages)

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	return s.recoverer(s.requestLogger(mux))
}

// requestLogger records one structured line per request with its status and
// duration.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// recoverer turns a panic in a handler into a 500 instead of killing the
// process and dropping every other in-flight request.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler",
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so there is nothing left to do but
		// let the client see a truncated body.
		return
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// Ping checks the datastores the API itself depends on.
func (s *Server) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}
