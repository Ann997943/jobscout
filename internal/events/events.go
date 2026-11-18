// Package events defines the change events produced by polling and a tiny
// in-process bus. The Bus interface is the seam where Kafka (or NATS, SQS…)
// slots in later: pollers publish to a topic, extractor/notifier consume.
package events

import (
	"context"
	"sync"
	"time"

	"github.com/Ann997943/jobscout/internal/model"
)

type Type string

const (
	New      Type = "new"
	Updated  Type = "updated"
	Closed   Type = "closed"
	Reopened Type = "reopened"
)

type Event struct {
	Type Type
	Job  model.Job
	At   time.Time
	// Bootstrap is true for jobs found the first time we ever poll a board.
	// They are not "new" to the world, so they must not trigger alerts.
	Bootstrap bool
}

type Handler func(ctx context.Context, e Event) error

// Bus is the pub/sub abstraction.
type Bus interface {
	Publish(ctx context.Context, evs ...Event) error
	Subscribe(name string, h Handler)
}

// MemoryBus delivers synchronously to every subscriber in registration order.
type MemoryBus struct {
	mu   sync.RWMutex
	subs []sub
	// OnError is called when a handler fails; delivery continues.
	OnError func(name string, e Event, err error)
}

type sub struct {
	name string
	h    Handler
}

func (b *MemoryBus) Subscribe(name string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, sub{name, h})
}

func (b *MemoryBus) Publish(ctx context.Context, evs ...Event) error {
	b.mu.RLock()
	subs := append([]sub(nil), b.subs...)
	b.mu.RUnlock()
	for _, e := range evs {
		for _, s := range subs {
			if err := s.h(ctx, e); err != nil && b.OnError != nil {
				b.OnError(s.name, e, err)
			}
		}
	}
	return ctx.Err()
}
