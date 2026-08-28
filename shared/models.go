// Package shared contains code used by both api-gateway and
// transcoder-worker. Keep package-level state to a minimum;
// everything is passed via constructor injection.
//
// This file defines the queue task types and the JSON
// payloads the api-gateway enqueues and the
// transcoder-worker consumes. Both services must agree on
// these names because Asynq dispatches by the type string;
// renaming a constant here would silently drop jobs.
//
// The structs also implement the asynq.Payload interface
// (Marshal + Unmarshal) so callers can go from payload ->
// *asynq.Task in one call. We provide a tiny NewTask helper
// per type to make the producer side one line at the call
// site.
package shared

// Task type identifiers. Values are stable wire-level
// contracts: changing them breaks dispatch. Add new
// constants here, never rename an existing one.
const (
	// TypeTranscodeVideo is enqueued by the api-gateway
	// after /confirm flips a video to PROCESSING. The
	// worker downloads the raw upload, runs ffprobe +
	// ffmpeg, uploads HLS + thumbnail, and updates the
	// row.
	TypeTranscodeVideo = "transcode:video"

	// TypeCleanupObjects deletes a batch of R2 keys. It
	// is enqueued by the api-gateway on size-invalid
	// upload-confirm and by the worker after tombstoning
	// a deleted video.
	TypeCleanupObjects = "cleanup:objects"

	// TypeCleanupVideo finalises a video marked DELETED
	// more than 24h ago. The worker fetches the row,
	// removes the R2 objects, then hard-deletes the row.
	TypeCleanupVideo = "cleanup:video"
)

// TranscodeVideoPayload is the body of a transcode:video
// task. The api-gateway fills in the videoID when
// /confirm succeeds; the worker reads it back to look up
// the row and start the transcode.
type TranscodeVideoPayload struct {
	VideoID string `json:"video_id"`
}

// CleanupObjectsPayload is the body of a cleanup:objects
// task. Keys is the list of R2 object keys to delete; an
// empty list is a no-op so callers can enqueue
// unconditionally.
type CleanupObjectsPayload struct {
	Keys []string `json:"keys"`
}

// CleanupVideoPayload is the body of a cleanup:video
// task. VideoID identifies the row to finalise.
type CleanupVideoPayload struct {
	VideoID string `json:"video_id"`
}
