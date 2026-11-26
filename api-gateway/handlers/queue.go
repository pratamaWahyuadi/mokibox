// Package handlers - queue.go defines the consumer-side
// enqueue interface used by every handler that hands work to
// the transcoder worker via Redis/asynq.
//
// shared.Enqueue* take the concrete *asynq.Client, which a
// unit test cannot provide without a live Redis. Wrapping
// the (few) enqueue calls the api-gateway actually makes
// behind a small interface keeps the handler bodies testable
// while production wiring keeps the real client through the
// asynqEnqueuer adapter.
package handlers

import (
	"time"

	"github.com/hibiken/asynq"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

// taskEnqueuer is the producer-side surface the handlers
// need. Options (asynq.MaxRetry / ProcessIn) are absorbed
// into the method shape so callers do not depend on asynq's
// option plumbing; the adapter applies the same option
// values the previous direct calls used, so queue behaviour
// is unchanged on the wire.
type taskEnqueuer interface {
	// EnqueueTranscode enqueues transcode:video with
	// asynq.MaxRetry(1) - the queue-level safety net only;
	// the application-level 3x retry lives in the worker.
	EnqueueTranscode(payload shared.TranscodeVideoPayload) (*asynq.TaskInfo, error)
	// EnqueueCleanupObjects enqueues cleanup:objects for the
	// given R2 keys (no-op on an empty key list).
	EnqueueCleanupObjects(payload shared.CleanupObjectsPayload) (*asynq.TaskInfo, error)
	// EnqueueCleanupVideo enqueues cleanup:video delayed by
	// the given grace period (asynq.ProcessIn).
	EnqueueCleanupVideo(payload shared.CleanupVideoPayload, delay time.Duration) (*asynq.TaskInfo, error)
}

// asynqEnqueuer adapts the production *asynq.Client to
// taskEnqueuer. Constructed once per handler by the
// constructors in this package; tests provide their own
// implementation instead.
type asynqEnqueuer struct{ client *asynq.Client }

// EnqueueTranscode implements taskEnqueuer. MaxRetry(1) is a
// queue-level safety net for transient Redis/network blips
// during pickup - NOT the application retry budget (the 3x
// policy is worker-owned via IncrementVideoRetry).
func (a asynqEnqueuer) EnqueueTranscode(payload shared.TranscodeVideoPayload) (*asynq.TaskInfo, error) {
	return shared.EnqueueTranscode(a.client, payload, asynq.MaxRetry(1))
}

// EnqueueCleanupObjects implements taskEnqueuer.
func (a asynqEnqueuer) EnqueueCleanupObjects(payload shared.CleanupObjectsPayload) (*asynq.TaskInfo, error) {
	return shared.EnqueueCleanupObjects(a.client, payload)
}

// EnqueueCleanupVideo implements taskEnqueuer.
func (a asynqEnqueuer) EnqueueCleanupVideo(payload shared.CleanupVideoPayload, delay time.Duration) (*asynq.TaskInfo, error) {
	return shared.EnqueueCleanupVideo(a.client, payload, asynq.ProcessIn(delay))
}
