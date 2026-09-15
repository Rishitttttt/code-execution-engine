# Code Execution Engine

[![CI](https://github.com/Rishitttttt/code-execution-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/Rishitttttt/code-execution-engine/actions)
[![Go Reference](https://pkg.go.dev/badge/github.com/Rishitttttt/code-execution-engine.svg)](https://pkg.go.dev/github.com/Rishitttttt/code-execution-engine)
[![Go Report Card](https://goreportcard.com/badge/github.com/Rishitttttt/code-execution-engine)](https://goreportcard.com/report/github.com/Rishitttttt/code-execution-engine)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Accepts source code over a REST API, runs it inside a locked-down, single-use Docker container, and returns stdout, stderr, exit code, and resource metrics.

Python, C, C++, Java, and Node.js. Two Go services, one `docker compose up`.

**Stack** — Go (`net/http`, no web framework) · PostgreSQL via sqlc + pgx · Redis + Asynq for queueing · Docker SDK for sandboxing · Docker Compose

---

## Contents

- [Architecture](#architecture)
- [Quickstart](#quickstart)
- [API](#api)
- [Sandboxing](#sandboxing)
- [How code gets in, and metrics get out](#how-code-gets-in-and-metrics-get-out)
- [What the numbers actually mean](#what-the-numbers-actually-mean)
- [Design decisions](#design-decisions)
- [Benchmarks](#benchmarks)
- [Layout](#layout)
- [Development](#development)
- [Known limitations](#known-limitations)

---

## Architecture

```
POST /v1/submissions ──▶ Postgres (durable) ──▶ Redis/Asynq (queue)
                                                      │
                                                      ▼
                                            worker pool (N goroutines)
                                                      │
                                            one sandbox container per job
                                              no network · read-only rootfs
                                              non-root · memory/CPU/pids caps
                                                      │
                                    ┌─────────────────┴─────────────────┐
                                    ▼                                   ▼
                          UPDATE submissions                  POST webhook_url
                          (poll GET /v1/…)                  (exponential backoff)
```

Two binaries. The API service is deliberately thin — validate, persist, enqueue, return `202`. Everything expensive happens in the worker, which owns the Docker socket and the sandbox lifecycle.

---

## Quickstart

Requires Docker and Docker Compose. Nothing else — Go is only needed to run the tests or the load harness.

```bash
git clone https://github.com/Rishitttttt/code-execution-engine.git
cd code-execution-engine
docker compose up --build -d
```

The worker pulls every language image before accepting work — roughly 2 GB on first boot, mostly the `gcc:14` image. Watch it with `docker compose logs -f worker` and wait for `worker starting`.

**Submit:**

```bash
curl -s localhost:8080/v1/submissions \
  -H 'content-type: application/json' \
  -d '{"language":"python","code":"print(sum(int(x) for x in input().split()))","stdin":"1 2 3"}'
```

```json
{
  "id": "0c0e1b8e-...",
  "status": "queued",
  "created_at": "2026-09-13T04:10:22Z",
  "result_url": "/v1/submissions/0c0e1b8e-..."
}
```

**Poll:**

```bash
curl -s localhost:8080/v1/submissions/0c0e1b8e-...
```

```json
{
  "id": "0c0e1b8e-...",
  "language": "python",
  "status": "completed",
  "stdout": "6\n",
  "stderr": "",
  "exit_code": 0,
  "timed_out": false,
  "oom_killed": false,
  "metrics": { "cpu_time_ms": 21, "wall_time_ms": 412, "memory_kb": 9284 }
}
```

---

## API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/submissions` | Submit code. Returns `202` immediately. |
| `GET` | `/v1/submissions/{id}` | Fetch one submission and its result. |
| `GET` | `/v1/submissions` | List submissions (`?limit=&offset=`). |
| `GET` | `/v1/languages` | Supported languages, versions, images. |
| `GET` | `/healthz` | Liveness. Touches no dependency. |
| `GET` | `/readyz` | Readiness. Checks Postgres and Redis. |

**Request body**

| Field | Type | Required | Notes |
|---|---|---|---|
| `language` | string | yes | `python` · `c` · `cpp` · `java` · `node` |
| `code` | string | yes | ≤ 64 KB, valid UTF-8 |
| `stdin` | string | no | ≤ 64 KB, piped to the program |
| `webhook_url` | string | no | http/https, public addresses only |

Java submissions must declare `public class Main`.

**Status values** — `queued` → `running` → `completed` \| `failed`.

`failed` means *the engine* failed (Docker unreachable, image missing). A program that crashes, exits non-zero, or fails to compile is `completed` — the engine did its job. That distinction is deliberate and runs through the whole codebase: it is why `handleExecute` returns `nil` for a compile error but an error for a daemon outage.

### Webhooks

When `webhook_url` is set, the worker enqueues a separate delivery job on its own queue. Retries are Asynq's, using exponential backoff with full jitter capped at 5 minutes. Redirects are not followed, and any `2xx` counts as delivered. Delivery failure never affects the stored result, so a dead receiver degrades the caller to polling rather than losing data.

URLs resolving to private, loopback, or link-local addresses are rejected — the worker can reach Postgres, Redis, and the Docker socket host, so an unchecked callback URL is an SSRF probe into that network. Which also means a receiver on your own machine is blocked by default. To test one locally:

```bash
WEBHOOK_ALLOW_PRIVATE=true docker compose up -d
# then submit with "webhook_url": "http://host.docker.internal:9099/hook"
```

That flag relaxes only the address check; malformed URLs and non-HTTP schemes are still rejected. Leave it off anywhere the API is not solely yours.

---

## Sandboxing

Every submission gets a fresh container that is destroyed afterwards.

| Control | Setting | What it stops |
|---|---|---|
| Network | `NetworkMode: none` | Exfiltration, dependency fetching, using the runner as a proxy |
| Filesystem | `ReadonlyRootfs`, tmpfs `/box` (`nosuid,nodev`, size-capped) | Tampering with the image; filling the host disk |
| User | `65534:65534` | Root-owned side effects; a whole class of escapes |
| Capabilities | `CapDrop: ALL` | `CAP_SYS_ADMIN` and friends |
| Privilege escalation | `no-new-privileges` | setuid binaries inside the image |
| Memory | `Memory` + `MemorySwap` equal | Host exhaustion. Equal values disable swap — without that, a container under pressure swaps instead of being OOM-killed and the limit quietly stops being one |
| CPU | `NanoCPUs` | One submission starving the pool |
| Processes | `PidsLimit: 128` | Fork bombs. Nothing else on this list stops `while True: os.fork()` |
| Open files | `nofile 256` | Descriptor exhaustion |
| File size | `fsize 32 MB` | Runaway writes inside tmpfs |
| Time | in-container timeout + host-side context budget | Infinite loops, and containers that ignore the first kill |
| Output | 1 MB cap per stream | A program printing forever OOM-ing the worker |

`/box` is mounted `exec` on purpose — compiled languages have to run a binary from it. `noexec` there would break C, C++, and Java outright.

The integration suite asserts these are real, not aspirational: it runs a fork bomb, a memory hog, a network connect, a write to `/etc`, and a `getuid()` check against a live daemon (`make test-integration`).

---

## How code gets in, and metrics get out

Two non-obvious mechanics, both chosen to dodge a specific trap.

**Code travels in an environment variable, base64-encoded.** The intuitive approach — `CopyToContainer` — silently fails here. A tmpfs mount is created at container start, so files copied before start are shadowed by the empty tmpfs that mounts over them. Starting the container first and then copying means an exec lifecycle and a second set of failure modes. Passing the source through the environment removes the ordering problem entirely and leaves stdin free for the program's actual input. The cost is a size ceiling: Linux caps one env entry at 128 KB, base64 inflates by 4/3, hence the 64 KB limit on code.

**Metrics are read from inside the container.** Docker's stats API streams at 1 Hz, which measures nothing useful about a 50 ms program. Instead the sandbox wrapper reads its own cgroup at exit — `memory.peak` and `cpu.stat` on cgroup v2, with v1 fallbacks — and prints one marker line to stderr. The worker splits that line off before the user ever sees stderr.

The marker carries a 128-bit random nonce generated per run, so a program that prints marker-shaped text cannot forge its own metrics. There is a unit test for exactly that.

---

## What the numbers actually mean

**`cpu_time_ms`** — CPU consumed by the program alone. The wrapper snapshots cgroup CPU after compiling and subtracts, so C++ build time is not billed to the program.

**`wall_time_ms`** — measured host-side around container start and exit, so it includes container startup (~100–300 ms). It is not the program's runtime.

**`memory_kb`** — the container's peak, which for C, C++, and Java includes the compiler, since compile and run share one cgroup. For those languages, read it as `max(compiler, program)`. Fixing this properly means two containers sharing a volume; the accuracy was not worth that complexity here, and reporting an inflated number honestly beats reporting a clean-looking wrong one.

**`timed_out` vs `oom_killed`** — both surface as SIGKILL (137) and are disambiguated by the elapsed-seconds counter recorded inside the container plus the daemon's `State.OOMKilled`. If the container dies before writing a marker at all, the worker falls back entirely to what the daemon observed.

---

## Design decisions

**Persist, then enqueue.** The row is written before the job. The reverse order lets a worker dequeue a job whose row does not exist yet. If the enqueue then fails, the submission is marked `failed` rather than left polling forever.

**Only the ID goes through Redis.** Code lives in Postgres. The queue stays small and there is exactly one source of truth for submission state.

**Execution jobs are idempotent-safe.** A retry that lands after the job already finished checks the status and returns instead of re-running the code.

**Container per submission, not a warm pool.** A pool would cut ~200–300 ms of startup, at the cost of proving that no state survives between tenants. Fresh containers make isolation the default rather than something to audit.

**Compile and run timeouts are separate.** A 2 s C++ compile and a 10 s runtime budget are different things; one combined timeout would report slow compiles as the user's program timing out.

Java is the tightest fit against `RUN_TIMEOUT`: JVM cold start alone can take several seconds on a loaded host, so a hello-world can time out under a budget that every other language clears easily. Dropping `RUN_TIMEOUT` below ~8 s starts failing Java submissions for reasons that have nothing to do with the submitted code. The integration suite hit exactly this and now runs the language checks at the production default.

**Weighted queues, not strict priority.** Webhooks are cheap and latency-sensitive but must never starve behind an execution backlog: 7/3.

**Plain `net/http`.** Go 1.22 method-and-wildcard patterns cover this API; a router dependency would earn nothing.

---

## Benchmarks

`make loadtest N=100 C=10` submits N jobs at concurrency C and reports end-to-end latency — submit, queue wait, container start, execute, and the poll that observes the result. That is what an API consumer feels, and it is deliberately larger than the engine's own `wall_time_ms`.

Measured on Windows 11 / Docker Desktop 29.2.1 (WSL2 backend), 60 Python jobs, 20 concurrent clients:

| `WORKER_CONCURRENCY` | Throughput | Mean latency | p50 |
|---|---|---|---|
| 4 | 0.86 /s | 20.3 s | 22.3 s |
| 8 | 0.84 /s | 20.2 s | 21.5 s |
| 16 | 0.95 /s | 18.1 s | 20.2 s |

Throughput is flat from 4 to 16 workers. That is the whole story: the worker pool is not the bottleneck, the Docker daemon is. Timing a bare container lifecycle on the same host confirms it:

```
create      907 ms
start+wait 1047 ms
remove      664 ms
            ~2.6 s per container, serial
```

At ~2.6 s of daemon work per submission, ~1 submission/sec is the ceiling no matter how many goroutines wait on it. Adding workers past that point only lengthens the queue — visible above as latency that does not improve proportionally.

Two things follow, and both are worth stating plainly:

1. **This is a Docker Desktop / WSL2 number, not a Linux one.** Container churn on a native Linux daemon is roughly an order of magnitude cheaper. The architecture is not what caps this at 1/s.
2. **An optimisation was tried and reverted.** Container removal costs 664 ms on the execution path, so cleanup was moved to a bounded background reaper to free the worker slot sooner. Measured result: 0.95 /s and 18.1 s mean — identical to synchronous cleanup, because the daemon was already saturated. The code was reverted. Complexity that does not show up in a measurement does not earn its place.

What did survive is smaller and provable: images confirmed present are cached in-process, removing one daemon round trip per submission.

Reproduce with `make loadtest N=60 C=20`.

---

## Layout

```
cmd/api            REST service entrypoint
cmd/worker         queue consumer entrypoint
internal/api       handlers, DTOs, middleware
internal/worker    Asynq job handlers
internal/executor  Docker sandbox — the interesting part
internal/lang      language registry (image, filename, compile/run commands)
internal/queue     Asynq task definitions and retry policy
internal/webhook   delivery client and SSRF validation
internal/db        sqlc-generated, do not edit
db/migrations      schema (applied by Postgres on first boot)
db/query           SQL that sqlc compiles into internal/db
scripts/loadtest   benchmark harness
```

Adding a language is one entry in `internal/lang/lang.go`. Nothing else changes.

---

## Development

```bash
make test              # unit tests, no Docker needed
make test-integration  # sandbox assertions against a real daemon
make lint              # vet + gofmt
make sqlc              # regenerate internal/db after editing db/query
make reset             # down -v; required after a schema change
```

Migrations are applied by Postgres from `/docker-entrypoint-initdb.d` on first boot only, so editing `db/migrations` means `make reset`.

---

## Known limitations

Listed because they are real, not because they are hypothetical.

- **The worker mounts `/var/run/docker.sock`.** That is root-equivalent access to the host. It is the standard local-dev arrangement and the reason the sandbox containers themselves are constrained so tightly, but it is not how this should run in production. Real deployment: dedicated worker hosts, or a socket proxy allowlisting only the container calls the engine makes.
- **No authentication or rate limiting.** An unauthenticated arbitrary-code-execution endpoint is fine on localhost and nowhere else. API keys plus a per-key token bucket would be the first thing to add.
- **Webhook SSRF validation resolves DNS at submit time.** A name that resolves public then private is a rebinding window. Closing it needs a `DialContext` that re-checks the resolved IP at connection time.
- **Peak memory includes the compiler** for C, C++, and Java — see [What the numbers actually mean](#what-the-numbers-actually-mean).
- **Schema changes require dropping the volume.** Fine locally; a real deployment wants `golang-migrate` and versioned migrations.
- **`gcc:14` is ~1.5 GB.** Official upstream images were chosen over custom ones; purpose-built images with only the toolchain would cut first-boot time substantially.
- **No metrics endpoint.** Queue depth, execution duration histograms, and per-language failure counters all belong in Prometheus.

---

## License

MIT — see [LICENSE](LICENSE).
