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
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	for key, processTime := range d.recentMessages {
		if now.Sub(processTime) > d.expirationDuration {
			delete(d.recentMessages, key)
		}
	}

	key := recentMessageKey{userID: userID, channelID: channelID, msgID: msgID}
	if _, exists := d.recentMessages[key]; exists {
		return true
	}
	d.recentMessages[key] = now
	return false
}
