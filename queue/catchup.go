package queue

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/daltoniam/overload/github"
)

// DeliverySource lists the App's recent webhook deliveries and redelivers
// one.
type DeliverySource interface {
	RecentDeliveries(ctx context.Context, since time.Time, limit int) ([]github.Delivery, error)
	Redeliver(ctx context.Context, deliveryID int64) error
}

// DeliveryLedger reports whether overload already recorded a delivery.
type DeliveryLedger interface {
	KnownDelivery(ctx context.Context, guid string) (bool, error)
}

// Webhook catch-up settings. GitHub keeps deliveries for three days and
// never retries an App delivery on its own.
const (
	catchUpWindow  = 24 * time.Hour
	catchUpSettle  = 2 * time.Minute
	catchUpLimit   = 500
	maxRedelivered = 20
)

// missedDeliveries picks deliveries that never reached overload: GitHub
// failed to deliver them (no response, a timeout or a 5xx), no attempt with
// the same GUID succeeded or is a redelivery already, overload has no
// record of them, and they are old enough that a slow first attempt is not
// still in flight. 4xx answers are overload's own decisions (for example a
// bad signature) and are not retried. Oldest first, at most maxRedelivered.
func missedDeliveries(ctx context.Context, ledger DeliveryLedger, deliveries []github.Delivery, now time.Time) ([]github.Delivery, error) {
	settled := map[string]bool{}
	for _, delivery := range deliveries {
		if delivery.Redelivery || delivered(delivery.StatusCode) {
			settled[delivery.GUID] = true
		}
	}
	var missed []github.Delivery
	seen := map[string]bool{}
	for _, delivery := range deliveries {
		if delivery.GUID == "" || settled[delivery.GUID] || seen[delivery.GUID] || !transportFailure(delivery.StatusCode) || now.Sub(delivery.DeliveredAt) < catchUpSettle {
			continue
		}
		seen[delivery.GUID] = true
		known, err := ledger.KnownDelivery(ctx, delivery.GUID)
		if err != nil {
			return nil, err
		}
		if !known {
			missed = append(missed, delivery)
		}
	}
	sort.Slice(missed, func(i, j int) bool { return missed[i].DeliveredAt.Before(missed[j].DeliveredAt) })
	if len(missed) > maxRedelivered {
		missed = missed[:maxRedelivered]
	}
	return missed, nil
}

func delivered(status int) bool { return status >= 200 && status < 300 }

func transportFailure(status int) bool { return status == 0 || status >= 500 }

// CatchUpWebhooks redelivers webhooks that never reached overload, for
// example during a restart, a tunnel outage or a DNS failure. It returns
// how many it asked GitHub to send again.
func CatchUpWebhooks(ctx context.Context, source DeliverySource, ledger DeliveryLedger, now time.Time) (int, error) {
	deliveries, err := source.RecentDeliveries(ctx, now.Add(-catchUpWindow), catchUpLimit)
	if err != nil {
		return 0, err
	}
	missed, err := missedDeliveries(ctx, ledger, deliveries, now)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, delivery := range missed {
		if err := source.Redeliver(ctx, delivery.ID); err != nil {
			slog.Warn("webhook redelivery failed", "delivery", delivery.GUID, "event", delivery.Event, "error", err)
			continue
		}
		slog.Info("redelivered a missed webhook", "delivery", delivery.GUID, "event", delivery.Event+"."+delivery.Action, "first_sent", delivery.DeliveredAt)
		sent++
	}
	return sent, nil
}
