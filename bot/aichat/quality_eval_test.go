package aichat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

type qualityFixture struct {
	name        string
	input       string
	prior       []slackContextMessage
	description string
}

type qualityScores struct {
	Funny         int    `json:"funny"`
	Natural       int    `json:"natural"`
	Relevant      int    `json:"relevant"`
	Nonrepetitive int    `json:"nonrepetitive"`
	Reason        string `json:"reason"`
}

// TestResponseQualityEvaluation is an opt-in model evaluation over fixed transcripts.
// Run with AICHAT_RUN_QUALITY_EVAL=1 and OPENAI_API_KEY set. Unlike prompt-shape
// tests, this generates and judges real replies so personality changes have a stable scorecard.
func TestResponseQualityEvaluation(t *testing.T) {
	if os.Getenv("AICHAT_RUN_QUALITY_EVAL") != "1" {
		t.Skip("set AICHAT_RUN_QUALITY_EVAL=1 to run model quality evaluation")
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Fatal("OPENAI_API_KEY is required for quality evaluation")
	}
	model := os.Getenv("AICHAT_EVAL_MODEL")
	if model == "" {
		model = "gpt-4.1-mini"
	}
	llm, err := openai.New(openai.WithToken(apiKey), openai.WithModel(model))
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAIChat(t, Config{Personas: map[string]string{"default": defaultPersonaPrompt}})
	a.ai = llm
	fixtures := []qualityFixture{
		{
			name: "casual-humor", input: "The deploy finished exactly when the meeting started. Coincidence?",
			description: "A light workplace observation where a concise, earned joke is welcome.",
			prior:       []slackContextMessage{{Text: "The deploy has been stuck for an hour", SenderName: "Sam"}},
		},
		{
			name: "technical-help", input: "Why would a Go HTTP handler leak goroutines after clients disconnect?",
			description: "A technical question requiring a useful, grounded answer rather than a bit.",
		},
		{
			name: "emotional-tone", input: "I worked hard on this proposal and the feedback was rough. I'm pretty discouraged.",
			description: "An emotional message requiring a natural, empathetic response without forced humor.",
		},
		{
			name: "avoid-repetition", input: "Any other way to describe this release?",
			description: "The reply must not recycle the prior bot punchline.",
			prior: []slackContextMessage{
				{Text: "How would you describe this release?", SenderName: "Lee"},
				{Text: "It has the stability of a shopping cart with one bad wheel.", IsBot: true, IsSelf: true},
			},
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			messages := a.buildMessages(fixture.input, UserDetails{FirstName: "Alex"}, "default", nil, fixture.prior)
			response, err := llm.GenerateContent(ctx, messages, generationOptions(0.4, 200)...)
			if err != nil {
				t.Fatalf("generate response: %v", err)
			}
			if response == nil || len(response.Choices) == 0 {
				t.Fatal("generator returned no response")
			}
			candidate := strings.TrimSpace(response.Choices[0].Content)
			scores := judgeResponseQuality(t, ctx, llm, fixture, candidate)
			t.Logf("candidate: %s", candidate)
			t.Logf("scores: %+v", scores)
			if scores.Funny < 3 || scores.Natural < 3 || scores.Relevant < 3 || scores.Nonrepetitive < 3 {
				t.Fatalf("response failed quality floor: %+v", scores)
			}
		})
	}
}

func judgeResponseQuality(t *testing.T, ctx context.Context, llm llms.Model, fixture qualityFixture, candidate string) qualityScores {
	t.Helper()
	prior := make([]string, 0, len(fixture.prior))
	for _, message := range fixture.prior {
		prior = append(prior, message.Text)
	}
	prompt := fmt.Sprintf(`Act as a strict response-quality evaluator. Score each dimension from 1 (bad) to 5 (excellent).
Funny means humor is genuinely witty when appropriate, or appropriately restrained when humor would be harmful.
Natural means it sounds like a thoughtful coworker, not a canned assistant.
Relevant means it directly addresses the current message using the transcript where useful.
Nonrepetitive means it does not recycle wording, observations, or punchlines from prior replies.
Return only JSON with integer keys funny, natural, relevant, nonrepetitive and a short string key reason.

Scenario: %s
Prior transcript: %s
Current message: %s
Candidate response: %s`, fixture.description, strings.Join(prior, " | "), fixture.input, candidate)
	response, err := llm.GenerateContent(ctx, []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeHuman, prompt),
	}, llms.WithTemperature(0), llms.WithMaxTokens(180))
	if err != nil {
		t.Fatalf("judge response: %v", err)
	}
	if response == nil || len(response.Choices) == 0 {
		t.Fatal("judge returned no response")
	}
	raw := strings.TrimSpace(response.Choices[0].Content)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	var scores qualityScores
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &scores); err != nil {
		t.Fatalf("decode judge scores %q: %v", raw, err)
	}
	return scores
}
