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
	"github.com/tmc/langchaingo/prompts"
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
	StickyDuration      *time.Duration     `json:"sticky_duration" yaml:"sticky_duration"`
	MaxContextMessages  *int              `json:"max_context_messages" yaml:"max_context_messages"`
	MaxContextAge       *time.Duration    `json:"max_context_age" yaml:"max_context_age"`
	MaxContextTokens    *int              `json:"max_context_tokens" yaml:"max_context_tokens"`
	Personas           map[string]string `json:"personas" yaml:"personas"`
}

type Config struct {
	DataDir             string
	Personas            map[string]string
	StickyDuration      time.Duration
	MaxContextMessages  int           // Maximum number of messages to include in context
	MaxContextAge       time.Duration // Maximum age of messages to include in context
	MaxContextTokens    int           // Approximate maximum tokens for context (rough estimate)
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
				UserID:   ev.User,
				Channel:  ev.Channel,
				Text:     ev.Text,
				Username: "",
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
				// For non-mentions, 40% chance to drop the event
				if random.Bool(0.4) {
					return
				}
			}
			a.handleMessageEvent(ctx, eventMessage{
				UserID:   ev.User,
				Channel:  ev.Channel,
				Text:     ev.Text,
				Username: ev.Username,
			})
		}
	}
}

type eventMessage struct {
	UserID   string
	Username string
	Channel  string
	Text     string
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
	
	content, err := a.chatPrompt(m.Text, userDetails, personaName, recentContext)
	if err != nil {
		a.log.Error("Failed to format prompt",
			zap.String("user", m.UserID),
			zap.String("channel", m.Channel),
			zap.String("text", eventMessage),
			zap.Error(err),
		)
		return
	}
	maxTemp := 1.0
	maxLength := 300
	if random.Bool(0.3) {
		maxTemp = 2.0
		maxLength = 1000
	}
	completion, err := a.ai.LLM().Call(ctx, content,
		llms.WithTemperature(random.Float(0.3, maxTemp)),
		llms.WithMaxTokens(1024),
		llms.WithMaxLength(random.Int(3, maxLength)),
		llms.WithTopP(0.9),
		llms.WithFrequencyPenalty(0.5),
		llms.WithStopWords([]string{
			"\n\n",
			"Human:",
			"User:",
		}))
	if err != nil {
		a.log.Error("Failed to generate content",
			zap.String("user", m.UserID),
			zap.String("channel", m.Channel),
			zap.String("text", eventMessage),
			zap.Error(err),
		)
		return
	}
	_, _, err = a.slack.Client().PostMessageContext(
		ctx,
		m.Channel,
		slack.MsgOptionText(completion, false),
		slack.MsgOptionAsUser(true),
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

func (a *AIChat) chatPrompt(input string, u UserDetails, personaName string, context []ConversationContext) (string, error) {
	persona := a.config.Personas[personaName]
	if persona == "" {
		// Fallback to hardcoded personas if not found in config
		persona = personas[personaName]
		if persona == "" {
			persona = glazerPrompt
		}
	}
	prompt := prompts.NewChatPromptTemplate([]prompts.MessageFormatter{
		prompts.NewSystemMessagePromptTemplate(persona, nil),
		prompts.NewSystemMessagePromptTemplate(
			`Your messages are as terse as possible to keep messages short.
			Do your best to always refer to what the user's query,
			however you are part of a larger conversation and so participate as a general member of the crowd.\n
			Details about the user:
			username={{.username}}, first name={{.firstName}}, last name={{.lastName}}, timezone={{.timezone}}.\n
			Prefer referring to the user by their first name when available.
			Use the user's timezone to make assumptions about their location.`,
			[]string{"username", "firstName", "lastName", "timezone"},
		),
		prompts.NewHumanMessagePromptTemplate(
			`{{.prefix}}\n{{.input}}`,
			[]string{"prefix", "input"},
		),
	})

	// Build conversation context prefix
	contextPrefix := ""
	if len(context) > 0 {
		contextPrefix = "Recent conversation:\n"
		for _, ctx := range context {
			if ctx.Role == "human" {
				contextPrefix += fmt.Sprintf("User: %s\n", ctx.Message)
			} else {
				contextPrefix += fmt.Sprintf("Assistant: %s\n", ctx.Message)
			}
		}
		contextPrefix += "\n"
	}

	result, err := prompt.Format(map[string]any{
		"username":  u.Username,
		"firstName": u.FirstName,
		"lastName":  u.LastName,
		"timezone":  u.TZ,
		"prefix":    contextPrefix,
		"input":     input,
	})
	if err != nil {
		return "", fmt.Errorf("format prompt: %w", err)
	}

	return result, nil
}
