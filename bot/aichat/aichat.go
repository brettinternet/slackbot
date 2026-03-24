// TODO: add persistence https://github.com/tmc/langchaingo/blob/main/examples/chains-conversation-memory-sqlite/chains_conversation_memory_sqlite.go
package aichat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"slackbot.arpa/tools/random"
)

const eventChannelSize = 10

type aiService interface {
	LLM() *openai.LLM
}

type slackService interface {
	Client() *slack.Client
	BotUserID() string
}

type FileConfig struct {
	StickyDuration     *time.Duration    `json:"sticky_duration" yaml:"sticky_duration"`
	MaxContextMessages *int              `json:"max_context_messages" yaml:"max_context_messages"`
	MaxContextAge      *time.Duration    `json:"max_context_age" yaml:"max_context_age"`
	MaxContextTokens   *int              `json:"max_context_tokens" yaml:"max_context_tokens"`
	Personas           map[string]string `json:"personas" yaml:"personas"`
}

type Config struct {
	DataDir            string
	Personas           map[string]string
	StickyDuration     time.Duration
	MaxContextMessages int           // Maximum number of messages to include in context
	MaxContextAge      time.Duration // Maximum age of messages to include in context
	MaxContextTokens   int           // Approximate maximum tokens for context (rough estimate)
}

type personaAssignment struct {
	Name      string    // The name of the persona
	Timestamp time.Time // When the persona was assigned
}

type AIChat struct {
	log            *zap.Logger
	config         Config
	slack          slackService
	ai             aiService
	context        *ContextStorage
	stopCh         chan struct{}
	eventsCh       chan slackevents.EventsAPIEvent
	isConnected    atomic.Bool
	eventlimiter   *rate.Limiter
	mentionLimiter *rate.Limiter
	stickyPersonas map[string]personaAssignment // userID -> personaAssignment
	mutex          sync.Mutex
}

func NewAIChat(log *zap.Logger, c Config, s slackService, a aiService) *AIChat {
	contextStorage, err := NewContextStorage(c.DataDir)
	if err != nil {
		log.Error("Failed to initialize context storage", zap.Error(err))
		// Continue without context storage - fallback gracefully
		contextStorage = nil
	}

	return &AIChat{
		log:            log,
		config:         c,
		slack:          s,
		ai:             a,
		context:        contextStorage,
		eventlimiter:   rate.NewLimiter(rate.Every(3*time.Minute), 5),
		mentionLimiter: rate.NewLimiter(rate.Every(1*time.Minute), 3),
		stickyPersonas: make(map[string]personaAssignment),
		stopCh:         make(chan struct{}),
		eventsCh:       make(chan slackevents.EventsAPIEvent, eventChannelSize),
	}
}

// ProcessorType returns a description of the processor type
func (c *AIChat) ProcessorType() string {
	return "aichat"
}

func (a *AIChat) Start(ctx context.Context) error {
	a.isConnected.Store(true)

	go a.handleEvents(ctx)

	return nil
}

func (a *AIChat) Stop(ctx context.Context) error {
	a.isConnected.Store(false)
	close(a.stopCh)

	if a.context != nil {
		if err := a.context.Close(); err != nil {
			a.log.Error("Failed to close context storage", zap.Error(err))
		}
	}

	return nil
}

// PushEvent adds an event to be processed by the AIChat feature
func (a *AIChat) PushEvent(event slackevents.EventsAPIEvent) {
	if !a.isConnected.Load() {
		return
	}

	select {
	case a.eventsCh <- event:
		// Event pushed successfully
	default:
		a.log.Warn("AIChat events channel full, dropping event.")
	}
}

// handleEvents processes Slack events
func (a *AIChat) handleEvents(ctx context.Context) {
	for {
		select {
		case <-a.stopCh:
			return
		case <-ctx.Done():
			return
		case event := <-a.eventsCh:
			a.processEvent(ctx, event)
		}
	}
}

// isBotMentioned checks if the bot is mentioned in the message text
func (a *AIChat) isBotMentioned(text string) bool {
	botUserID := a.slack.BotUserID()
	if botUserID == "" {
		return false
	}

	// Check for direct mention format: <@USERID>
	mentionFormat := fmt.Sprintf("<@%s>", botUserID)
	return strings.Contains(text, mentionFormat)
}

// processEvent handles a single Slack event
func (a *AIChat) processEvent(ctx context.Context, event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			a.log.Debug("Processing AppMentionEvent (direct bot mention)",
				zap.String("user", ev.User),
				zap.String("channel", ev.Channel),
				zap.String("text", ev.Text),
				zap.String("type", a.ProcessorType()),
			)
			// Ignore bot messages to prevent loops
			if ev.BotID != "" || ev.User == "" {
				return
			}
			if !a.mentionLimiter.Allow() {
				return
			}
			a.handleMessageEvent(ctx, eventMessage{
				UserID:          ev.User,
				Channel:         ev.Channel,
				Text:            ev.Text,
				Username:        "",
				ThreadTimeStamp: ev.ThreadTimeStamp,
			})
		case *slackevents.MessageEvent:
			a.log.Debug("Processing MessageEvent",
				zap.String("user", ev.User),
				zap.String("channel", ev.Channel),
				zap.String("text", ev.Text),
				zap.String("type", a.ProcessorType()),
			)
			// TODO: Queue messages received during rate limit and pick one to respond to
			// Ignore bot messages to prevent loops
			if ev.BotID != "" || ev.User == "" {
				return
			}
			if !a.eventlimiter.Allow() {
				a.log.Debug("Rate limit exceeded, dropping event",
					zap.String("user", ev.User),
					zap.String("channel", ev.Channel),
					zap.String("text", ev.Text),
					zap.String("type", a.ProcessorType()),
				)
				return
			}
			// Check if bot is mentioned - this is a fallback for mentions that didn't trigger AppMentionEvent
			isMentioned := a.isBotMentioned(ev.Text)
			if isMentioned {
				// Use mention rate limiter for direct mentions (fallback case)
				if !a.mentionLimiter.Allow() {
					a.log.Debug("Mention rate limit exceeded for MessageEvent fallback",
						zap.String("user", ev.User),
						zap.String("channel", ev.Channel),
						zap.String("text", ev.Text),
					)
					return
				}
			} else {
				// Calculate engagement probability based on recent conversation
				dropChance := a.calculateDropChance(ev.User, ev.Channel, ev.Text)
				if random.Bool(dropChance) {
					return
				}
			}
			a.handleMessageEvent(ctx, eventMessage{
				UserID:          ev.User,
				Channel:         ev.Channel,
				Text:            ev.Text,
				Username:        ev.Username,
				ThreadTimeStamp: ev.ThreadTimeStamp,
			})
		}
	}
}

type eventMessage struct {
	UserID          string
	Username        string
	Channel         string
	Text            string
	ThreadTimeStamp string
}

// handleMessageEvent processes a message event and generates a response
func (a *AIChat) handleMessageEvent(ctx context.Context, m eventMessage) {
	eventMessage := strings.TrimSpace(m.Text)

	a.log.Debug("Processing eventMessage",
		zap.String("user", m.UserID),
		zap.String("channel", m.Channel),
		zap.String("text", eventMessage),
		zap.String("type", a.ProcessorType()),
	)

	user, err := a.slack.Client().GetUserInfo(m.UserID)
	if err != nil {
		a.log.Error("Failed to get user info",
			zap.String("user", m.UserID),
			zap.String("channel", m.Channel),
			zap.String("text", eventMessage),
			zap.Error(err),
		)
	}
	var userDetails UserDetails
	if user != nil {
		userDetails = UserDetails{FirstName: user.Profile.FirstName, LastName: user.Profile.LastName, TZ: user.TZ}
	} else {
		userDetails = UserDetails{Username: m.Username}
	}

	personaName := a.userPersona(m.UserID)

	// Retrieve recent conversation context
	var recentContext []ConversationContext
	if a.context != nil {
		recentContext, err = a.context.GetRecentContext(m.UserID, m.Channel, personaName, &a.config)
		if err != nil {
			a.log.Warn("Failed to retrieve conversation context",
				zap.String("user", m.UserID),
				zap.String("channel", m.Channel),
				zap.Error(err),
			)
		}
	}

	messages := a.buildMessages(m.Text, userDetails, personaName, recentContext)

	// Weighted random length heavily favoring shorter responses
	var maxTokens int
	var temperature float64

	lengthVariation := random.Float(0.0, 1.0)
	// OpenAI allows at most 4 stop sequences — stopWordsForVariation enforces that.
	stopWords := stopWordsForVariation(lengthVariation)
	switch {
	case lengthVariation < 0.60: // Very short responses (60%) — single-line punchy reaction
		maxTokens = 40
		temperature = random.Float(0.7, 1.1)
	case lengthVariation < 0.85: // Short responses (25%)
		maxTokens = 80
		temperature = random.Float(0.6, 1.0)
	case lengthVariation < 0.95: // Medium responses (10%)
		maxTokens = 150
		temperature = random.Float(0.7, 1.1)
	default: // Longer responses (5%) — still not an essay
		maxTokens = 200
		temperature = random.Float(0.8, 1.2)
	}

	resp, err := a.ai.LLM().GenerateContent(ctx, messages,
		llms.WithTemperature(temperature),
		llms.WithMaxTokens(maxTokens),
		llms.WithTopP(0.9),
		llms.WithFrequencyPenalty(0.6),
		llms.WithPresencePenalty(0.3),
		llms.WithStopWords(stopWords))
	if err != nil {
		a.log.Error("Failed to generate content",
			zap.String("user", m.UserID),
			zap.String("channel", m.Channel),
			zap.String("text", eventMessage),
			zap.Error(err),
		)
		return
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Content == "" {
		a.log.Warn("Empty response from LLM",
			zap.String("user", m.UserID),
			zap.String("channel", m.Channel),
		)
		return
	}

	completion := strings.TrimSpace(resp.Choices[0].Content)

	msgOptions := []slack.MsgOption{
		slack.MsgOptionText(completion, false),
		slack.MsgOptionAsUser(true),
	}

	// If this is a threaded message, reply in the thread
	if m.ThreadTimeStamp != "" {
		msgOptions = append(msgOptions, slack.MsgOptionTS(m.ThreadTimeStamp))
	}

	_, _, err = a.slack.Client().PostMessageContext(
		ctx,
		m.Channel,
		msgOptions...,
	)
	if err != nil {
		a.log.Error("Failed to post response",
			zap.String("channel", m.Channel),
			zap.Error(err),
		)
		return
	}

	// Store conversation context
	if a.context != nil {
		now := time.Now()

		// Store user message
		userContext := ConversationContext{
			UserID:      m.UserID,
			ChannelID:   m.Channel,
			PersonaName: personaName,
			Message:     m.Text,
			Role:        "human",
			Timestamp:   now,
		}
		if err := a.context.StoreContext(userContext); err != nil {
			a.log.Warn("Failed to store user context",
				zap.String("user", m.UserID),
				zap.String("channel", m.Channel),
				zap.Error(err),
			)
		}

		// Store assistant response
		assistantContext := ConversationContext{
			UserID:      m.UserID,
			ChannelID:   m.Channel,
			PersonaName: personaName,
			Message:     completion,
			Role:        "assistant",
			Timestamp:   now.Add(time.Millisecond), // Ensure ordering
		}
		if err := a.context.StoreContext(assistantContext); err != nil {
			a.log.Warn("Failed to store assistant context",
				zap.String("user", m.UserID),
				zap.String("channel", m.Channel),
				zap.Error(err),
			)
		}
	}
}

// userPersona assigns a persona to a user and returns the persona name.
func (a *AIChat) userPersona(userID string) string {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if assignment, ok := a.stickyPersonas[userID]; ok {
		if time.Since(assignment.Timestamp) < a.config.StickyDuration {
			return assignment.Name
		}
		delete(a.stickyPersonas, userID)
	}

	personaName := a.randomPersonaName()
	a.stickyPersonas[userID] = personaAssignment{
		Name:      personaName,
		Timestamp: time.Now(),
	}
	return personaName
}

// randomPersonaName returns a random persona name from the configured personas
func (a *AIChat) randomPersonaName() string {
	if len(a.config.Personas) == 0 {
		// Fallback to default persona if no personas configured
		return "default"
	}

	personaNames := make([]string, 0, len(a.config.Personas))
	for name := range a.config.Personas {
		personaNames = append(personaNames, name)
	}

	return random.String(personaNames)
}

type UserDetails struct {
	Username  string
	FirstName string
	LastName  string
	TZ        string
}

// buildMessages constructs a properly typed chat message sequence for the LLM.
// Using GenerateContent with structured messages (rather than Call with a flattened
// string) ensures the model never emits role-label prefixes like "AI:" or "Assistant:".
func (a *AIChat) buildMessages(input string, u UserDetails, personaName string, context []ConversationContext) []llms.MessageContent {
	persona := a.config.Personas[personaName]
	if persona == "" {
		persona = personas[personaName]
		if persona == "" {
			persona = glazerPrompt
		}
	}

	conversationGuidance := ""
	if len(context) > 0 {
		recentMessages := 0
		for _, ctx := range context {
			if time.Since(ctx.Timestamp) < 10*time.Minute {
				recentMessages++
			}
		}
		if recentMessages >= 2 {
			conversationGuidance = "\nConversation is active — build on what's been said, don't restart."
		}
	}

	nameHint := ""
	if u.FirstName != "" {
		nameHint = fmt.Sprintf(" You know their name is %s — use it occasionally, not every message.", u.FirstName)
	}

	systemPrompt := fmt.Sprintf(`%s

You're in a Slack chat. Keep replies SHORT — one sentence usually, two max. Never write paragraphs, lists, or essays. This is casual chat, not a support ticket.%s%s`,
		persona,
		nameHint,
		conversationGuidance,
	)

	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
	}

	// Add conversation history as properly typed turns
	for _, ctx := range context {
		switch ctx.Role {
		case "human":
			messages = append(messages, llms.TextParts(llms.ChatMessageTypeHuman, ctx.Message))
		case "assistant":
			messages = append(messages, llms.TextParts(llms.ChatMessageTypeAI, ctx.Message))
		}
	}

	// Add the current user message
	messages = append(messages, llms.TextParts(llms.ChatMessageTypeHuman, input))

	return messages
}

// stopWordsForVariation returns stop sequences for the given length-variation bucket.
// OpenAI enforces a maximum of 4 stop sequences — this function must never exceed that.
// Role-label stops ("Human:", "Assistant:") are no longer needed since GenerateContent
// uses the chat API which never produces them.
func stopWordsForVariation(v float64) []string {
	if v < 0.60 {
		// Single-line replies: stop at first newline.
		return []string{"\n"}
	}
	// Longer tiers: stop at paragraph breaks.
	return []string{"\n\n"}
}

// calculateDropChance determines the probability of dropping a message based on engagement factors
func (a *AIChat) calculateDropChance(userID, channelID, text string) float64 {
	baseDropChance := 0.4 // Default 40% drop rate

	// Check for recent conversation with this user
	if a.context != nil {
		personaName := a.userPersona(userID)
		recentContext, err := a.context.GetRecentContext(userID, channelID, personaName, &a.config)
		if err == nil && len(recentContext) > 0 {
			// Find the most recent bot response
			var lastBotResponseTime time.Time
			for i := len(recentContext) - 1; i >= 0; i-- {
				if recentContext[i].Role == "assistant" {
					lastBotResponseTime = recentContext[i].Timestamp
					break
				}
			}

			if !lastBotResponseTime.IsZero() {
				timeSinceLastReply := time.Since(lastBotResponseTime)

				// Increase engagement if we recently replied (conversation momentum)
				switch {
				case timeSinceLastReply < 2*time.Minute:
					baseDropChance = 0.15 // Much more likely to continue conversation
				case timeSinceLastReply < 10*time.Minute:
					baseDropChance = 0.25 // Moderately more likely
				case timeSinceLastReply < 30*time.Minute:
					baseDropChance = 0.35 // Slightly more likely
				}

				// But don't be too aggressive - back off if we've responded a lot recently
				recentBotMessages := 0
				for _, ctx := range recentContext {
					if ctx.Role == "assistant" && time.Since(ctx.Timestamp) < 5*time.Minute {
						recentBotMessages++
					}
				}
				if recentBotMessages >= 3 {
					baseDropChance += 0.2 // Back off if we've been too chatty
				}
			}
		}
	}

	// Engagement factors based on message content
	textLower := strings.ToLower(text)

	// More likely to respond to questions
	if strings.Contains(textLower, "?") {
		baseDropChance -= 0.15
	}

	// More likely to respond to emotional content
	emotionalWords := []string{"excited", "frustrated", "confused", "help", "stuck", "wow", "amazing", "terrible", "annoying"}
	for _, word := range emotionalWords {
		if strings.Contains(textLower, word) {
			baseDropChance -= 0.1
			break
		}
	}

	// More likely to respond to conversational cues
	conversationalCues := []string{"thoughts", "think", "opinion", "anyone", "what do you", "how about"}
	for _, cue := range conversationalCues {
		if strings.Contains(textLower, cue) {
			baseDropChance -= 0.1
			break
		}
	}

	// Less likely to respond to very short messages (unless they're questions)
	if len(strings.TrimSpace(text)) < 10 && !strings.Contains(text, "?") {
		baseDropChance += 0.2
	}

	// Clamp between reasonable bounds
	if baseDropChance < 0.05 {
		baseDropChance = 0.05 // Always some chance of not responding
	}
	if baseDropChance > 0.8 {
		baseDropChance = 0.8 // Always some chance of responding
	}

	return baseDropChance
}
