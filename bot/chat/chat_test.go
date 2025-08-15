package chat

import (
	"context"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap/zaptest"
)

// mockSlackService for testing
type mockSlackService struct {
	client *slack.Client
}

func (m *mockSlackService) Client() *slack.Client {
	if m.client == nil {
		m.client = slack.New("test-token")
	}
	return m.client
}

func TestNewChat(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		PreferredUsers: []string{"user1", "user2"},
	}
	mockSlack := &mockSlackService{}

	chat := NewChat(logger, config, mockSlack)

	if chat == nil {
		t.Fatal("NewChat() returned nil")
	}

	if len(chat.config.PreferredUsers) != 2 {
		t.Errorf("NewChat() PreferredUsers length = %v, want 2", len(chat.config.PreferredUsers))
	}

	if chat.isConnected.Load() {
		t.Error("NewChat() should not be connected initially")
	}

	if chat.eventsCh == nil {
		t.Error("NewChat() should initialize eventsCh")
	}
}

func TestChat_Start(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		PreferredUsers: []string{"user1"},
	}
	mockSlack := &mockSlackService{}
	chat := NewChat(logger, config, mockSlack)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := chat.Start(ctx)
	if err != nil {
		t.Errorf("Start() error = %v, want nil", err)
	}

	if !chat.isConnected.Load() {
		t.Error("Start() should set isConnected to true")
	}

	// Stop to clean up
	_ = chat.Stop(ctx)
}

func TestChat_Stop(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{}
	mockSlack := &mockSlackService{}
	chat := NewChat(logger, config, mockSlack)

	ctx := context.Background()

	// Start first
	err := chat.Start(ctx)
	if err != nil {
		t.Errorf("Start() error = %v", err)
	}

	// Now stop
	err = chat.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}

	if chat.isConnected.Load() {
		t.Error("Stop() should set isConnected to false")
	}
}

func TestChat_SetConfig(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		PreferredUsers: []string{"user1"},
	}
	mockSlack := &mockSlackService{}
	chat := NewChat(logger, config, mockSlack)

	newConfig := FileConfig{
		Responses: []Response{
			{
				Pattern:  "hello",
				Message:  "Hi there!",
				IsRegexp: false,
			},
		},
	}

	err := chat.SetConfig(newConfig)
	if err != nil {
		t.Errorf("SetConfig() error = %v, want nil", err)
	}

	if len(chat.fileConfig.Responses) != 1 {
		t.Errorf("SetConfig() responses length = %v, want 1", len(chat.fileConfig.Responses))
	}

	if chat.fileConfig.Responses[0].Pattern != "hello" {
		t.Errorf("SetConfig() response pattern = %v, want 'hello'", chat.fileConfig.Responses[0].Pattern)
	}
}

func TestChat_ProcessSlackEvent_AppMention(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{}
	mockSlack := &mockSlackService{}
	chat := NewChat(logger, config, mockSlack)

	// Set up a simple response
	chat.fileConfig = FileConfig{
		Responses: []Response{
			{
				Pattern:  "hello",
				Message:  "Hi there!",
				IsRegexp: false,
			},
		},
	}

	ctx := context.Background()
	_ = chat.Start(ctx)
	defer func() { _ = chat.Stop(ctx) }()

	// Create an app mention event
	event := &slackevents.AppMentionEvent{
		Type:    "app_mention",
		User:    "user1",
		Text:    "<@bot> hello",
		Channel: "channel1",
		TimeStamp: "1234567890.123",
	}

	// Use PushEvent instead of ProcessSlackEvent
	chat.PushEvent(slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.AppMention),
			Data: event,
		},
	})

	// Give some time for async processing
	time.Sleep(10 * time.Millisecond)
}

func TestChat_PushEvent(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{}
	mockSlack := &mockSlackService{}
	chat := NewChat(logger, config, mockSlack)

	ctx := context.Background()
	_ = chat.Start(ctx)
	defer func() { _ = chat.Stop(ctx) }()

	// Create a test event
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
		},
	}

	// Should not panic or error
	chat.PushEvent(event)

	// Give some time for async processing
	time.Sleep(10 * time.Millisecond)
}


func TestChat_StopBeforeStart(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{}
	mockSlack := &mockSlackService{}
	chat := NewChat(logger, config, mockSlack)

	ctx := context.Background()

	// Stop before start should not error
	err := chat.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() before Start() error = %v, want nil", err)
	}
}

func BenchmarkChat_PushEvent(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := Config{}
	mockSlack := &mockSlackService{}
	chat := NewChat(logger, config, mockSlack)

	ctx := context.Background()
	_ = chat.Start(ctx)
	defer func() { _ = chat.Stop(ctx) }()

	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
		},
	}

	b.ResetTimer()
	for b.Loop() {
		chat.PushEvent(event)
	}
}