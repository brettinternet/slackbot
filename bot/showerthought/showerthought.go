package showerthought

import (
	"context"
	"fmt"
	"strings"
	"time"

	goslack "github.com/slack-go/slack"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
	"go.uber.org/zap"
	"slackbot.arpa/tools/random"
)

type aiService interface {
	LLM() *openai.LLM
}

type slackService interface {
	Client() *goslack.Client
	BotUserID() string
}

type FileConfig struct {
	Enabled            *bool `json:"enabled" yaml:"enabled"`
	BusinessHoursStart *int  `json:"business_hours_start" yaml:"business_hours_start"`
	BusinessHoursEnd   *int  `json:"business_hours_end" yaml:"business_hours_end"`
}

type Config struct {
	Enabled            bool
	NotifyChannel      string
	BusinessHoursStart int // hour in 24h local time (inclusive), default 9
	BusinessHoursEnd   int // hour in 24h local time (exclusive), default 17
}

type ShowerThought struct {
	log    *zap.Logger
	config Config
	slack  slackService
	ai     aiService
	stopCh chan struct{}
}

func New(log *zap.Logger, c Config, s slackService, a aiService) *ShowerThought {
	return &ShowerThought{
		log:    log,
		config: c,
		slack:  s,
		ai:     a,
		stopCh: make(chan struct{}),
	}
}

func (st *ShowerThought) Start(ctx context.Context) error {
	go st.run(ctx)
	return nil
}

func (st *ShowerThought) Stop(_ context.Context) error {
	close(st.stopCh)
	return nil
}

func (st *ShowerThought) run(ctx context.Context) {
	for {
		next := st.nextPostTime()
		st.log.Info("Next shower thought scheduled", zap.Time("at", next))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-st.stopCh:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			st.postShowerThought(ctx)
		}
	}
}

// nextPostTime returns a random time within the configured business hours (Mon–Fri, local time)
// within the next 7 days, at least 1 hour from now.
func (st *ShowerThought) nextPostTime() time.Time {
	now := time.Now()
	start := st.config.BusinessHoursStart
	end := st.config.BusinessHoursEnd

	// Collect all hourly slots within the business-hours window over the next 7 days.
	var candidates []time.Time
	for d := range 7 {
		day := now.AddDate(0, 0, d)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		for h := start; h < end; h++ {
			minute := random.Int(0, 59)
			t := time.Date(day.Year(), day.Month(), day.Day(), h, minute, 0, 0, day.Location())
			if t.After(now.Add(time.Hour)) {
				candidates = append(candidates, t)
			}
		}
	}

	if len(candidates) > 0 {
		return candidates[random.Int(0, len(candidates)-1)]
	}

	// Fallback: find the next weekday and schedule at the start of business hours.
	for d := range 7 {
		t := now.AddDate(0, 0, d+1)
		if t.Weekday() != time.Saturday && t.Weekday() != time.Sunday {
			return time.Date(t.Year(), t.Month(), t.Day(), start, 0, 0, 0, t.Location())
		}
	}

	// Should never reach here, but satisfy the compiler.
	return now.AddDate(0, 0, 1)
}

const systemPrompt = `You generate shower thoughts — those random, funny, or mildly philosophical ideas ` +
	`people have when their mind wanders. Generate exactly ONE shower thought. ` +
	`It should be surprising, outlandish, amusing, or make the reader say "huh, never thought of that." ` +
	`Keep it to 1–2 sentences. Output only the thought itself — no preamble, no quotes.`

func (st *ShowerThought) postShowerThought(ctx context.Context) {
	thought, err := st.generateShowerThought(ctx)
	if err != nil {
		st.log.Error("Failed to generate shower thought", zap.Error(err))
		return
	}

	msg := thought

	// ~35% of the time, direct the thought at a random channel member.
	if random.Bool(0.40) {
		if userID := st.randomChannelMember(ctx); userID != "" {
			msg = fmt.Sprintf("<@%s> %s", userID, thought)
		}
	}

	_, _, err = st.slack.Client().PostMessageContext(
		ctx,
		st.config.NotifyChannel,
		goslack.MsgOptionText(msg, false),
		goslack.MsgOptionAsUser(true),
	)
	if err != nil {
		st.log.Error("Failed to post shower thought",
			zap.String("channel", st.config.NotifyChannel),
			zap.Error(err),
		)
		return
	}

	st.log.Info("Posted shower thought", zap.String("channel", st.config.NotifyChannel))
}

// randomChannelMember returns a random non-bot member of the notify channel, or empty string on failure.
func (st *ShowerThought) randomChannelMember(ctx context.Context) string {
	members, _, err := st.slack.Client().GetUsersInConversationContext(ctx,
		&goslack.GetUsersInConversationParameters{ChannelID: st.config.NotifyChannel})
	if err != nil {
		st.log.Warn("Failed to fetch channel members for shower thought targeting", zap.Error(err))
		return ""
	}

	botID := st.slack.BotUserID()
	var eligible []string
	for _, id := range members {
		if id != botID {
			eligible = append(eligible, id)
		}
	}

	if len(eligible) == 0 {
		return ""
	}
	return eligible[random.Int(0, len(eligible)-1)]
}

func (st *ShowerThought) generateShowerThought(ctx context.Context) (string, error) {
	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
		llms.TextParts(llms.ChatMessageTypeHuman, "Give me a shower thought."),
	}

	resp, err := st.ai.LLM().GenerateContent(ctx, messages,
		llms.WithTemperature(random.Float(0.3, 1.5)),
		llms.WithMaxTokens(120),
		llms.WithTopP(0.95),
		llms.WithFrequencyPenalty(0.8),
		llms.WithPresencePenalty(0.5),
	)
	if err != nil {
		return "", fmt.Errorf("generate content: %w", err)
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Content == "" {
		return "", fmt.Errorf("empty response from LLM")
	}

	return strings.TrimSpace(resp.Choices[0].Content), nil
}
