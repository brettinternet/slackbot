package http

import (
	"container/list"
	"sync"
	"time"
)

const (
	// DefaultSlackEventDeduplicationWindow covers Slack's normal event retry period.
	DefaultSlackEventDeduplicationWindow = 5 * time.Minute
	// SlackEventDeduplicationCapacity is the maximum number of event IDs retained.
	SlackEventDeduplicationCapacity = 10_000
)

type deduplicationEntry struct {
	id        string
	expiresAt time.Time
}

type eventDeduplicator struct {
	mu        sync.Mutex
	retention time.Duration
	capacity  int
	entries   map[string]*list.Element
	order     *list.List
	now       func() time.Time
}

func newEventDeduplicator(retention time.Duration) *eventDeduplicator {
	if retention <= 0 {
		retention = DefaultSlackEventDeduplicationWindow
	}
	return &eventDeduplicator{
		retention: retention,
		capacity:  SlackEventDeduplicationCapacity,
		entries:   make(map[string]*list.Element),
		order:     list.New(),
		now:       time.Now,
	}
}

// IsDuplicate atomically records a usable Slack event ID and reports whether it
// was already recorded within the retention window. Empty IDs are never cached.
func (d *eventDeduplicator) IsDuplicate(id string) bool {
	duplicate, _ := d.accept(id, func() bool { return true })
	return duplicate
}

// accept runs dispatch once per usable event ID. An ID is recorded only when
// dispatch accepts the event, allowing Slack to retry while dispatch is unavailable.
func (d *eventDeduplicator) accept(id string, dispatch func() bool) (duplicate, accepted bool) {
	if id == "" {
		return false, dispatch()
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	d.removeExpired(now)
	if _, ok := d.entries[id]; ok {
		return true, true
	}
	if !dispatch() {
		return false, false
	}
	if len(d.entries) == d.capacity {
		d.removeOldest()
	}
	entry := deduplicationEntry{id: id, expiresAt: now.Add(d.retention)}
	d.entries[id] = d.order.PushBack(entry)
	return false, true
}

func (d *eventDeduplicator) removeExpired(now time.Time) {
	for element := d.order.Front(); element != nil; element = d.order.Front() {
		entry := element.Value.(deduplicationEntry)
		if entry.expiresAt.After(now) {
			return
		}
		d.remove(element, entry.id)
	}
}

func (d *eventDeduplicator) removeOldest() {
	if element := d.order.Front(); element != nil {
		d.remove(element, element.Value.(deduplicationEntry).id)
	}
}

func (d *eventDeduplicator) remove(element *list.Element, id string) {
	delete(d.entries, id)
	d.order.Remove(element)
}
