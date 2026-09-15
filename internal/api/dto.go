package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/Rishitttttt/code-execution-engine/internal/db"
)

// createSubmissionRequest is the POST /v1/submissions body.
type createSubmissionRequest struct {
	Language   string `json:"language"`
	Code       string `json:"code"`
	Stdin      string `json:"stdin,omitempty"`
	WebhookURL string `json:"webhook_url,omitempty"`
}

// createSubmissionResponse is returned with 202 Accepted. The result is not
// available yet; the caller polls the returned URL or waits for the webhook.
type createSubmissionResponse struct {
	ID        uuid.UUID `json:"id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ResultURL string    `json:"result_url"`
}

// submissionResponse is the full view of a submission.
//
// It is a hand-written struct rather than the sqlc row so the database schema
// can change without silently reshaping the public API, and so `code` is never
// echoed back in list responses.
type submissionResponse struct {
	ID       uuid.UUID `json:"id"`
	Language string    `json:"language"`
	Status   string    `json:"status"`

	Stdout        *string `json:"stdout"`
	Stderr        *string `json:"stderr"`
	ExitCode      *int32  `json:"exit_code"`
	CompileOutput *string `json:"compile_output,omitempty"`

	TimedOut  bool `json:"timed_out"`
	OOMKilled bool `json:"oom_killed"`

	Metrics *metrics `json:"metrics,omitempty"`

	// Error is set only for engine failures, never for a program that exited
	// non-zero on its own.
	Error *string `json:"error,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type metrics struct {
	CPUTimeMS  *int64 `json:"cpu_time_ms"`
	WallTimeMS *int64 `json:"wall_time_ms"`
	MemoryKB   *int64 `json:"memory_kb"`
}

type listResponse struct {
	Submissions []submissionResponse `json:"submissions"`
	Limit       int32                `json:"limit"`
	Offset      int32                `json:"offset"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// toResponse converts a database row into the public representation.
func toResponse(s db.Submission) submissionResponse {
	r := submissionResponse{
		ID:            s.ID,
		Language:      s.Language,
		Status:        string(s.Status),
		Stdout:        s.Stdout,
		Stderr:        s.Stderr,
		ExitCode:      s.ExitCode,
		CompileOutput: s.CompileOutput,
		TimedOut:      s.TimedOut,
		OOMKilled:     s.OomKilled,
		Error:         s.Error,
		CreatedAt:     s.CreatedAt,
		StartedAt:     s.StartedAt,
		FinishedAt:    s.FinishedAt,
	}
	if s.CpuTimeMs != nil || s.WallTimeMs != nil || s.MemoryKb != nil {
		r.Metrics = &metrics{
			CPUTimeMS:  s.CpuTimeMs,
			WallTimeMS: s.WallTimeMs,
			MemoryKB:   s.MemoryKb,
		}
	}
	return r
}
