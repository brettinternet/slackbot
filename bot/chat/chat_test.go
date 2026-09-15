package chat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

type reactionCall struct {
	reaction  string
	channel   string
	timestamp string
}

type postCall struct {
	channel         string
	message         string
	threadTimestamp string
}

type mockSlackService struct {
	mu          sync.Mutex
	reactions   []reactionCall
	posts       []postCall
	reactionErr error
	postErr     error
	postCh      chan postCall
}

func (m *mockSlackService) AddReaction(
	_ context.Context, reaction, channel, timestamp string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reactions = append(m.reactions, reactionCall{reaction, channel, timestamp})
	return m.reactionErr
}

func (m *mockSlackService) PostMessage(
	_ context.Context, channel, message, threadTimestamp string,
) error {
	call := postCall{channel, message, threadTimestamp}
	m.mu.Lock()
	m.posts = append(m.posts, call)
	m.mu.Unlock()
	if m.postCh != nil {
		select {
		case m.postCh <- call:
		default:
		}
	}
	return m.postErr
}

func (m *mockSlackService) calls() ([]reactionCall, []postCall) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]reactionCall(nil), m.reactions...), append([]postCall(nil), m.posts...)
}

func newTestChat(t *testing.T, cfg Config, service *mockSlackService) *Chat {
	t.Helper()
	chat, err := NewChat(zaptest.NewLogger(t), cfg, service)
	require.NoError(t, err)
	return chat
}

func messageEvent(user, text, timestamp string) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
			Data: &slackevents.MessageEvent{
				User:      user,
				Channel:   "channel1",
				Text:      text,
				TimeStamp: timestamp,
			},
		},
	}
}

func TestNewChatRejectsInvalidRegexp(t *testing.T) {
	_, err := NewChat(zaptest.NewLogger(t), Config{Responses: []Response{{
		Pattern:  "[",
		IsRegexp: true,
	}}}, &mockSlackService{})
	require.ErrorContains(t, err, `compile chat pattern "["`)
}

func TestChatMatchesAndPerformsConfiguredActions(t *testing.T) {
	service := &mockSlackService{}
	chat := newTestChat(t, Config{Responses: []Response{
		{
			Pattern:   "hello|hi",
			Message:   "first reply",
			Reactions: []string{"wave"},
			IsRegexp:  true,
		},
		{
			Pattern:   "HI",
			Message:   "second reply",
			Reactions: []string{"+1"},
		},
	}}, service)

	event := messageEvent("user1", "hi", "123.456")
	event.InnerEvent.Data.(*slackevents.MessageEvent).ThreadTimeStamp = "100.000"
	chat.processEvent(context.Background(), event)

	reactions, posts := service.calls()
	assert.Equal(t, []reactionCall{
		{"wave", "channel1", "123.456"},
		{"+1", "channel1", "123.456"},
	}, reactions)
	assert.Equal(t, []postCall{{"channel1", "first reply", "100.000"}}, posts)
}

func TestChatHandlesAppMention(t *testing.T) {
	service := &mockSlackService{}
	chat := newTestChat(t, Config{Responses: []Response{{Pattern: "hello", Message: "hi"}}}, service)

	chat.processEvent(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.AppMention),
			Data: &slackevents.AppMentionEvent{
				User:            "user1",
				Channel:         "channel1",
				Text:            "<@BOT123> hello",
				TimeStamp:       "123.456",
				ThreadTimeStamp: "100.000",
			},
		},
	})

	_, posts := service.calls()
	assert.Equal(t, []postCall{{"channel1", "hi", "100.000"}}, posts)
}

func TestChatStaticAndRandomMessages(t *testing.T) {
	service := &mockSlackService{}
	chat := newTestChat(t, Config{Responses: []Response{{
		Pattern:        "hello",
		Message:        "fixed",
		RandomMessages: []string{"one", "two"},
	}}}, service)

	chat.processEvent(context.Background(), messageEvent("user1", "hello", "1"))

	_, posts := service.calls()
	require.Len(t, posts, 2)
	assert.Equal(t, "fixed", posts[0].message)
	assert.Contains(t, []string{"one", "two"}, posts[1].message)
}

func TestChatSupportsRandomOnlyAndDeprecatedMessage(t *testing.T) {
	tests := []struct {
		name     string
		response Response
		want     []string
	}{
		{
			name:     "random only",
			response: Response{Pattern: "hello", RandomMessages: []string{"random"}},
			want:     []string{"random"},
		},
		{
			name:     "deprecated messages fallback",
			response: Response{Pattern: "hello", Messages: "legacy"},
			want:     []string{"legacy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &mockSlackService{}
			chat := newTestChat(t, Config{Responses: []Response{tt.response}}, service)
			chat.processEvent(context.Background(), messageEvent("user1", "hello", "1"))

			_, posts := service.calls()
			messages := make([]string, 0, len(posts))
			for _, post := range posts {
				messages = append(messages, post.message)
			}
			assert.Equal(t, tt.want, messages)
		})
	}
}

func TestChatDeduplicatesMessages(t *testing.T) {
	service := &mockSlackService{}
	chat := newTestChat(t, Config{Responses: []Response{{Pattern: "hello", Message: "hi"}}}, service)
	event := messageEvent("user1", "hello", "123.456")

	chat.processEvent(context.Background(), event)
	chat.processEvent(context.Background(), event)

	_, posts := service.calls()
	assert.Len(t, posts, 1)
}

func TestChatIgnoresBotsAndMessageSubtypes(t *testing.T) {
	service := &mockSlackService{}
	chat := newTestChat(t, Config{Responses: []Response{{Pattern: "hello", Message: "hi"}}}, service)

	botEvent := messageEvent("user1", "hello", "1")
	botEvent.InnerEvent.Data.(*slackevents.MessageEvent).BotID = "bot1"
	chat.processEvent(context.Background(), botEvent)

	editedEvent := messageEvent("user1", "hello", "2")
	editedEvent.InnerEvent.Data.(*slackevents.MessageEvent).SubType = "message_changed"
	chat.processEvent(context.Background(), editedEvent)

	_, posts := service.calls()
	assert.Empty(t, posts)
}

func TestChatSetConfigIsAtomic(t *testing.T) {
	service := &mockSlackService{}
	chat := newTestChat(t, Config{Responses: []Response{{Pattern: "old", Message: "old"}}}, service)

	err := chat.SetConfig(Config{Responses: []Response{{Pattern: "[", IsRegexp: true}}})
	require.Error(t, err)
	chat.processEvent(context.Background(), messageEvent("user1", "old", "1"))

	_, posts := service.calls()
	assert.Equal(t, []postCall{{"channel1", "old", ""}}, posts)
}

func TestChatConfigUpdatesAreRaceSafe(t *testing.T) {
	service := &mockSlackService{}
	chat := newTestChat(t, Config{Responses: []Response{{Pattern: "hello", Message: "hi"}}}, service)

	var wg sync.WaitGroup
	configErrors := make(chan error, 100)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			configErrors <- chat.SetConfig(Config{Responses: []Response{{
				Pattern: "hello", Message: "hi",
			}}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 100 {
			chat.processEvent(context.Background(), messageEvent("user1", "hello", string(rune(i+1))))
		}
	}()
	wg.Wait()
	close(configErrors)
	for err := range configErrors {
		assert.NoError(t, err)
	}
}

func TestChatCanRestartAndStopConcurrently(t *testing.T) {
	service := &mockSlackService{postCh: make(chan postCall, 1)}
	chat := newTestChat(t, Config{Responses: []Response{{Pattern: "hello", Message: "hi"}}}, service)

	require.NoError(t, chat.Start(context.Background()))
	require.NoError(t, chat.Stop(context.Background()))
	require.NoError(t, chat.Start(context.Background()))
	require.NoError(t, chat.PushEvent(messageEvent("user1", "hello", "1")))

	select {
	case <-service.postCh:
	case <-time.After(time.Second):
		t.Fatal("restarted chat did not process an event")
	}

	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errorsCh <- chat.Stop(context.Background())
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		assert.NoError(t, err)
	}
}

func TestChatStopsWhenRunContextIsCanceled(t *testing.T) {
	service := &mockSlackService{}
	chat := newTestChat(t, Config{}, service)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, chat.Start(ctx))
	cancel()
	require.Eventually(t, func() bool { return !chat.isConnected.Load() }, time.Second, time.Millisecond)
	require.NoError(t, chat.Stop(context.Background()))
}
