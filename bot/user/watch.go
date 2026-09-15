package user

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
	botlogging "slackbot.arpa/bot/logging"
)

const watchInterval = time.Minute
const stateSchemaVersion = 2

var ErrAlreadyStarted = errors.New("user watcher already started")

type slackService interface{ OrgURL() string }

type userSlackAPI interface {
	GetUsersContext(context.Context) ([]slack.User, error)
	PostMessageContext(context.Context, string, ...slack.MsgOption) (string, string, error)
	GetConversationInfoContext(context.Context, *slack.GetConversationInfoInput) (*slack.Channel, error)
	AuthTestContext(context.Context) (*slack.AuthTestResponse, error)
}

type clientAPI struct{ client *slack.Client }

func (c clientAPI) GetUsersContext(ctx context.Context) ([]slack.User, error) {
	return c.client.GetUsersContext(ctx)
}
func (c clientAPI) PostMessageContext(ctx context.Context, channel string, options ...slack.MsgOption) (string, string, error) {
	return c.client.PostMessageContext(ctx, channel, options...)
}
func (c clientAPI) GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error) {
	return c.client.GetConversationInfoContext(ctx, input)
}
func (c clientAPI) AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error) {
	return c.client.AuthTestContext(ctx)
}

type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
}
type Config struct {
	NotifyChannel string
	DataDir       string
}
type FileConfig struct {
	NotifyChannel *string `json:"notify_channel" yaml:"notify_channel"`
}

type notification struct {
	Type string `json:"type"`
	User User   `json:"user,omitempty"`
}
type persistedState struct {
	SchemaVersion   int            `json:"schema_version"`
	WorkspaceID     string         `json:"workspace_id"`
	Users           []User         `json:"users"`
	Pending         []notification `json:"pending_notifications,omitempty"`
	StartupNotified bool           `json:"startup_notified"`
}

type UserWatch struct {
	log             *zap.Logger
	slack           slackService
	api             userSlackAPI
	lifecycleMu     sync.Mutex
	saveMu          sync.Mutex
	stateMu         sync.Mutex
	notifyChannel   string
	knownUsers      map[string]*slack.User
	workspaceID     string
	pending         []notification
	startupNotified bool
	usersFile       string
	stateFile       string
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	cancel          context.CancelFunc
	workerDone      chan struct{}
	started         bool
	eventCh         chan slackevents.EventsAPIEvent
	pendingEvents   atomic.Int64
}

// NewUserWatch accepts the Slack service while retaining compatibility with callers that
// provide only the historical Client and OrgURL methods.
func NewUserWatch(log *zap.Logger, c Config, s interface{ OrgURL() string }) *UserWatch {
	log = botlogging.Component(log, "user-watch")
	service := slackService(s)
	var api userSlackAPI
	if candidate, ok := s.(userSlackAPI); ok {
		api = candidate
	}
	if api == nil {
		if candidate, ok := s.(interface{ Client() *slack.Client }); ok {
			if client := candidate.Client(); client != nil {
				api = clientAPI{client: client}
			}
		}
	}
	usersFile, stateFile := "", ""
	if c.DataDir != "" {
		if _, err := os.Stat(c.DataDir); os.IsNotExist(err) {
			if err := os.MkdirAll(c.DataDir, 0750); err != nil {
				log.Error("create user watcher data directory", zap.Error(err))
			}
		}
		usersFile = filepath.Join(c.DataDir, "users.json")
		stateFile = filepath.Join(c.DataDir, "user-watch-state.json")
	}
	return &UserWatch{log: log, slack: service, api: api, notifyChannel: c.NotifyChannel, knownUsers: make(map[string]*slack.User), usersFile: usersFile, stateFile: stateFile, eventCh: make(chan slackevents.EventsAPIEvent, 1)}
}

func (o *UserWatch) Start(ctx context.Context) error {
	o.lifecycleMu.Lock()
	defer o.lifecycleMu.Unlock()

	o.stateMu.Lock()
	if o.lifecycleCtx != nil {
		o.stateMu.Unlock()
		return ErrAlreadyStarted
	}
	channel := o.notifyChannel
	o.stateMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(ctx)
	o.stateMu.Lock()
	o.lifecycleCtx = lifecycleCtx
	o.lifecycleCancel = lifecycleCancel
	o.stateMu.Unlock()
	if channel == "" {
		o.log.Info("User watcher disabled - no notification channel configured")
		return nil
	}
	if err := o.startActive(lifecycleCtx, channel); err != nil {
		lifecycleCancel()
		o.stateMu.Lock()
		o.lifecycleCtx = nil
		o.lifecycleCancel = nil
		o.stateMu.Unlock()
		return err
	}
	return nil
}

// startActive starts reconciliation. lifecycleMu must be held by the caller.
func (o *UserWatch) startActive(ctx context.Context, channel string) error {
	if o.api == nil {
		return fmt.Errorf("slack watcher API is unavailable")
	}
	if !o.validateChannel(ctx, channel) {
		return fmt.Errorf("notification channel is invalid or inaccessible")
	}

	state, err := o.loadState()
	if err != nil {
		return fmt.Errorf("load user watcher state: %w", err)
	}
	auth, err := o.api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("get Slack workspace identity: %w", err)
	}
	workspaceID := auth.TeamID
	if workspaceID == "" {
		return fmt.Errorf("slack workspace identity is empty")
	}
	previous := map[string]*slack.User(nil)
	o.stateMu.Lock()
	// Replace, rather than append, so a stop/start on the same instance cannot
	// duplicate notifications loaded from durable state.
	o.pending = nil
	o.startupNotified = false
	o.stateMu.Unlock()
	if state != nil && state.SchemaVersion == stateSchemaVersion && state.WorkspaceID == workspaceID {
		previous = stateUsers(state.Users)
		o.stateMu.Lock()
		o.pending = append([]notification(nil), state.Pending...)
		o.startupNotified = state.StartupNotified
		o.stateMu.Unlock()
	} else if state != nil && state.SchemaVersion == 1 {
		o.log.Info("migrating legacy user baseline without emitting unverified changes")
	} else if state != nil && state.WorkspaceID != workspaceID {
		o.log.Warn("resetting user baseline because Slack workspace changed", zap.String("previous_workspace", state.WorkspaceID), zap.String("workspace", workspaceID))
	}

	current, err := o.fetchUsers(ctx)
	if err != nil {
		return fmt.Errorf("fetch initial user list: %w", err)
	}
	o.stateMu.Lock()
	o.knownUsers = current
	o.workspaceID = workspaceID
	o.notifyChannel = channel
	o.started = true
	o.cancel = nil
	o.workerDone = make(chan struct{})
	done := o.workerDone
	o.stateMu.Unlock()
	if previous != nil {
		o.queueDiff(ctx, previous, current)
	}
	if err := o.saveState(); err != nil {
		o.log.Warn("save initial user watcher state", zap.Error(err))
	}

	watchCtx, cancel := context.WithCancel(ctx)
	o.stateMu.Lock()
	o.cancel = cancel
	o.stateMu.Unlock()
	o.stateMu.Lock()
	startupNotified := o.startupNotified
	o.stateMu.Unlock()
	if !startupNotified {
		o.retryPending(watchCtx)
		o.stateMu.Lock()
		startupNotified = o.startupNotified
		startupPending := false
		for _, event := range o.pending {
			if event.Type == "startup" {
				startupPending = true
				break
			}
		}
		o.stateMu.Unlock()
		if !startupNotified && !startupPending {
			o.sendStartup(watchCtx)
		}
	}
	go o.worker(watchCtx, done)
	return nil
}

func (o *UserWatch) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Publish and invoke cancellation before waiting for a concurrent Start that
	// may be blocked in Slack I/O while holding lifecycleMu.
	o.stateMu.Lock()
	lifecycleCancel := o.lifecycleCancel
	workerCancel := o.cancel
	o.stateMu.Unlock()
	if lifecycleCancel != nil {
		lifecycleCancel()
	}
	if workerCancel != nil {
		workerCancel()
	}

	o.lifecycleMu.Lock()
	defer o.lifecycleMu.Unlock()
	o.stateMu.Lock()
	lifecycleCtx := o.lifecycleCtx
	o.stateMu.Unlock()
	if lifecycleCtx == nil {
		return nil
	}
	var err error
	if o.isStarted() {
		err = o.stopActive(ctx)
	}
	o.stateMu.Lock()
	o.lifecycleCtx = nil
	o.lifecycleCancel = nil
	o.stateMu.Unlock()
	return err
}

func (o *UserWatch) isStarted() bool {
	o.stateMu.Lock()
	defer o.stateMu.Unlock()
	return o.started
}

// stopActive stops the worker and clears active lifecycle state even when the
// final durable save fails. lifecycleMu must be held by the caller.
func (o *UserWatch) stopActive(ctx context.Context) error {
	o.stateMu.Lock()
	cancel, done := o.cancel, o.workerDone
	o.stateMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := o.saveState()
	o.stateMu.Lock()
	o.started = false
	o.cancel = nil
	o.workerDone = nil
	o.stateMu.Unlock()
	return err
}

func (o *UserWatch) worker(ctx context.Context, done chan struct{}) {
	defer func() {
		o.stateMu.Lock()
		if o.workerDone == done {
			o.started = false
			o.cancel = nil
			o.workerDone = nil
			if o.lifecycleCtx != nil && o.lifecycleCtx.Err() != nil {
				o.lifecycleCtx = nil
				o.lifecycleCancel = nil
			}
		}
		o.stateMu.Unlock()
		close(done)
	}()
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if o.isEnabled() {
				o.retryPending(ctx)
				if err := o.checkForUserChanges(ctx); err != nil {
					o.log.Error("reconcile Slack users", botlogging.Operation("reconcile_users"), zap.Error(err))
				}
			}
		case event := <-o.eventCh:
			if o.isEnabled() {
				if err := o.checkForUserChanges(ctx); err != nil {
					botlogging.ForSlackEvent(o.log, event).Error("reconcile Slack users after event",
						botlogging.Operation("reconcile_users"), zap.Error(err))
				}
			}
			o.pendingEvents.Add(-1)
		case <-ctx.Done():
			return
		}
	}
}

func (o *UserWatch) PushEvent(event slackevents.EventsAPIEvent) error {
	if !o.isEnabled() {
		return nil
	}
	if event.InnerEvent.Type == "team_join" || event.InnerEvent.Type == "user_change" {
		o.pendingEvents.Add(1)
		select {
		case o.eventCh <- event:
		default:
			o.pendingEvents.Add(-1)
		}
	}
	return nil
}

func (o *UserWatch) PendingEvents() int64 { return o.pendingEvents.Load() }

func (o *UserWatch) ProcessorType() string { return "user-watch" }

// UpdateNotifyChannel validates a replacement before making it visible to workers.
// If Start was called while disabled, enabling the channel starts reconciliation
// on the long-lived Start context rather than on this short-lived callback context.
func (o *UserWatch) UpdateNotifyChannel(ctx context.Context, channel string) error {
	o.lifecycleMu.Lock()
	defer o.lifecycleMu.Unlock()
	channel = strings.TrimSpace(channel)
	if ctx == nil {
		ctx = context.Background()
	}
	o.stateMu.Lock()
	oldChannel := o.notifyChannel
	lifecycleCtx := o.lifecycleCtx
	started := o.started
	o.stateMu.Unlock()
	if channel == oldChannel {
		return nil
	}
	if channel != "" {
		if o.api == nil || !o.validateChannel(ctx, channel) {
			return fmt.Errorf("notification channel is invalid or inaccessible")
		}
	}

	if channel == "" && started {
		o.stateMu.Lock()
		o.notifyChannel = ""
		o.stateMu.Unlock()
		return o.stopActive(ctx)
	}
	if channel != "" && lifecycleCtx != nil && !started {
		o.stateMu.Lock()
		o.notifyChannel = channel
		o.stateMu.Unlock()
		if err := o.startActive(lifecycleCtx, channel); err != nil {
			o.stateMu.Lock()
			o.notifyChannel = oldChannel
			o.stateMu.Unlock()
			return err
		}
		return nil
	}
	o.stateMu.Lock()
	o.notifyChannel = channel
	o.stateMu.Unlock()
	o.log.Info("user watcher notification channel updated", zap.String("channel", channel), zap.Bool("enabled", channel != ""))
	return nil
}

func (o *UserWatch) fetchUsers(ctx context.Context) (map[string]*slack.User, error) {
	users, err := o.api.GetUsersContext(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]*slack.User, len(users))
	for _, user := range users {
		if isValidUser(user) {
			copy := user
			result[user.ID] = &copy
		} else if user.ID != "" {
			o.log.Debug("skipping non-human Slack account", zap.String("user_id", user.ID), zap.Bool("bot", user.IsBot), zap.Bool("guest", user.IsRestricted || user.IsUltraRestricted), zap.Bool("deleted", user.Deleted))
		}
	}
	return result, nil
}

func isValidUser(user slack.User) bool {
	return user.ID != "" && !user.Deleted && !user.IsBot && !user.IsRestricted && !user.IsUltraRestricted
}

func (o *UserWatch) checkForUserChanges(ctx context.Context) error {
	o.stateMu.Lock()
	current := copyUsers(o.knownUsers)
	o.stateMu.Unlock()
	newUsers, err := o.fetchUsers(ctx)
	if err != nil {
		return err
	}
	var removed, added []slack.User
	modified := 0
	for id, user := range current {
		if _, ok := newUsers[id]; !ok {
			removed = append(removed, *user)
		}
	}
	for id, user := range newUsers {
		old, ok := current[id]
		if !ok {
			added = append(added, *user)
		} else if old.Name != user.Name || old.RealName != user.RealName {
			modified++
		}
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i].ID < removed[j].ID })
	sort.Slice(added, func(i, j int) bool { return added[i].ID < added[j].ID })
	o.stateMu.Lock()
	o.knownUsers = newUsers
	o.stateMu.Unlock()
	events := make([]notification, 0, len(removed)+len(added))
	for i := range removed {
		events = append(events, notification{Type: "deactivated_or_removed", User: toUser(removed[i])})
	}
	for i := range added {
		events = append(events, notification{Type: "added", User: toUser(added[i])})
	}
	if len(events) > 0 {
		o.deliverBatchOrQueue(ctx, events)
	}
	if len(removed)+len(added)+modified > 0 {
		o.log.Info("reconciled user changes", zap.Int("removed", len(removed)), zap.Int("added", len(added)), zap.Int("modified", modified), zap.Int("pending", o.pendingCount()))
		return o.saveState()
	}
	return nil
}

func (o *UserWatch) queueDiff(ctx context.Context, previous, current map[string]*slack.User) {
	var removed, added []string
	for id := range previous {
		if _, ok := current[id]; !ok {
			removed = append(removed, id)
		}
	}
	for id := range current {
		if _, ok := previous[id]; !ok {
			added = append(added, id)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	events := make([]notification, 0, len(removed)+len(added))
	for _, id := range removed {
		events = append(events, notification{Type: "deactivated_or_removed", User: toUser(*previous[id])})
	}
	for _, id := range added {
		events = append(events, notification{Type: "added", User: toUser(*current[id])})
	}
	if len(events) > 0 {
		o.deliverBatchOrQueue(ctx, events)
	}
}

func toUser(u slack.User) User { return User{ID: u.ID, Name: u.Name, RealName: u.RealName} }
func copyUsers(in map[string]*slack.User) map[string]*slack.User {
	out := make(map[string]*slack.User, len(in))
	for id, u := range in {
		c := *u
		out[id] = &c
	}
	return out
}
func stateUsers(users []User) map[string]*slack.User {
	out := make(map[string]*slack.User, len(users))
	for _, u := range users {
		c := slack.User{ID: u.ID, Name: u.Name, RealName: u.RealName}
		out[u.ID] = &c
	}
	return out
}

func (o *UserWatch) deliverBatchOrQueue(ctx context.Context, events []notification) {
	if o.sendBatch(ctx, events) {
		return
	}
	o.stateMu.Lock()
	o.pending = append(o.pending, events...)
	pending := len(o.pending)
	o.stateMu.Unlock()
	for _, event := range events {
		o.log.Error("Slack notification queued for retry", zap.String("event_type", event.Type), zap.String("user_id", event.User.ID), zap.Int("pending_count", pending))
	}
	if err := o.saveState(); err != nil {
		o.log.Error("persist notification queue", zap.Error(err))
	}
}

func (o *UserWatch) retryPending(ctx context.Context) {
	o.stateMu.Lock()
	pending := append([]notification(nil), o.pending...)
	o.stateMu.Unlock()
	if len(pending) == 0 {
		return
	}
	remaining := pending[:0]
	for _, event := range pending {
		if !o.sendNotification(ctx, event) {
			remaining = append(remaining, event)
		}
	}
	o.stateMu.Lock()
	o.pending = remaining
	count := len(remaining)
	o.stateMu.Unlock()
	o.log.Info("retried Slack notifications", zap.Int("pending_count", count))
	if len(remaining) != len(pending) {
		if err := o.saveState(); err != nil {
			o.log.Error("persist notification queue", zap.Error(err))
		}
	}
}

func (o *UserWatch) sendBatch(ctx context.Context, events []notification) bool {
	if len(events) == 0 {
		return true
	}
	if len(events) == 1 {
		return o.sendNotification(ctx, events[0])
	}
	o.stateMu.Lock()
	channel := o.notifyChannel
	count := len(o.knownUsers)
	o.stateMu.Unlock()
	if channel == "" {
		return false
	}
	lines := make([]string, 0, len(events))
	for _, event := range events {
		name := displayName(event.User)
		identity := "*" + escapeMrkdwn(name) + "*"
		verb := "added"
		if event.Type == "deactivated_or_removed" {
			verb = "deactivated or removed"
		}
		lines = append(lines, fmt.Sprintf("• User %s has been %s from the Slack organization.", identity, verb))
	}
	attachment := slack.Attachment{Color: "#36a64f", Title: "User membership changes", Text: strings.Join(lines, "\n"), Footer: fmt.Sprintf("Monitoring %d users", count), FooterIcon: "https://platform.slack-edge.com/img/default_application_icon.png", Ts: json.Number(fmt.Sprintf("%d", time.Now().Unix()))}
	_, _, err := o.api.PostMessageContext(ctx, channel, slack.MsgOptionAttachments(attachment), slack.MsgOptionAsUser(true))
	if err == nil {
		return true
	}
	o.log.Error("send Slack notification batch", zap.String("event_type", "reconciliation_batch"), zap.String("user_id", ""), zap.Int("pending_count", o.pendingCount()), zap.Error(err))
	return false
}

func (o *UserWatch) sendNotification(ctx context.Context, event notification) bool {
	o.stateMu.Lock()
	channel := o.notifyChannel
	count := len(o.knownUsers)
	o.stateMu.Unlock()
	if channel == "" {
		return false
	}
	if event.Type == "startup" {
		attachment := slack.Attachment{Color: "#36a64f", Title: "Status", Text: "🟢 *Slack user monitoring feature is now running*", Footer: fmt.Sprintf("Monitoring %d users", count), FooterIcon: "https://platform.slack-edge.com/img/default_application_icon.png", Ts: json.Number(fmt.Sprintf("%d", time.Now().Unix()))}
		_, _, err := o.api.PostMessageContext(ctx, channel, slack.MsgOptionAttachments(attachment), slack.MsgOptionAsUser(true))
		if err == nil {
			o.stateMu.Lock()
			o.startupNotified = true
			o.stateMu.Unlock()
			return true
		}
		o.log.Error("send Slack notification", zap.String("event_type", event.Type), zap.String("user_id", ""), zap.Int("pending_count", o.pendingCount()), zap.Error(err))
		return false
	}
	title, text, color := "User Added", "has been added", "#36a64f"
	if event.Type == "deactivated_or_removed" {
		title, text, color = "User Deactivated or Removed", "has been deactivated or removed", "#FF5733"
	}
	name := displayName(event.User)
	identity := fmt.Sprintf("*%s*", escapeMrkdwn(name))
	if event.User.RealName != "" && event.User.RealName != event.User.Name {
		identity = fmt.Sprintf("*%s* (%s)", escapeMrkdwn(event.User.RealName), escapeMrkdwn(event.User.Name))
	}
	message := fmt.Sprintf("User %s %s from the Slack organization.", identity, text)
	orgURL := ""
	if o.slack != nil {
		orgURL = o.slack.OrgURL()
	}
	actions := []slack.AttachmentAction{{Type: "button", Text: "View Profile", URL: fmt.Sprintf("%steam/%s", orgURL, event.User.ID)}}
	if event.User.RealName != "" {
		actions = append(actions, slack.AttachmentAction{Type: "button", Text: "View LinkedIn", URL: linkedinURL(event.User.RealName)})
	}
	attachment := slack.Attachment{Color: color, Title: ":wave: " + title, Text: message, Footer: fmt.Sprintf("User ID: %s; Monitoring %d users", event.User.ID, count), FooterIcon: "https://platform.slack-edge.com/img/default_application_icon.png", Ts: json.Number(fmt.Sprintf("%d", time.Now().Unix())), Actions: actions}
	_, _, err := o.api.PostMessageContext(ctx, channel, slack.MsgOptionAttachments(attachment), slack.MsgOptionAsUser(true))
	if err == nil {
		return true
	}
	o.log.Error("send Slack notification", zap.String("event_type", event.Type), zap.String("user_id", event.User.ID), zap.Int("pending_count", o.pendingCount()), zap.Error(err))
	return false
}

func displayName(u User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.RealName
}
func escapeMrkdwn(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}
func linkedinURL(name string) string {
	return "https://www.linkedin.com/search/results/people/?keywords=" + strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
}

func (o *UserWatch) sendStartup(ctx context.Context) {
	if !o.sendNotification(ctx, notification{Type: "startup"}) {
		o.stateMu.Lock()
		o.pending = append(o.pending, notification{Type: "startup"})
		o.stateMu.Unlock()
		o.log.Error("Slack startup notification queued for retry", zap.String("event_type", "startup"), zap.String("user_id", ""), zap.Int("pending_count", o.pendingCount()))
		_ = o.saveState()
	} else {
		_ = o.saveState()
	}
}
func (o *UserWatch) isEnabled() bool {
	o.stateMu.Lock()
	defer o.stateMu.Unlock()
	return o.notifyChannel != ""
}

func (o *UserWatch) pendingCount() int {
	o.stateMu.Lock()
	defer o.stateMu.Unlock()
	return len(o.pending)
}

func (o *UserWatch) saveState() error {
	if o.usersFile == "" && o.stateFile == "" {
		return nil
	}
	o.saveMu.Lock()
	defer o.saveMu.Unlock()
	o.stateMu.Lock()
	users := make([]User, 0, len(o.knownUsers))
	for _, u := range o.knownUsers {
		users = append(users, toUser(*u))
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	state := persistedState{SchemaVersion: stateSchemaVersion, WorkspaceID: o.workspaceID, Users: users, Pending: append([]notification(nil), o.pending...), StartupNotified: o.startupNotified}
	o.stateMu.Unlock()
	stateData, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if o.stateFile != "" {
		if err := writeJSONFile(o.stateFile, stateData); err != nil {
			return err
		}
	}
	// users.json remains the legacy array consumed by aichat and other callers.
	if o.usersFile != "" {
		usersData, err := json.MarshalIndent(users, "", "  ")
		if err != nil {
			return err
		}
		if err := writeJSONFile(o.usersFile, usersData); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONFile(path string, data []byte) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	return nil
}

func (o *UserWatch) loadState() (*persistedState, error) {
	if o.stateFile != "" {
		data, err := os.ReadFile(o.stateFile)
		if err == nil {
			var state persistedState
			if err := json.Unmarshal(data, &state); err != nil {
				return nil, fmt.Errorf("unmarshal user watcher state: %w", err)
			}
			return &state, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if o.usersFile == "" {
		return nil, nil
	}
	data, err := os.ReadFile(o.usersFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err == nil {
		return &state, nil
	}
	var users []User
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, err
	}
	return &persistedState{SchemaVersion: 1, Users: users}, nil
}

// Compatibility helpers for callers that used the original baseline-only storage methods.
func (o *UserWatch) saveUsersToDisk() error {
	if o.usersFile == "" {
		return nil
	}
	o.saveMu.Lock()
	defer o.saveMu.Unlock()
	o.stateMu.Lock()
	users := make([]User, 0, len(o.knownUsers))
	for _, u := range o.knownUsers {
		users = append(users, toUser(*u))
	}
	o.stateMu.Unlock()
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	return writeJSONFile(o.usersFile, data)
}
func (o *UserWatch) loadUsersFromDisk() (map[string]*slack.User, error) {
	if o.usersFile != "" {
		data, err := os.ReadFile(o.usersFile)
		if err == nil {
			var users []User
			if unmarshalErr := json.Unmarshal(data, &users); unmarshalErr == nil {
				return stateUsers(users), nil
			} else {
				var state persistedState
				if stateErr := json.Unmarshal(data, &state); stateErr == nil {
					return stateUsers(state.Users), nil
				}
				return nil, unmarshalErr
			}
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	state, err := o.loadState()
	if err != nil || state == nil {
		return nil, err
	}
	return stateUsers(state.Users), nil
}

func (o *UserWatch) validateChannel(ctx context.Context, channels ...string) bool {
	channel := ""
	if len(channels) > 0 {
		channel = channels[0]
	} else {
		o.stateMu.Lock()
		channel = o.notifyChannel
		o.stateMu.Unlock()
	}
	if channel == "" || len(channel) < 9 || (channel[0] != 'C' && channel[0] != 'G') || o.api == nil {
		return false
	}
	conversation, err := o.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channel, IncludeLocale: false})
	if err != nil || conversation == nil {
		return false
	}
	return !conversation.IsArchived && conversation.IsMember
}
