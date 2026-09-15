package aichat

import (
	"strings"
	"testing"
	"time"
)

func TestTokenCounterUsesConfiguredModelForUnicode(t *testing.T) {
	counter := newTokenCounter("gpt-4.1-mini")
	if counter.encoding == nil {
		t.Fatal("expected a tokenizer for gpt-4.1-mini")
	}
	if got := counter.messageTokens("こんにちは"); got != 4 {
		t.Fatalf("messageTokens(Unicode) = %d, want 4", got)
	}

	long := counter.messageTokens(strings.Repeat("token ", 100))
	if long <= counter.messageTokens("token") {
		t.Fatalf("long message token count = %d, want more than short message", long)
	}
}

func TestTokenCounterUnsupportedModelUsesConservativeFallback(t *testing.T) {
	counter := newTokenCounter("unsupported-test-model")
	if counter.encoding != nil {
		t.Fatal("unsupported model unexpectedly has an encoding")
	}
	if got := counter.messageTokens("a界🙂"); got != 11 {
		t.Fatalf("fallback messageTokens() = %d, want 11", got)
	}
}

func TestSelectContextTurnsHonorsExactTokenBoundary(t *testing.T) {
	turn := contextTurn{role: "human", text: "hello", timestamp: time.Now()}

	counter := newTokenCounter("gpt-4.1-mini")
	if got := selectContextTurns([]contextTurn{turn}, 1, 4, counter); len(got) != 1 {
		t.Fatalf("exact token budget excluded message: %#v", got)
	}
	if got := selectContextTurns([]contextTurn{turn}, 1, 3, counter); len(got) != 0 {
		t.Fatalf("undersized token budget included message: %#v", got)
	}
}
