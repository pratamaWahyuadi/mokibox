// r2_head/main.go - smoke helper: exits 0 when the R2 object
// at -key EXISTS, 1 when it is missing (NoSuchKey/404), 2 on
// any other error. Used by phase10_reconcile/run.sh to
// assert object presence/absence without printing secrets.
//
// Usage:
//   go run ./scripts/smoketest/r2_head -key uploads/<uid>/x.bin
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func main() {
	key := flag.String("key", "", "R2 object key")
	flag.Parse()

	if *key == "" {
		logFatal("missing -key")
	}

	ctx := context.Background()
	cfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion("auto"),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			os.Getenv("R2_ACCESS_KEY_ID"),
			os.Getenv("R2_SECRET_ACCESS_KEY"),
			"",
		)),
	)
	if err != nil {
		logFatal(err.Error())
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(os.Getenv("R2_ENDPOINT"))
		o.UsePathStyle = true
	})

	_, err = client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(os.Getenv("R2_BUCKET")),
		Key:    aws.String(*key),
	})
	if err == nil {
		fmt.Println("EXISTS")
		os.Exit(0)
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "404" || apiErr.ErrorCode() == "NoSuchKey") {
		fmt.Println("MISSING")
		os.Exit(1)
	}
	logFatal(fmt.Sprintf("head %s: %v", *key, err))
}

func logFatal(msg string) {
	fmt.Fprintln(os.Stderr, "r2_head: "+msg)
	os.Exit(2)
}
