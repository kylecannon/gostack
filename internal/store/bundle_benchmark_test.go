package store_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/store"
)

func BenchmarkBundleGet(b *testing.B) {
	bundle := store.NewBundle(newState)
	bundle.Get("000000000000", "us-east-1")

	for b.Loop() {
		bundle.Get("000000000000", "us-east-1")
	}
}
