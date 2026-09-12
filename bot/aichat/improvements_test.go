package aichat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

func TestContextMemoryIsSharedAcrossPersonas(t *testing.T) {
	storage, err := NewContextStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close() }()
	if err := storage.StoreContext(ConversationContext{UserID: "U1", ChannelID: "C1", PersonaName: "one", Message: "remember me", Role: "human", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	contexts, err := storage.GetRecentContext("U1", "C1", &Config{MaxContextMessages: 10})
	if err != nil || len(contexts) != 1 || contexts[0].Message != "remember me" {
		t.Fatalf("memory should not be partitioned by persona: contexts=%#v err=%v", contexts, err)
	}
}

func TestPersonaIsScopedByConversation(t *testing.T) {
	a := newTestAIChat(t, Config{Personas: map[string]string{"one": "1", "two": "2"}})
	first := a.userPersona("C1")
	if got := a.userPersona("C1"); got != first {
		t.Fatalf("persona changed within channel: %q -> %q", first, got)
	}
	threadScope := conversationScope("C1", "THREAD2")
	second := a.userPersona(threadScope)
	if got := a.stickyPersonas[threadScope].Name; got != second {
		t.Fatalf("missing scoped assignment for thread: %q", got)
	}
}

func TestPersonaAssignmentPersistsAcrossInstances(t *testing.T) {
	storage, err := NewContextStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close() }()
	cfg := Config{Personas: map[string]string{"one": "1", "two": "2"}, StickyDuration: time.Hour}
	firstChat := newTestAIChat(t, cfg)
	firstChat.context = storage
	first := firstChat.userPersona("C1")

	secondChat := newTestAIChat(t, cfg)
	secondChat.context = storage
	if got := secondChat.userPersona("C1"); got != first {
		t.Fatalf("persona changed after restart: %q -> %q", first, got)
	}
}

func TestSelectContextTurnsUsesNewestFirstBudget(t *testing.T) {
	now := time.Now()
	turns := []contextTurn{
		{role: "human", text: "old", timestamp: now.Add(-3 * time.Hour)},
		{role: "human", text: "new", timestamp: now.Add(-time.Minute)},
		{role: "ai", text: "middle", timestamp: now.Add(-time.Hour)},
	}
	selected := selectContextTurns(turns, 2, 100)
	if len(selected) != 2 || selected[0].text != "middle" || selected[1].text != "new" {
		t.Fatalf("expected newest context retained chronologically, got %#v", selected)
	}
	selected = selectContextTurns([]contextTurn{{text: "12345678", timestamp: now}}, 10, 1)
	if len(selected) != 0 {
		t.Fatalf("token budget should exclude oversized context: %#v", selected)
	}
}

func TestGenerationProfileIsDeterministicAndIntentAware(t *testing.T) {
	short := generationProfileForInput("nice")
	shortQuestion := generationProfileForInput("why?")
	technical := generationProfileForInput("How do I fix this API error in my function?")
	emotional := generationProfileForInput("I feel worried about this result")
	if short != generationProfileForInput("nice") {
		t.Fatal("short profile is not deterministic")
	}
	if shortQuestion.MaxTokens <= short.MaxTokens {
		t.Fatalf("questions should not use the short-chat profile: %#v vs %#v", shortQuestion, short)
	}
	if technical.MaxTokens <= short.MaxTokens || technical.Temperature >= short.Temperature {
		t.Fatalf("technical profile should be substantive and measured: %#v vs %#v", technical, short)
	}
	if emotional.MaxTokens <= short.MaxTokens {
		t.Fatalf("emotional profile should allow an appropriate response: %#v", emotional)
	}
}

func TestBuildMessagesThirdPartyBotIsHumanAndLabeled(t *testing.T) {
	a := newTestAIChat(t, Config{Personas: map[string]string{"p": "test"}})
	messages := a.buildMessages("current", UserDetails{}, "p", nil, []slackContextMessage{
		{Text: "integration update", IsBot: true, SenderID: "UBOTOTHER", SenderName: "Build Bot"},
		{Text: "our reply", IsBot: true, IsSelf: true, SenderID: "UBOTID"},
	})
	thirdParty, own := false, false
	for _, message := range messages[1:] {
		content := fmt.Sprint(message.Parts)
		if strings.Contains(content, "[Build Bot]") {
			thirdParty = message.Role == "human"
		}
		if strings.Contains(content, "our reply") {
			own = message.Role == "ai"
		}
	}
	if !thirdParty || !own {
		t.Fatalf("role mapping was not specific to this bot: %#v", messages)
	}
}

func TestBuildMessagesPromptIsGroundedAndAdaptive(t *testing.T) {
	a := newTestAIChat(t, Config{})
	messages := a.buildMessages("how do I fix this?", UserDetails{}, "default", nil, nil)
	prompt := fmt.Sprint(messages[0].Parts)
	for _, phrase := range []string{"relevant and grounded", "technical", "emotional", "humor only when it is earned", "Adapt response length"} {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("prompt missing %q: %s", phrase, prompt)
		}
	}
}

func TestIsShortAcknowledgement(t *testing.T) {
	for _, input := range []string{"lol", "Nice!", "thanks", "wow"} {
		if !isShortAcknowledgement(input) {
			t.Errorf("expected acknowledgement: %q", input)
		}
	}
	if isShortAcknowledgement("thanks for the detailed explanation") {
		t.Error("long acknowledgement should not be reacted to")
	}
}

func TestRateLimitIsPerChannel(t *testing.T) {
	a := newTestAIChat(t, Config{})
	for i := 0; i < 5; i++ {
		if !a.allowChannelEvent("C1") {
			t.Fatalf("event %d should be accepted in C1", i)
		}
	}
	if a.allowChannelEvent("C1") {
		t.Fatal("channel-specific burst limit should apply to C1")
	}
	if !a.allowChannelEvent("C2") {
		t.Fatal("C2 should not be exhausted by C1 traffic")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	a := newTestAIChat(t, Config{})
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.isConnected.Load() {
		t.Fatal("stopped AIChat remains connected")
	}
}

func TestStoredMemoryFallbackUsesConversationScope(t *testing.T) {
	storage, err := NewContextStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close() }()
	channelScope := conversationScope("C1", "")
	threadScope := conversationScope("C1", "123.456")
	for scope, message := range map[string]string{channelScope: "channel memory", threadScope: "thread memory"} {
		if err := storage.StoreContext(ConversationContext{
			UserID: "U1", ChannelID: scope, PersonaName: "default",
			Message: message, Role: "human", Timestamp: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	contexts, err := storage.GetRecentContext("U1", threadScope, &Config{MaxContextMessages: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !shouldLoadStoredContext(0) || len(contexts) != 1 || contexts[0].Message != "thread memory" {
		t.Fatalf("thread fallback crossed conversation scopes: %#v", contexts)
	}
	if shouldLoadStoredContext(1) {
		t.Fatal("stored fallback should not accompany live context")
	}
}

func TestStopCancelsGenerationAndDiscardsQueuedEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users.info":
			_, _ = fmt.Fprint(w, `{"ok":true,"user":{"id":"U1","name":"alex","profile":{"real_name":"Alex"}}}`)
		case "/conversations.history":
			_, _ = fmt.Fprint(w, `{"ok":true,"messages":[],"has_more":false}`)
		default:
			_, _ = fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer server.Close()

	generator := &blockingAI{started: make(chan struct{})}
	a := newTestAIChat(t, Config{})
	a.ai = generator
	a.slack = &mockSlack{botUserID: "UBOT", client: slack.New("token", slack.OptionAPIURL(server.URL+"/"))}
	a.userNames = newUserNameResolver("", a.slack.Client(), a.log)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.AppMentionEvent{
			User: "U1", Channel: "C1", Text: "<@UBOT> hello", TimeStamp: "1.0",
		}},
	}
	a.PushEvent(event)
	select {
	case <-generator.started:
	case <-time.After(time.Second):
		t.Fatal("generation did not start")
	}
	a.PushEvent(event)
	stopCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Stop(stopCtx); err != nil {
		t.Fatalf("stop did not honor cancellation: %v", err)
	}
	if got := a.Metrics().QueueDepth; got != 0 {
		t.Fatalf("queue depth after stop = %d", got)
	}
}

func TestConversationShardKeepsConversationTogether(t *testing.T) {
	event := func(channel, thread string) slackevents.EventsAPIEvent {
		return slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{Channel: channel, ThreadTimeStamp: thread},
		}}
	}
	first := conversationShard(event("C1", "T1"), workerCount)
	if got := conversationShard(event("C1", "T1"), workerCount); got != first {
		t.Fatalf("same conversation moved shards: %d -> %d", first, got)
	}
	mention := slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
		Data: &slackevents.AppMentionEvent{Channel: "C1", ThreadTimeStamp: "T1"},
	}}
	if got := conversationShard(mention, workerCount); got != first {
		t.Fatalf("message and mention for same thread use different shards: %d != %d", got, first)
	}
}

func TestFetchThreadContextPaginatesAndFiltersTrigger(t *testing.T) {
	var mutex sync.Mutex
	cursors := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		cursor := r.Form.Get("cursor")
		mutex.Lock()
		cursors = append(cursors, cursor)
		mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if cursor == "" {
			_, _ = fmt.Fprint(w, `{"ok":true,"messages":[{"text":"trigger","user":"U1","ts":"30.000001"},{"text":"old","user":"U1","ts":"10.000001"}],"has_more":true,"response_metadata":{"next_cursor":"page-2"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"messages":[{"text":"middle","user":"U2","ts":"20.000001"}],"has_more":false,"response_metadata":{"next_cursor":""}}`)
	}))
	defer server.Close()

	a := newTestAIChat(t, Config{})
	a.slack = &mockSlack{botUserID: "UBOT", client: slack.New("token", slack.OptionAPIURL(server.URL+"/"))}
	messages := a.fetchThreadContext(context.Background(), "C1", "10.000001", "30.000001")
	if len(messages) != 2 || messages[0].Text != "old" || messages[1].Text != "middle" {
		t.Fatalf("unexpected paginated context: %#v", messages)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if fmt.Sprint(cursors) != "[ page-2]" {
		t.Fatalf("pagination cursors = %v", cursors)
	}
}

func TestSlackReactionAndFallbackRequests(t *testing.T) {
	var mutex sync.Mutex
	forms := make(map[string]url.Values)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		formCopy := make(url.Values, len(r.Form))
		for key, values := range r.Form {
			formCopy[key] = append([]string(nil), values...)
		}
		mutex.Lock()
		forms[r.URL.Path] = formCopy
		mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			_, _ = fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"2.0","message":{"text":"ok"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	a := newTestAIChat(t, Config{})
	a.slack = &mockSlack{botUserID: "UBOT", client: slack.New("token", slack.OptionAPIURL(server.URL+"/"))}
	message := eventMessage{Channel: "C1", TimeStamp: "2.0", ThreadTimeStamp: "1.0"}
	a.reactToAcknowledgement(context.Background(), message, "tada")
	a.postFallback(context.Background(), message)

	mutex.Lock()
	defer mutex.Unlock()
	reaction := forms["/reactions.add"]
	if reaction.Get("name") != "tada" || reaction.Get("channel") != "C1" || reaction.Get("timestamp") != "2.0" {
		t.Fatalf("reaction request = %v", reaction)
	}
	fallback := forms["/chat.postMessage"]
	if fallback.Get("text") != fallbackResponse || fallback.Get("thread_ts") != "1.0" {
		t.Fatalf("fallback request = %v", fallback)
	}
}

func TestUserNameResolverCachesAPIResults(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"user":{"id":"U1","name":"ada","profile":{"real_name":"Ada Lovelace"}}}`)
	}))
	defer server.Close()
	resolver := newUserNameResolver("", slack.New("token", slack.OptionAPIURL(server.URL+"/")), newTestAIChat(t, Config{}).log)
	for range 2 {
		if got := resolver.resolve(context.Background(), "U1"); got != "Ada" {
			t.Fatalf("resolved name = %q", got)
		}
	}
	if calls != 1 {
		t.Fatalf("users.info calls = %d, want 1", calls)
	}
}

func TestQueueMetricsTrackDepthAndDrops(t *testing.T) {
	a := newTestAIChat(t, Config{})
	a.isConnected.Store(true)
	for i := 0; i < eventChannelSize+1; i++ {
		a.PushEvent(slackevents.EventsAPIEvent{})
	}
	metrics := a.Metrics()
	if metrics.QueueDepth != eventChannelSize || metrics.QueueDrops != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestAcknowledgementReactionsVary(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		seen[acknowledgementReaction()] = true
	}
	if len(seen) < 2 {
		t.Fatalf("reaction selection did not vary: %v", seen)
	}
}
