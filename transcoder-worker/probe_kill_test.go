package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ProbeFile kill tests (issue #39 residual gap, raised in
// PR #47 review discussion 2026-09-03).
//
// The smoke + runFFmpeg tests prove the context timeout
// kills a hung FFMPEG and that the shared task deadline
// bounds every pipeline step. The one subprocess without
// its own kill-specific test was ffprobe. Unlike ffmpeg,
// there is no obvious pathological INPUT that makes a
// healthy ffprobe run long - but a healthy ffprobe DOES
// hang forever when its stdout is a pipe nobody drains:
// ffprobe fills the 64 KiB pipe buffer, blocks on write,
// and never reaches EOF. ProbeFile wires stdout/stderr to
// bytes.Buffers via cmd.Stdout, so the drain path is what
// keeps it alive; the kill test pins that a genuinely
// blocked ffprobe still cannot outlive the context
// deadline.
//
// The same technique would hang the encode tests too, but
// ffmpeg already has real-work kills covered; this file
// exists so every subprocess the worker spawns (ffprobe,
// ffmpeg xN) has a direct kill assertion.

// mkfifo creates a named pipe at path. Used to build a
// deterministic "reader blocks forever" input for the
// probe kill test. Linux-only like the whole deployment;
// callers skip when it fails.
func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

// ffprobeAvailable reports whether the ffprobe binary is
// on PATH. Tests skip (not fail) without it, same policy
// as runffmpeg_test.go.
func ffprobeAvailable() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// TestProbeFile_KillsOnContextDeadline pins the residual
// gap from the issue #39 verification: a hung ffprobe is
// killed by the context deadline, the error surfaces
// promptly, and the error is ErrInvalidMedia (the handler's
// permanent-failure branch) rather than a raw ctx error.
//
// Hang mechanism: ffprobe is invoked on a FIFO with no
// writer. ffprobe opens the "file", blocks reading it, and
// never produces output or exits on its own - a fully
// deterministic hang, no pathological input needed.
func TestProbeFile_KillsOnContextDeadline(t *testing.T) {
	if !ffprobeAvailable() {
		t.Skip("ffprobe not on PATH; runtime smoke covers the pipeline on the dev VPS")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "hanging-input.mp4")

	// FIFO with no writer: any reader blocks forever.
	// ffprobe will open it and hang on read.
	if err := mkfifo(fifo); err != nil {
		t.Skipf("cannot create FIFO on this platform: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	_, err := ProbeFile(ctx, fifo)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ProbeFile returned nil for a probe that must have been killed")
	}
	// The kill must fire near the 1s deadline. ProbeFile
	// would block FOREVER without the ctx kill, so any
	// return at all proves the kill; the 5s bound catches
	// slow-kill regressions.
	if elapsed > 5*time.Second {
		t.Errorf("kill took %v; want it within ~1s of the deadline (missed kill?)", elapsed)
	}
	// ProbeFile wraps every non-zero ffprobe exit (the
	// SIGKILL exit included) in ErrInvalidMedia, which the
	// handler routes to the permanent-failure branch. The
	// behaviour is unchanged by the kill, but pin it so a
	// future refactor that lets the raw ctx error escape
	// (turning kills into retries) is caught here.
	if !errors.Is(err, ErrInvalidMedia) {
		t.Errorf("killed probe should surface ErrInvalidMedia (handler's permanent branch), got %q", err)
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") &&
		!strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("error should carry kill evidence, got %q", err)
	}
}

// TestProbeFile_ValidInputWithinBudget is the positive
// control: probing a real, readable video file completes
// well inside the budget and parses (the existing
// ffprobe_test.go covers ValidateMedia rules on the parsed
// struct; this asserts the subprocess path end-to-end).
func TestProbeFile_ValidInputWithinBudget(t *testing.T) {
	if !ffprobeAvailable() || !ffmpegAvailable() {
		t.Skip("ffprobe/ffmpeg not on PATH")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	out := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=640x480:rate=30",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", src)
	if o, err := out.CombinedOutput(); err != nil {
		t.Skipf("fixture generation failed: %v: %s", err, o)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := ProbeFile(ctx, src)
	if err != nil {
		t.Fatalf("probe within budget must succeed, got: %v", err)
	}
	if len(r.Streams) == 0 {
		t.Error("expected at least one stream in probe result")
	}
	if r.Format.Duration == "" {
		t.Error("expected parsed format duration")
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Errorf("fixture should still exist: %v", statErr)
	}
}
