package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runFFmpeg kill tests (issue #39). The runtime smoke proved
// the whole timeout architecture end-to-end (context kill ->
// retry ladder -> FAILED -> cleanup -> positive control,
// 21 PASS), but on this VPS the R2 download (~2.3 MB/s)
// always consumed the short budget first, so the kill landed
// on "download raw" rather than the ffmpeg encode step.
// These tests close the remaining criterion - "ffmpeg
// process is killed" - by invoking runFFmpeg DIRECTLY with a
// 1s context deadline against a real ffmpeg encode that
// would otherwise run for minutes. exec.CommandContext must
// deliver the kill signal.

// ffmpegAvailable reports whether an ffmpeg binary is on
// PATH. Tests skip (not fail) when it is absent so `make
// test` still passes on hosts without ffmpeg; the runtime
// smoke remains the authoritative verification there.
func ffmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// TestRunFFmpeg_KillsOnContextDeadline is the core issue
// #39 assertion: a genuinely long-running ffmpeg encode is
// killed when the context deadline elapses. Without the
// kill this encode would run for minutes (300s of noise at
// 480p); with it, runFFmpeg must return a non-nil error
// within a small multiple of the 1s budget.
func TestRunFFmpeg_KillsOnContextDeadline(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not on PATH; runtime smoke covers this on the dev VPS")
	}
	// runFFmpeg only touches the receiver, so a zero
	// Worker suffices (no logger / R2 / DB needed).
	w := &Worker{}
	workDir := t.TempDir()

	// 300s of 480p random-noise encode: far beyond the 1s
	// budget, and unlike mandelbrot the noise round-trip
	// cost survives compression (measured ~0.09s/frame on
	// the dev VPS; the deadline fires long before any
	// meaningful fraction of the encode completes).
	args := []string{
		"-y",
		"-f", "lavfi",
		"-i", "nullsrc=size=640x480:rate=30,geq=random(1)*255:128:128",
		"-t", "300",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		workDir + "/out.mp4",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	err := w.runFFmpeg(ctx, args, workDir)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runFFmpeg returned nil for an encode that must have been killed")
	}
	// The kill must happen near the 1s deadline, not after
	// the full encode. Generous upper bound absorbs process
	// spawn + cleanup jitter; a missed kill would take tens
	// of seconds and blow straight past it.
	if elapsed > 5*time.Second {
		t.Errorf("kill took %v; want it within ~1s of the deadline (missed kill?)", elapsed)
	}
	// The surfaced error must carry the kill evidence:
	// exec.CommandContext wraps the SIGKILL exit as
	// "signal: killed" once the deadline elapses.
	msg := err.Error()
	if !strings.Contains(msg, "signal: killed") && !strings.Contains(msg, "context deadline exceeded") {
		t.Errorf("error should name the kill signal / deadline, got %q", msg)
	}
}

// TestRunFFmpeg_CompletesWithinBudget is the positive
// control: an encode that fits inside the context budget
// must NOT be killed. Proves the deadline fires only when
// the budget actually elapses (issue #39 Technical Notes).
func TestRunFFmpeg_CompletesWithinBudget(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not on PATH; runtime smoke covers this on the dev VPS")
	}
	w := &Worker{}
	workDir := t.TempDir()

	// 1s of 480p testsrc: a sub-second encode on any
	// reasonable CPU, well inside the 30s budget.
	args := []string{
		"-y",
		"-f", "lavfi",
		"-i", "testsrc=duration=1:size=640x480:rate=30",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		workDir + "/out.mp4",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := w.runFFmpeg(ctx, args, workDir); err != nil {
		t.Fatalf("encode within budget must succeed, got: %v", err)
	}
	if _, statErr := os.Stat(workDir + "/out.mp4"); statErr != nil {
		t.Errorf("expected output file to exist: %v", statErr)
	}
}
