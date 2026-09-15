// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the union of settings used by both the API and the worker.
// Each binary reads only the fields it cares about.
type Config struct {
	// Shared
	DatabaseURL string
	RedisAddr   string

	// API
	APIAddr        string
	MaxCodeBytes   int
	MaxStdinBytes  int
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	ShutdownGrace  time.Duration
	DefaultPageCap int

	// Worker
	WorkerConcurrency int
	DockerHost        string
	PullImages        bool

	// Sandbox limits
	MemoryLimitMB  int64
	CPULimit       float64
	PidsLimit      int64
	CompileTimeout time.Duration
	RunTimeout     time.Duration
	TmpfsSizeMB    int64
	MaxOutputBytes int

	// Webhook
	WebhookTimeout    time.Duration
	WebhookMaxRetries int
	// WebhookAllowPrivate disables the SSRF address check so webhooks can be
	// tested against a receiver on the developer's own machine. Never enable
	// it where the API is reachable by anyone but the operator.
	WebhookAllowPrivate bool
}

// Load reads configuration from the environment, applying defaults for
// everything except DATABASE_URL and REDIS_ADDR, which must be set.
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL: env("DATABASE_URL", "postgres://ocee:ocee@localhost:5432/ocee?sslmode=disable"),
		RedisAddr:   env("REDIS_ADDR", "localhost:6379"),

		APIAddr:        env("API_ADDR", ":8080"),
		MaxCodeBytes:   envInt("MAX_CODE_BYTES", 64_000),
		MaxStdinBytes:  envInt("MAX_STDIN_BYTES", 64_000),
		ReadTimeout:    envDur("READ_TIMEOUT", 10*time.Second),
		WriteTimeout:   envDur("WRITE_TIMEOUT", 15*time.Second),
		ShutdownGrace:  envDur("SHUTDOWN_GRACE", 15*time.Second),
		DefaultPageCap: envInt("PAGE_CAP", 100),

		WorkerConcurrency: envInt("WORKER_CONCURRENCY", 8),
		DockerHost:        env("DOCKER_HOST", ""),
		PullImages:        envBool("PULL_IMAGES", true),

		MemoryLimitMB:  int64(envInt("MEMORY_LIMIT_MB", 256)),
		CPULimit:       envFloat("CPU_LIMIT", 1.0),
		PidsLimit:      int64(envInt("PIDS_LIMIT", 128)),
		CompileTimeout: envDur("COMPILE_TIMEOUT", 20*time.Second),
		RunTimeout:     envDur("RUN_TIMEOUT", 10*time.Second),
		TmpfsSizeMB:    int64(envInt("TMPFS_SIZE_MB", 64)),
		MaxOutputBytes: envInt("MAX_OUTPUT_BYTES", 1<<20),

		WebhookTimeout:      envDur("WEBHOOK_TIMEOUT", 10*time.Second),
		WebhookMaxRetries:   envInt("WEBHOOK_MAX_RETRIES", 5),
		WebhookAllowPrivate: envBool("WEBHOOK_ALLOW_PRIVATE", false),
	}

	if c.WorkerConcurrency < 1 {
		return nil, fmt.Errorf("WORKER_CONCURRENCY must be >= 1, got %d", c.WorkerConcurrency)
	}
	// Code arrives in the sandbox base64-encoded inside an environment
	// variable. Linux caps a single env entry at MAX_ARG_STRLEN (128 KiB),
	// and base64 inflates by 4/3, so anything above ~96 KB of source would
	// fail at exec time with a confusing error instead of a clean 400.
	if c.MaxCodeBytes > 90_000 {
		return nil, fmt.Errorf("MAX_CODE_BYTES must be <= 90000, got %d", c.MaxCodeBytes)
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDur(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
