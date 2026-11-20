package main

import (
	"testing"

	"github.com/google/uuid"
)

func TestReconcileUserPrefixesIncludeAvatarObjects(t *testing.T) {
	userID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	got := reconcileUserPrefixes(userID)
	want := [4]string{
		"uploads/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/",
		"thumbs/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/",
		"hls/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/",
		"avatars/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/",
	}
	if got != want {
		t.Fatalf("reconcile user prefixes = %v, want %v", got, want)
	}
}
