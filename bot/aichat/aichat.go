// TODO: add persistence https://github.com/tmc/langchaingo/blob/main/examples/chains-conversation-memory-sqlite/chains_conversation_memory_sqlite.go
package aichat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
	"github.com/tmc/langchaingo/prompts"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
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
	log          *zap.Logger
	config       Config
	slack        slackService
	ai           aiService
	stopCh       chan struct{}
	eventsCh     chan slackevents.EventsAPIEvent
	isConnected  atomic.Bool
	limiter      *rate.Limiter
	userPersonas map[string]string
	mutex        sync.Mutex
}

func NewAIChat(log *zap.Logger, c Config, s slackService, a aiService) *AIChat {
	return &AIChat{
		log:          log,
		config:       c,
		slack:        s,
		ai:           a,
		limiter:      rate.NewLimiter(rate.Limit(1), 3),
		userPersonas: make(map[string]string),
		stopCh:       make(chan struct{}),
		eventsCh:     make(chan slackevents.EventsAPIEvent, eventChannelSize),
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
			if a.limiter.Allow() {
				a.processEvent(ctx, event)
			}
		}
	}
}

// processEvent handles a single Slack event
func (a *AIChat) processEvent(ctx context.Context, event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			// Ignore bot messages to prevent loops
			if ev.BotID != "" || ev.User == "" {
				return
			}
			a.handleMessageEvent(ctx, ev)
		}
	}
}

type UserDetails struct {
	Username  string
	FirstName string
	LastName  string
	TZ        string
}

// handleMessageEvent processes a message event and responds if it matches a pattern
func (a *AIChat) userPersona(userID string) string {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	if personaName, ok := a.userPersonas[userID]; ok {
		return personaName
	}
	personaName := randomPersonaName()
	a.userPersonas[userID] = personaName
	return personaName
}

func (a *AIChat) handleMessageEvent(ctx context.Context, ev *slackevents.MessageEvent) {
	message := strings.TrimSpace(ev.Text)

	a.log.Debug("Processing message",
		zap.String("user", ev.User),
		zap.String("channel", ev.Channel),
		zap.String("text", message),
		zap.String("type", a.ProcessorType()),
	)

	user, err := a.slack.Client().GetUserInfo(ev.User)
	if err != nil {
		a.log.Error("Failed to get user info",
			zap.String("user", ev.User),
			zap.String("channel", ev.Channel),
			zap.String("text", message),
			zap.Error(err),
		)
	}
	var userDetails UserDetails
	if user != nil {
		userDetails = UserDetails{FirstName: user.Profile.FirstName, LastName: user.Profile.LastName, TZ: user.TZ}
	} else {
		userDetails = UserDetails{Username: ev.Username}
	}

	content, err := chatPrompt(ev.Text, userDetails, a.userPersona(ev.User))
	if err != nil {
		a.log.Error("Failed to format prompt",
			zap.String("user", ev.User),
			zap.String("channel", ev.Channel),
			zap.String("text", message),
			zap.Error(err),
		)
		return
	}
	completion, err := a.ai.LLM().Call(ctx, content,
		llms.WithMaxTokens(1024),
		llms.WithTemperature(2.0),
		llms.WithMaxLength(1024))
	if err != nil {
		a.log.Error("Failed to generate content",
			zap.String("user", ev.User),
			zap.String("channel", ev.Channel),
			zap.String("text", message),
			zap.Error(err),
		)
		return
	}
	_, _, err = a.slack.Client().PostMessageContext(
		ctx,
		ev.Channel,
		slack.MsgOptionText(completion, false),
		slack.MsgOptionAsUser(true),
	)
	if err != nil {
		a.log.Error("Failed to post response",
			zap.String("channel", ev.Channel),
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
			`Your messages are as terse as possible to keep messages short.\n
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
