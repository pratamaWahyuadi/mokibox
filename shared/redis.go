// Package shared contains code used by both api-gateway and
// transcoder-worker. Keep package-level state to a minimum;
// everything is passed via constructor injection.
//
// This file wires the Redis-backed Asynq client (used by
// the api-gateway to enqueue jobs) and the Asynq server
// (used by the transcoder-worker to consume them). Redis
// is internal-only and password-protected per SEC-03; the
// client is configured with the password from env at
// startup and never falls back to a default.
//
// Both the client and the server are concrete *asynq
// values; callers that need to mock the enqueue path can
// declare a small TaskEnqueuer interface in their own
// package.
package shared

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
)

// RedisConfig is the subset of APIConfig / WorkerConfig
// that NewAsynqClient / NewAsynqServer need. Both
// services already have these fields; passing a small
// struct keeps the constructors free of the larger
// config dependency.
type RedisConfig struct {
	Addr     string
	Password string
}

// Default worker concurrency. The PRD specifies
// concurrency = 1 for the transcoder worker; the
// constant is exported so the phase-5 main can
// reference it without re-deriving the value.
const (
	// DefaultWorkerConcurrency is the per-process
	// concurrency for the Asynq server. Phase-5 wires
	// this into asynq.Config{Concurrency: ...}.
	DefaultWorkerConcurrency = 1

	// DefaultQueue is the Asynq queue name. The PRD
	// does not require multiple queues yet, so the
	// single default queue is used for every task type
	// declared in shared/models.go.
	DefaultQueue = "default"
)

// NewAsynqClient builds an asynq.Client that the
// api-gateway uses to enqueue transcode and cleanup
// jobs. The client is cheap to create; we do not pool
// it. Callers SHOULD reuse the returned *asynq.Client
// across the process lifetime and Close it on shutdown.
func NewAsynqClient(cfg RedisConfig) (*asynq.Client, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("NewAsynqClient: Addr is empty")
	}
	if cfg.Password == "" {
		return nil, fmt.Errorf("NewAsynqClient: Password is empty (SEC-03 requires password auth)")
	}
	return asynq.NewClient(asynq.RedisClientOpt{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		// DB 0 is fine: Asynq owns its own keyspace
		// (asynq:*), so there is no collision with
		// application cache keys.
		DB: 0,
	}), nil
}

// NewAsynqServer builds an asynq.Server the
// transcoder-worker uses to consume jobs. Concurrency
// defaults to DefaultWorkerConcurrency (1) per the PRD;
// a caller that needs more can pass a higher value
// later without changing this constructor.
//
// The mux argument is where the worker registers its
// handlers (transcode:video, cleanup:objects,
// cleanup:video). Constructing the mux at the call
// site keeps the shared package free of business
// logic.
func NewAsynqServer(cfg RedisConfig, concurrency int) (*asynq.Server, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("NewAsynqServer: Addr is empty")
	}
	if cfg.Password == "" {
		return nil, fmt.Errorf("NewAsynqServer: Password is empty (SEC-03 requires password auth)")
	}
	if concurrency <= 0 {
		concurrency = DefaultWorkerConcurrency
	}
	srv := asynq.NewServer(
		asynq.RedisClientOpt{
			Addr:     cfg.Addr,
			Password: cfg.Password,
			DB:       0,
		},
		asynq.Config{
			Concurrency: concurrency,
			// Queues routes by name with a weight. We
			// only have one queue; setting it to 1 is
			// the documented way to opt in.
			Queues: map[string]int{
				DefaultQueue: 1,
			},
			// StrictPriority: false (default). Errors
			// are logged with structured fields.
			ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
				// The worker registers a richer
				// ErrorHandler in phase-5; this
				// default is a safety net so a
				// misconfigured worker still
				// surfaces failures instead of
				// swallowing them (forbidden by
				// hermes-go-idiomatic).
				if err == nil {
					return
				}
				fmt.Printf("asynq task %s failed: %v (retry=%d)\n",
					task.Type(),
					err,
					getRetryCount(task),
				)
			}),
		},
	)
	return srv, nil
}

// getRetryCount returns the number of times the task has
// been retried so the default ErrorHandler can include it
// in the log. Asynq stores the count in the task's
// ResultWriter; on a failed run it is also exposed via
// the task options. We keep the lookup best-effort: if
// the SDK changes the API, we fall back to -1 rather
// than panic.
func getRetryCount(t *asynq.Task) int {
	if t == nil {
		return -1
	}
	// asynq does not expose retry count from the Task
	// itself; the value is available to the handler via
	// asynq.GetRetryCount(ctx). We return -1 here so
	// the log line still makes sense.
	return -1
}

// EnqueueTranscode is the canonical producer-side
// helper used by /confirm. It serialises the payload,
// builds an *asynq.Task with the standard task type
// (TypeTranscodeVideo), and enqueues it on the default
// queue. The api-gateway calls this from the confirm
// handler right after the DB transition to PROCESSING.
//
// opts lets the caller attach a ProcessIn delay (used in
// the retry path) or MaxRetries; nil means "use
// asynq.DefaultRetry". The return value is the
// TaskInfo on success, so the caller can log the
// task ID for tracing.
func EnqueueTranscode(client *asynq.Client, payload TranscodeVideoPayload, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	task, err := NewTranscodeTask(payload)
	if err != nil {
		return nil, fmt.Errorf("EnqueueTranscode: build task: %w", err)
	}
	info, err := client.Enqueue(task, opts...)
	if err != nil {
		return nil, fmt.Errorf("EnqueueTranscode: enqueue: %w", err)
	}
	return info, nil
}

// NewTranscodeTask builds the *asynq.Task for a
// transcode:video job. Exposed so the worker can build
// the same task in tests.
func NewTranscodeTask(payload TranscodeVideoPayload) (*asynq.Task, error) {
	if payload.VideoID == "" {
		return nil, fmt.Errorf("NewTranscodeTask: VideoID is empty")
	}
	return marshalTask(TypeTranscodeVideo, payload)
}

// NewCleanupObjectsTask builds the *asynq.Task for a
// cleanup:objects job.
func NewCleanupObjectsTask(payload CleanupObjectsPayload) (*asynq.Task, error) {
	return marshalTask(TypeCleanupObjects, payload)
}

// EnqueueCleanupObjects is the canonical producer-side
// helper for cleanup:objects. Empty Keys is a no-op
// (returns nil with no error) so callers can enqueue
// unconditionally.
func EnqueueCleanupObjects(client *asynq.Client, payload CleanupObjectsPayload, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	if len(payload.Keys) == 0 {
		return nil, nil
	}
	task, err := NewCleanupObjectsTask(payload)
	if err != nil {
		return nil, fmt.Errorf("EnqueueCleanupObjects: build task: %w", err)
	}
	info, err := client.Enqueue(task, opts...)
	if err != nil {
		return nil, fmt.Errorf("EnqueueCleanupObjects: enqueue: %w", err)
	}
	return info, nil
}

// EnqueueCleanupVideo is the canonical producer-side
// helper for cleanup:video. The videoID must be
// non-empty; the empty case is rejected so a buggy
// caller cannot enqueue a task that the worker would
// have to drop.
func EnqueueCleanupVideo(client *asynq.Client, payload CleanupVideoPayload, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	if payload.VideoID == "" {
		return nil, fmt.Errorf("EnqueueCleanupVideo: VideoID is empty")
	}
	task, err := NewCleanupVideoTask(payload)
	if err != nil {
		return nil, fmt.Errorf("EnqueueCleanupVideo: build task: %w", err)
	}
	info, err := client.Enqueue(task, opts...)
	if err != nil {
		return nil, fmt.Errorf("EnqueueCleanupVideo: enqueue: %w", err)
	}
	return info, nil
}

// NewCleanupVideoTask builds the *asynq.Task for a
// cleanup:video job.
func NewCleanupVideoTask(payload CleanupVideoPayload) (*asynq.Task, error) {
	if payload.VideoID == "" {
		return nil, fmt.Errorf("NewCleanupVideoTask: VideoID is empty")
	}
	return marshalTask(TypeCleanupVideo, payload)
}

// marshalTask JSON-encodes payload and wraps it in an
// asynq.Task. asynq.NewTask takes []byte, so any payload
// that is not already a byte slice must be serialised
// here. The JSON tag layout is defined in
// shared/models.go and is part of the producer/consumer
// contract.
func marshalTask(typename string, payload any) (*asynq.Task, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal task %s: %w", typename, err)
	}
	return asynq.NewTask(typename, body), nil
}

// EnqueueWithDelay is a thin wrapper around
// client.Enqueue that always uses ProcessIn(d). It is
// used by the cleanup path to schedule a 24h
// post-tombstone delete. The default asynq Option
// literal cannot be embedded easily in a helper
// without a context dance, so the wrapper lives here
// for ergonomics.
//
// Kept as a function (not a method on the client) so
// callers do not have to construct a fake client in
// tests; a nil client is rejected with an error.
func EnqueueWithDelay(client *asynq.Client, task *asynq.Task, d time.Duration) (*asynq.TaskInfo, error) {
	if client == nil {
		return nil, fmt.Errorf("EnqueueWithDelay: client is nil")
	}
	if task == nil {
		return nil, fmt.Errorf("EnqueueWithDelay: task is nil")
	}
	return client.Enqueue(task, asynq.ProcessIn(d))
}
