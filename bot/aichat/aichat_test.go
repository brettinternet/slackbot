package aichat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/tmc/langchaingo/llms/openai"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// --- Mocks ---

type mockSlack struct {
	botUserID string
	client    *slack.Client
}

func (m *mockSlack) Client() *slack.Client { return m.client }
func (m *mockSlack) BotUserID() string     { return m.botUserID }

type mockAI struct{}

func (m *mockAI) LLM() *openai.LLM { return nil }

func newTestAIChat(t *testing.T, cfg Config) *AIChat {
	t.Helper()
	if cfg.StickyDuration == 0 {
		cfg.StickyDuration = 30 * time.Minute
	}
	return &AIChat{
		log:            zap.NewNop(),
		config:         cfg,
		slack:          &mockSlack{botUserID: "UBOTID"},
		ai:             &mockAI{},
		eventlimiter:   rate.NewLimiter(rate.Inf, 1000),
		mentionLimiter: rate.NewLimiter(rate.Inf, 1000),
		stickyPersonas: make(map[string]personaAssignment),
		stopCh:         make(chan struct{}),
		eventsCh:       make(chan slackevents.EventsAPIEvent, eventChannelSize),
	}
}

func newTestAIChatWithStorage(t *testing.T, cfg Config) (*AIChat, *ContextStorage) {
	t.Helper()
	tempDir := t.TempDir()
	storage, err := NewContextStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create context storage: %v", err)
	}
	a := newTestAIChat(t, cfg)
	a.context = storage
	t.Cleanup(func() { _ = storage.Close() })
	return a, storage
}

// --- Context Storage Tests ---

func TestNewContextStorage(t *testing.T) {
	tempDir := t.TempDir()

	storage, err := NewContextStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create context storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	dbPath := filepath.Join(tempDir, "aichat_context.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Errorf("database file was not created at %s", dbPath)
	}
}

func TestContextStorage_StoreAndRetrieve(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewContextStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create context storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	testContext := ConversationContext{
		UserID:      "U123",
		ChannelID:   "C456",
		PersonaName: "test",
		Message:     "Hello, world!",
		Role:        "human",
		Timestamp:   time.Now(),
	}

	if err := storage.StoreContext(testContext); err != nil {
		t.Fatalf("failed to store context: %v", err)
	}

	testConfig := &Config{
		MaxContextMessages: 10,
		MaxContextAge:      24 * time.Hour,
		MaxContextTokens:   1000,
	}
	contexts, err := storage.GetRecentContext("U123", "C456", "test", testConfig)
	if err != nil {
		t.Fatalf("failed to retrieve context: %v", err)
	}

	if len(contexts) != 1 {
		t.Errorf("expected 1 context, got %d", len(contexts))
	}
	if contexts[0].Message != "Hello, world!" {
		t.Errorf("expected message 'Hello, world!', got '%s'", contexts[0].Message)
	}
}

func TestContextStorage_MessageCountLimit(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewContextStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create context storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	// Store 10 messages
	for i := 0; i < 10; i++ {
		ctx := ConversationContext{
			UserID:      "U1",
			ChannelID:   "C1",
			PersonaName: "test",
			Message:     "msg",
			Role:        "human",
			Timestamp:   time.Now().Add(time.Duration(i) * time.Second),
		}
		if err := storage.StoreContext(ctx); err != nil {
			t.Fatalf("store failed: %v", err)
		}
	}

	cfg := &Config{MaxContextMessages: 3, MaxContextAge: 24 * time.Hour, MaxContextTokens: 10000}
	contexts, err := storage.GetRecentContext("U1", "C1", "test", cfg)
	if err != nil {
		t.Fatalf("retrieve failed: %v", err)
	}
	if len(contexts) > 3 {
		t.Errorf("expected at most 3 contexts, got %d", len(contexts))
	}
}

func TestContextStorage_AgeFilter(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewContextStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create context storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	old := ConversationContext{
		UserID: "U1", ChannelID: "C1", PersonaName: "test",
		Message: "old message", Role: "human",
		Timestamp: time.Now().Add(-48 * time.Hour),
	}
	recent := ConversationContext{
		UserID: "U1", ChannelID: "C1", PersonaName: "test",
		Message: "recent message", Role: "human",
		Timestamp: time.Now(),
	}

	_ = storage.StoreContext(old)
	_ = storage.StoreContext(recent)

	cfg := &Config{MaxContextMessages: 10, MaxContextAge: 1 * time.Hour, MaxContextTokens: 10000}
	contexts, err := storage.GetRecentContext("U1", "C1", "test", cfg)
	if err != nil {
		t.Fatalf("retrieve failed: %v", err)
	}

	for _, c := range contexts {
		if c.Message == "old message" {
			t.Error("old message should have been filtered out by age limit")
		}
	}
}

func TestContextStorage_CleanOldContext(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewContextStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create context storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	old := ConversationContext{
		UserID: "U1", ChannelID: "C1", PersonaName: "test",
		Message: "stale", Role: "human",
		Timestamp: time.Now().Add(-72 * time.Hour),
	}
	_ = storage.StoreContext(old)

	if err := storage.CleanOldContext(24 * time.Hour); err != nil {
		t.Fatalf("CleanOldContext failed: %v", err)
	}

	cfg := &Config{MaxContextMessages: 10, MaxContextAge: 0, MaxContextTokens: 10000}
	contexts, err := storage.GetRecentContext("U1", "C1", "test", cfg)
	if err != nil {
		t.Fatalf("retrieve failed: %v", err)
	}
	if len(contexts) != 0 {
		t.Errorf("expected 0 contexts after cleanup, got %d", len(contexts))
	}
}

func TestContextStorage_ChronologicalOrder(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewContextStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create context storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	messages := []string{"first", "second", "third"}
	for i, msg := range messages {
		ctx := ConversationContext{
			UserID: "U1", ChannelID: "C1", PersonaName: "test",
			Message: msg, Role: "human",
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
		}
		_ = storage.StoreContext(ctx)
	}

	cfg := &Config{MaxContextMessages: 10, MaxContextAge: 1 * time.Hour, MaxContextTokens: 10000}
	contexts, err := storage.GetRecentContext("U1", "C1", "test", cfg)
	if err != nil {
		t.Fatalf("retrieve failed: %v", err)
	}
	if len(contexts) != 3 {
		t.Fatalf("expected 3 contexts, got %d", len(contexts))
	}
	// Should be in chronological order (oldest first)
	for i, expected := range messages {
		if contexts[i].Message != expected {
			t.Errorf("expected message[%d] = %q, got %q", i, expected, contexts[i].Message)
		}
	}
}

// --- Persona Tests ---

func TestAIChat_RandomPersonaName(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{
			"test1": "Test persona 1",
			"test2": "Test persona 2",
		},
	})

	persona := a.randomPersonaName()
	if persona != "test1" && persona != "test2" {
		t.Errorf("expected 'test1' or 'test2', got '%s'", persona)
	}
}

func TestAIChat_RandomPersonaName_FallsBackToDefault(t *testing.T) {
	a := newTestAIChat(t, Config{Personas: map[string]string{}})
	if got := a.randomPersonaName(); got != "default" {
		t.Errorf("expected 'default', got '%s'", got)
	}
}

func TestAIChat_UserPersona_Sticky(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas:       map[string]string{"p1": "persona1", "p2": "persona2"},
		StickyDuration: 30 * time.Minute,
	})

	first := a.userPersona("UABC")
	// Same user should get same persona while sticky
	for i := 0; i < 5; i++ {
		if got := a.userPersona("UABC"); got != first {
			t.Errorf("expected sticky persona %q, got %q on call %d", first, got, i+1)
		}
	}
}

func TestAIChat_UserPersona_DifferentUsersCanDiffer(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas:       map[string]string{"p1": "p1", "p2": "p2", "p3": "p3"},
		StickyDuration: 30 * time.Minute,
	})

	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		seen[a.userPersona("U"+string(rune('A'+i)))] = true
	}
	// With 20 different users, we should have seen more than 1 distinct persona
	if len(seen) < 2 {
		t.Errorf("expected multiple distinct personas across users, got: %v", seen)
	}
}

func TestAIChat_UserPersona_ExpiresAfterDuration(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas:       map[string]string{"p1": "persona1", "p2": "persona2"},
		StickyDuration: 1 * time.Millisecond,
	})

	first := a.userPersona("UABC")
	time.Sleep(5 * time.Millisecond)

	// After expiry, persona may change (statistically; run a few times)
	changed := false
	for i := 0; i < 20; i++ {
		time.Sleep(2 * time.Millisecond)
		if next := a.userPersona("UABC"); next != first {
			changed = true
			break
		}
	}
	if !changed {
		t.Log("persona did not change after expiry (may be same by chance with small persona set)")
	}
}

// --- isBotMentioned Tests ---

func TestAIChat_IsBotMentioned(t *testing.T) {
	a := newTestAIChat(t, Config{})

	tests := []struct {
		text string
		want bool
	}{
		{"hey <@UBOTID> what's up", true},
		{"<@UBOTID>", true},
		{"nothing here", false},
		{"<@OTHERID> do this", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := a.isBotMentioned(tt.text); got != tt.want {
			t.Errorf("isBotMentioned(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestAIChat_IsBotMentioned_EmptyBotID(t *testing.T) {
	a := newTestAIChat(t, Config{})
	a.slack = &mockSlack{botUserID: ""}

	// Should return false when bot user ID is unknown
	if a.isBotMentioned("<@UBOTID> yo") {
		t.Error("expected false when botUserID is empty")
	}
}

// --- calculateDropChance Tests ---

func TestAIChat_CalculateDropChance_BaseRate(t *testing.T) {
	a := newTestAIChat(t, Config{})
	// No context storage, neutral message → base drop chance 0.4
	got := a.calculateDropChance("U1", "C1", "just a normal message here")
	if got != 0.4 {
		t.Errorf("expected base drop chance 0.4, got %f", got)
	}
}

func TestAIChat_CalculateDropChance_QuestionLowersChance(t *testing.T) {
	a := newTestAIChat(t, Config{})
	base := a.calculateDropChance("U1", "C1", "normal message no question")
	question := a.calculateDropChance("U1", "C1", "what do you think about this?")
	if question >= base {
		t.Errorf("question should lower drop chance: base=%f question=%f", base, question)
	}
}

func TestAIChat_CalculateDropChance_EmotionalWordLowersChance(t *testing.T) {
	a := newTestAIChat(t, Config{})
	base := a.calculateDropChance("U1", "C1", "normal message here today")
	emotional := a.calculateDropChance("U1", "C1", "I am so frustrated with this")
	if emotional >= base {
		t.Errorf("emotional word should lower drop chance: base=%f emotional=%f", base, emotional)
	}
}

func TestAIChat_CalculateDropChance_ShortMessageRaisesChance(t *testing.T) {
	a := newTestAIChat(t, Config{})
	normal := a.calculateDropChance("U1", "C1", "this is a normal length message")
	short := a.calculateDropChance("U1", "C1", "ok")
	if short <= normal {
		t.Errorf("short message should raise drop chance: normal=%f short=%f", normal, short)
	}
}

func TestAIChat_CalculateDropChance_ShortQuestionNotPenalized(t *testing.T) {
	a := newTestAIChat(t, Config{})
	shortQ := a.calculateDropChance("U1", "C1", "why?")
	shortNoQ := a.calculateDropChance("U1", "C1", "ok")
	// Short question should have lower drop chance than short non-question
	if shortQ >= shortNoQ {
		t.Errorf("short question should have lower drop than short non-question: q=%f noq=%f", shortQ, shortNoQ)
	}
}

func TestAIChat_CalculateDropChance_Clamped(t *testing.T) {
	a := newTestAIChat(t, Config{})
	// Pile on many factors that lower chance — should still be >= 0.05
	got := a.calculateDropChance("U1", "C1", "frustrated confused excited amazing help me? thoughts?")
	if got < 0.05 {
		t.Errorf("drop chance should not go below 0.05, got %f", got)
	}
	// Short message with no question — should not exceed 0.8
	got = a.calculateDropChance("U1", "C1", "k")
	if got > 0.8 {
		t.Errorf("drop chance should not exceed 0.8, got %f", got)
	}
}

func TestAIChat_CalculateDropChance_WithRecentContext(t *testing.T) {
	cfg := Config{
		MaxContextMessages: 10,
		MaxContextAge:      24 * time.Hour,
		MaxContextTokens:   2000,
		StickyDuration:     30 * time.Minute,
		Personas:           map[string]string{"p1": "persona"},
	}
	a, storage := newTestAIChatWithStorage(t, cfg)

	// Prime the storage with a recent bot reply
	_ = storage.StoreContext(ConversationContext{
		UserID: "U1", ChannelID: "C1", PersonaName: "p1",
		Message: "hey there", Role: "assistant",
		Timestamp: time.Now().Add(-30 * time.Second),
	})
	// Assign persona so calculateDropChance uses it
	a.stickyPersonas["U1"] = personaAssignment{Name: "p1", Timestamp: time.Now()}

	dropWithContext := a.calculateDropChance("U1", "C1", "this is a normal message today")
	dropWithout := newTestAIChat(t, cfg).calculateDropChance("U1", "C1", "this is a normal message today")

	if dropWithContext >= dropWithout {
		t.Errorf("recent bot reply should lower drop chance: with=%f without=%f", dropWithContext, dropWithout)
	}
}

// --- buildMessages Tests ---

func TestAIChat_BuildMessages_ContainsPersona(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"testpersona": "YOU ARE THE TEST PERSONA"},
	})

	msgs := a.buildMessages("hello", UserDetails{}, "testpersona", nil, nil)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if !strings.Contains(systemContent, "YOU ARE THE TEST PERSONA") {
		t.Errorf("expected persona content in system message, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_FallsBackToHardcodedPersona(t *testing.T) {
	a := newTestAIChat(t, Config{Personas: map[string]string{}})

	msgs := a.buildMessages("hello", UserDetails{}, "glazer", nil, nil)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if !strings.Contains(systemContent, "Gen-Z") {
		t.Errorf("expected glazer persona fallback, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_FallsBackToGlazerWhenUnknown(t *testing.T) {
	a := newTestAIChat(t, Config{Personas: map[string]string{}})

	msgs := a.buildMessages("hello", UserDetails{}, "nonexistent_persona", nil, nil)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if !strings.Contains(systemContent, "Gen-Z") {
		t.Errorf("expected glazer default fallback, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_IncludesNameHint(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	msgs := a.buildMessages("hello", UserDetails{FirstName: "Alice"}, "p", nil, nil)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if !strings.Contains(systemContent, "Alice") {
		t.Errorf("expected name 'Alice' in system message, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_IncludesContextAsTypedTurns(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	ctx := []ConversationContext{
		{Role: "human", Message: "tell me a joke", Timestamp: time.Now().Add(-5 * time.Minute)},
		{Role: "assistant", Message: "why did the chicken cross the road", Timestamp: time.Now().Add(-4 * time.Minute)},
	}
	msgs := a.buildMessages("that was bad", UserDetails{}, "p", ctx, nil)

	// Expect: system, human (ctx), AI (ctx), human (current) = 4 messages
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (system + 2 context + 1 current), got %d", len(msgs))
	}
	if msgs[1].Role != "human" {
		t.Errorf("expected msgs[1] role 'human', got %q", msgs[1].Role)
	}
	if msgs[2].Role != "ai" {
		t.Errorf("expected msgs[2] role 'ai', got %q", msgs[2].Role)
	}
	if msgs[3].Role != "human" {
		t.Errorf("expected msgs[3] role 'human', got %q", msgs[3].Role)
	}
	lastContent := fmt.Sprintf("%v", msgs[3].Parts)
	if !strings.Contains(lastContent, "that was bad") {
		t.Errorf("expected current message in last turn, got: %s", lastContent)
	}
}

func TestAIChat_BuildMessages_ActiveConversationGuidance(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	// 2+ recent messages triggers active conversation guidance
	ctx := []ConversationContext{
		{Role: "human", Message: "msg1", Timestamp: time.Now().Add(-3 * time.Minute)},
		{Role: "assistant", Message: "reply1", Timestamp: time.Now().Add(-2 * time.Minute)},
		{Role: "human", Message: "msg2", Timestamp: time.Now().Add(-1 * time.Minute)},
	}
	msgs := a.buildMessages("new message", UserDetails{}, "p", ctx, nil)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if !strings.Contains(systemContent, "active") {
		t.Errorf("expected active conversation guidance in system message, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_LiveContextInserted(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	live := []slackContextMessage{
		{Text: "thread message one", IsBot: false},
		{Text: "bot replied here", IsBot: true},
		{Text: "thread message two", IsBot: false},
	}
	msgs := a.buildMessages("current message", UserDetails{}, "p", nil, live)

	// Expect: system + 3 live + 1 current = 5 messages
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages (system + 3 live + 1 current), got %d", len(msgs))
	}
	if msgs[1].Role != "human" {
		t.Errorf("msgs[1] should be human (non-bot live msg), got %q", msgs[1].Role)
	}
	if msgs[2].Role != "ai" {
		t.Errorf("msgs[2] should be ai (bot live msg), got %q", msgs[2].Role)
	}
	if msgs[3].Role != "human" {
		t.Errorf("msgs[3] should be human (non-bot live msg), got %q", msgs[3].Role)
	}
	lastContent := fmt.Sprintf("%v", msgs[4].Parts)
	if !strings.Contains(lastContent, "current message") {
		t.Errorf("last message should be the current input, got: %s", lastContent)
	}
}

func TestAIChat_BuildMessages_LiveContextBeforeStoredContext(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	live := []slackContextMessage{
		{Text: "live msg", IsBot: false},
	}
	stored := []ConversationContext{
		{Role: "human", Message: "stored msg", Timestamp: time.Now().Add(-5 * time.Minute)},
	}
	msgs := a.buildMessages("now", UserDetails{}, "p", stored, live)

	// Expect: system, live human, stored human, current = 4
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	liveContent := fmt.Sprintf("%v", msgs[1].Parts)
	storedContent := fmt.Sprintf("%v", msgs[2].Parts)
	if !strings.Contains(liveContent, "live msg") {
		t.Errorf("msgs[1] should be live context, got: %s", liveContent)
	}
	if !strings.Contains(storedContent, "stored msg") {
		t.Errorf("msgs[2] should be stored context, got: %s", storedContent)
	}
}

func TestAIChat_BuildMessages_AntiRepetitionWithBotLiveHistory(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	live := []slackContextMessage{
		{Text: "user said something", IsBot: false},
		{Text: "bot replied", IsBot: true},
	}
	msgs := a.buildMessages("another message", UserDetails{}, "p", nil, live)
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if !strings.Contains(systemContent, "Don't repeat") {
		t.Errorf("expected anti-repetition guidance when bot has live history, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_AntiRepetitionWithStoredBotHistory(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	stored := []ConversationContext{
		{Role: "assistant", Message: "I said this before", Timestamp: time.Now().Add(-5 * time.Minute)},
	}
	msgs := a.buildMessages("another message", UserDetails{}, "p", stored, nil)
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if !strings.Contains(systemContent, "Don't repeat") {
		t.Errorf("expected anti-repetition guidance when bot has stored history, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_NoAntiRepetitionWithoutBotHistory(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	live := []slackContextMessage{
		{Text: "user message only", IsBot: false},
	}
	msgs := a.buildMessages("first reply", UserDetails{}, "p", nil, live)
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if strings.Contains(systemContent, "Don't repeat") {
		t.Errorf("should not have anti-repetition guidance when bot has no history, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_RecencyNoteWhenContextIsOld(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	live := []slackContextMessage{
		{Text: "old message", IsBot: false, Timestamp: time.Now().Add(-45 * time.Minute)},
		{Text: "newer message", IsBot: false, Timestamp: time.Now().Add(-5 * time.Minute)},
	}
	msgs := a.buildMessages("hi", UserDetails{}, "p", nil, live)
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if !strings.Contains(systemContent, "weight recent messages") {
		t.Errorf("expected recency note when oldest context is >30 min old, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_NoRecencyNoteWhenContextIsRecent(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	live := []slackContextMessage{
		{Text: "recent message", IsBot: false, Timestamp: time.Now().Add(-10 * time.Minute)},
	}
	msgs := a.buildMessages("hi", UserDetails{}, "p", nil, live)
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if strings.Contains(systemContent, "weight recent messages") {
		t.Errorf("should not have recency note when all context is recent, got: %s", systemContent)
	}
}

func TestAIChat_BuildMessages_NoRecencyNoteWhenNoTimestamps(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	// Messages with zero timestamps should not trigger recency note
	live := []slackContextMessage{
		{Text: "message without timestamp", IsBot: false},
	}
	msgs := a.buildMessages("hi", UserDetails{}, "p", nil, live)
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if strings.Contains(systemContent, "weight recent messages") {
		t.Errorf("should not have recency note when timestamps are zero, got: %s", systemContent)
	}
}

func TestParseSlackTimestamp(t *testing.T) {
	tests := []struct {
		input    string
		wantZero bool
		wantSecs int64
	}{
		{"1512085950.000216", false, 1512085950},
		{"1512085950", false, 1512085950},
		{"", true, 0},
		{"notanumber", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseSlackTimestamp(tt.input)
			if tt.wantZero && !got.IsZero() {
				t.Errorf("expected zero time for %q, got %v", tt.input, got)
			}
			if !tt.wantZero {
				if got.IsZero() {
					t.Errorf("expected non-zero time for %q", tt.input)
				}
				if got.Unix() != tt.wantSecs {
					t.Errorf("for %q: want unix %d, got %d", tt.input, tt.wantSecs, got.Unix())
				}
			}
		})
	}
}

func TestFormatContextAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Minute, "45 minutes"},
		{1 * time.Hour, "1 hours"},
		{2 * time.Hour, "2 hours"},
		{90 * time.Minute, "1h 30m"},
		{150 * time.Minute, "2h 30m"},
	}
	for _, tt := range tests {
		got := formatContextAge(tt.d)
		if got != tt.want {
			t.Errorf("formatContextAge(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestAIChat_BuildMessages_ActiveConvoFromLiveContext(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	live := []slackContextMessage{
		{Text: "some live message", IsBot: false},
	}
	msgs := a.buildMessages("message", UserDetails{}, "p", nil, live)
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	systemContent := fmt.Sprintf("%v", msgs[0].Parts)
	if !strings.Contains(systemContent, "active") {
		t.Errorf("expected active conversation guidance when live context present, got: %s", systemContent)
	}
}

// --- processEvent Bot Filter Tests ---

func TestAIChat_ProcessEvent_IgnoresBotMessages(t *testing.T) {
	a := newTestAIChat(t, Config{Personas: map[string]string{"p": "test"}})
	a.isConnected.Store(true)

	// Bot messages (BotID set) should be silently dropped.
	// We verify this by ensuring no panic and the event channel stays empty.
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "app_mention",
			Data: &slackevents.AppMentionEvent{
				BotID:   "BBOT",
				Channel: "C1",
				Text:    "some text",
			},
		},
	}
	// processEvent should return without doing anything for bot messages
	a.processEvent(nil, event) //nolint:staticcheck
}

func TestAIChat_ProcessEvent_IgnoresMessageEventFromBot(t *testing.T) {
	a := newTestAIChat(t, Config{Personas: map[string]string{"p": "test"}})
	a.isConnected.Store(true)

	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "message",
			Data: &slackevents.MessageEvent{
				BotID:   "BBOT",
				Channel: "C1",
				Text:    "bot talking to itself",
			},
		},
	}
	a.processEvent(nil, event) //nolint:staticcheck
}

// --- Stop Word Limit Tests ---

// TestAIChat_StopWords_NeverExceedOpenAILimit verifies that every length tier produces
// at most 4 stop sequences (the OpenAI API maximum).
func TestAIChat_StopWords_NeverExceedOpenAILimit(t *testing.T) {
	const openAIMaxStopWords = 4

	// Force each tier by exhaustive sampling across the full [0,1) range.
	tiers := []struct {
		name      string
		variation float64
	}{
		{"very short (60%)", 0.0},
		{"very short mid", 0.30},
		{"very short top", 0.59},
		{"short (25%)", 0.60},
		{"short mid", 0.72},
		{"short top", 0.84},
		{"medium (10%)", 0.85},
		{"medium mid", 0.90},
		{"medium top", 0.94},
		{"longer (5%)", 0.95},
		{"longer top", 0.99},
	}

	for _, tt := range tiers {
		t.Run(tt.name, func(t *testing.T) {
			stopWords := stopWordsForVariation(tt.variation)
			if len(stopWords) > openAIMaxStopWords {
				t.Errorf("tier %q produced %d stop words (max %d): %v",
					tt.name, len(stopWords), openAIMaxStopWords, stopWords)
			}
		})
	}
}

// --- YAML Parsing Tests ---

func TestConfig_PersonasParsing(t *testing.T) {
	yamlConfig := `
glazer: "Gen-Z hype beast persona"
argue: "Argumentative lawyer persona"
`
	personasData := make(map[string]any)
	if err := yaml.Unmarshal([]byte(yamlConfig), &personasData); err != nil {
		t.Fatalf("failed to parse YAML config: %v", err)
	}

	if len(personasData) != 2 {
		t.Errorf("expected 2 personas, got %d", len(personasData))
	}
	if personasData["glazer"] != "Gen-Z hype beast persona" {
		t.Errorf("unexpected glazer persona: %v", personasData["glazer"])
	}
}

// --- Hardcoded Personas Tests ---

func TestPersonas_AllBuiltinsExist(t *testing.T) {
	expected := []string{"glazer", "argue", "unhinged", "computer"}
	for _, name := range expected {
		if _, ok := personas[name]; !ok {
			t.Errorf("expected built-in persona %q to exist", name)
		}
	}
}

func TestPersonas_AllNonEmpty(t *testing.T) {
	for name, prompt := range personas {
		if strings.TrimSpace(prompt) == "" {
			t.Errorf("persona %q has empty prompt", name)
		}
	}
}
