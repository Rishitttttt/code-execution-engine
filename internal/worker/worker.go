// Package worker consumes execution and webhook jobs from Asynq.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rishitttttt/code-execution-engine/internal/config"
	"github.com/Rishitttttt/code-execution-engine/internal/db"
	"github.com/Rishitttttt/code-execution-engine/internal/executor"
	"github.com/Rishitttttt/code-execution-engine/internal/lang"
	"github.com/Rishitttttt/code-execution-engine/internal/queue"
	"github.com/Rishitttttt/code-execution-engine/internal/webhook"
)

// Worker holds the dependencies shared by every job handler.
type Worker struct {
	cfg     *config.Config
	q       *db.Queries
	exec    *executor.Executor
	asynq   *asynq.Client
	webhook *webhook.Client
	log     *slog.Logger
}

// New builds a Worker.
func New(cfg *config.Config, pool *pgxpool.Pool, ex *executor.Executor, ac *asynq.Client, log *slog.Logger) *Worker {
	return &Worker{
		cfg:     cfg,
		q:       db.New(pool),
		exec:    ex,
		asynq:   ac,
		webhook: webhook.New(cfg.WebhookTimeout),
		log:     log,
	}
}

// Mux returns the Asynq handler registry.
func (w *Worker) Mux() *asynq.ServeMux {
	mux := asynq.NewServeMux()
	mux.HandleFunc(queue.TypeExecute, w.handleExecute)
	mux.HandleFunc(queue.TypeWebhook, w.handleWebhook)
	return mux
}

// handleExecute runs one submission end to end.
//
// The error convention matters: returning an error tells Asynq to retry. A
// user program that fails to compile or exits non-zero is a *completed*
// execution and must not be retried, so those paths return nil.
func (w *Worker) handleExecute(ctx context.Context, t *asynq.Task) error {
	var p queue.ExecutePayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		// A malformed payload will never parse, no matter how often it runs.
		return fmt.Errorf("unmarshal payload: %w: %w", err, asynq.SkipRetry)
	}
	log := w.log.With("submission", p.SubmissionID)

	sub, err := w.q.GetSubmission(ctx, p.SubmissionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("submission %s not found: %w", p.SubmissionID, asynq.SkipRetry)
		}
		return fmt.Errorf("load submission: %w", err)
	}
	// A retry that lands after the job already finished must not run the code
	// a second time or clobber a delivered result.
	if sub.Status == db.SubmissionStatusCompleted || sub.Status == db.SubmissionStatusFailed {
		log.Info("skipping already-finished submission", "status", sub.Status)
		return nil
	}

	spec, ok := lang.Get(sub.Language)
	if !ok {
		// Only reachable if a language was removed from the registry while
		// jobs for it were still queued.
		w.fail(ctx, p.SubmissionID, "unsupported language "+sub.Language)
		return nil
	}

	if _, err := w.q.MarkSubmissionRunning(ctx, p.SubmissionID); err != nil {
		return fmt.Errorf("mark running: %w", err)
	}

	started := time.Now()
	outcome, err := w.exec.Execute(ctx, spec, sub.Code, sub.Stdin)
	if err != nil {
		// An engine failure is worth retrying: the daemon may be restarting.
		// Only record it as terminal once Asynq has given up.
		log.Error("execution failed", "err", err, "elapsed_ms", time.Since(started).Milliseconds())
		if isFinalAttempt(ctx) {
			w.fail(ctx, p.SubmissionID, "execution engine error: "+err.Error())
			w.enqueueWebhook(ctx, sub)
			return nil
		}
		return fmt.Errorf("execute: %w", err)
	}

	updated, err := w.q.CompleteSubmission(ctx, db.CompleteSubmissionParams{
		ID:            p.SubmissionID,
		Stdout:        &outcome.Stdout,
		Stderr:        &outcome.Stderr,
		ExitCode:      &outcome.ExitCode,
		CompileOutput: nilIfEmpty(outcome.CompileOutput),
		CpuTimeMs:     &outcome.CPUTimeMS,
		WallTimeMs:    &outcome.WallTimeMS,
		MemoryKb:      &outcome.MemoryKB,
		TimedOut:      outcome.TimedOut,
		OomKilled:     outcome.OOMKilled,
	})
	if err != nil {
		return fmt.Errorf("store result: %w", err)
	}

	log.Info("execution complete",
		"language", spec.ID,
		"exit_code", outcome.ExitCode,
		"cpu_ms", outcome.CPUTimeMS,
		"wall_ms", outcome.WallTimeMS,
		"memory_kb", outcome.MemoryKB,
		"timed_out", outcome.TimedOut,
		"oom", outcome.OOMKilled,
		"compile_error", outcome.CompileError,
	)

	w.enqueueWebhook(ctx, updated)
	return nil
}

// handleWebhook delivers one result. Returning an error hands the retry
// schedule back to Asynq, which applies the backoff policy from the queue
// package rather than this handler sleeping and holding a worker slot.
func (w *Worker) handleWebhook(ctx context.Context, t *asynq.Task) error {
	var p queue.WebhookPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("unmarshal payload: %w: %w", err, asynq.SkipRetry)
	}

	sub, err := w.q.GetSubmission(ctx, p.SubmissionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("submission %s not found: %w", p.SubmissionID, asynq.SkipRetry)
		}
		return fmt.Errorf("load submission: %w", err)
	}

	body := buildWebhookBody(sub)
	if err := w.webhook.Deliver(ctx, p.URL, body); err != nil {
		w.log.Warn("webhook delivery failed",
			"submission", p.SubmissionID,
			"url", p.URL,
			"err", err,
		)
		return err
	}
	w.log.Info("webhook delivered", "submission", p.SubmissionID, "url", p.URL)
	return nil
}

func (w *Worker) enqueueWebhook(ctx context.Context, sub db.Submission) {
	if sub.WebhookUrl == nil || *sub.WebhookUrl == "" {
		return
	}
	task, err := queue.NewWebhookTask(sub.ID, *sub.WebhookUrl, w.cfg.WebhookMaxRetries)
	if err != nil {
		w.log.Error("build webhook task", "submission", sub.ID, "err", err)
		return
	}
	if _, err := w.asynq.EnqueueContext(ctx, task); err != nil {
		// The execution result is already durable in Postgres, so a lost
		// notification degrades the caller to polling rather than losing data.
		w.log.Error("enqueue webhook task", "submission", sub.ID, "err", err)
	}
}

func (w *Worker) fail(ctx context.Context, id uuid.UUID, msg string) {
	if _, err := w.q.FailSubmission(ctx, db.FailSubmissionParams{ID: id, Error: &msg}); err != nil {
		w.log.Error("mark submission failed", "submission", id, "err", err)
	}
}

// isFinalAttempt reports whether Asynq has exhausted this task's retries, so
// the handler can record a terminal failure instead of returning an error that
// would be dropped into the dead-letter queue with the row still 'running'.
func isFinalAttempt(ctx context.Context) bool {
	count, okCount := asynq.GetRetryCount(ctx)
	limit, okLimit := asynq.GetMaxRetry(ctx)
	if !okCount || !okLimit {
		// Outside an Asynq handler (tests, direct calls) treat the attempt as
		// final so a failure is still recorded somewhere.
		return true
	}
	return count >= limit
}

// buildWebhookBody is the JSON delivered to a webhook URL. It intentionally
// mirrors GET /v1/submissions/{id} minus the source code, so a receiver can
// use one parser for both.
func buildWebhookBody(s db.Submission) map[string]any {
	return map[string]any{
		"id":             s.ID,
		"language":       s.Language,
		"status":         string(s.Status),
		"stdout":         s.Stdout,
		"stderr":         s.Stderr,
		"exit_code":      s.ExitCode,
		"compile_output": s.CompileOutput,
		"timed_out":      s.TimedOut,
		"oom_killed":     s.OomKilled,
		"metrics": map[string]any{
			"cpu_time_ms":  s.CpuTimeMs,
			"wall_time_ms": s.WallTimeMs,
			"memory_kb":    s.MemoryKb,
		},
		"error":       s.Error,
		"created_at":  s.CreatedAt,
		"finished_at": s.FinishedAt,
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
