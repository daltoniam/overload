package gateway

import (
	"context"
	"errors"
	"sync"
)

// errQueueFull means too many requests are already waiting for a model.
var errQueueFull = errors.New("queue full")

// fairQueue hands a model's request slots to waiting callers in turn, so
// one person sending many requests cannot starve the others: when a slot
// frees, it goes to the next caller in round-robin order, not to the
// oldest request.
type fairQueue struct {
	mu         sync.Mutex
	slots      int
	inUse      int
	maxWaiting int
	waiting    int
	queues     map[string][]chan struct{}
	order      []string
}

func newFairQueue(slots, maxWaiting int) *fairQueue {
	return &fairQueue{slots: max(slots, 1), maxWaiting: maxWaiting, queues: map[string][]chan struct{}{}}
}

// setSlots follows a change to the model's parallel slots.
func (queue *fairQueue) setSlots(slots int) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.slots = max(slots, 1)
	queue.fill()
}

// acquire waits for a slot for caller and returns the function that frees
// it.
func (queue *fairQueue) acquire(ctx context.Context, caller string) (func(), error) {
	queue.mu.Lock()
	if queue.inUse < queue.slots && queue.waiting == 0 {
		queue.inUse++
		queue.mu.Unlock()
		return queue.release, nil
	}
	if queue.waiting >= queue.maxWaiting {
		queue.mu.Unlock()
		return nil, errQueueFull
	}
	ready := make(chan struct{}, 1)
	if len(queue.queues[caller]) == 0 {
		queue.order = append(queue.order, caller)
	}
	queue.queues[caller] = append(queue.queues[caller], ready)
	queue.waiting++
	queue.mu.Unlock()
	select {
	case <-ready:
		return queue.release, nil
	case <-ctx.Done():
		queue.mu.Lock()
		defer queue.mu.Unlock()
		select {
		case <-ready:
			// The slot was handed over as the caller gave up; pass it on.
			queue.inUse--
			queue.fill()
		default:
			queue.remove(caller, ready)
		}
		return nil, ctx.Err()
	}
}

func (queue *fairQueue) release() {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.inUse--
	queue.fill()
}

// fill hands free slots to waiting callers. The caller holds mu.
func (queue *fairQueue) fill() {
	for queue.inUse < queue.slots && len(queue.order) > 0 {
		queue.handOff()
	}
}

// handOff gives a slot to the next waiting caller in turn. The caller
// holds mu.
func (queue *fairQueue) handOff() {
	caller := queue.order[0]
	queue.order = queue.order[1:]
	waiters := queue.queues[caller]
	ready := waiters[0]
	if len(waiters) == 1 {
		delete(queue.queues, caller)
	} else {
		queue.queues[caller] = waiters[1:]
		queue.order = append(queue.order, caller)
	}
	queue.waiting--
	queue.inUse++
	ready <- struct{}{}
}

// remove drops a waiter that gave up. The caller holds mu.
func (queue *fairQueue) remove(caller string, ready chan struct{}) {
	waiters := queue.queues[caller]
	for index, waiter := range waiters {
		if waiter == ready {
			waiters = append(waiters[:index:index], waiters[index+1:]...)
			queue.waiting--
			break
		}
	}
	if len(waiters) > 0 {
		queue.queues[caller] = waiters
		return
	}
	delete(queue.queues, caller)
	for index, name := range queue.order {
		if name == caller {
			queue.order = append(queue.order[:index:index], queue.order[index+1:]...)
			break
		}
	}
}
