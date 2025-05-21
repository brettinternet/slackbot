package vibecheck

import (
	"sync"
	"time"
)

type recentMessageKey struct {
	userID    string
	channelID string
	msgID     string // Using the message timestamp as an ID
}

type messageDeduplicator struct {
	mu                 sync.RWMutex
	recentMessages     map[recentMessageKey]time.Time
	expirationDuration time.Duration
}

// newMessageDeduplicator creates a new deduplicator with the given expiration duration
func newMessageDeduplicator(expirationDuration time.Duration) *messageDeduplicator {
	return &messageDeduplicator{
		recentMessages:     make(map[recentMessageKey]time.Time),
		expirationDuration: expirationDuration,
	}
}

// IsDupe checks if a message has been processed recently
// Returns true if it's a duplicate (should be skipped) and false if it's new
func (d *messageDeduplicator) IsDupe(userID, channelID, msgID string) bool {
	d.mu.RLock()
	key := recentMessageKey{userID: userID, channelID: channelID, msgID: msgID}
	_, exists := d.recentMessages[key]
	d.mu.RUnlock()

	if exists {
		return true
	}

	d.mu.Lock()
	d.recentMessages[key] = time.Now()
	d.mu.Unlock()

	go d.cleanup()

	return false
}

func (d *messageDeduplicator) cleanup() {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()

	for key, processTime := range d.recentMessages {
		if now.Sub(processTime) > d.expirationDuration {
			delete(d.recentMessages, key)
		}
	}
}
