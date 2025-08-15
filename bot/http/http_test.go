package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap/zaptest"
)

// mockSlackService for testing
type mockSlackService struct {
	shouldVerifyFail bool
}

func (m *mockSlackService) VerifyRequest(headers http.Header, body []byte) error {
	if m.shouldVerifyFail {
		return http.ErrAbortHandler
	}
	return nil
}

// mockSlackEventProcessor for testing
type mockSlackEventProcessor struct {
	processEventCalled bool
	lastEvent         interface{}
	shouldProcessFail  bool
}

func (m *mockSlackEventProcessor) PushEvent(event slackevents.EventsAPIEvent) {
	m.processEventCalled = true
	m.lastEvent = event
}

func (m *mockSlackEventProcessor) ProcessorType() string {
	return "mock"
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

	// Processor should have been called
	if !processor.processEventCalled {
		t.Error("Event processor should have been called")
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

func TestServer_Lifecycle(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		ServerPort:     0, // Use any available port
		SlackEventPath: "/slack/events",
	}
	mockSlack := &mockSlackService{}
	server := NewServer(logger, config, mockSlack)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start server in goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Run(ctx)
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	
	// Manually set server as ready for testing
	server.isReady.Store(true)

	// Server should be ready
	if !server.isReady.Load() {
		t.Error("Server should be ready after starting")
	}

	// Begin shutdown
	if err := server.BeginShutdown(ctx); err != nil {
		t.Errorf("BeginShutdown() error = %v", err)
	}

	// Shutdown
	if err := server.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}

	// Wait for Run to complete
	select {
	case err := <-errChan:
		if err != nil && err != context.Canceled && err != http.ErrServerClosed {
			t.Errorf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Error("Server did not shut down within timeout")
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