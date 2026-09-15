// Package executor runs untrusted source code inside a locked-down,
// single-use Docker container and reports what happened.
package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/Rishitttttt/code-execution-engine/internal/config"
	"github.com/Rishitttttt/code-execution-engine/internal/lang"
)

const workdir = "/box"

// Outcome is the result of running one submission.
//
// A program that compiles and then exits non-zero is a *successful* execution:
// Err stays nil and ExitCode carries the program's status. Err is reserved for
// failures of the engine itself.
type Outcome struct {
	Stdout        string
	Stderr        string
	CompileOutput string
	ExitCode      int32

	CPUTimeMS  int64
	WallTimeMS int64
	MemoryKB   int64

	TimedOut     bool
	OOMKilled    bool
	CompileError bool
}

// Executor creates and supervises sandbox containers.
type Executor struct {
	docker *client.Client
	cfg    *config.Config
	log    *slog.Logger

	pullOnce sync.Map // image ref -> *sync.Once
	// imageReady caches images already confirmed present, so the common path
	// does not spend a daemon round trip re-inspecting them on every job.
	imageReady sync.Map // image ref -> struct{}
}

// New connects to the Docker daemon described by the environment.
func New(cfg *config.Config, log *slog.Logger) (*Executor, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connect to docker: %w", err)
	}
	return &Executor{docker: cli, cfg: cfg, log: log}, nil
}

// Ping verifies the Docker daemon is reachable.
func (e *Executor) Ping(ctx context.Context) error {
	_, err := e.docker.Ping(ctx)
	return err
}

// Close releases the Docker client.
func (e *Executor) Close() error { return e.docker.Close() }

// WarmImages pulls every language image up front so the first submission of
// each language does not pay a multi-hundred-megabyte download inside its
// execution timeout.
func (e *Executor) WarmImages(ctx context.Context) {
	if !e.cfg.PullImages {
		return
	}
	for _, ref := range lang.Images() {
		if err := e.ensureImage(ctx, ref); err != nil {
			e.log.Warn("image warm-up failed", "image", ref, "err", err)
			continue
		}
		e.log.Info("image ready", "image", ref)
	}
}

// Execute compiles (when required) and runs code, returning what the program
// produced. The returned error is non-nil only for engine-level failures.
func (e *Executor) Execute(ctx context.Context, spec lang.Spec, code, stdin string) (*Outcome, error) {
	if err := e.ensureImage(ctx, spec.Image); err != nil {
		return nil, err
	}

	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}

	// The engine's own deadline sits above the in-container timeouts so a
	// wedged container (a shell that ignores SIGKILL delivery, a stuck image
	// layer) still gets torn down rather than pinning a worker forever.
	budget := e.cfg.RunTimeout + e.cfg.CompileTimeout + 30*time.Second
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	id, err := e.createContainer(runCtx, spec, code, nonce)
	if err != nil {
		return nil, err
	}
	defer e.remove(id)

	return e.runContainer(runCtx, id, nonce, stdin)
}

func (e *Executor) createContainer(ctx context.Context, spec lang.Spec, code, nonce string) (string, error) {
	env := []string{
		"OCEE_NONCE=" + nonce,
		"OCEE_WORKDIR=" + workdir,
		"OCEE_FILE=" + spec.Filename,
		"OCEE_CODE_B64=" + base64.StdEncoding.EncodeToString([]byte(code)),
		"OCEE_COMPILE=" + spec.Compile,
		"OCEE_RUN=" + spec.Run,
		"OCEE_COMPILE_SECS=" + strconv.Itoa(int(e.cfg.CompileTimeout.Seconds())),
		"OCEE_RUN_SECS=" + strconv.Itoa(int(e.cfg.RunTimeout.Seconds())),
		// The rootfs is read-only, so anything that reaches for a home or a
		// scratch directory has to be pointed at the tmpfs workdir.
		"HOME=" + workdir,
		"TMPDIR=" + workdir,
	}

	cfg := &container.Config{
		Image:           spec.Image,
		Cmd:             []string{"/bin/sh", "-c", sandboxScript},
		Env:             env,
		WorkingDir:      workdir,
		User:            "65534:65534", // nobody
		Tty:             false,         // keeps stdout and stderr separately framed
		OpenStdin:       true,
		StdinOnce:       true,
		AttachStdin:     true,
		AttachStdout:    true,
		AttachStderr:    true,
		NetworkDisabled: true,
		ExposedPorts:    nat.PortSet{},
	}

	host := &container.HostConfig{
		NetworkMode:    "none",
		ReadonlyRootfs: true,
		AutoRemove:     false, // the exit state has to be inspectable first
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Tmpfs: map[string]string{
			// exec is required: compiled languages run a binary from here.
			workdir: fmt.Sprintf("rw,exec,nosuid,nodev,size=%dm,mode=1777", e.cfg.TmpfsSizeMB),
		},
		Resources: container.Resources{
			Memory: e.cfg.MemoryLimitMB << 20,
			// Equal to Memory, i.e. swap disabled. Without this a container
			// under memory pressure swaps instead of being OOM-killed and the
			// memory limit silently stops being a limit.
			MemorySwap: e.cfg.MemoryLimitMB << 20,
			NanoCPUs:   int64(e.cfg.CPULimit * 1e9),
			// The single most important knob against fork bombs; no-network
			// and memory caps do nothing to stop one.
			PidsLimit: &e.cfg.PidsLimit,
			Ulimits: []*container.Ulimit{
				{Name: "nofile", Soft: 256, Hard: 256},
				{Name: "fsize", Soft: 32 << 20, Hard: 32 << 20},
			},
		},
		LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{
			"max-size": "8m",
			"max-file": "1",
		}},
	}

	resp, err := e.docker.ContainerCreate(ctx, cfg, host, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	return resp.ID, nil
}

func (e *Executor) runContainer(ctx context.Context, id, nonce, stdin string) (*Outcome, error) {
	// Attach before start so no output is missed on fast-exiting programs.
	att, err := e.docker.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	defer att.Close()

	started := time.Now()
	if err := e.docker.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}

	// Wait is registered *after* start, never before. A created-but-unstarted
	// container already satisfies WaitConditionNotRunning, so waiting first
	// races: on a fast daemon the wait is answered immediately with exit code
	// 0 before the container has run at all. Waiting afterwards is safe here
	// because AutoRemove is off, so the exit status survives for the daemon to
	// report even if the container is already gone.
	waitCh, errCh := e.docker.ContainerWait(ctx, id, container.WaitConditionNotRunning)

	// Feed stdin in the background: a program that never reads it would
	// otherwise block this goroutine on a full pipe buffer forever.
	go func() {
		if stdin != "" {
			_, _ = io.Copy(att.Conn, strings.NewReader(stdin))
		}
		_ = att.CloseWrite()
	}()

	var outBuf, errBuf bytes.Buffer
	copyDone := make(chan error, 1)
	go func() {
		cap := int64(e.cfg.MaxOutputBytes)
		_, err := stdcopy.StdCopy(
			&limitedWriter{w: &outBuf, remaining: cap},
			&limitedWriter{w: &errBuf, remaining: cap},
			att.Reader,
		)
		copyDone <- err
	}()

	var exitCode int64
	select {
	case werr := <-errCh:
		if werr != nil {
			return nil, fmt.Errorf("wait: %w", werr)
		}
	case st := <-waitCh:
		exitCode = st.StatusCode
	case <-ctx.Done():
		e.kill(id)
		return nil, fmt.Errorf("execution exceeded engine budget: %w", ctx.Err())
	}
	wall := time.Since(started)

	// The stream closes once the container exits; bound the wait so a wedged
	// attach cannot outlive the container it belongs to. Losing this race
	// costs the metrics marker, which is the last thing written, so the grace
	// is generous rather than tight.
	select {
	case <-copyDone:
	case <-time.After(15 * time.Second):
		e.log.Warn("output stream did not close after exit", "container", id[:12])
	}

	inspected, ierr := e.docker.ContainerInspect(ctx, id)
	oom := ierr == nil && inspected.State != nil && inspected.State.OOMKilled

	return e.buildOutcome(outBuf.String(), errBuf.String(), nonce, exitCode, wall, oom), nil
}

// buildOutcome reconciles the in-container marker with what the daemon
// observed from the outside.
func (e *Executor) buildOutcome(stdout, stderr, nonce string, containerExit int64, wall time.Duration, oom bool) *Outcome {
	out := &Outcome{
		Stdout:     stdout,
		WallTimeMS: wall.Milliseconds(),
		OOMKilled:  oom,
	}

	cleanErr, m, found := extractMarker(stderr, nonce)
	out.Stderr = cleanErr

	if !found {
		// No marker means the shell never reached its final line: the kernel
		// or the daemon took the container down mid-flight.
		out.ExitCode = int32(containerExit)
		switch {
		case oom:
			out.MemoryKB = e.cfg.MemoryLimitMB << 10
		case wall >= e.cfg.RunTimeout:
			out.TimedOut = true
		}
		return out
	}

	out.CPUTimeMS = m.cpuUsec / 1000
	out.MemoryKB = m.memBytes / 1024
	out.ExitCode = int32(m.rc)

	switch m.phase {
	case "compile":
		out.CompileError = true
		out.CompileOutput = m.compileOutput
		// 137 is SIGKILL, which for the compile step only comes from `timeout`.
		if m.rc == 137 {
			out.TimedOut = true
		}
	case "run":
		// 137 is ambiguous: it is both the timeout kill and the OOM kill.
		// The elapsed-seconds counter recorded inside the container settles it.
		if m.rc == 137 && !oom && m.secs >= int64(e.cfg.RunTimeout.Seconds()) {
			out.TimedOut = true
		}
	}
	return out
}

type marker struct {
	phase         string
	rc            int64
	cpuUsec       int64
	memBytes      int64
	secs          int64
	compileOutput string
}

// extractMarker pulls the trailing metrics line out of stderr and returns the
// stderr the user should actually see.
func extractMarker(stderr, nonce string) (string, marker, bool) {
	idx := strings.LastIndex(stderr, nonce+" phase=")
	if idx < 0 {
		return stderr, marker{}, false
	}
	line := stderr[idx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}

	// The marker is printed with a leading newline to survive a program whose
	// stderr lacks a trailing one; drop that separator too.
	clean := stderr[:idx]
	clean = strings.TrimSuffix(clean, "\n")

	m := marker{}
	for _, field := range strings.Fields(strings.TrimPrefix(line, nonce+" ")) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch k {
		case "phase":
			m.phase = v
		case "rc":
			m.rc, _ = strconv.ParseInt(v, 10, 64)
		case "cpu_usec":
			m.cpuUsec, _ = strconv.ParseInt(v, 10, 64)
		case "mem_bytes":
			m.memBytes, _ = strconv.ParseInt(v, 10, 64)
		case "secs":
			m.secs, _ = strconv.ParseInt(v, 10, 64)
		case "compile_b64":
			if raw, err := base64.StdEncoding.DecodeString(v); err == nil {
				m.compileOutput = string(raw)
			}
		}
	}
	if m.cpuUsec < 0 {
		m.cpuUsec = 0
	}
	return clean, m, m.phase != ""
}

// ensureImage pulls an image at most once per process per reference.
func (e *Executor) ensureImage(ctx context.Context, ref string) error {
	if _, cached := e.imageReady.Load(ref); cached {
		return nil
	}
	if _, _, err := e.docker.ImageInspectWithRaw(ctx, ref); err == nil {
		e.imageReady.Store(ref, struct{}{})
		return nil
	}
	if !e.cfg.PullImages {
		return fmt.Errorf("image %s not present locally and PULL_IMAGES is disabled", ref)
	}

	v, _ := e.pullOnce.LoadOrStore(ref, &sync.Once{})
	once := v.(*sync.Once)

	var pullErr error
	once.Do(func() {
		e.log.Info("pulling image", "image", ref)
		rc, err := e.docker.ImagePull(ctx, ref, image.PullOptions{})
		if err != nil {
			pullErr = fmt.Errorf("pull %s: %w", ref, err)
			// A failed pull must not be cached as done, or every later
			// submission for this language fails with a stale success.
			e.pullOnce.Delete(ref)
			return
		}
		defer rc.Close()
		// The pull only completes once its progress stream is drained.
		if _, err := io.Copy(io.Discard, rc); err != nil {
			pullErr = fmt.Errorf("pull %s: %w", ref, err)
			e.pullOnce.Delete(ref)
		}
	})
	if pullErr != nil {
		return pullErr
	}

	// Another goroutine may have owned the Once; confirm the image landed.
	if _, _, err := e.docker.ImageInspectWithRaw(ctx, ref); err != nil {
		return fmt.Errorf("image %s unavailable after pull: %w", ref, err)
	}
	e.imageReady.Store(ref, struct{}{})
	return nil
}

func (e *Executor) kill(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.docker.ContainerKill(ctx, id, "SIGKILL"); err != nil && !client.IsErrNotFound(err) {
		e.log.Warn("kill container", "container", id[:12], "err", err)
	}
}

func (e *Executor) remove(id string) {
	// Detached from the request context on purpose: cleanup must still happen
	// when the caller's context is what got cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := e.docker.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !client.IsErrNotFound(err) {
		e.log.Warn("remove container", "container", id[:12], "err", err)
	}
}

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return "__OCEE_" + hex.EncodeToString(b) + "__", nil
}

// limitedWriter caps how much a submission can push into memory. Writes past
// the cap are dropped rather than erroring, so a chatty program is truncated
// instead of failing.
type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	chunk := p
	if int64(len(chunk)) > l.remaining {
		chunk = chunk[:l.remaining]
	}
	n, err := l.w.Write(chunk)
	l.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

var errUnsupportedLanguage = errors.New("unsupported language")

// ErrUnsupportedLanguage is returned when a submission names a language the
// engine does not know.
func ErrUnsupportedLanguage() error { return errUnsupportedLanguage }
