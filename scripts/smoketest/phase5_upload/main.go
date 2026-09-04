// upload-test/main.go - smoke helper to push a test
// mp4 to R2 at a specific key. Used by phase 5 end-to-end
// smoke. Reads R2_* env vars, opens the R2 client, and
// PUTs the local file.
//
// Usage:
//   go run ./scripts/smoketest/phase5_upload -file /tmp/test.mp4 -key uploads/x/y/source.mp4
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

func main() {
	var (
		file = flag.String("file", "", "local file to upload")
		key  = flag.String("key", "", "R2 key (path inside bucket)")
	)
	flag.Parse()

	if *file == "" || *key == "" {
		log.Fatal("missing -file or -key")
	}

	client, err := shared.NewR2Client(context.Background(), shared.R2Config{
		AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("R2_BUCKET"),
		Endpoint:        os.Getenv("R2_ENDPOINT"),
	})
	if err != nil {
		log.Fatalf("NewR2Client: %v", err)
	}
	ctx := context.Background()
	if err := client.UploadFile(ctx, *key, *file, "video/mp4"); err != nil {
		log.Fatalf("UploadFile: %v", err)
	}
	fmt.Printf("uploaded %s -> s3://%s/%s\n", *file, client.Bucket(), *key)
}