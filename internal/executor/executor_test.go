package executor

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Rishitttttt/code-execution-engine/internal/config"
)

const testNonce = "__OCEE_deadbeef__"

func testExecutor() *Executor {
	return &Executor{
		cfg: &config.Config{
			RunTimeout:     10 * time.Second,
			CompileTimeout: 20 * time.Second,
			MemoryLimitMB:  256,
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func markerLine(phase string, rc, cpu, mem, secs int64, compile string) string {
	return fmt.Sprintf("\n%s phase=%s rc=%d cpu_usec=%d mem_bytes=%d secs=%d compile_b64=%s\n",
		testNonce, phase, rc, cpu, mem, secs, base64.StdEncoding.EncodeToString([]byte(compile)))
}

func TestExtractMarkerSeparatesProgramStderr(t *testing.T) {
	stderr := "traceback line 1\ntraceback line 2" + markerLine("run", 1, 5000, 2_097_152, 0, "")

	clean, m, ok := extractMarker(stderr, testNonce)
	if !ok {
		t.Fatal("expected marker to be found")
	}
	if clean != "traceback line 1\ntraceback line 2" {
		t.Errorf("program stderr was not preserved verbatim: %q", clean)
	}
	if m.phase != "run" || m.rc != 1 || m.cpuUsec != 5000 || m.memBytes != 2_097_152 {
		t.Errorf("marker parsed incorrectly: %+v", m)
	}
}

func TestExtractMarkerMissing(t *testing.T) {
	// A container killed before the shell finished never writes a marker.
	clean, _, ok := extractMarker("segmentation fault\n", testNonce)
	if ok {
		t.Fatal("expected no marker")
	}
	if clean != "segmentation fault\n" {
		t.Errorf("stderr should pass through untouched, got %q", clean)
	}
}

// A submission that prints something marker-shaped must not be able to
// overwrite its own reported metrics: the real marker is appended last and
// carries the unguessable nonce.
func TestExtractMarkerIgnoresSpoofedLine(t *testing.T) {
	spoof := "__OCEE_0000__ phase=run rc=0 cpu_usec=0 mem_bytes=0 secs=0 compile_b64=\n"
	stderr := spoof + markerLine("run", 42, 1000, 4096, 0, "")

	_, m, ok := extractMarker(stderr, testNonce)
	if !ok {
		t.Fatal("expected real marker to be found")
	}
	if m.rc != 42 {
		t.Errorf("spoofed marker won; rc = %d, want 42", m.rc)
	}
}

func TestBuildOutcomeCompileError(t *testing.T) {
	e := testExecutor()
	stderr := markerLine("compile", 1, 900_000, 100<<20, 0, "main.c:3:1: error: expected ';'")

	out := e.buildOutcome("", stderr, testNonce, 0, 1200*time.Millisecond, false)

	if !out.CompileError {
		t.Error("expected CompileError to be set")
	}
	if !strings.Contains(out.CompileOutput, "expected ';'") {
		t.Errorf("compiler diagnostics missing: %q", out.CompileOutput)
	}
	if out.Stderr != "" {
		t.Errorf("compile diagnostics must not leak into program stderr: %q", out.Stderr)
	}
	if out.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", out.ExitCode)
	}
}

// 137 is SIGKILL, which the run phase sees both on timeout and on OOM. The
// elapsed-seconds counter recorded inside the container is what tells them
// apart, so each combination is pinned here.
func TestBuildOutcomeDistinguishesTimeoutFromOOM(t *testing.T) {
	e := testExecutor()

	tests := []struct {
		name        string
		secs        int64
		oomFlag     bool
		wantTimeout bool
		wantOOM     bool
	}{
		{"killed at the timeout boundary", 10, false, true, false},
		{"killed early by the OOM killer", 0, true, false, true},
		{"OOM flag wins over a long runtime", 10, true, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stderr := markerLine("run", 137, 2000, 268_435_456, tc.secs, "")
			out := e.buildOutcome("", stderr, testNonce, 137, 11*time.Second, tc.oomFlag)

			if out.TimedOut != tc.wantTimeout {
				t.Errorf("TimedOut = %v, want %v", out.TimedOut, tc.wantTimeout)
			}
			if out.OOMKilled != tc.wantOOM {
				t.Errorf("OOMKilled = %v, want %v", out.OOMKilled, tc.wantOOM)
			}
		})
	}
}

// Without a marker the engine has to fall back on what the daemon observed.
func TestBuildOutcomeNoMarkerFallsBackToDaemonState(t *testing.T) {
	e := testExecutor()

	out := e.buildOutcome("", "", testNonce, 137, 15*time.Second, false)
	if !out.TimedOut {
		t.Error("a container that outlived the run timeout should report a timeout")
	}

	out = e.buildOutcome("", "", testNonce, 137, time.Second, true)
	if !out.OOMKilled || out.TimedOut {
		t.Errorf("expected OOM only, got timeout=%v oom=%v", out.TimedOut, out.OOMKilled)
	}
}

func TestBuildOutcomeNormalExitConvertsUnits(t *testing.T) {
	e := testExecutor()
	// 1_500_000 µs of CPU, 4 MiB peak.
	stderr := markerLine("run", 0, 1_500_000, 4_194_304, 1, "")

	out := e.buildOutcome("hello\n", stderr, testNonce, 0, 1600*time.Millisecond, false)

	if out.CPUTimeMS != 1500 {
		t.Errorf("CPUTimeMS = %d, want 1500", out.CPUTimeMS)
	}
	if out.MemoryKB != 4096 {
		t.Errorf("MemoryKB = %d, want 4096", out.MemoryKB)
	}
	if out.WallTimeMS != 1600 {
		t.Errorf("WallTimeMS = %d, want 1600", out.WallTimeMS)
	}
	if out.Stdout != "hello\n" || out.TimedOut || out.OOMKilled || out.CompileError {
		t.Errorf("unexpected outcome: %+v", out)
	}
}

func TestLimitedWriterTruncatesButKeepsAccepting(t *testing.T) {
	var buf bytes.Buffer
	w := &limitedWriter{w: &buf, remaining: 10}

	// Reports the full length so io.Copy does not treat truncation as a short
	// write and abort the stream.
	n, err := w.Write([]byte("0123456789abcdef"))
	if err != nil || n != 16 {
		t.Fatalf("Write = (%d, %v), want (16, nil)", n, err)
	}
	if _, err := w.Write([]byte("more")); err != nil {
		t.Fatalf("writes past the cap should be dropped, not fail: %v", err)
	}
	if buf.String() != "0123456789" {
		t.Errorf("buffered %q, want %q", buf.String(), "0123456789")
	}
}

func TestNonceIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		n, err := newNonce()
		if err != nil {
			t.Fatal(err)
		}
		if seen[n] {
			t.Fatalf("nonce repeated: %s", n)
		}
		seen[n] = true
	}
}
