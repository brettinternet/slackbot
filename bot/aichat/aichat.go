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
}

type FileConfig struct{}

type Config struct{}

type AIChat struct {
	log            *zap.Logger
	config         Config
	slack          slackService
	ai             aiService
	stopCh         chan struct{}
	eventsCh       chan slackevents.EventsAPIEvent
	isConnected    atomic.Bool
	eventlimiter   *rate.Limiter
	mentionLimiter *rate.Limiter
	stickyPersonas map[string]string // userID -> personaName
	mutex          sync.Mutex
}

func NewAIChat(log *zap.Logger, c Config, s slackService, a aiService) *AIChat {
	return &AIChat{
		log:            log,
		config:         c,
		slack:          s,
		ai:             a,
		eventlimiter:   rate.NewLimiter(rate.Every(3*time.Minute), 5),
		mentionLimiter: rate.NewLimiter(rate.Every(1*time.Minute), 3),
		stickyPersonas: make(map[string]string),
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

// processEvent handles a single Slack event
func (a *AIChat) processEvent(ctx context.Context, event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
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
			// TODO: Queue messages received during rate limit and pick one to respond to
			// Ignore bot messages to prevent loops
			if ev.BotID != "" || ev.User == "" {
				return
			}
			if !a.eventlimiter.Allow() {
				return
			}
			// 40% chance to drop the event where the bot is not mentioned
			if random.Bool(0.6) {
				return
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

type UserDetails struct {
	Username  string
	FirstName string
	LastName  string
	TZ        string
}

// handleMessageEvent processes a eventMessage event and responds if it matches a pattern
func (a *AIChat) userPersona(userID string) string {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	if personaName, ok := a.stickyPersonas[userID]; ok {
		return personaName
	}
	personaName := randomPersonaName()
	a.stickyPersonas[userID] = personaName
	return personaName
}

type eventMessage struct {
	UserID   string
	Username string
	Channel  string
	Text     string
}

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
	content, err := chatPrompt(m.Text, userDetails, personaName)
	if err != nil {
		a.log.Error("Failed to format prompt",
			zap.String("user", m.UserID),
			zap.String("channel", m.Channel),
			zap.String("text", eventMessage),
			zap.Error(err),
		)
		return
	}
	maxLength := 300
	if random.Bool(0.05) {
		maxLength = 1000
	}
	completion, err := a.ai.LLM().Call(ctx, content,
		llms.WithTemperature(random.Float(0.3, 1.0)),
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
}

func chatPrompt(input string, u UserDetails, personaName string) (string, error) {
	persona := personas[personaName]
	if persona == "" {
		persona = glazerPrompt
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

	result, err := prompt.Format(map[string]any{
		"username":  u.Username,
		"firstName": u.FirstName,
		"lastName":  u.LastName,
		"timezone":  u.TZ,
		"prefix":    "",
		"input":     input,
	})
	if err != nil {
		return "", fmt.Errorf("format prompt: %w", err)
	}

	return result, nil
}
