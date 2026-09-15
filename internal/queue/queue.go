// Package queue defines the Asynq task types shared by the API (producer)
// and the worker (consumer).
package queue

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// Task type names. Changing one is a wire-format break: jobs already sitting
// in Redis under the old name will never be picked up.
const (
	TypeExecute = "submission:execute"
	TypeWebhook = "webhook:deliver"
)

// Queue names, listed here with the priority weights the worker registers.
const (
	QueueDefault  = "default"
	QueueWebhooks = "webhooks"
)

// ExecutePayload identifies the submission to run. Only the ID travels through
// Redis; the code itself is read from Postgres by the worker, which keeps the
// queue small and gives one source of truth for submission state.
type ExecutePayload struct {
	SubmissionID uuid.UUID `json:"submission_id"`
}

// WebhookPayload identifies a finished submission to notify about.
type WebhookPayload struct {
	SubmissionID uuid.UUID `json:"submission_id"`
	URL          string    `json:"url"`
}

// NewExecuteTask builds the task enqueued when a submission is accepted.
func NewExecuteTask(id uuid.UUID, timeout time.Duration) (*asynq.Task, error) {
	b, err := json.Marshal(ExecutePayload{SubmissionID: id})
	if err != nil {
		return nil, fmt.Errorf("marshal execute payload: %w", err)
	}
	return asynq.NewTask(TypeExecute, b,
		asynq.Queue(QueueDefault),
		// Execution is not idempotent-free but it is safe to repeat: the
		// result row is overwritten. Two retries covers a daemon blip without
		// re-running an expensive job indefinitely.
		asynq.MaxRetry(2),
		asynq.Timeout(timeout),
		// Keep finished jobs briefly so `asynq dash` can show what ran.
		asynq.Retention(1*time.Hour),
	), nil
}

// NewWebhookTask builds the delivery task for a completed submission.
func NewWebhookTask(id uuid.UUID, url string, maxRetry int) (*asynq.Task, error) {
	b, err := json.Marshal(WebhookPayload{SubmissionID: id, URL: url})
	if err != nil {
		return nil, fmt.Errorf("marshal webhook payload: %w", err)
	}
	return asynq.NewTask(TypeWebhook, b,
		asynq.Queue(QueueWebhooks),
		asynq.MaxRetry(maxRetry),
		asynq.Timeout(30*time.Second),
		asynq.Retention(1*time.Hour),
	), nil
}

// RetryDelay is exponential backoff with full jitter, capped at 5 minutes.
//
// Asynq's built-in policy is also exponential, but its jitter is narrow. Full
// jitter matters here because a receiver that goes down takes every in-flight
// webhook with it, and they would otherwise retry in lockstep and hammer the
// receiver the moment it comes back.
func RetryDelay(n int, _ error, _ *asynq.Task) time.Duration {
	const (
		base = 2 * time.Second
		max  = 5 * time.Minute
	)
	backoff := float64(base) * math.Pow(2, float64(n))
	if backoff > float64(max) || math.IsInf(backoff, 1) {
		backoff = float64(max)
	}
	return time.Duration(rand.Int63n(int64(backoff)) + int64(base))
}
