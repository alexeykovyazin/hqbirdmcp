package main

import (
	"context"
	"testing"
	"time"
)

// P6.2 T6 / improvement-plan A.2: the service Stop path must cancel the
// foreground run and wait for its cleanup, bounded by the SCM deadline.
func TestStopAndWaitDrains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // simulate deferred cleanup work
		close(done)
	}()
	start := time.Now()
	stopAndWait(cancel, done, 5*time.Second)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("stopAndWait returned before cleanup finished (%v)", elapsed)
	}
	select {
	case <-done:
	default:
		t.Fatal("done not closed after stopAndWait")
	}
}

func TestStopAndWaitBoundedWhenStuck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stuck := make(chan struct{}) // never closed
	start := time.Now()
	stopAndWait(cancel, stuck, 100*time.Millisecond)
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("stopAndWait exceeded its bound (%v)", elapsed)
	}
	if ctx.Err() == nil {
		t.Fatal("cancel was not invoked")
	}
}
