// ffprobe.go - SEC-01 media validation.
//
// MokiBox validates every uploaded media file with
// ffprobe BEFORE handing it to FFmpeg. FFmpeg's parser
// has a long history of parser exploits (CVE after CVE),
// so the rule is: do not trust the file enough to feed
// it into FFmpeg until ffprobe has confirmed it is a
// sane media file with a whitelisted codec.
//
// Layout of this file:
//
//   - ffprobeStream / ffprobeFormat / ffprobeResult
//     mirror the JSON ffprobe produces with
//     -print_format json -show_streams -show_format.
//     BitRate is a string in ffprobe's output (because
//     it is not always representable in int32) so we
//     parse it ourselves.
//
//   - allowlists: videoCodecAllowlist (h264, hevc, vp9,
//     av1) and audioCodecAllowlist (aac, opus, mp3).
//     Anything outside the lists is rejected - this is
//     what stops a "raw bytes / executable pretending to
//     be media" attack from sneaking past.
//
//   - minDuration / maxDuration (1s .. 180s) and
//     minDimension / maxDimension (16 .. 4096) and
//     maxBitRate (25 Mbps) are wire-level limits defined
//     in the PRD. Changing them changes the public
//     contract for video uploads.
//
//   - MinUploadBytes / MaxUploadBytes are duplicated
//     here as a defensive consistency check (they also
//     live in api-gateway/handlers/video.go for the
//     confirm path). Defining them once in the
//     transcoder-worker is fine because the worker is
//     the last line of defence - it re-validates the
//     R2 object even if /confirm missed an edge case.
//
//   - ProbeFile shells out to the ffprobe binary. It is
//     the only function in this file that touches
//     the filesystem or runs a subprocess.
//
//   - ValidateMedia is a pure function over an
//     ffprobeResult so it can be table-tested in
//     ffprobe_test.go without a running ffprobe.
//
//   - ErrInvalidMedia is the sentinel returned when
//     validation fails. The HandleTranscode handler
//     uses errors.Is to drive the "mark FAILED, no
//     retry" branch (FR-VIDEO-04).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// Allowlists and physical limits. PRD FR-VIDEO-04 /
// FR-VIDEO-05. Stored as package-level vars so a test
// can override them via t.Cleanup if a future phase
// loosens the limits.
var (
	videoCodecAllowlist = map[string]struct{}{
		"h264": {},
		"hevc": {},
		"vp9":  {},
		"av1":  {},
	}
	audioCodecAllowlist = map[string]struct{}{
		"aac":  {},
		"opus": {},
		"mp3":  {},
	}
)

// Per-file limits. Constants (not vars) so they cannot
// drift across tests; tests that want different limits
// can build their own ffprobeResult literal.
const (
	minDuration = 1.0
	maxDuration = 180.0
	minDimension = 16
	maxDimension = 4096
	maxBitRate   = 25_000_000

	// MinUploadBytes / MaxUploadBytes are the same
	// numbers the api-gateway enforces at /confirm.
	// Duplicated here because the worker is the last
	// line of defence - if a row somehow got past
	// /confirm with an undersized object (manual SQL,
	// future migration script, etc.), the worker must
	// still refuse to transcode it.
	MinUploadBytes int64 = 1024
	MaxUploadBytes int64 = 200 * 1024 * 1024
)

// ErrInvalidMedia is the sentinel returned by
// ValidateMedia and ProbeFile when the uploaded file
// fails security / format validation. The handler
// inspects it with errors.Is and routes the row to
// MarkVideoFailed without retrying (FR-VIDEO-04).
//
// Local to the transcoder-worker package because it is
// only meaningful to the worker pipeline; HTTP handlers
// never receive this error and would not need to map it
// to a wire code.
var ErrInvalidMedia = errors.New("invalid media file")

// ffprobeStream mirrors the JSON ffprobe emits for one
// stream. Width / Height are only set for video streams;
// audio streams leave them at zero. BitRate is left as a
// string because ffprobe prints very large values that
// do not always fit in int32.
type ffprobeStream struct {
	CodecName string `json:"codec_name"`
	CodecType string `json:"codec_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	BitRate   string `json:"bit_rate"`
}

// ffprobeFormat mirrors the JSON ffprobe emits for the
// container-level format. Duration and Size are strings
// for the same reason as BitRate.
type ffprobeFormat struct {
	Duration string `json:"duration"`
	Size     string `json:"size"`
	BitRate  string `json:"bit_rate"`
}

// ffprobeResult is the parsed JSON output of
// `ffprobe -v error -print_format json -show_format -show_streams`.
type ffprobeResult struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// ProbeFile runs ffprobe on the local file at filePath
// and returns the parsed JSON. The call is wrapped with
// context so the handler can enforce a deadline; the
// subprocess is killed when ctx is cancelled.
//
// Returns:
//   - ErrInvalidMedia if ffprobe exits non-zero (file
//     unreadable, no streams, malformed container).
//   - any other error for I/O failures (path missing,
//     permission denied, etc.).
func ProbeFile(ctx context.Context, filePath string) (ffprobeResult, error) {
	if filePath == "" {
		return ffprobeResult{}, fmt.Errorf("ProbeFile: filePath is empty")
	}
	cmd := exec.CommandContext(ctx,
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		filePath,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// ffprobe exits non-zero for missing files,
		// malformed containers, and "no streams found".
		// All of these are user input problems from the
		// worker's perspective, so they surface as
		// ErrInvalidMedia and the handler short-circuits
		// to MarkVideoFailed. The original error is
		// preserved in the message: for a ctx deadline
		// kill it names "context deadline exceeded" /
		// "signal: killed", so an operator reading the
		// log can distinguish a killed probe from a
		// corrupt file (found by probe_kill_test.go, PR
		// #47 review discussion, issue #39 residual gap).
		return ffprobeResult{}, fmt.Errorf("%w: ffprobe %s: %v: %s", ErrInvalidMedia, filePath, err, stderr.String())
	}
	var r ffprobeResult
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return ffprobeResult{}, fmt.Errorf("ProbeFile: parse ffprobe output: %w", err)
	}
	return r, nil
}

// ValidateMedia runs the security / format checks on a
// parsed ffprobe result. It is a pure function so it can
// be table-tested without a running ffprobe binary.
//
// sizeBytes is the actual R2 object size from
// HeadObject; it MUST be within MinUploadBytes ..
// MaxUploadBytes or the upload is treated as garbage.
//
// Returns:
//   - nil when every rule passes.
//   - an error wrapping ErrInvalidMedia otherwise.
//     Callers can use errors.Is to detect validation
//     failure and avoid retrying.
func ValidateMedia(r ffprobeResult, sizeBytes int64) error {
	// --- R2 size check ---
	if sizeBytes < MinUploadBytes || sizeBytes > MaxUploadBytes {
		return fmt.Errorf("%w: object size %d outside allowed [%d, %d]",
			ErrInvalidMedia, sizeBytes, MinUploadBytes, MaxUploadBytes)
	}

	// --- at least one video stream ---
	videoStreamCount := 0
	for _, s := range r.Streams {
		if s.CodecType == "video" {
			videoStreamCount++
			if s.CodecName == "" {
				return fmt.Errorf("%w: video stream has empty codec_name", ErrInvalidMedia)
			}
			if _, ok := videoCodecAllowlist[s.CodecName]; !ok {
				return fmt.Errorf("%w: video codec %q not in allowlist (h264, hevc, vp9, av1)",
					ErrInvalidMedia, s.CodecName)
			}
			if s.Width < minDimension || s.Width > maxDimension {
				return fmt.Errorf("%w: video width %d outside allowed [%d, %d]",
					ErrInvalidMedia, s.Width, minDimension, maxDimension)
			}
			if s.Height < minDimension || s.Height > maxDimension {
				return fmt.Errorf("%w: video height %d outside allowed [%d, %d]",
					ErrInvalidMedia, s.Height, minDimension, maxDimension)
			}
			if s.BitRate != "" {
				br, err := strconv.ParseInt(s.BitRate, 10, 64)
				if err != nil {
					return fmt.Errorf("%w: video bit_rate %q: %v", ErrInvalidMedia, s.BitRate, err)
				}
				if br > maxBitRate {
					return fmt.Errorf("%w: video bit_rate %d exceeds %d",
						ErrInvalidMedia, br, maxBitRate)
				}
			}
		}
	}
	if videoStreamCount == 0 {
		return fmt.Errorf("%w: no video stream in file", ErrInvalidMedia)
	}

	// --- audio: optional, allowlist if present ---
	for _, s := range r.Streams {
		if s.CodecType == "audio" {
			if _, ok := audioCodecAllowlist[s.CodecName]; !ok {
				return fmt.Errorf("%w: audio codec %q not in allowlist (aac, opus, mp3)",
					ErrInvalidMedia, s.CodecName)
			}
		}
		// Subtitle / data streams are silently skipped;
		// the API contract does not mention them and
		// ffprobe would have rejected the file already
		// if they caused a parse error.
	}

	// --- duration ---
	if r.Format.Duration == "" {
		return fmt.Errorf("%w: missing duration in format", ErrInvalidMedia)
	}
	dur, err := strconv.ParseFloat(r.Format.Duration, 64)
	if err != nil {
		return fmt.Errorf("%w: parse duration %q: %v", ErrInvalidMedia, r.Format.Duration, err)
	}
	if dur < minDuration || dur > maxDuration {
		return fmt.Errorf("%w: duration %.3fs outside allowed [%.0f, %.0f]",
			ErrInvalidMedia, dur, minDuration, maxDuration)
	}

	return nil
}