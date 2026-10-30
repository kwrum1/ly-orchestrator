package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunCommandBoundedReturnsOnProcessThatIgnoresContext(t *testing.T) {
	start := time.Now()
	_, err := runCommandBounded(context.Background(), 150*time.Millisecond, "sh", "-c", "sleep 5")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrCommandTimeout) {
		t.Fatalf("error = %v, want ErrCommandTimeout", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("bounded command returned after %s, want under 2s", elapsed)
	}
}
