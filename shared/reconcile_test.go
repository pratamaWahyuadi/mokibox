package shared

import (
	"testing"
)

// reconcile_test.go - unit coverage for the issue #44
// producer contract. The full Detection/Action/Verification
// flow runs in scripts/smoketest/phase10_reconcile against
// live Postgres + R2 + Redis; here we pin the wire format
// and the guard so a rename cannot silently drop tasks.

func TestNewReconcileTickTask_TypeAndPayload(t *testing.T) {
	task, err := NewReconcileTickTask(ReconcileTickPayload{BatchSize: 7, DryRun: true})
	if err != nil {
		t.Fatalf("NewReconcileTickTask: %v", err)
	}
	if task.Type() != TypeReconcileTick {
		t.Errorf("task type = %q, want %q", task.Type(), TypeReconcileTick)
	}
	// marshalTask JSON-wraps the payload; the fields must
	// survive the round-trip with stable names.
	got := string(task.Payload())
	for _, want := range []string{`"batch_size":7`, `"dry_run":true`} {
		if !contains(got, want) {
			t.Errorf("payload %q missing %q", got, want)
		}
	}
}

func TestEnqueueReconcileTick_Guard(t *testing.T) {
	// BatchSize <= 0 must be rejected before any client
	// call (nil client would panic otherwise).
	if _, err := EnqueueReconcileTick(nil, ReconcileTickPayload{BatchSize: 0}); err == nil {
		t.Error("expected error for BatchSize=0, got nil")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
