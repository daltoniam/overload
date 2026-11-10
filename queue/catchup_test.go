package queue

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/daltoniam/overload/github"
)

type fakeDeliveries struct {
	deliveries  []github.Delivery
	redelivered []int64
	known       map[string]bool
}

func (source *fakeDeliveries) RecentDeliveries(context.Context, time.Time, int) ([]github.Delivery, error) {
	return source.deliveries, nil
}

func (source *fakeDeliveries) Redeliver(_ context.Context, id int64) error {
	source.redelivered = append(source.redelivered, id)
	return nil
}

func (source *fakeDeliveries) KnownDelivery(_ context.Context, guid string) (bool, error) {
	return source.known[guid], nil
}

func TestCatchUpWebhooks(t *testing.T) {
	now := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)
	at := func(minutesAgo int) time.Time { return now.Add(-time.Duration(minutesAgo) * time.Minute) }
	source := &fakeDeliveries{known: map[string]bool{"recorded": true}, deliveries: []github.Delivery{
		{ID: 1, GUID: "timed-out", Event: "pull_request", StatusCode: 500, DeliveredAt: at(30)},
		{ID: 2, GUID: "no-response", Event: "pull_request", StatusCode: 0, DeliveredAt: at(40)},
		{ID: 3, GUID: "ok", Event: "pull_request", StatusCode: 202, DeliveredAt: at(20)},
		{ID: 4, GUID: "rejected-by-overload", Event: "pull_request", StatusCode: 401, DeliveredAt: at(20)},
		{ID: 5, GUID: "recorded", Event: "pull_request", StatusCode: 502, DeliveredAt: at(20)},
		{ID: 6, GUID: "too-recent", Event: "pull_request", StatusCode: 500, DeliveredAt: at(1)},
		{ID: 7, GUID: "retried-ok", Event: "pull_request", StatusCode: 500, DeliveredAt: at(50)},
		{ID: 8, GUID: "retried-ok", Event: "pull_request", StatusCode: 202, DeliveredAt: at(45), Redelivery: true},
		{ID: 9, GUID: "already-redelivered", Event: "pull_request", StatusCode: 500, DeliveredAt: at(60)},
		{ID: 10, GUID: "already-redelivered", Event: "pull_request", StatusCode: 500, DeliveredAt: at(55), Redelivery: true},
	}}
	sent, err := CatchUpWebhooks(context.Background(), source, source, now)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 2 || fmt.Sprint(source.redelivered) != "[2 1]" {
		t.Fatalf("sent %d redelivered %v, want the two unrecorded transport failures, oldest first", sent, source.redelivered)
	}
}

func TestCatchUpWebhooksCapsRedeliveries(t *testing.T) {
	now := time.Now()
	source := &fakeDeliveries{known: map[string]bool{}}
	for index := range maxRedelivered + 5 {
		source.deliveries = append(source.deliveries, github.Delivery{ID: int64(index + 1), GUID: fmt.Sprint("g", index), StatusCode: 503, DeliveredAt: now.Add(-time.Hour)})
	}
	if sent, err := CatchUpWebhooks(context.Background(), source, source, now); err != nil || sent != maxRedelivered {
		t.Fatalf("sent %d err %v, want %d", sent, err, maxRedelivered)
	}
}
