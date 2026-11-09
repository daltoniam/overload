package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFairQueueTakesTurns(t *testing.T) {
	queue := newFairQueue(1, 10)
	ctx := context.Background()
	release, err := queue.acquire(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 4)
	start := func(caller string) {
		go func() {
			done, err := queue.acquire(ctx, caller)
			if err != nil {
				t.Error(err)
				return
			}
			order <- caller
			done()
		}()
		waitFor(t, func() bool {
			queue.mu.Lock()
			defer queue.mu.Unlock()
			return len(queue.queues[caller]) > 0
		})
	}
	// Alice queues three more before Bob queues one; Bob still goes second.
	start("alice")
	start("alice")
	start("bob")
	release()
	var got []string
	for range 3 {
		got = append(got, <-order)
	}
	if got[0] != "alice" || got[1] != "bob" || got[2] != "alice" {
		t.Fatalf("order = %v", got)
	}
}

func TestFairQueueLimitsAndCancellation(t *testing.T) {
	queue := newFairQueue(1, 1)
	release, _ := queue.acquire(context.Background(), "a")
	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() {
		_, err := queue.acquire(ctx, "b")
		waited <- err
	}()
	waitFor(t, func() bool {
		queue.mu.Lock()
		defer queue.mu.Unlock()
		return queue.waiting == 1
	})
	if _, err := queue.acquire(context.Background(), "c"); !errors.Is(err, errQueueFull) {
		t.Fatalf("over the waiting limit: %v", err)
	}
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	release()
	queue.mu.Lock()
	inUse, waiting := queue.inUse, queue.waiting
	queue.mu.Unlock()
	if inUse != 0 || waiting != 0 {
		t.Fatalf("leaked: in use %d, waiting %d", inUse, waiting)
	}
	queue.setSlots(3)
	for range 3 {
		if _, err := queue.acquire(context.Background(), "d"); err != nil {
			t.Fatal(err)
		}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
