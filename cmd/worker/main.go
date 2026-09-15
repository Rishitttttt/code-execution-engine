// Command worker consumes execution jobs and runs them in sandbox containers.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rishitttttt/code-execution-engine/internal/config"
	"github.com/Rishitttttt/code-execution-engine/internal/executor"
	"github.com/Rishitttttt/code-execution-engine/internal/queue"
	"github.com/Rishitttttt/code-execution-engine/internal/worker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("worker exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connectDB(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	ex, err := executor.New(cfg, log)
	if err != nil {
		return err
	}
	defer ex.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = ex.Ping(pingCtx)
	cancel()
	if err != nil {
		return err
	}
	log.Info("connected to docker daemon")

	// Pull every language image before accepting work. Doing it lazily would
	// charge the first submission of each language a multi-hundred-megabyte
	// download against its own execution timeout.
	warmCtx, cancelWarm := context.WithTimeout(ctx, 20*time.Minute)
	ex.WarmImages(warmCtx)
	cancelWarm()

	redis := asynq.RedisClientOpt{Addr: cfg.RedisAddr}
	client := asynq.NewClient(redis)
	defer client.Close()

	w := worker.New(cfg, pool, ex, client, log)

	srv := asynq.NewServer(redis, asynq.Config{
		// One goroutine per concurrent container. The practical ceiling is
		// host CPU and memory: every slot can hold a container using up to
		// MEMORY_LIMIT_MB and CPU_LIMIT cores.
		Concurrency: cfg.WorkerConcurrency,
		Queues: map[string]int{
			// Weighted, not strict: webhooks are cheap and latency-sensitive,
			// but must never starve behind a backlog of executions.
			queue.QueueDefault:  7,
			queue.QueueWebhooks: 3,
		},
		RetryDelayFunc:  asynq.RetryDelayFunc(queue.RetryDelay),
		ShutdownTimeout: cfg.ShutdownGrace,
		Logger:          asynqLogger{log},
		ErrorHandler: asynq.ErrorHandlerFunc(func(_ context.Context, t *asynq.Task, err error) {
			log.Error("task failed", "type", t.Type(), "err", err)
		}),
	})

	log.Info("worker starting", "concurrency", cfg.WorkerConcurrency)
	// Run blocks until SIGTERM/SIGINT, draining in-flight jobs first.
	if err := srv.Run(w.Mux()); err != nil {
		return err
	}
	log.Info("worker stopped cleanly")
	return nil
}

func connectDB(ctx context.Context, url string, log *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	const attempts = 30
	for i := 1; ; i++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			log.Info("connected to postgres")
			return pool, nil
		}
		if i == attempts || ctx.Err() != nil {
			pool.Close()
			return nil, err
		}
		log.Warn("waiting for postgres", "attempt", i, "err", err)
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		}
	}
}

// asynqLogger adapts slog to the interface Asynq expects.
type asynqLogger struct{ l *slog.Logger }

func (a asynqLogger) Debug(args ...any) { a.l.Debug("asynq", "msg", args) }
func (a asynqLogger) Info(args ...any)  { a.l.Info("asynq", "msg", args) }
func (a asynqLogger) Warn(args ...any)  { a.l.Warn("asynq", "msg", args) }
func (a asynqLogger) Error(args ...any) { a.l.Error("asynq", "msg", args) }
func (a asynqLogger) Fatal(args ...any) { a.l.Error("asynq fatal", "msg", args) }
