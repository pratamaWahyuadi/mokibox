package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// buildFfmpegArgs tests. The transcode handler builds the
// ffmpeg argv for each variant + the thumbnail inline; the
// arg construction is extracted into pure helpers so the
// HLS flags, codec settings, and segment pattern can be
// pinned by unit tests without running ffmpeg (the full
// pipeline is exercised by the runtime smoke + issue #39
// timeout-kill verification).
func TestBuildVariantArgs(t *testing.T) {
	sourcePath := "/tmp/transcode/vid-abc/source.bin"
	indexPath := filepath.Join("hls", "u1", "vid-abc", "480p", "index.m3u8")
	segmentPattern := filepath.Join("hls", "u1", "vid-abc", "480p", "segment_%04d.ts")

	v := transcodeVariants[0] // 480p
	got := buildVariantArgs(sourcePath, indexPath, segmentPattern, v)

	want := []string{
		"-y",
		"-i", sourcePath,
		"-vf", "scale=-2:480",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-c:a", "aac",
		"-b:a", "128k",
		"-hls_time", "6",
		"-hls_playlist_type", "vod",
		"-hls_segment_filename", segmentPattern,
		indexPath,
	}
	if len(got) != len(want) {
		t.Fatalf("arg count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildVariantArgs_720pScale(t *testing.T) {
	v := transcodeVariants[1] // 720p
	args := buildVariantArgs("/src", "/out/index.m3u8", "/out/seg_%04d.ts", v)
	// The scale filter must derive from the variant dir
	// (720p -> scale=-2:720) and the output path must be
	// the LAST arg so ffmpeg writes the playlist.
	found := false
	for i, a := range args {
		if a == "-vf" && i+1 < len(args) {
			if args[i+1] != "scale=-2:720" {
				t.Errorf("720p scale filter = %q, want scale=-2:720", args[i+1])
			}
			found = true
		}
	}
	if !found {
		t.Error("no -vf flag in args")
	}
	if last := args[len(args)-1]; last != "/out/index.m3u8" {
		t.Errorf("output path must be the last arg, got %q", last)
	}
}

func TestBuildThumbnailArgs(t *testing.T) {
	got := buildThumbnailArgs("/tmp/transcode/vid-abc/source.bin", "/tmp/transcode/vid-abc/thumb.jpg")
	want := []string{
		"-y",
		"-i", "/tmp/transcode/vid-abc/source.bin",
		"-ss", "00:00:01",
		"-frames:v", "1",
		"-vf", "scale=480:-1",
		"/tmp/transcode/vid-abc/thumb.jpg",
	}
	if len(got) != len(want) {
		t.Fatalf("arg count = %d, want %d\ngot:  %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestVariantList pins the two shipped variants so an
// accidental edit (dropping 720p, adding 1080p with wrong
// bandwidth) is caught here, not in a live encode.
func TestVariantList(t *testing.T) {
	if len(transcodeVariants) != 2 {
		t.Fatalf("transcodeVariants len = %d, want 2", len(transcodeVariants))
	}
	if transcodeVariants[0].Dir != "480p" || transcodeVariants[0].Resolution != "640x480" || transcodeVariants[0].Bandwidth != 800_000 {
		t.Errorf("480p variant = %+v", transcodeVariants[0])
	}
	if transcodeVariants[1].Dir != "720p" || transcodeVariants[1].Resolution != "1280x720" || transcodeVariants[1].Bandwidth != 1_500_000 {
		t.Errorf("720p variant = %+v", transcodeVariants[1])
	}
}

// TestScaleFilterDerivation documents that the -vf scale
// comes from trimming the trailing "p" of the dir name.
// A variant named e.g. "1080p" would yield scale=-2:1080.
func TestScaleFilterDerivation(t *testing.T) {
	for _, v := range transcodeVariants {
		want := "scale=-2:" + strings.TrimSuffix(v.Dir, "p")
		args := buildVariantArgs("/src", "/out/index.m3u8", "/out/seg_%04d.ts", v)
		for i, a := range args {
			if a == "-vf" {
				if got := args[i+1]; got != want {
					t.Errorf("variant %s: -vf = %q, want %q", v.Dir, got, want)
				}
			}
		}
	}
}
