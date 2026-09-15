package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
	botmetrics "slackbot.arpa/bot/metrics"
)

// mockSlackService for testing
type mockSlackService struct {
	shouldVerifyFail bool
	verifyCalls      int
	verifiedBody     []byte
}

func (m *mockSlackService) VerifyRequest(headers http.Header, body []byte) error {
	m.verifyCalls++
	m.verifiedBody = append([]byte(nil), body...)
	if m.shouldVerifyFail {
		return http.ErrAbortHandler
	}
	return nil
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("read failure")
}

// mockSlackEventProcessor for testing
type mockSlackEventProcessor struct {
	processEventCalled atomic.Bool
	lastEventMu        sync.Mutex
	lastEvent          any
}

func (m *mockSlackEventProcessor) PushEvent(event slackevents.EventsAPIEvent) error {
	m.lastEventMu.Lock()
	m.lastEvent = event
	m.lastEventMu.Unlock()
	m.processEventCalled.Store(true)
	return nil
}

func (m *mockSlackEventProcessor) ProcessorType() string {
	return "mock"
}

func TestServer_MetricsEndpoint(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		server := NewServer(zaptest.NewLogger(t), Config{}, &mockSlackService{})
		response := httptest.NewRecorder()
		server.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("metrics status = %d, want %d", response.Code, http.StatusNotFound)
		}
	})

	t.Run("enabled at configured path", func(t *testing.T) {
		server := NewServer(zaptest.NewLogger(t), Config{Metrics: botmetrics.Config{
			Enabled: true,
			Path:    "/internal/metrics",
		}}, &mockSlackService{})

		server.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
		response := httptest.NewRecorder()
		server.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/internal/metrics", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("metrics status = %d, want %d", response.Code, http.StatusOK)
		}
		body := response.Body.String()
		if !strings.Contains(body, `slackbot_http_requests_total{route="health",status="200"} 1`) {
			t.Fatalf("metrics body missing health request counter:\n%s", body)
		}
	})
}

func TestProcessorQueueDepthIncludesInFlightWork(t *testing.T) {
	metricSet := botmetrics.New()
	processor := &blockingEventProcessor{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	dispatcher := newSlackEventDispatcher(zaptest.NewLogger(t), processor, metricSet)
	done := make(chan struct{})
	go func() {
		dispatcher.run()
		close(done)
	}()

	if !dispatcher.enqueue(slackevents.EventsAPIEvent{}) {
		t.Fatal("enqueue() = false, want true")
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	assertMetricContains(t, metricSet, `slackbot_processor_queue_depth{processor="unknown"} 1`)

	close(processor.release)
	deadline := time.Now().Add(time.Second)
	for dispatcher.pending.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	assertMetricContains(t, metricSet, `slackbot_processor_queue_depth{processor="unknown"} 0`)
	close(dispatcher.queue)
	<-done
}

func assertMetricContains(t *testing.T, metricSet *botmetrics.Metrics, want string) {
	t.Helper()
	response := httptest.NewRecorder()
	metricSet.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), want) {
		t.Fatalf("metrics body missing %q:\n%s", want, response.Body.String())
	}
}

func TestNewServer(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		ServerPort:     8080,
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{}

	server := NewServer(logger, config, mockSlack)

	if server == nil {
		t.Fatal("NewServer() returned nil")
	}

	if server.config.ServerPort != 8080 {
		t.Errorf("NewServer() ServerPort = %v, want %v", server.config.ServerPort, 8080)
	}

	if server.config.SlackEventPath != "/slack/events" {
		t.Errorf("NewServer() SlackEventPath = %v, want %v", server.config.SlackEventPath, "/slack/events")
	}

	if server.serveMux == nil {
		t.Error("NewServer() should initialize serveMux")
	}
}

func TestServer_HealthEndpoints(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		ServerPort:     8080,
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{}
	server := NewServer(logger, config, mockSlack)

	// Test readiness endpoint when not ready
	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()
	server.serveMux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Readiness endpoint should return 503 when not ready, got %d", w.Code)
	}

	// Set server as ready
	server.isReady.Store(true)

	// Test readiness endpoint when ready
	req = httptest.NewRequest("GET", "/ready", nil)
	w = httptest.NewRecorder()
	server.serveMux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Readiness endpoint should return 200 when ready, got %d", w.Code)
	}

	// Test health endpoint
	req = httptest.NewRequest("GET", "/health", nil)
	w = httptest.NewRecorder()
	server.serveMux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Health endpoint should return 200, got %d", w.Code)
	}

	// Test healthz endpoint during shutdown
	server.isShuttingDown.Store(true)
	req = httptest.NewRequest("GET", "/healthz", nil)
	w = httptest.NewRecorder()
	server.serveMux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Healthz endpoint should return 503 during shutdown, got %d", w.Code)
	}
}

func TestServer_SlackEventsEndpoint(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		ServerPort:     8080,
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{}
	server := NewServer(logger, config, mockSlack)

	// Test URL challenge (Slack verification)
	challengeBody := `{"challenge": "test-challenge", "type": "url_verification"}`
	req := httptest.NewRequest("POST", "/slack/events", bytes.NewBufferString(challengeBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.serveMux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("URL challenge should return 200, got %d", w.Code)
	}

	expected := `{"challenge":"test-challenge"}`
	if w.Body.String() != expected {
		t.Errorf("URL challenge response = %v, want %v", w.Body.String(), expected)
	}
	if got := string(mockSlack.verifiedBody); got != challengeBody {
		t.Errorf("VerifyRequest() body = %q, want original body %q", got, challengeBody)
	}
}

func TestServer_SlackEventsEndpoint_RequestValidation(t *testing.T) {
	newServer := func(t *testing.T) (*Server, *mockSlackService) {
		t.Helper()
		mockSlack := &mockSlackService{}
		server := NewServer(zaptest.NewLogger(t), Config{SlackEventPath: "/slack/events"}, mockSlack)
		return server, mockSlack
	}

	t.Run("method", func(t *testing.T) {
		server, mockSlack := newServer(t)
		req := httptest.NewRequest(http.MethodGet, "/slack/events", nil)
		w := httptest.NewRecorder()

		server.serveMux.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
		if got := w.Header().Get("Allow"); got != http.MethodPost {
			t.Errorf("Allow header = %q, want %q", got, http.MethodPost)
		}
		if mockSlack.verifyCalls != 0 {
			t.Errorf("VerifyRequest() calls = %d, want 0", mockSlack.verifyCalls)
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		server, mockSlack := newServer(t)
		body := strings.Repeat("x", int(SlackEventMaxBodyBytes)+1)
		req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
		w := httptest.NewRecorder()

		server.serveMux.ServeHTTP(w, req)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("oversized request status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
		}
		if mockSlack.verifyCalls != 0 {
			t.Errorf("VerifyRequest() calls = %d, want 0", mockSlack.verifyCalls)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		server, mockSlack := newServer(t)
		const body = `{"type":`
		req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
		w := httptest.NewRecorder()

		server.serveMux.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("malformed request status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if got := string(mockSlack.verifiedBody); got != body {
			t.Errorf("VerifyRequest() body = %q, want original body %q", got, body)
		}
	})

	t.Run("body read failure", func(t *testing.T) {
		server, _ := newServer(t)
		req := httptest.NewRequest(http.MethodPost, "/slack/events", nil)
		req.Body = io.NopCloser(errorReader{})
		w := httptest.NewRecorder()

		server.serveMux.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("read failure status = %d, want %d", w.Code, http.StatusInternalServerError)
		}
	})
}

func TestServer_SlackEventsEndpoint_VerificationFail(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		ServerPort:     8080,
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{shouldVerifyFail: true}
	server := NewServer(logger, config, mockSlack)

	challengeBody := `{"challenge": "test-challenge", "type": "url_verification"}`
	req := httptest.NewRequest("POST", "/slack/events", bytes.NewBufferString(challengeBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.serveMux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Failed verification should return 401, got %d", w.Code)
	}
	if got := string(mockSlack.verifiedBody); got != challengeBody {
		t.Errorf("VerifyRequest() body = %q, want original body %q", got, challengeBody)
	}
}

func TestServer_RegisterEventProcessor(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		ServerPort:     8080,
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{}
	server := NewServer(logger, config, mockSlack)

	// Initially no processors
	if len(server.slackEventProcessors) != 0 {
		t.Errorf("Initial processors count = %d, want 0", len(server.slackEventProcessors))
	}

	// Register a processor
	processor := &mockSlackEventProcessor{}
	server.RegisterEventProcessor(processor)

	if len(server.slackEventProcessors) != 1 {
		t.Errorf("After registration processors count = %d, want 1", len(server.slackEventProcessors))
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestServer_EventProcessing(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		ServerPort:     8080,
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{}
	server := NewServer(logger, config, mockSlack)

	// Register a processor
	processor := &mockSlackEventProcessor{}
	server.RegisterEventProcessor(processor)

	// Send an event
	eventBody := `{"type": "event_callback", "event": {"type": "message", "text": "hello"}}`
	req := httptest.NewRequest("POST", "/slack/events", bytes.NewBufferString(eventBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.serveMux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Event processing should return 200, got %d", w.Code)
	}

	// Processor dispatch is asynchronous.
	deadline := time.Now().Add(time.Second)
	for !processor.processEventCalled.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !processor.processEventCalled.Load() {
		t.Error("Event processor should have been called")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

type blockingEventProcessor struct {
	started   chan struct{}
	release   chan struct{}
	calls     atomic.Int64
	active    atomic.Int64
	maxActive atomic.Int64
}

func (p *blockingEventProcessor) PushEvent(slackevents.EventsAPIEvent) error {
	p.calls.Add(1)
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for maximum := p.maxActive.Load(); active > maximum; maximum = p.maxActive.Load() {
		if p.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-p.release
	return nil
}

func (p *blockingEventProcessor) ProcessorType() string { return "blocked" }

type healthyEventProcessor struct {
	processed chan struct{}
	calls     atomic.Int64
}

func (p *healthyEventProcessor) PushEvent(slackevents.EventsAPIEvent) error {
	p.calls.Add(1)
	select {
	case p.processed <- struct{}{}:
	default:
	}
	return nil
}

func (p *healthyEventProcessor) ProcessorType() string { return "healthy" }

type failingEventProcessor struct {
	calls atomic.Int64
}

func (p *failingEventProcessor) PushEvent(slackevents.EventsAPIEvent) error {
	p.calls.Add(1)
	return errors.New("processor failure")
}

func (p *failingEventProcessor) ProcessorType() string { return "failing" }

type panickingEventProcessor struct {
	calls atomic.Int64
}

func (p *panickingEventProcessor) PushEvent(slackevents.EventsAPIEvent) error {
	p.calls.Add(1)
	panic("processor failure")
}

func (p *panickingEventProcessor) ProcessorType() string { return "panicking" }

type acceptingSlackService struct{}

func (acceptingSlackService) VerifyRequest(http.Header, []byte) error { return nil }

type blockingSlackService struct {
	started chan struct{}
	release chan struct{}
}

func (s *blockingSlackService) VerifyRequest(http.Header, []byte) error {
	close(s.started)
	<-s.release
	return nil
}

type internallyQueuedProcessor struct {
	pending atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (p *internallyQueuedProcessor) PushEvent(slackevents.EventsAPIEvent) error {
	p.pending.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	go func() {
		<-p.release
		p.pending.Add(-1)
	}()
	return nil
}

func (p *internallyQueuedProcessor) ProcessorType() string { return "internally-queued" }
func (p *internallyQueuedProcessor) PendingEvents() int64  { return p.pending.Load() }

func postSlackEvent(server *Server, text string) *httptest.ResponseRecorder {
	body := `{"type":"event_callback","event":{"type":"message","text":"` + text + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.serveMux.ServeHTTP(w, req)
	return w
}

func TestServer_BeginShutdownPreservesInFlightRequest(t *testing.T) {
	slackService := &blockingSlackService{started: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(zaptest.NewLogger(t), Config{SlackEventPath: "/slack/events"}, slackService)
	processor := &mockSlackEventProcessor{}
	server.RegisterEventProcessor(processor)

	response := make(chan int, 1)
	go func() { response <- postSlackEvent(server, "in flight").Code }()
	select {
	case <-slackService.started:
	case <-time.After(time.Second):
		t.Fatal("request did not enter verification")
	}
	if err := server.BeginShutdown(context.Background()); err != nil {
		t.Fatalf("BeginShutdown() error = %v", err)
	}
	close(slackService.release)
	if got := <-response; got != http.StatusOK {
		t.Fatalf("in-flight event status = %d, want %d", got, http.StatusOK)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if !processor.processEventCalled.Load() {
		t.Error("in-flight event was acknowledged but not dispatched")
	}
}

func TestServer_ClosedDispatchRequestsRetry(t *testing.T) {
	slackService := &blockingSlackService{started: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(zaptest.NewLogger(t), Config{SlackEventPath: "/slack/events"}, slackService)
	processor := &mockSlackEventProcessor{}
	server.RegisterEventProcessor(processor)

	response := make(chan int, 1)
	go func() { response <- postSlackEvent(server, "past deadline").Code }()
	select {
	case <-slackService.started:
	case <-time.After(time.Second):
		t.Fatal("request did not enter verification")
	}
	<-server.closeDispatchQueues()
	close(slackService.release)
	if got := <-response; got != http.StatusServiceUnavailable {
		t.Fatalf("request after dispatch cutoff status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if processor.processEventCalled.Load() {
		t.Error("request after dispatch cutoff was unexpectedly dispatched")
	}
}

func TestServer_SaturatedProcessorDoesNotDelayAcknowledgement(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	server := NewServer(zap.New(core), Config{SlackEventPath: "/slack/events"}, acceptingSlackService{})
	processor := &blockingEventProcessor{started: make(chan struct{}, 1), release: make(chan struct{})}
	server.RegisterEventProcessor(processor)
	defer func() {
		close(processor.release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	if got := postSlackEvent(server, "first").Code; got != http.StatusOK {
		t.Fatalf("first event status = %d, want %d", got, http.StatusOK)
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not receive first event")
	}
	for range SlackEventQueueCapacity {
		server.dispatchSlackEvent(slackevents.EventsAPIEvent{})
	}

	response := make(chan int, 1)
	go func() { response <- postSlackEvent(server, "sensitive message").Code }()
	select {
	case got := <-response:
		if got != http.StatusOK {
			t.Errorf("saturated event status = %d, want %d", got, http.StatusOK)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("webhook acknowledgement waited for the saturated processor")
	}

	entries := logs.FilterMessage("Slack event processor queue full; dropping event").All()
	if len(entries) != 1 {
		t.Fatalf("queue overflow log count = %d, want 1", len(entries))
	}
	if strings.Contains(entries[0].Message+fmt.Sprint(entries[0].ContextMap()), "sensitive message") {
		t.Error("queue overflow log leaked message content")
	}
}

func TestServer_StalledProcessorDoesNotDelayAnother(t *testing.T) {
	server := NewServer(zaptest.NewLogger(t), Config{}, acceptingSlackService{})
	stalled := &blockingEventProcessor{started: make(chan struct{}, 1), release: make(chan struct{})}
	healthy := &healthyEventProcessor{processed: make(chan struct{}, 1)}
	server.RegisterEventProcessor(stalled)
	server.RegisterEventProcessor(healthy)

	server.dispatchSlackEvent(slackevents.EventsAPIEvent{})
	select {
	case <-stalled.started:
	case <-time.After(time.Second):
		t.Fatal("stalled processor did not receive event")
	}
	select {
	case <-healthy.processed:
	case <-time.After(time.Second):
		t.Fatal("healthy processor waited for stalled processor")
	}

	close(stalled.release)
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestServer_FailingProcessorDoesNotStopDispatch(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	server := NewServer(zap.New(core), Config{}, acceptingSlackService{})
	failed := &failingEventProcessor{}
	healthy := &healthyEventProcessor{processed: make(chan struct{}, 2)}
	server.RegisterEventProcessor(failed)
	server.RegisterEventProcessor(healthy)

	server.dispatchSlackEvent(slackevents.EventsAPIEvent{})
	server.dispatchSlackEvent(slackevents.EventsAPIEvent{})
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if got := failed.calls.Load(); got != 2 {
		t.Errorf("failing processor calls = %d, want 2", got)
	}
	if got := healthy.calls.Load(); got != 2 {
		t.Errorf("healthy processor calls = %d, want 2", got)
	}
	if got := logs.FilterMessage("Slack event processor failed").Len(); got != 2 {
		t.Errorf("processor error log count = %d, want 2", got)
	}
}

func TestServer_PanickingProcessorDoesNotStopDispatch(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	server := NewServer(zap.New(core), Config{}, acceptingSlackService{})
	failed := &panickingEventProcessor{}
	healthy := &healthyEventProcessor{processed: make(chan struct{}, 2)}
	server.RegisterEventProcessor(failed)
	server.RegisterEventProcessor(healthy)

	server.dispatchSlackEvent(slackevents.EventsAPIEvent{})
	server.dispatchSlackEvent(slackevents.EventsAPIEvent{})
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if got := failed.calls.Load(); got != 2 {
		t.Errorf("panicking processor calls = %d, want 2", got)
	}
	if got := healthy.calls.Load(); got != 2 {
		t.Errorf("healthy processor calls = %d, want 2", got)
	}
	if got := logs.FilterMessage("Slack event processor panicked").Len(); got != 2 {
		t.Errorf("panic log count = %d, want 2", got)
	}
}

func TestServer_ConcurrentDeliveryAndShutdownDrain(t *testing.T) {
	server := NewServer(zaptest.NewLogger(t), Config{SlackEventPath: "/slack/events"}, acceptingSlackService{})
	processor := &blockingEventProcessor{started: make(chan struct{}, 1), release: make(chan struct{})}
	server.RegisterEventProcessor(processor)

	const eventCount = 25
	var requests sync.WaitGroup
	requests.Add(eventCount)
	statuses := make(chan int, eventCount)
	for range eventCount {
		go func() {
			defer requests.Done()
			statuses <- postSlackEvent(server, "hello").Code
		}()
	}
	requests.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent event status = %d, want %d", status, http.StatusOK)
		}
	}

	close(processor.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := processor.calls.Load(); got != eventCount {
		t.Errorf("processed events = %d, want %d", got, eventCount)
	}
	if got := processor.maxActive.Load(); got != 1 {
		t.Errorf("maximum processor concurrency = %d, want 1", got)
	}
}

func TestServer_ShutdownWaitsForProcessorInternalQueue(t *testing.T) {
	server := NewServer(zaptest.NewLogger(t), Config{}, acceptingSlackService{})
	processor := &internallyQueuedProcessor{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	server.RegisterEventProcessor(processor)
	server.dispatchSlackEvent(slackevents.EventsAPIEvent{})
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not accept event")
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- server.Shutdown(context.Background()) }()
	select {
	case err := <-shutdown:
		t.Fatalf("Shutdown() returned before internal queue drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(processor.release)
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown() did not finish after internal queue drained")
	}
}

func TestServer_ShutdownReportsUndrainedEvents(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	server := NewServer(zap.New(core), Config{}, acceptingSlackService{})
	processor := &blockingEventProcessor{started: make(chan struct{}, 1), release: make(chan struct{})}
	server.RegisterEventProcessor(processor)
	defer func() {
		close(processor.release)
		<-server.dispatchDone
	}()

	server.dispatchSlackEvent(slackevents.EventsAPIEvent{})
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not receive event")
	}
	server.dispatchSlackEvent(slackevents.EventsAPIEvent{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want deadline exceeded", err)
	}
	entries := logs.FilterMessage("Slack processor events abandoned at shutdown deadline").All()
	if len(entries) != 1 {
		t.Fatalf("abandoned-event log count = %d, want 1", len(entries))
	}
	if got := entries[0].ContextMap()["count"]; got != int64(1) {
		t.Errorf("abandoned-event count = %v, want 1", got)
	}
	inFlight := logs.FilterMessage("Slack processor events still in flight at shutdown deadline").All()
	if len(inFlight) != 1 {
		t.Fatalf("in-flight event log count = %d, want 1", len(inFlight))
	}
	if got := inFlight[0].ContextMap()["count"]; got != int64(1) {
		t.Errorf("in-flight event count = %v, want 1", got)
	}
}

func TestServer_RunAfterShutdownStartsDoesNotListen(t *testing.T) {
	for _, shutdown := range []struct {
		name string
		call func(*Server) error
	}{
		{name: "begin shutdown", call: func(server *Server) error {
			return server.BeginShutdown(context.Background())
		}},
		{name: "shutdown", call: func(server *Server) error {
			return server.Shutdown(context.Background())
		}},
	} {
		t.Run(shutdown.name, func(t *testing.T) {
			server := NewServer(zap.NewNop(), Config{}, &mockSlackService{})
			var listenCalls atomic.Int32
			server.listen = func(string, string) (net.Listener, error) {
				listenCalls.Add(1)
				return nil, errors.New("unexpected listen")
			}
			if err := shutdown.call(server); err != nil {
				t.Fatalf("shutdown error = %v", err)
			}

			if err := server.Run(context.Background()); err == nil {
				t.Fatal("Run() error = nil, want shutdown-started error")
			}
			if got := listenCalls.Load(); got != 0 {
				t.Fatalf("listen calls = %d, want 0", got)
			}
		})
	}
}

func TestServer_BeginShutdown(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		ServerPort:     8080,
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{}
	server := NewServer(logger, config, mockSlack)

	// Initially not shutting down
	if server.isShuttingDown.Load() {
		t.Error("Server should not be shutting down initially")
	}

	ctx := context.Background()
	err := server.BeginShutdown(ctx)
	if err != nil {
		t.Errorf("BeginShutdown() error = %v, want nil", err)
	}

	// Should be shutting down now
	if !server.isShuttingDown.Load() {
		t.Error("Server should be shutting down after BeginShutdown()")
	}
}

func TestServer_LifecycleReadinessWithoutOptionalProcessors(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := NewServer(zaptest.NewLogger(t), Config{SlackEventPath: "/slack/events"}, &mockSlackService{})
	server.listen = func(_, _ string) (net.Listener, error) { return listener, nil }

	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Run(context.Background())
	}()

	readyURL := "http://" + listener.Addr().String() + "/ready"
	var response *http.Response
	for range 100 {
		response, err = http.Get(readyURL) //nolint:gosec // Test server uses a loopback address.
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatalf("request ready endpoint: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ready status after startup = %d, want %d", response.StatusCode, http.StatusOK)
	}

	if err := server.BeginShutdown(context.Background()); err != nil {
		t.Fatalf("BeginShutdown() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/ready", nil)
	recorder := httptest.NewRecorder()
	server.serveMux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("ready status during shutdown = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}

	select {
	case err := <-errChan:
		if err != nil {
			t.Errorf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Error("Server did not shut down within timeout")
	}
}

func TestServer_StartupFailureStaysUnready(t *testing.T) {
	server := NewServer(zaptest.NewLogger(t), Config{}, &mockSlackService{})
	server.listen = func(_, _ string) (net.Listener, error) {
		return nil, errors.New("dependency unavailable")
	}

	if err := server.Run(context.Background()); err == nil {
		t.Fatal("Run() error = nil, want listener failure")
	}
	if server.isReady.Load() {
		t.Error("server became ready after startup dependency failed")
	}
}

func TestServer_DefaultConfig(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{} // Empty config
	mockSlack := &mockSlackService{}
	server := NewServer(logger, config, mockSlack)

	// Should use defaults
	if server.config.ServerPort != 0 {
		t.Errorf("Empty config ServerPort = %v, want 0", server.config.ServerPort)
	}
}

func BenchmarkServer_HealthEndpoint(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := Config{
		ServerPort:     8080,
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{}
	server := NewServer(logger, config, mockSlack)
	server.isReady.Store(true)

	req := httptest.NewRequest("GET", "/ready", nil)

	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		server.serveMux.ServeHTTP(w, req)
	}
}

func BenchmarkServer_SlackEvents(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := Config{
		ServerPort:     8080,
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{}
	server := NewServer(logger, config, mockSlack)

	eventBody := `{"type": "event_callback", "event": {"type": "message", "text": "hello"}}`
	req := httptest.NewRequest("POST", "/api/slack/events", bytes.NewBufferString(eventBody))
	req.Header.Set("Content-Type", "application/json")

	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		server.serveMux.ServeHTTP(w, req)
	}
}
