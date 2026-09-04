// r2-get/main.go - smoke helper that downloads a single
// R2 key and prints it to stdout. Used to spot-check
// generated files (e.g. master.m3u8) from the smoke test.
//
// Usage:
//   go run ./scripts/smoketest/r2_get -key hls/user/vid/master.m3u8
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
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func main() {
	var key = flag.String("key", "", "R2 object key")
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
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(os.Getenv("R2_BUCKET")),
		Key:    aws.String(*key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errAs(err, &nsk) {
			log.Fatalf("NoSuchKey: %s", *key)
		}
		log.Fatal(err)
	}
	defer out.Body.Close()
	size := int(aws.ToInt64(out.ContentLength))
	if size == 0 {
		size = 4096
	}
	buf := make([]byte, size)
	n, err := out.Body.Read(buf)
	if err != nil && err.Error() != "EOF" {
		log.Fatal(err)
	}
	fmt.Printf("%s", buf[:n])
}

// errAs is a tiny shim so we don't need to import
// errors just for one errors.As call.
func errAs(err error, target any) bool {
	if _, isNoSuch := err.(*types.NoSuchKey); isNoSuch {
		return true
	}
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
		if _, isNoSuch := err.(*types.NoSuchKey); isNoSuch {
			return true
		}
	}
	return false
}