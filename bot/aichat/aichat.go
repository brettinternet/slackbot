// TODO: add persistence https://github.com/tmc/langchaingo/blob/main/examples/chains-conversation-memory-sqlite/chains_conversation_memory_sqlite.go
package aichat

import (
	"context"
	"fmt"
	"strconv"
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

// slackContextMessage represents a message fetched from live Slack thread or channel history.
type slackContextMessage struct {
	Text       string
	IsBot      bool
	Timestamp  time.Time // zero if unknown
	SenderID   string    // Slack user ID for non-bot messages
	SenderName string    // resolved first name, populated after fetch
}

// parseSlackTimestamp parses a Slack message timestamp string (e.g. "1512085950.000216") to time.Time.
func parseSlackTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	// Slack timestamps are "seconds.microseconds"; second precision is fine for context purposes.
	if dot := strings.IndexByte(ts, '.'); dot > 0 {
		ts = ts[:dot]
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

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
			if !isMentioned {
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

// fetchThreadContext retrieves all messages in a Slack thread for LLM context.
// Returns messages in chronological order, excluding the triggering (last) message.
// Thread messages are not age-filtered — the entire thread is always relevant context.
func (a *AIChat) fetchThreadContext(ctx context.Context, channelID, threadTS string) []slackContextMessage {
	client := a.slack.Client()
	if client == nil {
		return nil
	}
	msgs, _, _, err := client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     25,
	})
	if err != nil {
		a.log.Warn("Failed to fetch thread context", zap.String("channel", channelID), zap.Error(err))
		return nil
	}
	if len(msgs) <= 1 {
		// Only the parent message (or nothing) — no prior thread context
		return nil
	}
	botID := a.slack.BotUserID()
	// Exclude the last message — it's the one we're responding to
	result := make([]slackContextMessage, 0, len(msgs)-1)
	for _, msg := range msgs[:len(msgs)-1] {
		if strings.TrimSpace(msg.Text) == "" {
			continue
		}
		isBot := msg.User == botID || msg.BotID != ""
		result = append(result, slackContextMessage{
			Text:      msg.Text,
			IsBot:     isBot,
			Timestamp: parseSlackTimestamp(msg.Timestamp),
			SenderID:  msg.User,
		})
	}
	return result
}

// fetchChannelContext retrieves recent messages from a Slack channel for LLM context.
// Messages older than MaxContextAge (default 2h) are excluded via the Slack API's Oldest
// filter so stale context never reaches the LLM.
// Returns messages in chronological order, excluding the most recent (triggering) message.
func (a *AIChat) fetchChannelContext(ctx context.Context, channelID string) []slackContextMessage {
	client := a.slack.Client()
	if client == nil {
		return nil
	}

	maxAge := a.config.MaxContextAge
	if maxAge == 0 {
		maxAge = 2 * time.Hour
	}
	oldest := strconv.FormatInt(time.Now().Add(-maxAge).Unix(), 10)

	history, err := client.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Oldest:    oldest,
		Limit:     16, // fetch one extra so we can drop the triggering message
	})
	if err != nil {
		a.log.Warn("Failed to fetch channel context", zap.String("channel", channelID), zap.Error(err))
		return nil
	}
	msgs := history.Messages
	if len(msgs) == 0 {
		return nil
	}
	// Messages are newest-first; drop index 0 (the message we're responding to), then reverse
	msgs = msgs[1:]
	botID := a.slack.BotUserID()
	result := make([]slackContextMessage, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if strings.TrimSpace(msg.Text) == "" {
			continue
		}
		isBot := msg.User == botID || msg.BotID != ""
		result = append(result, slackContextMessage{
			Text:      msg.Text,
			IsBot:     isBot,
			Timestamp: parseSlackTimestamp(msg.Timestamp),
			SenderID:  msg.User,
		})
	}
	return result
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
		userDetails = UserDetails{UserID: m.UserID, FirstName: user.Profile.FirstName, LastName: user.Profile.LastName, TZ: user.TZ}
	} else {
		userDetails = UserDetails{UserID: m.UserID, Username: m.Username}
	}

	personaName := a.userPersona(m.UserID)

	// Fetch live Slack context for richer, thread-aware responses.
	// For threads, the thread history IS the full conversation — use it directly and skip
	// stored SQLite context (which is keyed per-user and would be redundant/noisy).
	// For non-thread messages, fetch recent channel messages to understand the flow.
	var recentContext []ConversationContext
	var liveContext []slackContextMessage

	if m.ThreadTimeStamp != "" {
		liveContext = a.fetchThreadContext(ctx, m.Channel, m.ThreadTimeStamp)
		// Thread history provides full context; stored history would overlap
	} else {
		liveContext = a.fetchChannelContext(ctx, m.Channel)
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
	}

	if len(liveContext) > 0 {
		resolver := newUserNameResolver(a.config.DataDir, a.slack.Client(), a.log)
		for i := range liveContext {
			if !liveContext[i].IsBot && liveContext[i].SenderID != "" {
				liveContext[i].SenderName = resolver.resolve(ctx, liveContext[i].SenderID)
			}
		}
	}

	messages := a.buildMessages(m.Text, userDetails, personaName, recentContext, liveContext)

	// Weighted random length heavily favoring shorter responses
	var maxTokens int
	var temperature float64

	lengthVariation := random.Float(0.0, 1.0)
	// OpenAI allows at most 4 stop sequences — stopWordsForVariation enforces that.
	stopWords := stopWordsForVariation(lengthVariation)
	switch {
	case lengthVariation < 0.60: // Very short responses (60%) — single-line punchy reaction
		maxTokens = 40
		temperature = random.Float(0.3, 1.5)
	case lengthVariation < 0.85: // Short responses (25%)
		maxTokens = 80
		temperature = random.Float(0.3, 1.2)
	case lengthVariation < 0.95: // Medium responses (10%)
		maxTokens = 150
		temperature = random.Float(0.1, 2.0)
	default: // Longer responses (5%) — still not an essay
		maxTokens = 200
		temperature = random.Float(0.1, 1.5)
	}

	resp, err := a.ai.LLM().GenerateContent(ctx, messages,
		llms.WithTemperature(temperature),
		llms.WithMaxTokens(maxTokens),
		llms.WithTopP(0.9),
		llms.WithFrequencyPenalty(1.0),
		llms.WithPresencePenalty(0.6),
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

	// Strip any self-mentions the LLM may have generated.
	if botID := a.slack.BotUserID(); botID != "" {
		completion = strings.ReplaceAll(completion, fmt.Sprintf("<@%s>", botID), "")
		completion = strings.TrimSpace(completion)
	}

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
	UserID    string
	Username  string
	FirstName string
	LastName  string
	TZ        string
}

// formatContextAge formats a duration for display in the system prompt recency note.
func formatContextAge(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%d hours", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

// buildMessages constructs a properly typed chat message sequence for the LLM.
// Using GenerateContent with structured messages (rather than Call with a flattened
// string) ensures the model never emits role-label prefixes like "AI:" or "Assistant:".
//
// liveContext contains messages fetched directly from Slack (thread or channel history)
// and is placed first so the LLM sees the full conversational flow.
// storedContext contains the bot's own conversation history with this user from SQLite.
func (a *AIChat) buildMessages(input string, u UserDetails, personaName string, storedContext []ConversationContext, liveContext []slackContextMessage) []llms.MessageContent {
	persona := a.config.Personas[personaName]
	if persona == "" {
		persona = personas[personaName]
		if persona == "" {
			persona = glazerPrompt
		}
	}

	// Determine conversation state for system prompt guidance
	activeConvo := len(liveContext) > 0
	hasBotResponses := false
	var oldestLive time.Time

	if !activeConvo {
		for _, ctx := range storedContext {
			if time.Since(ctx.Timestamp) < 10*time.Minute {
				activeConvo = true
				break
			}
		}
	}
	for _, msg := range liveContext {
		if msg.IsBot {
			hasBotResponses = true
		}
		if !msg.Timestamp.IsZero() && (oldestLive.IsZero() || msg.Timestamp.Before(oldestLive)) {
			oldestLive = msg.Timestamp
		}
	}
	if !hasBotResponses {
		for _, ctx := range storedContext {
			if ctx.Role == "assistant" {
				hasBotResponses = true
				break
			}
		}
	}

	guidance := ""
	if activeConvo {
		guidance += "\nConversation is active — build on what's been said, don't restart."
	}
	if hasBotResponses {
		guidance += "\nDon't repeat a punchline, roast, or observation you've already made in this conversation."
	}
	// When context spans a significant window, signal that older messages carry less weight.
	if !oldestLive.IsZero() && time.Since(oldestLive) > 30*time.Minute {
		age := time.Since(oldestLive).Round(time.Minute)
		guidance += fmt.Sprintf("\nContext spans up to %s back; weight recent messages more heavily than older ones.", formatContextAge(age))
	}

	// Identify the person we're replying to.
	targetName := u.FirstName
	if targetName == "" {
		targetName = u.Username
	}

	targetHint := ""
	if targetName != "" {
		targetHint = fmt.Sprintf(" You are responding to %s's message specifically — that's who your reply is for.", targetName)
	}

	nameHint := ""
	if u.FirstName != "" {
		nameHint = fmt.Sprintf(" Use their name (%s) occasionally, not every message.", u.FirstName)
	}

	mentionHint := ""
	if u.UserID != "" {
		mentionHint = fmt.Sprintf(
			"\nOccasionally (not every message) you may @-reply using <@%s> — but only when it feels natural, like kicking off a direct reaction. Most replies should NOT start with a mention. Never @mention anyone else from the context.",
			u.UserID,
		)
	}

	systemPrompt := fmt.Sprintf(`%s

You're in a Slack chat. Keep replies SHORT — one sentence usually, two max. Never write paragraphs, lists, or essays. This is casual chat, not a support ticket. Be funny, absurd, or very wise.%s%s%s%s`,
		persona,
		targetHint,
		nameHint,
		mentionHint,
		guidance,
	)

	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
	}

	// Add live Slack context (thread or recent channel messages) as typed turns.
	// This gives the LLM the real conversational flow happening in Slack.
	for _, msg := range liveContext {
		if msg.IsBot {
			messages = append(messages, llms.TextParts(llms.ChatMessageTypeAI, msg.Text))
		} else {
			text := msg.Text
			if msg.SenderName != "" && msg.SenderID != u.UserID {
				text = "[" + msg.SenderName + "]: " + text
			}
			messages = append(messages, llms.TextParts(llms.ChatMessageTypeHuman, text))
		}
	}

	// Add stored conversation history (user-specific memory from past sessions).
	for _, ctx := range storedContext {
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
