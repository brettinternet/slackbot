package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap/zaptest"
)

func TestEventDeduplicatorRecordsOnlyAcceptedEvents(t *testing.T) {
	deduplicator := newEventDeduplicator(time.Minute)
	duplicate, accepted := deduplicator.accept("retryable", func() bool { return false })
	if duplicate || accepted {
		t.Fatalf("rejected event result = (%t, %t), want (false, false)", duplicate, accepted)
	}
	duplicate, accepted = deduplicator.accept("retryable", func() bool { return true })
	if duplicate || !accepted {
		t.Fatalf("accepted event result = (%t, %t), want (false, true)", duplicate, accepted)
	}
	called := false
	duplicate, accepted = deduplicator.accept("retryable", func() bool {
		called = true
		return true
	})
	if !duplicate || !accepted || called {
		t.Fatalf("duplicate result = (%t, %t), dispatch called = %t", duplicate, accepted, called)
	}
}

func TestEventDeduplicator(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	deduplicator := newEventDeduplicator(time.Minute)
	deduplicator.now = func() time.Time { return now }
	deduplicator.capacity = 2

	if deduplicator.IsDuplicate("") {
		t.Fatal("empty event ID reported as duplicate on first delivery")
	}
	if deduplicator.IsDuplicate("") {
		t.Fatal("empty event ID reported as duplicate on repeated delivery")
	}
	if deduplicator.IsDuplicate("first") {
		t.Fatal("new event ID reported as duplicate")
	}
	if !deduplicator.IsDuplicate("first") {
		t.Fatal("repeated event ID not reported as duplicate")
	}
	if deduplicator.IsDuplicate("second") || deduplicator.IsDuplicate("third") {
		t.Fatal("new event ID reported as duplicate")
	}
	if got := len(deduplicator.entries); got != 2 {
		t.Fatalf("retained event IDs = %d, want 2", got)
	}
	if deduplicator.IsDuplicate("first") {
		t.Fatal("oldest event ID was not evicted at capacity")
	}

	now = now.Add(time.Minute)
	if deduplicator.IsDuplicate("first") {
		t.Fatal("expired event ID reported as duplicate")
	}
}

type countingEventProcessor struct {
	calls atomic.Int64
}

func (p *countingEventProcessor) PushEvent(slackevents.EventsAPIEvent) error {
	p.calls.Add(1)
	return nil
}
func (p *countingEventProcessor) ProcessorType() string { return "counting" }

func postSlackEventID(server *Server, eventID string) int {
	body := fmt.Sprintf(`{"type":"event_callback","event_id":%q,"event":{"type":"message"}}`, eventID)
	request := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	response := httptest.NewRecorder()
	server.serveMux.ServeHTTP(response, request)
	return response.Code
}

func TestServer_DeduplicatesSlackEventRetries(t *testing.T) {
	server := NewServer(zaptest.NewLogger(t), Config{SlackEventPath: "/slack/events"}, acceptingSlackService{})
	processor := &countingEventProcessor{}
	server.RegisterEventProcessor(processor)

	if status := postSlackEventID(server, "Ev123"); status != http.StatusOK {
		t.Fatalf("first delivery status = %d, want %d", status, http.StatusOK)
	}
	if status := postSlackEventID(server, "Ev123"); status != http.StatusOK {
		t.Fatalf("duplicate delivery status = %d, want %d", status, http.StatusOK)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := processor.calls.Load(); got != 1 {
		t.Fatalf("processor calls = %d, want 1", got)
	}
}

func TestServer_DeduplicationIsConcurrencySafe(t *testing.T) {
	server := NewServer(zaptest.NewLogger(t), Config{SlackEventPath: "/slack/events"}, acceptingSlackService{})
	processor := &countingEventProcessor{}
	server.RegisterEventProcessor(processor)

	const deliveries = 100
	statuses := make(chan int, deliveries)
	var requests sync.WaitGroup
	requests.Add(deliveries)
	for range deliveries {
		go func() {
			defer requests.Done()
			statuses <- postSlackEventID(server, "EvConcurrent")
		}()
	}
	requests.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Errorf("delivery status = %d, want %d", status, http.StatusOK)
		}
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := processor.calls.Load(); got != 1 {
		t.Fatalf("processor calls = %d, want 1", got)
	}
}

func TestServer_DispatchesEventsWithoutUsableID(t *testing.T) {
	server := NewServer(zaptest.NewLogger(t), Config{SlackEventPath: "/slack/events"}, acceptingSlackService{})
	processor := &countingEventProcessor{}
	server.RegisterEventProcessor(processor)

	for range 2 {
		if status := postSlackEventID(server, ""); status != http.StatusOK {
			t.Fatalf("delivery status = %d, want %d", status, http.StatusOK)
		}
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := processor.calls.Load(); got != 2 {
		t.Fatalf("processor calls = %d, want 2", got)
	}
}
