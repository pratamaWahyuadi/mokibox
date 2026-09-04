// Package main runs the transcoder-worker binary. The
// worker consumes Asynq tasks from Redis (transcode:video,
// cleanup:objects, cleanup:video) and never exposes any
// HTTP port - SEC-02 requires process isolation, and
// NFR-11 leaves no business reason for the worker to be
// reachable from the public internet.
//
// Structure:
//
//	main.go      - lifecycle: load config, build clients,
//	               wire the Asynq mux, install signal
//	               handlers, run srv.Run / srv.Shutdown.
//	ffprobe.go   - SEC-01: ffprobe + ValidateMedia. Pure
//	               functions so they can be table-tested
//	               without a running ffmpeg binary.
//	transcode.go - HandleTranscode (FR-VIDEO-04..09).
//	cleanup.go   - HandleCleanupObjects + HandleCleanupVideo
//	               (NFR-13, FR-VIDEO-11).
//
// All dependencies (*sql.DB pool, sqlc queries, R2
// client, asynq server, logger) are wired into the
// Worker struct via NewWorker so the handlers can be
// table-tested in phase 10 by substituting mocks.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hibiken/asynq"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// Worker holds every dependency the handlers need. DB
// is the single *sql.DB pool (via pgx stdlib) shared by
// sqlc Queries (read path) and Queries.WithTx
// (transaction path). The concrete struct is returned
// from NewWorker; handlers that want to mock can
// declare a small interface in their own package.
//
// Logger is the slog.Logger that all handlers use to log
// failures with structured fields. Per
// hermes-go-idiomatic: structured logs always carry
// "video_id" / "task_id" / "op" so failures can be
// correlated back to the request that produced them.
type Worker struct {
	DB      *sql.DB
	Queries *db.Queries
	R2      *shared.R2Client
	Asynq   *asynq.Client // for re-enqueue + cleanup enqueue
	Cfg     *shared.WorkerConfig
	Logger  *slog.Logger
}

// NewWorker wires all dependencies. Any nil dep is
// rejected at startup so the failure mode is loud and
// immediate instead of a nil-pointer panic from a handler
// at runtime (defence-in-depth per mokibox-go-shared).
//
// Asynq client is built here (the worker also produces
// retry tasks and cleanup tasks, not just processes them)
// so the constructor can return a single, fully-wired
// Worker value.
func NewWorker(ctx context.Context, cfg *shared.WorkerConfig, logger *slog.Logger) (*Worker, error) {
	if cfg == nil {
		return nil, fmt.Errorf("NewWorker: cfg is nil")
	}
	if logger == nil {
		return nil, fmt.Errorf("NewWorker: logger is nil")
	}

	sqlDB, err := shared.NewSQLDB(ctx, cfg.WorkerDatabaseURL, shared.WorkerPoolMaxConns, shared.WorkerPoolMaxConns)
	if err != nil {
		return nil, fmt.Errorf("NewWorker: open postgres pool: %w", err)
	}

	queries := db.New(sqlDB)

	r2, err := shared.NewR2Client(ctx, shared.R2Config{
		AccountID:       cfg.R2AccountID,
		AccessKeyID:     cfg.R2AccessKeyID,
		SecretAccessKey: cfg.R2SecretAccessKey,
		Bucket:          cfg.R2Bucket,
		Endpoint:        cfg.R2Endpoint,
	})
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("NewWorker: r2 client: %w", err)
	}

	asynqClient, err := shared.NewAsynqClient(shared.RedisConfig{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	})
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("NewWorker: asynq client: %w", err)
	}

	return &Worker{
		DB:      sqlDB,
		Queries: queries,
		R2:      r2,
		Asynq:   asynqClient,
		Cfg:     cfg,
		Logger:  logger,
	}, nil
}

// Close releases every pooled resource. Called from
// main on shutdown so the process exits cleanly under
// SIGINT / SIGTERM. Order matters: asynq client first
// (no shared resources), then the DB pool.
func (w *Worker) Close() {
	if w.Asynq != nil {
		_ = w.Asynq.Close()
	}
	if w.DB != nil {
		_ = w.DB.Close()
	}
}

// registerHandlers wires the three task types the worker
// consumes. The handler bodies live in transcode.go
// (HandleTranscode) and cleanup.go
// (HandleCleanupObjects, HandleCleanupVideo); splitting
// them from main.go keeps each pipeline readable and
// lets issue-level commits stay focused.
func (w *Worker) registerHandlers(mux *asynq.ServeMux) {
	mux.HandleFunc(shared.TypeTranscodeVideo, w.HandleTranscode)
	mux.HandleFunc(shared.TypeCleanupObjects, w.HandleCleanupObjects)
	mux.HandleFunc(shared.TypeCleanupVideo, w.HandleCleanupVideo)
	mux.HandleFunc(shared.TypeReconcileTick, w.HandleReconcileTick)
}

// run blocks until ctx is cancelled or the asynq server
// returns. The shared.NewAsynqServer default ErrorHandler
// already logs task failures; if it ever stops being
// sufficient we can install a richer one here without
// touching shared/redis.go.
func (w *Worker) run(ctx context.Context) error {
	srv, err := shared.NewAsynqServer(shared.RedisConfig{
		Addr:     w.Cfg.RedisAddr,
		Password: w.Cfg.RedisPassword,
	}, shared.DefaultWorkerConcurrency)
	if err != nil {
		return fmt.Errorf("build asynq server: %w", err)
	}

	mux := asynq.NewServeMux()
	w.registerHandlers(mux)

	w.Logger.Info("transcoder-worker starting",
		"redis_addr", w.Cfg.RedisAddr,
		"transcode_timeout", w.Cfg.TranscodeTimeout.String(),
		"r2_bucket", w.Cfg.R2Bucket,
	)

	// Reconciliation sweeper (issue #44): a ticker in this
	// process enqueues reconcile:tick on the configured
	// cadence; the asynq server above consumes it in its
	// own processing slot, so a slow sweep never blocks
	// transcode/cleanup tasks. Cancelled with the root
	// context on shutdown.
	go w.runReconcileTicker(ctx)

	if err := srv.Run(mux); err != nil {
		// asynq.Server.Run returns the error that
		// caused it to stop (Redis disconnect, signal
		// received, etc.). We propagate it so main can
		// decide between clean-exit (signal) and
		// fatal (real failure).
		return fmt.Errorf("asynq server run: %w", err)
	}
	return nil
}

// main is the binary entrypoint. It loads config, builds
// a Worker, installs SIGINT/SIGTERM handling, runs the
// asynq server, and tears down on exit.
//
// Exit codes:
//   0 - clean shutdown on signal.
//   1 - startup error (config, deps, server build).
//   2 - asynq server stopped with a non-signal error.
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := shared.LoadWorker()
	if err != nil {
		logger.Error("LoadWorker failed", "err", err)
		os.Exit(1)
	}

	w, err := NewWorker(rootCtx, cfg, logger)
	if err != nil {
		logger.Error("NewWorker failed", "err", err)
		os.Exit(1)
	}
	defer w.Close()

	// Signal handler: cancel the root context so srv.Run
	// returns and main can clean up. We treat both
	// SIGINT and SIGTERM as graceful exits (Docker
	// sends SIGTERM, Ctrl-C sends SIGINT).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		logger.Info("signal received, shutting down", "signal", s.String())
		cancel()
	}()

	if err := w.run(rootCtx); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("clean shutdown after context cancel")
			os.Exit(0)
		}
		logger.Error("worker stopped with error", "err", err)
		os.Exit(2)
	}
	logger.Info("worker stopped cleanly")
}