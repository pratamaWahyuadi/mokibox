// r2-list/main.go - smoke helper that lists R2 keys
// under a given prefix. Uses the S3 ListObjectsV2
// API via aws-sdk-go-v2 directly.
//
// Usage:
//   go run ./scripts/smoketest/r2_list -prefix uploads/smoke/
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	var prefix = flag.String("prefix", "", "key prefix to list")
	flag.Parse()

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
	bucket := os.Getenv("R2_BUCKET")
	pager := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(*prefix),
	})
	count := 0
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			log.Fatal(err)
		}
		for _, obj := range page.Contents {
			fmt.Printf("  %s (%d bytes)\n", aws.ToString(obj.Key), aws.ToInt64(obj.Size))
			count++
		}
	}
	fmt.Printf("total %d objects under %s in %s\n", count, *prefix, bucket)
}