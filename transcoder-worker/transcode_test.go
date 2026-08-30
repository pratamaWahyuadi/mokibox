// transcode_test.go - table-driven tests for the small
// pure helpers in transcode.go. The transcode handler
// itself runs ffprobe + ffmpeg + R2 and is exercised
// end-to-end in the runtime smoke; here we only cover
// the functions that can be unit-tested without I/O.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFFmpegDuration(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"30.5", 30.5},
		{"0.000", 0.0},
		{"180", 180},
		{"", 0}, // empty -> 0 (treated as transient in handler)
		{"abc", 0},
	}
	for _, c := range cases {
		if got := parseFFmpegDuration(c.in); got != c.want {
			t.Errorf("parseFFmpegDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRandomHex(t *testing.T) {
	a, err := randomHex(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 16 { // 8 bytes hex -> 16 chars
		t.Errorf("len(randomHex(8)) = %d, want 16", len(a))
	}
	b, err := randomHex(8)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two consecutive randomHex(8) returned identical value: %s", a)
	}
}

func TestWriteMasterPlaylist(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "master.m3u8")
	variants := []struct {
		Dir        string
		Resolution string
		Bandwidth  int
	}{
		{"480p", "640x480", 800_000},
		{"720p", "1280x720", 1_500_000},
	}
	if err := writeMasterPlaylist(out, "hls/user-1/vid-1", variants); err != nil {
		t.Fatal(err)
	}
	got, err := readFile(out)
	if err != nil {
		t.Fatal(err)
	}
	wantSubs := []string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"BANDWIDTH=800000",
		"RESOLUTION=640x480",
		"hls/user-1/vid-1/480p/index.m3u8",
		"BANDWIDTH=1500000",
		"RESOLUTION=1280x720",
		"hls/user-1/vid-1/720p/index.m3u8",
	}
	for _, s := range wantSubs {
		if !strings.Contains(got, s) {
			t.Errorf("master.m3u8 missing substring %q\nfull output:\n%s", s, got)
		}
	}
}

// readFile is a tiny helper to keep the test imports
// focused on what's actually exercised.
func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}