package aichat

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/tmc/langchaingo/llms"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
	botlogging "slackbot.arpa/bot/logging"
	"slackbot.arpa/tools/random"
)

// slackContextMessage represents a message fetched from live Slack thread or channel history.
type slackContextMessage struct {
	Text       string
	IsBot      bool      // any bot, including third-party integrations
	IsSelf     bool      // this AIChat bot specifically
	Timestamp  time.Time // zero if unknown
	SenderID   string    // Slack user ID for non-bot messages
	SenderName string    // resolved user or integration name
}

// parseSlackTimestamp parses a Slack message timestamp string (e.g. "1512085950.000216") to time.Time.
func parseSlackTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	// Preserve fractional seconds so adjacent Slack messages have stable ordering.
	parts := strings.SplitN(ts, ".", 2)
	secs, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}
	}
	var nanos int64
	if len(parts) == 2 {
		fraction := parts[1]
		if len(fraction) > 9 {
			fraction = fraction[:9]
		}
		for len(fraction) < 9 {
			fraction += "0"
		}
		nanos, err = strconv.ParseInt(fraction, 10, 64)
		if err != nil {
			return time.Time{}
		}
	}
	return time.Unix(secs, nanos)
}

const (
	eventChannelSize       = 100
	workerCount            = 3
	contextCleanupInterval = time.Hour
)

type aiService interface {
	GenerateContent(context.Context, []llms.MessageContent, ...llms.CallOption) (*llms.ContentResponse, error)
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
	RateLimitEnabled   *bool             `json:"rate_limit_enabled" yaml:"rate_limit_enabled"`
	Personas           map[string]string `json:"personas" yaml:"personas"`
}

type Config struct {
	Enabled            bool
	DataDir            string
	Personas           map[string]string
	StickyDuration     time.Duration
	MaxContextMessages int           // Maximum number of messages to include in context
	MaxContextAge      time.Duration // Maximum age of messages to include in context
	MaxContextTokens   int           // Approximate maximum tokens for context (rough estimate)
	RateLimitEnabled   bool          // When false, per-channel rate limiting is bypassed
}

type personaAssignment struct {
	Name      string    // The name of the persona
	Timestamp time.Time // When the persona was assigned
}

type AIChat struct {
	log             *zap.Logger
	config          Config
	configMu        sync.RWMutex
	slack           slackService
	ai              aiService
	context         *ContextStorage
	userNames       *userNameResolver
	stopCh          chan struct{}
	eventsCh        chan slackevents.EventsAPIEvent
	isConnected     atomic.Bool
	stickyPersonas  map[string]personaAssignment // conversation scope -> personaAssignment
	mutex           sync.Mutex
	limiterMutex    sync.Mutex
	channelLimiters map[string]*rate.Limiter
	startOnce       sync.Once
	stopOnce        sync.Once
	closeOnce       sync.Once
	workers         sync.WaitGroup
	shutdownDone    chan struct{}
	queueDepth      atomic.Int64
	queueDrops      atomic.Uint64
	pendingEvents   atomic.Int64
	generationCount atomic.Uint64
	generationNanos atomic.Int64
}

// Metrics is a point-in-time snapshot of AI chat queue and generation activity.
type Metrics struct {
	QueueDepth             int64
	QueueDrops             uint64
	GenerationCount        uint64
	GenerationLatencyTotal time.Duration
}

// Metrics returns a point-in-time snapshot suitable for an application's metrics exporter.
func (a *AIChat) Metrics() Metrics {
	return Metrics{
		QueueDepth:             a.queueDepth.Load(),
		QueueDrops:             a.queueDrops.Load(),
		GenerationCount:        a.generationCount.Load(),
		GenerationLatencyTotal: time.Duration(a.generationNanos.Load()),
	}
}

func NewAIChat(log *zap.Logger, c Config, s slackService, a aiService) (*AIChat, error) {
	log = botlogging.Component(log, "aichat")
	c = cloneConfig(c)
	contextStorage, err := NewContextStorage(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("initialize context storage: %w", err)
	}

	return &AIChat{
		log:             log,
		config:          c,
		slack:           s,
		ai:              a,
		context:         contextStorage,
		userNames:       newUserNameResolver(c.DataDir, s.Client(), log),
		stickyPersonas:  make(map[string]personaAssignment),
		channelLimiters: make(map[string]*rate.Limiter),
		stopCh:          make(chan struct{}),
		shutdownDone:    make(chan struct{}),
		eventsCh:        make(chan slackevents.EventsAPIEvent, eventChannelSize),
	}, nil
}

// ProcessorType returns a description of the processor type
func (c *AIChat) ProcessorType() string {
	return "aichat"
}

func (a *AIChat) Start(ctx context.Context) error {
	a.startOnce.Do(func() {
		a.isConnected.Store(true)
		a.workers.Add(2)
		go func() {
			defer a.workers.Done()
			a.handleEvents(ctx)
		}()
		go func() {
			defer a.workers.Done()
			a.runContextCleanup(ctx)
		}()
	})
	return nil
}

func (a *AIChat) runContextCleanup(ctx context.Context) {
	if a.context == nil {
		return
	}
	a.cleanupExpiredContext()
	ticker := time.NewTicker(contextCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanupExpiredContext()
		}
	}
}

func (a *AIChat) cleanupExpiredContext() {
	config := a.configSnapshot()
	a.mutex.Lock()
	defer a.mutex.Unlock()

	counts, err := a.context.CleanExpired(config.MaxContextAge, config.StickyDuration)
	if err != nil {
		a.log.Warn("Failed to clean expired AI chat data",
			botlogging.Operation("cleanup_context"), zap.Error(err))
		return
	}
	if config.StickyDuration > 0 {
		cutoff := time.Now().Add(-config.StickyDuration)
		for scope, assignment := range a.stickyPersonas {
			if assignment.Timestamp.Before(cutoff) {
				delete(a.stickyPersonas, scope)
			}
		}
	}
	if counts.Contexts > 0 || counts.Personas > 0 {
		a.log.Info("Cleaned expired AI chat data",
			botlogging.Operation("cleanup_context"),
			zap.Int64("contexts", counts.Contexts),
			zap.Int64("personas", counts.Personas))
	}
}

// ClearContext removes persisted messages and persona state for one exact channel
// or thread scope.
func (a *AIChat) ClearContext(scope string) (DeletionCounts, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return DeletionCounts{}, fmt.Errorf("conversation scope is required")
	}
	if a.context == nil {
		return DeletionCounts{}, fmt.Errorf("AI chat context storage is unavailable")
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()
	counts, err := a.context.DeleteConversationScope(scope)
	if err != nil {
		return DeletionCounts{}, err
	}
	delete(a.stickyPersonas, scope)
	return counts, nil
}

// SetConfig atomically applies reloadable AI chat and persona settings.
func (a *AIChat) SetConfig(c Config) {
	a.configMu.Lock()
	// Storage is opened at construction and therefore remains restart-required.
	c.DataDir = a.config.DataDir
	a.config = cloneConfig(c)
	a.configMu.Unlock()

	// A rate-limit mode change starts with fresh limiter state.
	a.limiterMutex.Lock()
	a.channelLimiters = make(map[string]*rate.Limiter)
	a.limiterMutex.Unlock()
}

func (a *AIChat) configSnapshot() Config {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	return cloneConfig(a.config)
}

func cloneConfig(c Config) Config {
	c.Personas = maps.Clone(c.Personas)
	return c
}

func (a *AIChat) enabled() bool {
	return a.ai != nil && a.configSnapshot().Enabled
}

func (a *AIChat) Stop(ctx context.Context) error {
	a.stopOnce.Do(func() {
		a.isConnected.Store(false)
		close(a.stopCh)
		go func() {
			a.workers.Wait()
			a.closeOnce.Do(func() {
				if a.context != nil {
					if err := a.context.Close(); err != nil {
						a.log.Error("Failed to close context storage", zap.Error(err))
					}
				}
			})
			close(a.shutdownDone)
		}()
	})
	select {
	case <-a.shutdownDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PushEvent adds an event to be processed by the AIChat feature
func (a *AIChat) PushEvent(event slackevents.EventsAPIEvent) error {
	if !a.isConnected.Load() || !a.enabled() {
		return nil
	}

	a.queueDepth.Add(1)
	a.pendingEvents.Add(1)
	select {
	case a.eventsCh <- event:
	default:
		a.queueDepth.Add(-1)
		a.pendingEvents.Add(-1)
		a.queueDrops.Add(1)
		botlogging.ForSlackEvent(a.log, event).Warn("AIChat events channel full, dropping event",
			botlogging.Operation("enqueue_event"),
			zap.Int64("queue_depth", a.queueDepth.Load()),
			zap.Uint64("queue_drops", a.queueDrops.Load()))
	}
	return nil
}

func (a *AIChat) PendingEvents() int64 { return a.pendingEvents.Load() }

func (a *AIChat) allowChannelEvent(channelID string) bool {
	a.limiterMutex.Lock()
	defer a.limiterMutex.Unlock()
	if a.channelLimiters == nil {
		a.channelLimiters = make(map[string]*rate.Limiter)
	}
	limiter := a.channelLimiters[channelID]
	if limiter == nil {
		limiter = rate.NewLimiter(rate.Every(3*time.Minute), 5)
		a.channelLimiters[channelID] = limiter
	}
	return limiter.Allow()
}

// handleEvents shards events by conversation. Each shard is serial, preserving
// order within a channel or thread while retaining concurrency across conversations.
func (a *AIChat) handleEvents(ctx context.Context) {
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	shards := make([]chan slackevents.EventsAPIEvent, workerCount)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for i := range shards {
		shards[i] = make(chan slackevents.EventsAPIEvent, eventChannelSize)
		go func(events <-chan slackevents.EventsAPIEvent) {
			defer workers.Done()
			for {
				select {
				case <-workerCtx.Done():
					for range events {
						a.queueDepth.Add(-1)
						a.pendingEvents.Add(-1)
					}
					return
				case event, ok := <-events:
					if !ok {
						return
					}
					a.queueDepth.Add(-1)
					a.processEventSafely(workerCtx, event)
					a.pendingEvents.Add(-1)
				}
			}
		}(shards[i])
	}
	defer func() {
		cancelWorkers()
		for _, shard := range shards {
			close(shard)
		}
		for {
			select {
			case <-a.eventsCh:
				a.queueDepth.Add(-1)
				a.pendingEvents.Add(-1)
			default:
				workers.Wait()
				return
			}
		}
	}()

	for {
		select {
		case <-a.stopCh:
			return
		case <-ctx.Done():
			return
		case event := <-a.eventsCh:
			shard := conversationShard(event, len(shards))
			select {
			case shards[shard] <- event:
			case <-a.stopCh:
				a.queueDepth.Add(-1)
				a.pendingEvents.Add(-1)
				return
			case <-ctx.Done():
				a.queueDepth.Add(-1)
				a.pendingEvents.Add(-1)
				return
			}
		}
	}
}

func conversationShard(event slackevents.EventsAPIEvent, count int) int {
	key := ""
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		key = conversationScope(ev.Channel, ev.ThreadTimeStamp)
	case *slackevents.MessageEvent:
		key = conversationScope(ev.Channel, ev.ThreadTimeStamp)
	}
	shard := 0
	for _, character := range []byte(key) {
		shard = (shard*31 + int(character)) % count
	}
	return shard
}

// Literal addressing is intentionally narrower than a word search: an explicit
// greeting/leading address or a trailing "..., bot" counts, while prose such as
// "ask the bot" does not bypass unsolicited-response controls.
var botWordPattern = regexp.MustCompile(`(?i)^\s*(?:@?bot\b|(?:hey|hi|hello|yo)\s+@?bot\b)|[,;:]\s*@?bot\s*[?!.,;:]*\s*$`)

// isBotMentioned checks if the bot is mentioned in the message text — either via
// a proper Slack @-mention (<@USERID>) or by the literal word "bot".
func (a *AIChat) isBotMentioned(text string) bool {
	botUserID := a.slack.BotUserID()
	if botUserID != "" && strings.Contains(text, fmt.Sprintf("<@%s>", botUserID)) {
		return true
	}
	return botWordPattern.MatchString(text)
}

func (a *AIChat) processEventSafely(ctx context.Context, event slackevents.EventsAPIEvent) {
	ctx = botlogging.WithSlackEvent(ctx, a.log, event)
	log := botlogging.FromContext(ctx, a.log)
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error("AIChat event handler panicked",
				botlogging.Operation("process_event"), zap.Any("panic", recovered))
		}
	}()
	a.processEvent(ctx, event)
}

// processEvent handles a single Slack event
func (a *AIChat) processEvent(ctx context.Context, event slackevents.EventsAPIEvent) {
	log := botlogging.FromContext(ctx, botlogging.ForSlackEvent(a.log, event))
	config := a.configSnapshot()
	if !config.Enabled {
		return
	}
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			log.Debug("Processing AppMentionEvent (direct bot mention)",
				botlogging.Operation("process_event"),
				zap.String("user", ev.User),
				zap.String("channel", ev.Channel),
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
				TimeStamp:       ev.TimeStamp,
				ThreadTimeStamp: ev.ThreadTimeStamp,
				DirectMention:   true,
			})
		case *slackevents.MessageEvent:
			log.Debug("Processing MessageEvent",
				botlogging.Operation("process_event"),
				zap.String("user", ev.User),
				zap.String("channel", ev.Channel),
				zap.String("type", a.ProcessorType()),
			)
			if ev.BotID != "" || ev.User == "" {
				return
			}
			// Direct mentions bypass rate limit and drop chance, like AppMentionEvent.
			if !a.isBotMentioned(ev.Text) {
				if config.RateLimitEnabled && !a.allowChannelEvent(ev.Channel) {
					log.Debug("Rate limit exceeded, dropping event",
						botlogging.Operation("rate_limit_event"),
						zap.String("user", ev.User),
						zap.String("channel", ev.Channel),
						zap.String("type", a.ProcessorType()),
					)
					return
				}
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
				TimeStamp:       ev.TimeStamp,
				ThreadTimeStamp: ev.ThreadTimeStamp,
				DirectMention:   a.isBotMentioned(ev.Text),
			})
		}
	}
}

type eventMessage struct {
	UserID          string
	Username        string
	Channel         string
	Text            string
	TimeStamp       string // triggering Slack message timestamp
	ThreadTimeStamp string
	DirectMention   bool
}

// fetchThreadContext retrieves all messages in a Slack thread. The triggering
// message is excluded by its exact Slack timestamp, never by list position.
func (a *AIChat) fetchThreadContext(ctx context.Context, channelID, threadTS, triggeringTS string) []slackContextMessage {
	log := botlogging.FromContext(ctx, a.log)
	client := a.slack.Client()
	if client == nil {
		return nil
	}
	params := &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     100,
	}
	var msgs []slack.Message
	for {
		page, hasMore, nextCursor, err := client.GetConversationRepliesContext(ctx, params)
		if err != nil {
			log.Warn("Failed to fetch thread context", botlogging.Operation("fetch_thread_context"),
				zap.String("channel", channelID), zap.Error(err))
			return nil
		}
		msgs = append(msgs, page...)
		if !hasMore || nextCursor == "" {
			break
		}
		params.Cursor = nextCursor
	}
	if len(msgs) == 0 {
		return nil
	}
	sort.SliceStable(msgs, func(i, j int) bool {
		return parseSlackTimestamp(msgs[i].Timestamp).Before(parseSlackTimestamp(msgs[j].Timestamp))
	})
	botID := a.slack.BotUserID()
	result := make([]slackContextMessage, 0, len(msgs))
	for _, msg := range msgs {
		if triggeringTS != "" && msg.Timestamp == triggeringTS {
			continue
		}
		if strings.TrimSpace(msg.Text) == "" {
			continue
		}
		isSelf := botID != "" && msg.User == botID
		result = append(result, slackContextMessage{
			Text:       msg.Text,
			IsBot:      msg.BotID != "" || isSelf,
			IsSelf:     isSelf,
			Timestamp:  parseSlackTimestamp(msg.Timestamp),
			SenderID:   msg.User,
			SenderName: msg.Username,
		})
	}
	return result
}

// fetchChannelContext retrieves recent messages from a Slack channel for LLM context.
// Messages older than MaxContextAge (default 2h) are excluded via the Slack API's Oldest
// filter so stale context never reaches the LLM.
// Returns messages in chronological order, excluding the triggering timestamp when present.
func (a *AIChat) fetchChannelContext(ctx context.Context, channelID, triggeringTS string) []slackContextMessage {
	log := botlogging.FromContext(ctx, a.log)
	client := a.slack.Client()
	if client == nil {
		return nil
	}

	maxAge := a.configSnapshot().MaxContextAge
	if maxAge == 0 {
		maxAge = 2 * time.Hour
	}
	oldest := strconv.FormatInt(time.Now().Add(-maxAge).Unix(), 10)

	history, err := client.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Oldest:    oldest,
		Limit:     16,
	})
	if err != nil {
		log.Warn("Failed to fetch channel context", botlogging.Operation("fetch_channel_context"),
			zap.String("channel", channelID), zap.Error(err))
		return nil
	}
	msgs := history.Messages
	if len(msgs) == 0 {
		return nil
	}
	// Slack returns newest-first. Filter the triggering message by exact timestamp;
	// the newest item is not necessarily the event that caused this callback.
	botID := a.slack.BotUserID()
	result := make([]slackContextMessage, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if triggeringTS != "" && msg.Timestamp == triggeringTS {
			continue
		}
		if strings.TrimSpace(msg.Text) == "" {
			continue
		}
		isSelf := botID != "" && msg.User == botID
		result = append(result, slackContextMessage{
			Text: msg.Text, IsBot: msg.BotID != "" || isSelf, IsSelf: isSelf,
			Timestamp: parseSlackTimestamp(msg.Timestamp), SenderID: msg.User, SenderName: msg.Username,
		})
	}
	return result
}

// handleMessageEvent processes a message event and generates a response
func (a *AIChat) handleMessageEvent(ctx context.Context, m eventMessage) {
	log := botlogging.FromContext(ctx, a.log)
	eventMessage := strings.TrimSpace(m.Text)

	log.Debug("Processing eventMessage",
		botlogging.Operation("process_message"),
		zap.String("user", m.UserID),
		zap.String("channel", m.Channel),
		zap.String("type", a.ProcessorType()),
	)

	if !m.DirectMention && isShortAcknowledgement(eventMessage) {
		a.reactToAcknowledgement(ctx, m, acknowledgementReaction())
		return
	}

	user, err := a.slack.Client().GetUserInfo(m.UserID)
	if err != nil {
		log.Error("Failed to get user info",
			botlogging.Operation("get_user_info"),
			zap.String("user", m.UserID),
			zap.String("channel", m.Channel),
			zap.Error(err),
		)
	}
	var userDetails UserDetails
	if user != nil {
		userDetails = UserDetails{UserID: m.UserID, FirstName: user.Profile.FirstName, LastName: user.Profile.LastName, TZ: user.TZ}
	} else {
		userDetails = UserDetails{UserID: m.UserID, Username: m.Username}
	}

	scope := conversationScope(m.Channel, m.ThreadTimeStamp)
	personaName := a.userPersonaWithLogger(log, scope)

	// Prefer live Slack context because it has the actual channel chronology. Stored
	// channel memory is a fallback for API failures or channels with no recent messages.
	// Threads never fall back to channel memory because it may describe an unrelated conversation.
	var recentContext []ConversationContext
	var liveContext []slackContextMessage
	if m.ThreadTimeStamp != "" {
		liveContext = a.fetchThreadContext(ctx, m.Channel, m.ThreadTimeStamp, m.TimeStamp)
	} else {
		liveContext = a.fetchChannelContext(ctx, m.Channel, m.TimeStamp)
	}
	config := a.configSnapshot()
	if shouldLoadStoredContext(len(liveContext)) && a.context != nil {
		recentContext, err = a.context.GetRecentContext(m.UserID, scope, &config)
		if err != nil {
			log.Warn("Failed to retrieve conversation context",
				botlogging.Operation("retrieve_conversation_context"),
				zap.String("user", m.UserID), zap.String("channel", m.Channel), zap.Error(err))
		}
	}

	if len(liveContext) > 0 {
		if a.userNames == nil {
			a.userNames = newUserNameResolver(config.DataDir, a.slack.Client(), a.log)
		}
		for i := range liveContext {
			if !liveContext[i].IsBot && liveContext[i].SenderID != "" {
				liveContext[i].SenderName = a.userNames.resolve(ctx, liveContext[i].SenderID)
			}
		}
	}

	messages := a.buildMessages(m.Text, userDetails, personaName, recentContext, liveContext)
	profile := generationProfileForInput(eventMessage)
	resp, err := a.generateContent(ctx, messages,
		generationOptions(profile.Temperature, profile.MaxTokens)...)
	if err != nil {
		log.Error("Failed to generate content",
			botlogging.Operation("generate_content"),
			zap.String("user", m.UserID), zap.String("channel", m.Channel), zap.Error(err))
		if m.DirectMention {
			a.postFallback(ctx, m)
		}
		return
	}

	if resp == nil || len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Content) == "" {
		log.Warn("Empty response from LLM", botlogging.Operation("generate_content"),
			zap.String("user", m.UserID), zap.String("channel", m.Channel))
		if m.DirectMention {
			a.postFallback(ctx, m)
		}
		return
	}

	completion := strings.TrimSpace(resp.Choices[0].Content)

	// Strip any self-mentions the LLM may have generated.
	if botID := a.slack.BotUserID(); botID != "" {
		completion = strings.ReplaceAll(completion, fmt.Sprintf("<@%s>", botID), "")
		completion = strings.TrimSpace(completion)
	}
	if completion == "" {
		if m.DirectMention {
			a.postFallback(ctx, m)
		}
		return
	}

	// Disabling AI chat while a generation is in flight must suppress its side effect.
	if !a.enabled() {
		return
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
		log.Error("Failed to post response",
			botlogging.Operation("post_response"),
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
			ChannelID:   scope,
			PersonaName: personaName,
			Message:     m.Text,
			Role:        "human",
			Timestamp:   now,
		}
		if err := a.context.StoreContext(userContext); err != nil {
			log.Warn("Failed to store user context",
				botlogging.Operation("store_user_context"),
				zap.String("user", m.UserID),
				zap.String("channel", m.Channel),
				zap.Error(err),
			)
		}

		// Store assistant response
		assistantContext := ConversationContext{
			UserID:      m.UserID,
			ChannelID:   scope,
			PersonaName: personaName,
			Message:     completion,
			Role:        "assistant",
			Timestamp:   now.Add(time.Millisecond), // Ensure ordering
		}
		if err := a.context.StoreContext(assistantContext); err != nil {
			log.Warn("Failed to store assistant context",
				botlogging.Operation("store_assistant_context"),
				zap.String("user", m.UserID),
				zap.String("channel", m.Channel),
				zap.Error(err),
			)
		}
	}
}

func shouldLoadStoredContext(liveContextMessages int) bool {
	return liveContextMessages == 0
}

func conversationScope(channelID, threadTS string) string {
	if threadTS == "" {
		return channelID
	}
	return channelID + ":thread:" + threadTS
}

// userPersona assigns one persona to a channel or thread and keeps it stable while
// that conversation remains active, including across process restarts.
func (a *AIChat) userPersona(scope string) string {
	return a.userPersonaWithLogger(a.log, scope)
}

func (a *AIChat) userPersonaWithLogger(log *zap.Logger, scope string) string {
	config := a.configSnapshot()
	a.mutex.Lock()
	defer a.mutex.Unlock()
	now := time.Now()
	assignment, ok := a.stickyPersonas[scope]
	if a.context != nil {
		persisted, persistedOK, err := a.context.TouchPersonaAssignment(scope, now, config.StickyDuration)
		if err != nil {
			log.Warn("Failed to retrieve persona assignment",
				botlogging.Operation("retrieve_persona_assignment"), zap.String("scope", scope), zap.Error(err))
		} else {
			// Persistence is authoritative so an administrative clear performed by
			// another process also invalidates this process's cache.
			assignment, ok = persisted, persistedOK
			if !ok {
				delete(a.stickyPersonas, scope)
			}
		}
	}
	_, personaStillConfigured := config.Personas[assignment.Name]
	if ok && personaStillConfigured && (config.StickyDuration <= 0 || now.Sub(assignment.Timestamp) < config.StickyDuration) {
		assignment.Timestamp = now
		a.stickyPersonas[scope] = assignment
		return assignment.Name
	}
	personaName := a.randomPersonaName()
	assignment = personaAssignment{Name: personaName, Timestamp: now}
	a.stickyPersonas[scope] = assignment
	a.storePersonaAssignment(log, scope, assignment)
	return personaName
}

func (a *AIChat) storePersonaAssignment(log *zap.Logger, scope string, assignment personaAssignment) {
	if a.context == nil {
		return
	}
	if err := a.context.StorePersonaAssignment(scope, assignment); err != nil {
		log.Warn("Failed to persist persona assignment",
			botlogging.Operation("persist_persona_assignment"), zap.String("scope", scope), zap.Error(err))
	}
}

// randomPersonaName returns a random persona name from the configured personas
func (a *AIChat) randomPersonaName() string {
	config := a.configSnapshot()
	if len(config.Personas) == 0 {
		// Fallback to default persona if no personas configured
		return "default"
	}

	personaNames := make([]string, 0, len(config.Personas))
	for name := range config.Personas {
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
type contextTurn struct {
	role      string
	text      string
	timestamp time.Time
	priority  int // live Slack context wins ties with stored memory
	order     int // stable ordering when timestamps are unavailable/equal
}

func estimateTokens(text string) int {
	return max(1, (len([]rune(text))+3)/4)
}

// selectContextTurns applies one newest-first message and token budget after all
// context sources have been combined. Returned turns are chronological for the API.
func selectContextTurns(turns []contextTurn, maxMessages, maxTokens int) []contextTurn {
	if maxMessages <= 0 {
		maxMessages = 50
	}
	if maxTokens <= 0 {
		maxTokens = 2000
	}
	for i := range turns {
		for j := i + 1; j < len(turns); j++ {
			if turns[j].timestamp.After(turns[i].timestamp) ||
				(turns[j].timestamp.Equal(turns[i].timestamp) &&
					(turns[j].priority > turns[i].priority ||
						(turns[j].priority == turns[i].priority && turns[j].order > turns[i].order))) {
				turns[i], turns[j] = turns[j], turns[i]
			}
		}
	}
	selected := make([]contextTurn, 0, min(maxMessages, len(turns)))
	tokens := 0
	for _, turn := range turns {
		if len(selected) >= maxMessages || tokens+estimateTokens(turn.text) > maxTokens {
			continue
		}
		selected = append(selected, turn)
		tokens += estimateTokens(turn.text)
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	return selected
}

func (a *AIChat) buildMessages(input string, u UserDetails, personaName string, storedContext []ConversationContext, liveContext []slackContextMessage) []llms.MessageContent {
	config := a.configSnapshot()
	// Live Slack history is authoritative for an active channel/thread. Stored
	// memory is only a fallback, preventing duplicate turns and temporal inversion.
	if len(liveContext) > 0 {
		storedContext = nil
	}
	persona := config.Personas[personaName]
	if persona == "" {
		persona = personas[personaName]
	}
	if persona == "" {
		persona = defaultPersonaPrompt
	}

	turns := make([]contextTurn, 0, len(liveContext)+len(storedContext))
	seen := make(map[string]bool)
	hasBotResponses := false
	var oldest time.Time
	for _, msg := range liveContext {
		if strings.TrimSpace(msg.Text) == "" {
			continue
		}
		role := "human"
		if msg.IsSelf {
			role = "ai"
			hasBotResponses = true
		}
		text := msg.Text
		if role == "human" {
			name := msg.SenderName
			if name == "" {
				name = msg.SenderID
			}
			if name == "" {
				name = "other participant"
			}
			text = fmt.Sprintf("[%s]: %s", name, text)
		}
		turns = append(turns, contextTurn{role: role, text: text, timestamp: msg.Timestamp, priority: 1, order: len(turns)})
		seen[role+"\x00"+msg.Text] = true
		if !msg.Timestamp.IsZero() && (oldest.IsZero() || msg.Timestamp.Before(oldest)) {
			oldest = msg.Timestamp
		}
	}
	for _, ctx := range storedContext {
		if ctx.Role != "human" && ctx.Role != "assistant" || strings.TrimSpace(ctx.Message) == "" {
			continue
		}
		role := "human"
		if ctx.Role == "assistant" {
			role = "ai"
			hasBotResponses = true
		}
		key := role + "\x00" + ctx.Message
		if seen[key] {
			continue
		}
		text := ctx.Message
		if role == "human" {
			text = "[user]: " + text
		}
		turns = append(turns, contextTurn{role: role, text: text, timestamp: ctx.Timestamp, order: len(turns)})
	}
	selected := selectContextTurns(turns, config.MaxContextMessages, config.MaxContextTokens)
	guidance := ""
	if len(selected) > 0 {
		guidance += "\nConversation is active; respond to the current message and build on relevant context."
	}
	if hasBotResponses {
		guidance += "\nDon't repeat prior wording, punchlines, or observations."
	}
	if !oldest.IsZero() && time.Since(oldest) > 30*time.Minute {
		guidance += fmt.Sprintf("\nOlder context is less important; weight recent messages more heavily (%s back).", formatContextAge(time.Since(oldest).Round(time.Minute)))
	}
	targetName := u.FirstName
	if targetName == "" {
		targetName = u.Username
	}
	targetHint := ""
	if targetName != "" {
		targetHint = fmt.Sprintf(" You are responding to %s specifically.", targetName)
	}
	nameHint := ""
	if u.FirstName != "" {
		nameHint = fmt.Sprintf(" Use %s's name occasionally, not mechanically.", u.FirstName)
	}
	mentionHint := ""
	if u.UserID != "" {
		mentionHint = fmt.Sprintf(" Do not mention anyone from context; a direct reply may use <@%s> only when natural.", u.UserID)
	}
	systemPrompt := fmt.Sprintf(`%s

Be relevant and grounded. Match the conversation's tone. Answer sincere, technical, and emotional content with appropriate care and substance. Use humor only when it is earned; avoid canned phrases, repetition, and forced gimmicks. Adapt response length to the user's request: concise for casual chat and as detailed as needed for a real question. Do not claim actions or facts that are not supported by the conversation.%s%s%s%s`, persona, targetHint, nameHint, mentionHint, guidance)
	messages := []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt)}
	for _, turn := range selected {
		role := llms.ChatMessageTypeHuman
		if turn.role == "ai" {
			role = llms.ChatMessageTypeAI
		}
		messages = append(messages, llms.TextParts(role, turn.text))
	}
	currentSpeaker := u.FirstName
	if currentSpeaker == "" {
		currentSpeaker = u.Username
	}
	if currentSpeaker == "" {
		currentSpeaker = u.UserID
	}
	if currentSpeaker == "" {
		currentSpeaker = "user"
	}
	messages = append(messages, llms.TextParts(llms.ChatMessageTypeHuman, fmt.Sprintf("[%s]: %s", currentSpeaker, input)))
	return messages
}

func (a *AIChat) generateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	started := time.Now()
	response, err := a.ai.GenerateContent(ctx, messages, options...)
	a.generationCount.Add(1)
	a.generationNanos.Add(time.Since(started).Nanoseconds())
	return response, err
}

type generationProfile struct {
	Temperature float64
	MaxTokens   int
}

// generationProfileForInput keeps generation predictable while allowing enough room
// for questions and substantive technical or emotional messages.
func generationProfileForInput(input string) generationProfile {
	text := strings.TrimSpace(strings.ToLower(input))
	if isShortAcknowledgement(text) {
		return generationProfile{Temperature: 0.4, MaxTokens: 64}
	}
	technical := []string{"code", "error", "bug", "api", "sql", "deploy", "config", "function", "how do", "why does"}
	for _, word := range technical {
		if strings.Contains(text, word) {
			return generationProfile{Temperature: 0.25, MaxTokens: 240}
		}
	}
	emotional := []string{"feel", "feeling", "sad", "angry", "frustrated", "excited", "worried", "stressed", "upset"}
	for _, word := range emotional {
		if strings.Contains(text, word) {
			return generationProfile{Temperature: 0.55, MaxTokens: 160}
		}
	}
	if strings.Contains(text, "?") {
		return generationProfile{Temperature: 0.35, MaxTokens: 200}
	}
	if len([]rune(text)) <= 24 {
		return generationProfile{Temperature: 0.4, MaxTokens: 64}
	}
	return generationProfile{Temperature: 0.45, MaxTokens: 160}
}

func isShortAcknowledgement(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.Trim(text, "!.,:;?—- ")
	switch text {
	case "lol", "lmao", "nice", "thanks", "thank you", "thx", "wow", "ok", "okay", "great":
		return true
	default:
		return false
	}
}

func acknowledgementReaction() string {
	return random.String([]string{"+1", "heart", "raised_hands", "tada"})
}

func (a *AIChat) reactToAcknowledgement(ctx context.Context, m eventMessage, reaction string) {
	if m.TimeStamp == "" || a.slack.Client() == nil {
		return
	}
	if err := a.slack.Client().AddReactionContext(ctx, reaction, slack.ItemRef{Channel: m.Channel, Timestamp: m.TimeStamp}); err != nil {
		botlogging.FromContext(ctx, a.log).Debug("Failed to react to acknowledgement",
			botlogging.Operation("add_acknowledgement_reaction"), zap.Error(err))
	}
}

const fallbackResponse = "I’m having a little trouble thinking right now—please try again."

func (a *AIChat) postFallback(ctx context.Context, m eventMessage) {
	options := []slack.MsgOption{slack.MsgOptionText(fallbackResponse, false), slack.MsgOptionAsUser(true)}
	if m.ThreadTimeStamp != "" {
		options = append(options, slack.MsgOptionTS(m.ThreadTimeStamp))
	}
	if _, _, err := a.slack.Client().PostMessageContext(ctx, m.Channel, options...); err != nil {
		botlogging.FromContext(ctx, a.log).Error("Failed to post fallback response",
			botlogging.Operation("post_fallback_response"), zap.String("channel", m.Channel), zap.Error(err))
	}
}

// generationOptions returns model-compatible options for an AI chat response.
// Response length is bounded with max tokens rather than stop sequences because
// some models reject the stop parameter.
func generationOptions(temperature float64, maxTokens int) []llms.CallOption {
	return []llms.CallOption{
		llms.WithTemperature(temperature),
		llms.WithMaxTokens(maxTokens),
		llms.WithFrequencyPenalty(0.35),
		llms.WithPresencePenalty(0.15),
	}
}

// calculateDropChance determines the probability of dropping a message based on engagement factors
func (a *AIChat) calculateDropChance(userID, channelID, text string) float64 {
	baseDropChance := 0.25
	config := a.configSnapshot()

	if a.context != nil {
		recentContext, err := a.context.GetRecentContext(userID, channelID, &config)
		if err == nil && len(recentContext) > 0 {
			var lastBotResponseTime time.Time
			for i := len(recentContext) - 1; i >= 0; i-- {
				if recentContext[i].Role == "assistant" {
					lastBotResponseTime = recentContext[i].Timestamp
					break
				}
			}

			if !lastBotResponseTime.IsZero() {
				timeSinceLastReply := time.Since(lastBotResponseTime)

				switch {
				case timeSinceLastReply < 2*time.Minute:
					baseDropChance = 0.45
				case timeSinceLastReply < 10*time.Minute:
					baseDropChance = 0.35
				case timeSinceLastReply < 30*time.Minute:
					baseDropChance = 0.30
				}

				// Back off if we've been too chatty — preserves anti-spam safety valve.
				recentBotMessages := 0
				for _, ctx := range recentContext {
					if ctx.Role == "assistant" && time.Since(ctx.Timestamp) < 5*time.Minute {
						recentBotMessages++
					}
				}
				if recentBotMessages >= 3 {
					baseDropChance += 0.2
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
