package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Rishitttttt/code-execution-engine/internal/db"
	"github.com/Rishitttttt/code-execution-engine/internal/lang"
	"github.com/Rishitttttt/code-execution-engine/internal/queue"
	"github.com/Rishitttttt/code-execution-engine/internal/webhook"
)

func (s *Server) handleCreateSubmission(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Bound the body before parsing so an oversized payload is rejected at the
	// socket rather than after being buffered into memory. The slack above the
	// code limit leaves room for stdin and the JSON envelope itself.
	limit := int64(s.cfg.MaxCodeBytes + s.cfg.MaxStdinBytes + 4096)
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	var req createSubmissionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", limit))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	// A second JSON value in the body would otherwise be silently ignored.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "body must contain exactly one JSON object")
		return
	}

	spec, ok := lang.Get(strings.ToLower(strings.TrimSpace(req.Language)))
	if !ok {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("unsupported language %q; see GET /v1/languages", req.Language))
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	if len(req.Code) > s.cfg.MaxCodeBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("code exceeds %d bytes", s.cfg.MaxCodeBytes))
		return
	}
	if len(req.Stdin) > s.cfg.MaxStdinBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("stdin exceeds %d bytes", s.cfg.MaxStdinBytes))
		return
	}
	// Postgres text columns reject invalid UTF-8, so catching it here turns a
	// confusing driver error into a clear 400.
	if !utf8.ValidString(req.Code) || !utf8.ValidString(req.Stdin) {
		writeError(w, http.StatusBadRequest, "code and stdin must be valid UTF-8")
		return
	}

	var webhookURL *string
	if u := strings.TrimSpace(req.WebhookURL); u != "" {
		if err := webhook.Validate(u, s.cfg.WebhookAllowPrivate); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		webhookURL = &u
	}

	id := uuid.New()
	sub, err := s.q.CreateSubmission(ctx, db.CreateSubmissionParams{
		ID:         id,
		Language:   spec.ID,
		Code:       req.Code,
		Stdin:      req.Stdin,
		WebhookUrl: webhookURL,
	})
	if err != nil {
		s.log.Error("create submission", "err", err)
		writeError(w, http.StatusInternalServerError, "could not persist submission")
		return
	}

	// Persist first, enqueue second. The reverse order allows a worker to pick
	// up a job for a row that does not exist yet.
	// The queue-level timeout has to sit above the engine's own budget, or
	// Asynq reclaims the job while the container is still legitimately running.
	taskTimeout := s.cfg.CompileTimeout + s.cfg.RunTimeout + 90*time.Second
	task, err := queue.NewExecuteTask(id, taskTimeout)
	if err != nil {
		s.log.Error("build execute task", "err", err)
		writeError(w, http.StatusInternalServerError, "could not enqueue submission")
		return
	}
	if _, err := s.asynq.EnqueueContext(ctx, task); err != nil {
		// The row is already 'queued' but nothing will run it. Mark it failed
		// rather than leaving a submission that polls forever.
		if _, ferr := s.q.FailSubmission(ctx, db.FailSubmissionParams{
			ID:    id,
			Error: strPtr("could not enqueue execution job"),
		}); ferr != nil {
			s.log.Error("mark submission failed after enqueue error", "id", id, "err", ferr)
		}
		s.log.Error("enqueue execute task", "id", id, "err", err)
		writeError(w, http.StatusServiceUnavailable, "execution queue unavailable")
		return
	}

	w.Header().Set("Location", "/v1/submissions/"+id.String())
	writeJSON(w, http.StatusAccepted, createSubmissionResponse{
		ID:        sub.ID,
		Status:    string(sub.Status),
		CreatedAt: sub.CreatedAt,
		ResultURL: "/v1/submissions/" + id.String(),
	})
}

func (s *Server) handleGetSubmission(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a UUID")
		return
	}

	sub, err := s.q.GetSubmission(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "submission not found")
			return
		}
		s.log.Error("get submission", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not read submission")
		return
	}
	writeJSON(w, http.StatusOK, toResponse(sub))
}

func (s *Server) handleListSubmissions(w http.ResponseWriter, r *http.Request) {
	limit := clampQueryInt(r, "limit", 20, 1, s.cfg.DefaultPageCap)
	offset := clampQueryInt(r, "offset", 0, 0, 1_000_000)

	rows, err := s.q.ListSubmissions(r.Context(), db.ListSubmissionsParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		s.log.Error("list submissions", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list submissions")
		return
	}

	out := make([]submissionResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, toResponse(row))
	}
	writeJSON(w, http.StatusOK, listResponse{
		Submissions: out,
		Limit:       int32(limit),
		Offset:      int32(offset),
	})
}

func (s *Server) handleListLanguages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"languages": lang.All()})
}

// handleHealthz reports that the process is alive. It deliberately touches no
// dependency, so a Postgres outage does not make the orchestrator restart a
// perfectly healthy API.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports whether the API can actually serve traffic.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{"postgres": "ok", "redis": "ok"}
	code := http.StatusOK

	if err := s.pool.Ping(r.Context()); err != nil {
		checks["postgres"] = err.Error()
		code = http.StatusServiceUnavailable
	}
	if _, err := s.inspect.Queues(); err != nil {
		checks["redis"] = err.Error()
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"checks": checks})
}

func clampQueryInt(r *http.Request, key string, def, min, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func strPtr(s string) *string { return &s }
