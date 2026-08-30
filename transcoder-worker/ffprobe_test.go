// ffprobe_test.go - table-driven tests for ValidateMedia.
//
// ValidateMedia is a pure function over an ffprobeResult
// literal, so we can exercise every rule without a
// running ffprobe binary. ProbeFile (which actually
// shells out) is intentionally NOT tested here - it
// needs the binary and is exercised in the runtime
// smoke test for issue C.
package main

import (
	"errors"
	"strings"
	"testing"
)

// validResult returns a minimal-but-passing ffprobeResult
// so individual tests can mutate the fields they care
// about without restating the happy path every time.
func validResult() ffprobeResult {
	return ffprobeResult{
		Streams: []ffprobeStream{
			{
				CodecType: "video",
				CodecName: "h264",
				Width:     1280,
				Height:    720,
				BitRate:   "1500000",
			},
			{
				CodecType: "audio",
				CodecName: "aac",
			},
		},
		Format: ffprobeFormat{
			Duration: "30.5",
			Size:     "1000000",
		},
	}
}

func TestValidateMedia_Accepts(t *testing.T) {
	r := validResult()
	if err := ValidateMedia(r, 1024); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateMedia_AcceptsWithoutAudio(t *testing.T) {
	r := validResult()
	// drop the audio stream
	r.Streams = r.Streams[:1]
	if err := ValidateMedia(r, 1024); err != nil {
		t.Fatalf("video-only file should pass, got %v", err)
	}
}

func TestValidateMedia_AcceptsBoundaryDurations(t *testing.T) {
	// exactly min and max
	for _, d := range []string{"1.0", "180.0"} {
		r := validResult()
		r.Format.Duration = d
		if err := ValidateMedia(r, 1024); err != nil {
			t.Errorf("duration=%s should pass, got %v", d, err)
		}
	}
}

func TestValidateMedia_AcceptsBoundaryDimensions(t *testing.T) {
	for _, w := range []int{16, 4096} {
		for _, h := range []int{16, 4096} {
			r := validResult()
			r.Streams[0].Width = w
			r.Streams[0].Height = h
			if err := ValidateMedia(r, 1024); err != nil {
				t.Errorf("dimensions=%dx%d should pass, got %v", w, h, err)
			}
		}
	}
}

func TestValidateMedia_Rejects(t *testing.T) {
	type tc struct {
		name     string
		mutate   func(*ffprobeResult)
		size     int64
		wantSubs string // expected substring of error message
	}
	cases := []tc{
		{
			name:     "size below min",
			mutate:   func(r *ffprobeResult) {},
			size:     100,
			wantSubs: "object size",
		},
		{
			name:     "size above max",
			mutate:   func(r *ffprobeResult) {},
			size:     300 * 1024 * 1024,
			wantSubs: "object size",
		},
		{
			name: "no video stream",
			mutate: func(r *ffprobeResult) {
				r.Streams = r.Streams[1:] // drop video, keep audio
			},
			size:     1024,
			wantSubs: "no video stream",
		},
		{
			name: "video codec not in allowlist",
			mutate: func(r *ffprobeResult) {
				r.Streams[0].CodecName = "mpeg4"
			},
			size:     1024,
			wantSubs: "video codec",
		},
		{
			name: "video codec empty",
			mutate: func(r *ffprobeResult) {
				r.Streams[0].CodecName = ""
			},
			size:     1024,
			wantSubs: "empty codec_name",
		},
		{
			name: "audio codec not in allowlist",
			mutate: func(r *ffprobeResult) {
				r.Streams[1].CodecName = "flac"
			},
			size:     1024,
			wantSubs: "audio codec",
		},
		{
			name: "duration too short",
			mutate: func(r *ffprobeResult) {
				r.Format.Duration = "0.5"
			},
			size:     1024,
			wantSubs: "duration",
		},
		{
			name: "duration too long",
			mutate: func(r *ffprobeResult) {
				r.Format.Duration = "200.0"
			},
			size:     1024,
			wantSubs: "duration",
		},
		{
			name: "missing duration",
			mutate: func(r *ffprobeResult) {
				r.Format.Duration = ""
			},
			size:     1024,
			wantSubs: "missing duration",
		},
		{
			name: "width too small",
			mutate: func(r *ffprobeResult) {
				r.Streams[0].Width = 8
			},
			size:     1024,
			wantSubs: "width",
		},
		{
			name: "width too large",
			mutate: func(r *ffprobeResult) {
				r.Streams[0].Width = 8192
			},
			size:     1024,
			wantSubs: "width",
		},
		{
			name: "height too small",
			mutate: func(r *ffprobeResult) {
				r.Streams[0].Height = 8
			},
			size:     1024,
			wantSubs: "height",
		},
		{
			name: "height too large",
			mutate: func(r *ffprobeResult) {
				r.Streams[0].Height = 8192
			},
			size:     1024,
			wantSubs: "height",
		},
		{
			name: "bitrate too high",
			mutate: func(r *ffprobeResult) {
				r.Streams[0].BitRate = "50000000"
			},
			size:     1024,
			wantSubs: "bit_rate",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := validResult()
			c.mutate(&r)
			err := ValidateMedia(r, c.size)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrInvalidMedia) {
				t.Errorf("expected errors.Is(err, ErrInvalidMedia) true, got %v", err)
			}
			if c.wantSubs != "" && !strings.Contains(err.Error(), c.wantSubs) {
				t.Errorf("expected error to contain %q, got %q", c.wantSubs, err.Error())
			}
		})
	}
}

func TestValidateMedia_AcceptsAllWhitelistedCodecs(t *testing.T) {
	for _, codec := range []string{"h264", "hevc", "vp9", "av1"} {
		r := validResult()
		r.Streams[0].CodecName = codec
		if err := ValidateMedia(r, 1024); err != nil {
			t.Errorf("video codec %q should pass, got %v", codec, err)
		}
	}
	for _, codec := range []string{"aac", "opus", "mp3"} {
		r := validResult()
		// replace audio stream
		r.Streams = []ffprobeStream{r.Streams[0], {CodecType: "audio", CodecName: codec}}
		if err := ValidateMedia(r, 1024); err != nil {
			t.Errorf("audio codec %q should pass, got %v", codec, err)
		}
	}
}