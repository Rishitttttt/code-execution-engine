// Command api serves the OCEE REST API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rishitttttt/code-execution-engine/internal/api"
	"github.com/Rishitttttt/code-execution-engine/internal/config"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("api exited", "err", err)
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

	redis := asynq.RedisClientOpt{Addr: cfg.RedisAddr}
	client := asynq.NewClient(redis)
	defer client.Close()
	inspector := asynq.NewInspector(redis)
	defer inspector.Close()

	srv := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           api.NewServer(cfg, pool, client, inspector, log).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.APIAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received", "grace", cfg.ShutdownGrace.String())
	}

	// Let in-flight requests finish so a deploy does not turn into a burst of
	// client-side errors.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("api stopped cleanly")
	return nil
}

// connectDB waits for Postgres to accept connections. Compose starts the API
// and the database together, so the first few attempts routinely fail while
// Postgres is still initialising.
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
