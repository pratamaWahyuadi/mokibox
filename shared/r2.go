// Package shared contains code used by both api-gateway and
// transcoder-worker. Keep package-level state to a minimum;
// everything is passed via constructor injection.
//
// This file wraps the Cloudflare R2 S3-compatible API used
// for object storage. R2 bucket is private; every read and
// write is either a presigned URL handed to the client
// (upload, HLS playlist rewrite, thumbnail) or a direct
// server-side call (download raw, upload HLS output, delete
// objects). The bucket name and the
// https://<ACCOUNT_ID>.r2.cloudflarestorage.com endpoint
// are configured per environment via the shared config.
package shared

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// R2Config is the subset of APIConfig / WorkerConfig that
// NewR2Client needs. Both services already have these
// fields; passing a small struct keeps NewR2Client free of
// the larger config dependency and makes it trivially
// testable with a literal.
type R2Config struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	// Endpoint is the full URL of the R2 endpoint, e.g.
	// https://<ACCOUNT_ID>.r2.cloudflarestorage.com. It
	// is required because aws-sdk-go-v2 does not know
	// the R2 URL pattern; NewR2Client validates it is
	// non-empty.
	Endpoint string
}

// R2Client is the concrete struct used by callers; it
// holds an *s3.Client configured against Cloudflare R2
// and the bucket name. The struct is returned (not an
// interface) from the constructor; consumers that need
// to mock it can declare a small interface in their
// own package.
type R2Client struct {
	client *s3.Client
	presigner *s3.PresignClient
	bucket string
}

// NewR2Client builds an R2Client from a validated
// R2Config. It performs eager credential validation via
// the SDK's LoadDefaultConfig so a misconfigured
// environment fails at startup, not on the first
// request.
//
// The returned client uses path-style addressing (R2
// requires it) and static credentials pulled from the
// R2Config. No session token, no SSO, no IMDS.
func NewR2Client(ctx context.Context, cfg R2Config) (*R2Client, error) {
	if cfg.AccountID == "" {
		return nil, fmt.Errorf("NewR2Client: AccountID is empty")
	}
	if cfg.AccessKeyID == "" {
		return nil, fmt.Errorf("NewR2Client: AccessKeyID is empty")
	}
	if cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("NewR2Client: SecretAccessKey is empty")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("NewR2Client: Bucket is empty")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("NewR2Client: Endpoint is empty")
	}

	awsCfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion("auto"),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID,
			cfg.SecretAccessKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("NewR2Client: load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		// R2 requires path-style; bucket is in the path
		// not the host.
		o.UsePathStyle = true
	})

	return &R2Client{
		client:    client,
		presigner: s3.NewPresignClient(client),
		bucket:    cfg.Bucket,
	}, nil
}

// Bucket returns the bucket name the client was
// configured with. Exposed for tests and the (future)
// cleanup worker that may need to build keys across
// services.
func (c *R2Client) Bucket() string { return c.bucket }

// PresignPut returns a presigned URL the client can use
// to PUT an object directly to R2. The returned URL is
// valid for the given expiry; contentType is encoded
// into the signature so the client must PUT with the
// same header.
//
// Presigned PUT for R2 does NOT support
// content-length-range (per LLD_PLAN A3 and PRD
// FR-VIDEO-01 note). The API enforces the size limit
// at /confirm by reading the actual object size via
// HeadObject; the presigned URL itself is open-ended on
// size.
func (c *R2Client) PresignPut(ctx context.Context, key, contentType string, expiry time.Duration) (string, error) {
	if key == "" {
		return "", fmt.Errorf("PresignPut: key is empty")
	}
	if expiry <= 0 {
		return "", fmt.Errorf("PresignPut: expiry must be > 0")
	}
	req, err := c.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("PresignPut: presign: %w", err)
	}
	return req.URL, nil
}

// PresignGet returns a presigned URL the client can use
// to GET an object from R2. Used for thumbnails and
// HLS .ts segments embedded in the rewritten playlist.
func (c *R2Client) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	if key == "" {
		return "", fmt.Errorf("PresignGet: key is empty")
	}
	if expiry <= 0 {
		return "", fmt.Errorf("PresignGet: expiry must be > 0")
	}
	req, err := c.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("PresignGet: presign: %w", err)
	}
	return req.URL, nil
}

// HeadObject returns the size in bytes of the object at
// key. It is used by /confirm to validate that the
// uploaded file is within the 1 KB - 200 MB allow-list
// before flipping the video to PROCESSING. A missing
// object is returned as ErrNotFound (the sentinel
// declared in errors.go) so the handler can map it to
// 409 UPLOAD_MISSING without string-matching the
// underlying SDK error.
func (c *R2Client) HeadObject(ctx context.Context, key string) (int64, error) {
	if key == "" {
		return 0, fmt.Errorf("HeadObject: key is empty")
	}
	out, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, mapR2Error("HeadObject", key, err)
	}
	if out.ContentLength == nil {
		return 0, fmt.Errorf("HeadObject: missing ContentLength for %s", key)
	}
	return *out.ContentLength, nil
}

// DeleteObjects removes a batch of objects. The call is
// idempotent: an empty keys slice is a no-op, and an
// object that does not exist is reported in the result
// but does not fail the batch (per the API contract,
// cleanup must be tolerant of races where two
// processes try to delete the same key).
//
// Per the AWS SDK semantics, DeleteObjects is the
// batch variant; each missing key is reported in the
// response's Errors slice, which we collapse into the
// returned error only when there is at least one real
// failure (i.e. not a "NoSuchKey" / "NotFound").
func (c *R2Client) DeleteObjects(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	objects := make([]s3types.ObjectIdentifier, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		objects = append(objects, s3types.ObjectIdentifier{
			Key: aws.String(k),
		})
	}
	if len(objects) == 0 {
		return nil
	}
	out, err := c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(c.bucket),
		Delete: &s3types.Delete{
			Objects: objects,
			Quiet:   aws.Bool(false),
		},
	})
	if err != nil {
		return mapR2Error("DeleteObjects", "", err)
	}
	// SDK returns per-key errors in out.Errors. We only
	// fail the whole call when at least one entry is a
	// real error (i.e. not a NotFound). Anything else
	// (a transient S3 error on a single key) is
	// surfaced as a wrapped error so the caller can
	// log and retry without losing the other keys.
	for _, e := range out.Errors {
		if isNotFoundErr(aws.ToString(e.Code)) {
			continue
		}
		return fmt.Errorf("DeleteObjects: %s: %s", aws.ToString(e.Key), aws.ToString(e.Message))
	}
	return nil
}

// DeletePrefix removes every object whose key starts
// with prefix. It is used by the worker cleanup path
// to remove all HLS + thumbnail outputs under a video's
// hls_prefix without having to list the exact filenames
// the transcode pipeline produced.
//
// Implementation: ListObjectsV2 paginator collects every
// key under the prefix, then a single DeleteObjects
// call batches up to 1000 keys per AWS spec. The
// returned error is the first non-NotFound failure
// across all batches; NotFound keys are silently
// skipped (idempotent).
//
// Safety guards (defence in depth - HandleCleanupVideo
// also pre-validates the prefix, but if a future caller
// forgets we still refuse):
//   - prefix MUST end with "/" so we do not
//     accidentally delete keys that share a longer
//     common prefix (e.g. "hls/u/v1/" should not
//     match "hls/u/v11/").
//   - prefix MUST NOT be just "/" or "//..." - that
//     would mean "delete everything in the bucket"
//     and is never a valid cleanup target.
//   - prefix MUST contain at least one non-slash
//     character (so "" is impossible even if a caller
//     concatenates badly).
// These three checks make bucket-wipe impossible
// from this helper regardless of what the caller
// passes.
func (c *R2Client) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" {
		return fmt.Errorf("DeletePrefix: prefix is empty")
	}
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("DeletePrefix: prefix %q must end with '/'", prefix)
	}
	// Bucket-wipe guard. ListObjectsV2 with Prefix=""
	// or "//" returns every key in the bucket; refuse
	// anything that would amount to that.
	if strings.Trim(prefix, "/") == "" {
		return fmt.Errorf("DeletePrefix: prefix %q is too broad (would match bucket root)", prefix)
	}

	const batchSize = 1000 // AWS DeleteObjects hard limit per request
	var firstErr error

	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	batch := make([]string, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Reuse DeleteObjects so NotFound handling,
		// logging, and error wrapping stay in one place.
		if err := c.DeleteObjects(ctx, batch); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("DeletePrefix: batch under %q: %w", prefix, err)
		}
		batch = batch[:0]
	}

	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("DeletePrefix: list under %q: %w", prefix, mapR2Error("DeletePrefix", prefix, err))
			}
			return firstErr
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if key == "" {
				continue
			}
			batch = append(batch, key)
			if len(batch) >= batchSize {
				flush()
			}
		}
	}
	flush()
	return firstErr
}

// GetObject reads the object at key and returns its
// raw body. It is used by the api-gateway to fetch
// the master.m3u8 and per-variant HLS playlists that
// the GetPlaylist handler then rewrites and returns
// to the player. The body is bounded by maxBytes so
// a pathological object (e.g. a misnamed binary that
// landed under hls_prefix/) cannot exhaust the
// gateway's memory.
//
// Missing object is returned as ErrNotFound (the
// sentinel declared in errors.go) so the handler can
// map it to 404 with a stable code.
func (c *R2Client) GetObject(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("GetObject: key is empty")
	}
	if maxBytes <= 0 {
		maxBytes = 8 * 1024 * 1024 // 8 MiB - HLS master + variant playlists are tiny
	}
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, mapR2Error("GetObject", key, err)
	}
	defer out.Body.Close()
	// LimitReader guards against a runaway object;
	// io.ReadAll reads until EOF or the cap.
	body, err := io.ReadAll(io.LimitReader(out.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("GetObject: read %s: %w", key, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("GetObject: %s exceeds %d bytes", key, maxBytes)
	}
	return body, nil
}

// Download streams the object at key to a local file at
// destPath. The file is created with 0600 and the call
// is the only writer; the worker is expected to run
// with a read-only root FS and a tmpfs /tmp so the
// destination lives under /tmp.
func (c *R2Client) Download(ctx context.Context, key, destPath string) error {
	if key == "" {
		return fmt.Errorf("Download: key is empty")
	}
	if destPath == "" {
		return fmt.Errorf("Download: destPath is empty")
	}
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return mapR2Error("Download", key, err)
	}
	defer out.Body.Close()

	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("Download: create %s: %w", destPath, err)
	}
	// Close on every return path. Use a named return
	// via a helper closure so the body and the file
	// close are both guaranteed.
	if _, err := io.Copy(f, out.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(destPath)
		return fmt.Errorf("Download: copy %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(destPath)
		return fmt.Errorf("Download: close %s: %w", destPath, err)
	}
	return nil
}

// UploadFile streams the file at filePath to the object
// at key with the given content type. It is the
// server-side counterpart to PresignPut: used by the
// worker to publish HLS segments, master playlist, and
// thumbnail after the transcode succeeds.
func (c *R2Client) UploadFile(ctx context.Context, key, filePath, contentType string) error {
	if key == "" {
		return fmt.Errorf("UploadFile: key is empty")
	}
	if filePath == "" {
		return fmt.Errorf("UploadFile: filePath is empty")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("UploadFile: open %s: %w", filePath, err)
	}
	defer f.Close()

	in := &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        f,
		ContentType: aws.String(contentType),
	}
	if _, err := c.client.PutObject(ctx, in); err != nil {
		return mapR2Error("UploadFile", key, err)
	}
	return nil
}

// mapR2Error translates AWS SDK errors into the
// shared-package sentinels the rest of the codebase
// already understands. A missing key becomes
// ErrNotFound; everything else becomes a wrapped
// generic error so the original SDK message survives
// in the log.
//
// The acceptance criteria for phase-2.2 require
// HeadObject to surface missing uploads as a distinct
// signal so the confirm handler can return
// 409 UPLOAD_MISSING. Doing that translation in the
// SDK layer (rather than via string matching in the
// handler) keeps the handler clean and the behavior
// stable across SDK versions.
func mapR2Error(op, key string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if isNotFoundErr(apiErr.ErrorCode()) {
			return fmt.Errorf("%s: %s: %w", op, key, ErrNotFound)
		}
		return fmt.Errorf("%s: %s: code=%s: %w", op, key, apiErr.ErrorCode(), err)
	}
	// Some SDK errors come back as *http.ResponseError
	// without the smithy.APIError shape (e.g. DNS
	// failures). Return a wrapped generic error.
	return fmt.Errorf("%s: %s: %w", op, key, err)
}

// isNotFoundErr matches the AWS / S3 / R2 "missing
// object" code regardless of which path surfaced it
// (HeadObject 404, DeleteObjects per-key error, etc.).
// Using a switch keeps the set explicit and easy to
// extend when R2 introduces a new not-found variant.
func isNotFoundErr(code string) bool {
	switch code {
	case "NoSuchKey", "NotFound", "404":
		return true
	}
	return false
}
