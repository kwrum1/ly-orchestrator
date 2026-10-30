package pppoeclient

import (
	"testing"
	"time"
)

// A fresh session retries prefix acquisition quickly, but the delay stays
// bounded; an established lease keeps the conservative renewal backoff.
func TestDelegatedPrefixRetryDelayIsFastAndBounded(t *testing.T) {
	if got := delegatedPrefixRetryDelay(true, 1); got != delegatedPrefixRetryInterval {
		t.Fatalf("renew failure delay = %s, want %s", got, delegatedPrefixRetryInterval)
	}
	want := []time.Duration{
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		8 * time.Second,
		8 * time.Second,
	}
	for index, expected := range want {
		if got := delegatedPrefixRetryDelay(false, index+1); got != expected {
			t.Fatalf("initial acquisition retry %d = %s, want %s", index+1, got, expected)
		}
	}
	if got := delegatedPrefixRetryDelay(false, 0); got != delegatedPrefixInitialRetryInterval {
		t.Fatalf("zero failures delay = %s, want %s", got, delegatedPrefixInitialRetryInterval)
	}
	if delegatedPrefixMaxInitialRetryInterval > delegatedPrefixRetryInterval {
		t.Fatal("initial retry must never exceed the steady-state retry interval")
	}
}
