package backupsvc

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestDrainUntilBounded(t *testing.T) {
	base := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		ch := make(chan string, 8)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			drainUntil(ctx, ch, nil)
			close(done)
		}()
		ch <- "msg"
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("drainUntil did not return after cancel")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := runtime.NumGoroutine(); n > base+10 {
		t.Fatalf("goroutines grew %d → %d (C8)", base, n)
	}
}
