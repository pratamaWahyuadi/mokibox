// reconcile_enqueue/main.go - producer-side helper for
// issue #44: enqueues one reconcile:tick task on the
// worker's queue. Backs `make reconcile-once` and
// `make reconcile-once-dry`.
//
// Usage:
//   go run ./scripts/smoketest/reconcile_enqueue -batch 100 -dry-run=false
//
// Reads REDIS_ADDR / REDIS_PASSWORD from env. Exit 0 on
// success.
package main

import (
	"flag"
	"log"
	"os"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

func main() {
	batch := flag.Int("batch", 100, "max tombstoned users examined per tick")
	dryRun := flag.Bool("dry-run", false, "log orphan candidates without enqueueing cleanup")
	flag.Parse()

	if *batch <= 0 {
		log.Fatal("-batch must be > 0")
	}

	client, err := shared.NewAsynqClient(shared.RedisConfig{
		Addr:     os.Getenv("REDIS_ADDR"),
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	if err != nil {
		log.Fatalf("NewAsynqClient: %v", err)
	}
	defer client.Close()

	info, err := shared.EnqueueReconcileTick(client, shared.ReconcileTickPayload{
		BatchSize: *batch,
		DryRun:    *dryRun,
	})
	if err != nil {
		log.Fatalf("enqueue: %v", err)
	}
	log.Printf("reconcile:tick enqueued: id=%s queue=%s batch=%d dry_run=%v\n",
		info.ID, info.Queue, *batch, *dryRun)
}
