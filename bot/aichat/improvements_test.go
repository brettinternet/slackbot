package aichat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
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
	second := a.userPersona("THREAD2")
	if got := a.stickyPersonas["THREAD2"].Name; got != second {
		t.Fatalf("missing scoped assignment for thread: %q", got)
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
