package vibecheck

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
	botlogging "slackbot.arpa/bot/logging"
	botmetrics "slackbot.arpa/bot/metrics"
	"slackbot.arpa/tools/random"
)

const (
	defaultKickDelay        = 5 * time.Second
	defaultRejoinKickDelay  = 2 * time.Second
	defaultKickRetryDelay   = time.Second
	defaultReinviteInterval = 10 * time.Second
	defaultMaximumKickTries = 3
	messageDedupeDuration   = 30 * time.Second
	eventQueueCapacity      = 100
)

var (
	pattern           = regexp.MustCompile(`(?i).*vibe.*`)
	ErrAlreadyStarted = errors.New("vibecheck already started")
)

type slackAPI interface {
	AddReactionContext(context.Context, string, slack.ItemRef) error
	PostMessageContext(context.Context, string, ...slack.MsgOption) (string, string, error)
	KickUserFromConversationContext(context.Context, string, string) error
	InviteUsersToConversationContext(context.Context, string, ...string) (*slack.Channel, error)
}

type FileConfig struct {
	GoodReactions []string       `json:"good_reactions" yaml:"good_reactions"`
	GoodText      []string       `json:"good_text" yaml:"good_text"`
	BadReactions  []string       `json:"bad_reactions" yaml:"bad_reactions"`
	BadText       []string       `json:"bad_text" yaml:"bad_text"`
	BanDuration   *time.Duration `json:"ban_duration" yaml:"ban_duration"`
}

type Config struct {
	Enabled        bool
	PreferredUsers []string
	DataDir        string
	BanDuration    time.Duration
	GoodReactions  []string
	GoodText       []string
	BadReactions   []string
	BadText        []string
}

type vibecheckRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	ticker *time.Ticker
	done   chan struct{}
	wake   chan struct{}
	space  chan struct{}
	wg     sync.WaitGroup
	err    error

	queueMu         sync.Mutex
	queue           []slackevents.EventsAPIEvent
	queueFullLogged atomic.Bool
	pendingEvents   atomic.Int64
}

func (r *vibecheckRun) enqueue(event slackevents.EventsAPIEvent) bool {
	for {
		if r.ctx.Err() != nil {
			return false
		}
		r.queueMu.Lock()
		if len(r.queue) < eventQueueCapacity {
			r.queue = append(r.queue, event)
			r.pendingEvents.Add(1)
			r.queueMu.Unlock()
			select {
			case r.wake <- struct{}{}:
			default:
			}
			return true
		}
		r.queueMu.Unlock()
		select {
		case <-r.ctx.Done():
			return false
		case <-r.space:
		}
	}
}

func (r *vibecheckRun) next() (slackevents.EventsAPIEvent, bool) {
	for {
		if r.ctx.Err() != nil {
			return slackevents.EventsAPIEvent{}, false
		}
		r.queueMu.Lock()
		if len(r.queue) > 0 {
			event := r.queue[0]
			r.queue[0] = slackevents.EventsAPIEvent{}
			r.queue = r.queue[1:]
			r.queueFullLogged.Store(false)
			r.queueMu.Unlock()
			select {
			case r.space <- struct{}{}:
			default:
			}
			return event, true
		}
		r.queueMu.Unlock()

		select {
		case <-r.ctx.Done():
			return slackevents.EventsAPIEvent{}, false
		case <-r.wake:
		}
	}
}

func (r *vibecheckRun) pending() int {
	r.queueMu.Lock()
	defer r.queueMu.Unlock()
	return len(r.queue)
}

// Vibecheck handles responding to messages to verify the user's vibe.
type Vibecheck struct {
	log      *zap.Logger
	configMu sync.RWMutex
	config   Config
	api      slackAPI

	lifecycleMu sync.Mutex
	run         atomic.Pointer[vibecheckRun]
	kickedUsers *kickedUsersManager
	dedupe      *messageDeduplicator
	metrics     *botmetrics.Metrics

	kickDelay        time.Duration
	rejoinKickDelay  time.Duration
	kickRetryDelay   time.Duration
	reinviteInterval time.Duration
	maximumKickTries int
}

func NewVibecheck(log *zap.Logger, config Config, api slackAPI, metricSet ...*botmetrics.Metrics) (*Vibecheck, error) {
	log = botlogging.Component(log, "vibecheck")
	kickedUsers, err := newKickedUsersManager(log, config.DataDir)
	if err != nil {
		return nil, fmt.Errorf("initialize kicked users: %w", err)
	}
	var m *botmetrics.Metrics
	if len(metricSet) > 0 {
		m = metricSet[0]
	}
	return &Vibecheck{
		log:              log,
		config:           config,
		api:              api,
		kickedUsers:      kickedUsers,
		dedupe:           newMessageDeduplicator(messageDedupeDuration),
		metrics:          m,
		kickDelay:        defaultKickDelay,
		rejoinKickDelay:  defaultRejoinKickDelay,
		kickRetryDelay:   defaultKickRetryDelay,
		reinviteInterval: defaultReinviteInterval,
		maximumKickTries: defaultMaximumKickTries,
	}, nil
}

func (c *Vibecheck) ProcessorType() string { return "vibecheck" }

func (c *Vibecheck) Start(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.run.Load() != nil {
		return ErrAlreadyStarted
	}
	if c.api == nil {
		return errors.New("vibecheck Slack API is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	runCtx, cancel := context.WithCancel(ctx)
	run := &vibecheckRun{
		ctx:    runCtx,
		cancel: cancel,
		ticker: time.NewTicker(c.reinviteInterval),
		done:   make(chan struct{}),
		wake:   make(chan struct{}, 1),
		space:  make(chan struct{}, 1),
	}
	run.wg.Add(2)
	c.run.Store(run)
	go c.handleEvents(run)
	go c.checkReinvites(run)
	go c.finishRun(run)
	c.log.Debug("Vibecheck feature started successfully.")
	return nil
}

func (c *Vibecheck) finishRun(run *vibecheckRun) {
	run.wg.Wait()
	run.ticker.Stop()
	run.err = c.kickedUsers.Flush()
	c.run.CompareAndSwap(run, nil)
	close(run.done)
}

func (c *Vibecheck) Stop(ctx context.Context) error {
	return c.stopRun(ctx, c.run.Load())
}

func (c *Vibecheck) stopRun(ctx context.Context, run *vibecheckRun) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if run == nil {
		return nil
	}
	run.cancel()

	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if current := c.run.Load(); current != nil && current != run {
		return nil
	}
	select {
	case <-run.done:
		if pending := run.pending(); pending > 0 {
			c.log.Warn("Discarding queued vibecheck events during shutdown", zap.Int("count", pending))
		}
		return run.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Vibecheck) PendingEvents() int64 {
	if run := c.run.Load(); run != nil {
		return run.pendingEvents.Load()
	}
	return 0
}

func (c *Vibecheck) PushEvent(event slackevents.EventsAPIEvent) error {
	if !isRelevantEvent(event) {
		return nil
	}
	run := c.run.Load()
	if run == nil {
		return nil
	}
	if run.pending() >= eventQueueCapacity && run.queueFullLogged.CompareAndSwap(false, true) {
		botlogging.ForSlackEvent(c.log, event).Warn("Vibecheck event queue is full; applying backpressure",
			botlogging.Operation("enqueue_event"), zap.Int("queue_capacity", eventQueueCapacity))
	}
	run.enqueue(event)
	return nil
}

func isRelevantEvent(event slackevents.EventsAPIEvent) bool {
	if event.Type != slackevents.CallbackEvent {
		return false
	}
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		return ev.BotID == "" && ev.User != "" && pattern.MatchString(strings.TrimSpace(ev.Text))
	case *slackevents.MemberJoinedChannelEvent:
		return ev.User != "" && ev.Channel != ""
	default:
		return false
	}
}

func (c *Vibecheck) handleEvents(run *vibecheckRun) {
	defer run.wg.Done()
	for {
		event, ok := run.next()
		if !ok {
			return
		}
		c.processEvent(run, event)
		run.pendingEvents.Add(-1)
	}
}

func (c *Vibecheck) processEvent(run *vibecheckRun, event slackevents.EventsAPIEvent) {
	log := botlogging.ForSlackEvent(c.log, event)
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		c.handleMessageEvent(run, log, ev)
	case *slackevents.MemberJoinedChannelEvent:
		c.handleMemberJoinedEvent(run, log, ev)
	}
}

func (c *Vibecheck) handleMessageEvent(run *vibecheckRun, log *zap.Logger, ev *slackevents.MessageEvent) {
	log.Debug("Processing message", botlogging.Operation("process_message"),
		zap.String("user", ev.User), zap.String("channel", ev.Channel), zap.String("type", c.ProcessorType()))
	if c.dedupe.IsDupe(ev.User, ev.Channel, ev.TimeStamp) {
		return
	}

	config := c.getConfig()
	if !config.Enabled {
		return
	}

	weight := 0.8
	if time.Now().Local().Weekday() == time.Wednesday {
		weight = 0.2
	}
	passed := random.Bool(weight)
	if c.metrics != nil {
		c.metrics.IncVibecheckOutcome(passed)
	}
	reaction := "vibecheck"
	if passed {
		reaction = "ok"
	}
	if err := c.api.AddReactionContext(run.ctx, reaction, slack.NewRefToMessage(ev.Channel, ev.TimeStamp)); err != nil {
		log.Error("Failed to add reaction", botlogging.Operation("add_reaction"),
			zap.String("channel", ev.Channel), zap.String("user", ev.User), zap.Error(err))
	}

	options := []slack.MsgOption{slack.MsgOptionText(randomResponse(passed, config), false), slack.MsgOptionAsUser(true)}
	if ev.ThreadTimeStamp != "" {
		options = append(options, slack.MsgOptionTS(ev.ThreadTimeStamp))
	}
	if _, _, err := c.api.PostMessageContext(run.ctx, ev.Channel, options...); err != nil {
		log.Error("Failed to post response", botlogging.Operation("post_response"),
			zap.String("channel", ev.Channel), zap.Error(err))
	}

	if passed || slices.Contains(config.PreferredUsers, ev.User) || slices.Contains(config.PreferredUsers, ev.Username) {
		return
	}
	ban, err := c.kickedUsers.AddKickedUser(ev.User, ev.Channel, config.BanDuration)
	if err != nil {
		log.Error("Failed to persist ban; user will not be kicked", botlogging.Operation("persist_ban"),
			zap.String("channel", ev.Channel), zap.String("user", ev.User), zap.Error(err))
		return
	}
	c.scheduleKickWithLog(run, log, c.kickDelay, ban, "low vibe")
}

func (c *Vibecheck) handleMemberJoinedEvent(run *vibecheckRun, log *zap.Logger, ev *slackevents.MemberJoinedChannelEvent) {
	user, banned := c.kickedUsers.IsUserBanned(ev.User, ev.Channel)
	if !banned {
		return
	}
	timeRemaining := time.Until(user.ReinviteAt)
	c.scheduleKickWithLog(run, log, c.rejoinKickDelay, user, "active ban")

	minutes := int(timeRemaining.Minutes())
	seconds := int(timeRemaining.Seconds()) % 60
	timeMessage := fmt.Sprintf("%d seconds", seconds)
	if minutes > 0 {
		timeMessage = fmt.Sprintf("%d minutes and %d seconds", minutes, seconds)
	}
	message := fmt.Sprintf("🚫 User is still banned for %s. Please wait before rejoining.", timeMessage)
	if _, _, err := c.api.PostMessageContext(run.ctx, ev.Channel, slack.MsgOptionText(message, false), slack.MsgOptionAsUser(true)); err != nil {
		log.Error("Failed to post ban time remaining message", botlogging.Operation("post_ban_status"),
			zap.String("channel", ev.Channel), zap.Error(err))
	}
}

func (c *Vibecheck) scheduleKick(run *vibecheckRun, delay time.Duration, ban kickedUser, reason string) {
	c.scheduleKickWithLog(run, c.log, delay, ban, reason)
}

func (c *Vibecheck) scheduleKickWithLog(
	run *vibecheckRun, log *zap.Logger, delay time.Duration, ban kickedUser, reason string,
) {
	if run.ctx.Err() != nil {
		return
	}
	run.pendingEvents.Add(1)
	run.wg.Add(1)
	go func() {
		defer run.wg.Done()
		defer run.pendingEvents.Add(-1)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-run.ctx.Done():
			return
		case <-timer.C:
		}

		retryDelay := c.kickRetryDelay
		for attempt := 1; attempt <= c.maximumKickTries; attempt++ {
			current, active := c.kickedUsers.IsUserBanned(ban.UserID, ban.ChannelID)
			if !active || !current.KickedAt.Equal(ban.KickedAt) {
				return
			}
			attemptCtx, cancel := context.WithDeadline(run.ctx, current.ReinviteAt)
			err := c.api.KickUserFromConversationContext(attemptCtx, ban.ChannelID, ban.UserID)
			cancel()
			if err == nil {
				log.Info("User kicked from channel", zap.String("channel", ban.ChannelID), zap.String("user", ban.UserID),
					zap.String("reason", reason), zap.Int("attempt", attempt))
				return
			}
			if run.ctx.Err() != nil {
				return
			}
			if attempt == c.maximumKickTries {
				log.Error("Failed to kick user from channel", botlogging.Operation("kick_user"),
					zap.String("channel", ban.ChannelID), zap.String("user", ban.UserID),
					zap.Int("attempts", attempt), zap.Error(err))
				return
			}
			log.Warn("Kick failed; retrying", botlogging.Operation("kick_user"),
				zap.String("channel", ban.ChannelID), zap.String("user", ban.UserID),
				zap.Int("attempt", attempt), zap.Error(err))
			timer.Reset(retryDelay)
			select {
			case <-run.ctx.Done():
				return
			case <-timer.C:
			}
			retryDelay *= 2
		}
	}()
}

func (c *Vibecheck) SetConfig(config Config) {
	c.configMu.Lock()
	c.config = config
	c.configMu.Unlock()
}

func (c *Vibecheck) getConfig() Config {
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.config
}

func (c *Vibecheck) checkReinvites(run *vibecheckRun) {
	defer run.wg.Done()
	for {
		select {
		case <-run.ctx.Done():
			return
		case <-run.ticker.C:
			c.processReinvites(run.ctx)
		}
	}
}

func (c *Vibecheck) processReinvites(ctx context.Context) {
	if err := c.kickedUsers.Flush(); err != nil {
		c.log.Error("Failed to retry kicked users persistence", zap.Error(err))
	}
	for _, user := range c.kickedUsers.GetUsersToReinvite() {
		if _, err := c.api.InviteUsersToConversationContext(ctx, user.ChannelID, user.UserID); err != nil {
			c.log.Error("Failed to reinvite user to channel", zap.String("channel", user.ChannelID),
				zap.String("user", user.UserID), zap.Error(err))
			continue
		}
		if err := c.kickedUsers.MarkReinvited(user); err != nil {
			c.log.Error("Reinvite succeeded but state was not persisted", zap.String("channel", user.ChannelID),
				zap.String("user", user.UserID), zap.Error(err))
		}
	}
	if err := c.kickedUsers.CleanupReinvitedUsers(); err != nil {
		c.log.Error("Failed to clean up reinvited users", zap.Error(err))
	}
}
