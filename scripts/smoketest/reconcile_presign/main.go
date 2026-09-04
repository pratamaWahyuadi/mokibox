// reconcile_presign/main.go - smoke helper for issue #44:
// prints a presigned PUT or GET URL for an arbitrary R2
// key. Used by phase10_reconcile/run.sh to seed orphan
// objects and verify object existence without the
// api-gateway (which only presigns keys it created).
//
// Usage:
//   go run ./scripts/smoketest/reconcile_presign -put -key uploads/<uid>/seed.bin
//   go run ./scripts/smoketest/reconcile_presign -get -key hls/<uid>/x.bin
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	put := flag.Bool("put", false, "presign PUT (default GET)")
	key := flag.String("key", "", "R2 object key")
	flag.Parse()

	if *key == "" {
		log.Fatal("missing -key")
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
		log.Fatal(err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(os.Getenv("R2_ENDPOINT"))
		o.UsePathStyle = true
	})
	presigner := s3.NewPresignClient(client)

	if *put {
		req, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(os.Getenv("R2_BUCKET")),
			Key:    aws.String(*key),
		}, s3.WithPresignExpires(5*time.Minute))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(req.URL)
		return
	}

	req, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(os.Getenv("R2_BUCKET")),
		Key:    aws.String(*key),
	}, s3.WithPresignExpires(5*time.Minute))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(req.URL)
}
