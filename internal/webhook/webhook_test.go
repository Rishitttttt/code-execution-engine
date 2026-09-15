package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateRejectsInternalTargets(t *testing.T) {
	// The worker sits on the compose network with a route to Postgres, Redis
	// and the Docker socket host, so a caller-supplied URL is an SSRF vector.
	bad := []string{
		"http://127.0.0.1:8080/hook",
		"http://localhost/hook",
		"https://10.0.0.5/hook",
		"http://192.168.1.1/hook",
		"http://172.16.4.4/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/hook",
		"ftp://example.com/hook",
		"file:///etc/passwd",
		"not-a-url",
		"http://",
	}
	for _, u := range bad {
		if err := Validate(u, false); err == nil {
			t.Errorf("Validate(%q) = nil, want rejection", u)
		}
	}
}

func TestValidateAcceptsPublicHTTPS(t *testing.T) {
	if err := Validate("https://example.com/hooks/ocee", false); err != nil {
		t.Errorf("Validate rejected a public URL: %v", err)
	}
}

// The escape hatch exists for local development, but it must only relax the
// address check, never the scheme or syntax checks.
func TestAllowPrivateOnlyRelaxesTheAddressCheck(t *testing.T) {
	if err := Validate("http://127.0.0.1:9000/hook", true); err != nil {
		t.Errorf("loopback should be permitted when explicitly allowed: %v", err)
	}
	for _, u := range []string{"file:///etc/passwd", "not-a-url", "http://"} {
		if err := Validate(u, true); err == nil {
			t.Errorf("Validate(%q, true) = nil; malformed URLs must still be rejected", u)
		}
	}
}

func TestDeliverPostsJSONAndSucceedsOn2xx(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := New(5*time.Second).Deliver(context.Background(), srv.URL, map[string]any{"status": "completed"})
	if err != nil {
		t.Fatalf("Deliver returned %v", err)
	}
	if got["status"] != "completed" {
		t.Errorf("receiver saw %v", got)
	}
}

func TestDeliverReportsRetryableErrorOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	err := New(5*time.Second).Deliver(context.Background(), srv.URL, map[string]any{})
	if err == nil {
		t.Fatal("expected an error so Asynq schedules a retry")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should name the status, got %v", err)
	}
}

// A receiver that 3xx-redirects would otherwise be able to bounce the request
// to an address Validate already rejected.
func TestDeliverDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	err := New(5*time.Second).Deliver(context.Background(), redirector.URL, map[string]any{})
	if err == nil {
		t.Error("a 302 is not a delivery and should be reported as a failure")
	}
	if hits.Load() != 0 {
		t.Errorf("redirect target was contacted %d times, want 0", hits.Load())
	}
}
