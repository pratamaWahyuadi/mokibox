// enqueue-test/main.go - smoke-test helper for issue C / D.
// Builds an *asynq.Task matching what the api-gateway
// would produce and enqueues it via shared.EnqueueTranscode
// / shared.EnqueueCleanupObjects / shared.EnqueueCleanupVideo.
//
// Usage (from host):
//   go run ./scripts/smoketest/phase5_enqueue -op transcode -videoID <uuid>
//   go run ./scripts/smoketest/phase5_enqueue -op cleanup-objects -keys k1,k2
//   go run ./scripts/smoketest/phase5_enqueue -op cleanup-video -videoID <uuid>
//
// Reads REDIS_ADDR / REDIS_PASSWORD from env.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/hibiken/asynq"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

func main() {
	var (
		op      = flag.String("op", "", "transcode | cleanup-objects | cleanup-video")
		videoID = flag.String("videoID", "", "video UUID (transcode / cleanup-video)")
		keys    = flag.String("keys", "", "comma-separated R2 keys (cleanup-objects)")
		delay   = flag.Duration("delay", 0, "ProcessIn delay (default 0 = immediate)")
	)
	flag.Parse()

	if *op == "" {
		log.Fatal("missing -op")
	}

	client, err := shared.NewAsynqClient(shared.RedisConfig{
		Addr:     os.Getenv("REDIS_ADDR"),
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	if err != nil {
		log.Fatalf("NewAsynqClient: %v", err)
	}
	defer client.Close()

	var info *asynq.TaskInfo
	switch *op {
	case "transcode":
		if *videoID == "" {
			log.Fatal("-videoID required")
		}
		info, err = shared.EnqueueTranscode(client, shared.TranscodeVideoPayload{VideoID: *videoID}, asynq.ProcessIn(*delay))
	case "cleanup-objects":
		if *keys == "" {
			log.Fatal("-keys required")
		}
		ks := strings.Split(*keys, ",")
		info, err = shared.EnqueueCleanupObjects(client, shared.CleanupObjectsPayload{Keys: ks}, asynq.ProcessIn(*delay))
	case "cleanup-video":
		if *videoID == "" {
			log.Fatal("-videoID required")
		}
		info, err = shared.EnqueueCleanupVideo(client, shared.CleanupVideoPayload{VideoID: *videoID}, asynq.ProcessIn(*delay))
	default:
		log.Fatalf("unknown -op %q", *op)
	}
	if err != nil {
		log.Fatalf("enqueue: %v", err)
	}
	fmt.Printf("enqueued: id=%s queue=%s type=%s\n", info.ID, info.Queue, info.Type)
}