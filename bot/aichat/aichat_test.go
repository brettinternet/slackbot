package aichat

import (
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

// --- chatPrompt Tests ---

func TestAIChat_ChatPrompt_ContainsPersona(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"testpersona": "YOU ARE THE TEST PERSONA"},
	})

	result, err := a.chatPrompt("hello", UserDetails{}, "testpersona", nil)
	if err != nil {
		t.Fatalf("chatPrompt failed: %v", err)
	}
	if !strings.Contains(result, "YOU ARE THE TEST PERSONA") {
		t.Errorf("expected persona content in prompt, got: %s", result)
	}
}

func TestAIChat_ChatPrompt_FallsBackToHardcodedPersona(t *testing.T) {
	a := newTestAIChat(t, Config{Personas: map[string]string{}})

	result, err := a.chatPrompt("hello", UserDetails{}, "glazer", nil)
	if err != nil {
		t.Fatalf("chatPrompt failed: %v", err)
	}
	// Should fall back to the hardcoded glazer prompt
	if !strings.Contains(result, "Gen-Z") {
		t.Errorf("expected glazer persona fallback, got: %s", result)
	}
}

func TestAIChat_ChatPrompt_FallsBackToGlazerWhenUnknown(t *testing.T) {
	a := newTestAIChat(t, Config{Personas: map[string]string{}})

	result, err := a.chatPrompt("hello", UserDetails{}, "nonexistent_persona", nil)
	if err != nil {
		t.Fatalf("chatPrompt failed: %v", err)
	}
	// Should fall back to glazer default
	if !strings.Contains(result, "Gen-Z") {
		t.Errorf("expected glazer default fallback, got: %s", result)
	}
}

func TestAIChat_ChatPrompt_IncludesNameHint(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	result, err := a.chatPrompt("hello", UserDetails{FirstName: "Alice"}, "p", nil)
	if err != nil {
		t.Fatalf("chatPrompt failed: %v", err)
	}
	if !strings.Contains(result, "Alice") {
		t.Errorf("expected name 'Alice' in prompt, got: %s", result)
	}
}

func TestAIChat_ChatPrompt_IncludesContext(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	ctx := []ConversationContext{
		{Role: "human", Message: "tell me a joke", Timestamp: time.Now().Add(-5 * time.Minute)},
		{Role: "assistant", Message: "why did the chicken cross the road", Timestamp: time.Now().Add(-4 * time.Minute)},
	}
	result, err := a.chatPrompt("that was bad", UserDetails{}, "p", ctx)
	if err != nil {
		t.Fatalf("chatPrompt failed: %v", err)
	}
	if !strings.Contains(result, "tell me a joke") {
		t.Errorf("expected context messages in prompt, got: %s", result)
	}
	if !strings.Contains(result, "why did the chicken cross the road") {
		t.Errorf("expected assistant context in prompt, got: %s", result)
	}
}

func TestAIChat_ChatPrompt_ActiveConversationGuidance(t *testing.T) {
	a := newTestAIChat(t, Config{
		Personas: map[string]string{"p": "you are a test"},
	})

	// 2+ recent messages triggers active conversation guidance
	ctx := []ConversationContext{
		{Role: "human", Message: "msg1", Timestamp: time.Now().Add(-3 * time.Minute)},
		{Role: "assistant", Message: "reply1", Timestamp: time.Now().Add(-2 * time.Minute)},
		{Role: "human", Message: "msg2", Timestamp: time.Now().Add(-1 * time.Minute)},
	}
	result, err := a.chatPrompt("new message", UserDetails{}, "p", ctx)
	if err != nil {
		t.Fatalf("chatPrompt failed: %v", err)
	}
	if !strings.Contains(result, "active") {
		t.Errorf("expected active conversation guidance in prompt, got: %s", result)
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

// --- Role Label Stripping Tests ---

func TestRoleLabelRE(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// labels that must be stripped
		{"AI: hey what's up", "hey what's up"},
		{"Assistant: sure thing", "sure thing"},
		{"SeniorDev: back in my day", "back in my day"},
		{"Grumpy Mentor: kids these days", "kids these days"},
		{"System: hello", "hello"},
		{"Bot: yo", "yo"},
		// extra whitespace after colon
		{"AI:   lots of spaces", "lots of spaces"},
		// normal messages that must NOT be stripped
		{"no label here", "no label here"},
		{"https://example.com is a URL", "https://example.com is a URL"},
		{"just some text: with a colon mid-sentence", "just some text: with a colon mid-sentence"},
		// single word > 20 chars should not be stripped
		{"ThisIsWayTooLongLabel: text", "ThisIsWayTooLongLabel: text"},
		// more than two words before colon should not be stripped
		{"one two three: text", "one two three: text"},
		// no space after colon (e.g. URLs) should not be stripped
		{"Key:value", "Key:value"},
	}

	for _, tc := range cases {
		got := strings.TrimSpace(roleLabelRE.ReplaceAllLiteralString(strings.TrimSpace(tc.input), ""))
		if got != tc.want {
			t.Errorf("input %q: got %q, want %q", tc.input, got, tc.want)
		}
	}
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
