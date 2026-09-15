package queue

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestExecuteTaskCarriesOnlyTheID(t *testing.T) {
	id := uuid.New()
	task, err := NewExecuteTask(id, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if task.Type() != TypeExecute {
		t.Errorf("type = %q, want %q", task.Type(), TypeExecute)
	}

	// Code stays in Postgres; the queue is not a data store.
	var p ExecutePayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		t.Fatal(err)
	}
	if p.SubmissionID != id {
		t.Errorf("submission id = %s, want %s", p.SubmissionID, id)
	}

	var raw map[string]any
	_ = json.Unmarshal(task.Payload(), &raw)
	if len(raw) != 1 {
		t.Errorf("payload carries extra fields: %v", raw)
	}
}

func TestWebhookTaskUsesItsOwnQueue(t *testing.T) {
	task, err := NewWebhookTask(uuid.New(), "https://example.com/hook", 5)
	if err != nil {
		t.Fatal(err)
	}
	var p WebhookPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://example.com/hook" {
		t.Errorf("url = %q", p.URL)
	}
	if task.Type() != TypeWebhook {
		t.Errorf("type = %q, want %q", task.Type(), TypeWebhook)
	}
}

func TestRetryDelayGrowsAndStaysCapped(t *testing.T) {
	const cap = 5 * time.Minute
	err := errors.New("receiver down")

	var prevMax time.Duration
	for attempt := 0; attempt < 12; attempt++ {
		// Full jitter makes each call random, so the ceiling is what gets
		// asserted rather than an exact value.
		var maxSeen time.Duration
		for i := 0; i < 200; i++ {
			d := RetryDelay(attempt, err, nil)
			if d <= 0 {
				t.Fatalf("attempt %d produced a non-positive delay %s", attempt, d)
			}
			if d > cap+2*time.Second {
				t.Fatalf("attempt %d exceeded the cap: %s", attempt, d)
			}
			if d > maxSeen {
				maxSeen = d
			}
		}
		if attempt < 6 && maxSeen < prevMax {
			t.Errorf("backoff shrank at attempt %d: %s < %s", attempt, maxSeen, prevMax)
		}
		prevMax = maxSeen
	}
}

// Full jitter exists so a receiver coming back online is not hit by every
// pending webhook at the same instant.
func TestRetryDelayIsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[RetryDelay(5, nil, nil)] = true
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct delays across 50 calls; jitter is too narrow", len(seen))
	}
}
