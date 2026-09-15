-- name: CreateSubmission :one
INSERT INTO submissions (id, language, code, stdin, webhook_url)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetSubmission :one
SELECT * FROM submissions WHERE id = $1;

-- name: ListSubmissions :many
SELECT * FROM submissions
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: MarkSubmissionRunning :one
UPDATE submissions
SET status = 'running', started_at = now()
WHERE id = $1
RETURNING *;

-- name: CompleteSubmission :one
UPDATE submissions
SET status         = 'completed',
    stdout         = $2,
    stderr         = $3,
    exit_code      = $4,
    compile_output = $5,
    cpu_time_ms    = $6,
    wall_time_ms   = $7,
    memory_kb      = $8,
    timed_out      = $9,
    oom_killed     = $10,
    finished_at    = now()
WHERE id = $1
RETURNING *;

-- name: FailSubmission :one
UPDATE submissions
SET status      = 'failed',
    error       = $2,
    finished_at = now()
WHERE id = $1
RETURNING *;
