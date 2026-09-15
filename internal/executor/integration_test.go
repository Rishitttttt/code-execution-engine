//go:build integration

// These tests drive a real Docker daemon and pull real language images.
// Run them with:
//
//	go test -tags=integration -timeout 20m ./internal/executor/...
package executor

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Rishitttttt/code-execution-engine/internal/config"
	"github.com/Rishitttttt/code-execution-engine/internal/lang"
)

func newTestExecutor(t *testing.T) *Executor {
	// The production default. Anything shorter makes Java flaky here: JVM cold
	// start alone can take several seconds on a loaded host, and a tighter
	// budget tests the host's mood rather than the engine.
	return newTestExecutorWithRunTimeout(t, 10*time.Second)
}

func newTestExecutorWithRunTimeout(t *testing.T, runTimeout time.Duration) *Executor {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunTimeout = runTimeout
	cfg.CompileTimeout = 30 * time.Second

	e, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Ping(ctx); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
	return e
}

func run(t *testing.T, e *Executor, langID, code, stdin string) *Outcome {
	t.Helper()
	spec, ok := lang.Get(langID)
	if !ok {
		t.Fatalf("unknown language %s", langID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	out, err := e.Execute(ctx, spec, code, stdin)
	if err != nil {
		t.Fatalf("%s: engine error: %v", langID, err)
	}
	return out
}

func TestEachLanguageRunsHelloWorld(t *testing.T) {
	e := newTestExecutor(t)

	cases := []struct{ id, code string }{
		{"python", `print("hello")`},
		{"node", `console.log("hello")`},
		{"c", "#include <stdio.h>\nint main(void){printf(\"hello\\n\");return 0;}"},
		{"cpp", "#include <iostream>\nint main(){std::cout << \"hello\" << std::endl;}"},
		{"java", "public class Main { public static void main(String[] a){ System.out.println(\"hello\"); } }"},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			out := run(t, e, tc.id, tc.code, "")
			if strings.TrimSpace(out.Stdout) != "hello" {
				// Include the classification flags: an empty stdout caused by
				// a timeout looks identical to one caused by a broken command
				// unless they are printed.
				t.Errorf("stdout = %q, stderr = %q, compile_output = %q, timed_out = %v, oom = %v, wall = %dms",
					out.Stdout, out.Stderr, out.CompileOutput, out.TimedOut, out.OOMKilled, out.WallTimeMS)
			}
			if out.ExitCode != 0 {
				t.Errorf("exit code = %d", out.ExitCode)
			}
			if out.WallTimeMS <= 0 {
				t.Error("wall time was not measured")
			}
		})
	}
}

func TestStdinIsPipedToTheProgram(t *testing.T) {
	e := newTestExecutor(t)
	out := run(t, e, "python", "import sys\nprint(sum(int(x) for x in sys.stdin))", "1\n2\n3\n")
	if strings.TrimSpace(out.Stdout) != "6" {
		t.Errorf("stdout = %q, want 6", out.Stdout)
	}
}

func TestNonZeroExitIsNotAnEngineFailure(t *testing.T) {
	e := newTestExecutor(t)
	out := run(t, e, "python", "import sys; sys.exit(3)", "")
	if out.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", out.ExitCode)
	}
	if out.TimedOut || out.OOMKilled || out.CompileError {
		t.Errorf("a clean non-zero exit was misclassified: %+v", out)
	}
}

func TestCompileErrorIsReportedSeparately(t *testing.T) {
	e := newTestExecutor(t)
	out := run(t, e, "c", "int main(void){ this is not c }", "")
	if !out.CompileError {
		t.Fatalf("expected a compile error, got %+v", out)
	}
	if out.CompileOutput == "" {
		t.Error("compiler diagnostics were not captured")
	}
	if out.Stdout != "" {
		t.Errorf("nothing should have run, but stdout = %q", out.Stdout)
	}
}

func TestInfiniteLoopHitsTheRunTimeout(t *testing.T) {
	// Short budget on purpose: Python starts fast, so this measures the
	// timeout rather than interpreter startup.
	e := newTestExecutorWithRunTimeout(t, 5*time.Second)
	out := run(t, e, "python", "while True: pass", "")
	if !out.TimedOut {
		t.Errorf("expected TimedOut, got %+v", out)
	}
}

func TestMemoryHogIsOOMKilled(t *testing.T) {
	e := newTestExecutor(t)
	out := run(t, e, "python", "x = bytearray(1024*1024*1024)", "")
	if !out.OOMKilled {
		t.Errorf("expected the memory limit to be enforced, got %+v", out)
	}
}

// The sandbox has no network namespace at all, so this must fail rather than
// resolve or connect.
func TestNetworkIsUnreachable(t *testing.T) {
	e := newTestExecutor(t)
	code := `
import socket, sys
try:
    socket.create_connection(("1.1.1.1", 53), timeout=3)
    print("REACHED")
except Exception as exc:
    print("BLOCKED", type(exc).__name__)
`
	out := run(t, e, "python", code, "")
	if strings.Contains(out.Stdout, "REACHED") {
		t.Errorf("sandbox reached the network: %q", out.Stdout)
	}
}

func TestRootFilesystemIsReadOnly(t *testing.T) {
	e := newTestExecutor(t)
	code := `
try:
    open("/etc/ocee-escape", "w").write("x")
    print("WROTE")
except OSError as exc:
    print("BLOCKED", exc.errno)
`
	out := run(t, e, "python", code, "")
	if strings.Contains(out.Stdout, "WROTE") {
		t.Errorf("root filesystem was writable: %q", out.Stdout)
	}
}

func TestProcessRunsAsNonRoot(t *testing.T) {
	e := newTestExecutor(t)
	out := run(t, e, "python", "import os; print(os.getuid())", "")
	if strings.TrimSpace(out.Stdout) == "0" {
		t.Error("submission ran as root")
	}
}

// The pids limit is the only control that stops a fork bomb; no-network and
// memory caps do nothing here.
func TestForkBombIsContained(t *testing.T) {
	e := newTestExecutor(t)
	code := `
import os
count = 0
try:
    while True:
        os.fork()
        count += 1
except OSError:
    pass
`
	done := make(chan *Outcome, 1)
	go func() { done <- run(t, e, "python", code, "") }()

	select {
	case <-done:
		// Reaching a verdict at all is the assertion: the container was
		// contained and torn down instead of taking the host with it.
	case <-time.After(3 * time.Minute):
		t.Fatal("fork bomb was not contained within 3 minutes")
	}
}

func TestOversizedOutputIsTruncated(t *testing.T) {
	e := newTestExecutor(t)
	out := run(t, e, "python", `print("A" * 5_000_000)`, "")
	if len(out.Stdout) > e.cfg.MaxOutputBytes+1024 {
		t.Errorf("stdout was %d bytes, cap is %d", len(out.Stdout), e.cfg.MaxOutputBytes)
	}
}
