package vibecheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeKickedUsersStore struct {
	mu         sync.Mutex
	users      map[string]kickedUser
	loadErr    error
	saveErrors []error
}

func (s *fakeKickedUsersStore) Load() (map[string]kickedUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return cloneKickedUsers(s.users), nil
}

func (s *fakeKickedUsersStore) Save(users map[string]kickedUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.saveErrors) > 0 {
		err := s.saveErrors[0]
		s.saveErrors = s.saveErrors[1:]
		if err != nil {
			return err
		}
	}
	s.users = cloneKickedUsers(users)
	return nil
}

func (s *fakeKickedUsersStore) failNext(err error) {
	s.mu.Lock()
	s.saveErrors = append(s.saveErrors, err)
	s.mu.Unlock()
}

type fakeSlackAPI struct {
	mu           sync.Mutex
	kickErrors   []error
	inviteErrors []error
	kickCalls    int
	inviteCalls  int
	postCalls    int
	postBlock    <-chan struct{}
}

func (f *fakeSlackAPI) AddReactionContext(context.Context, string, slack.ItemRef) error { return nil }

func (f *fakeSlackAPI) PostMessageContext(ctx context.Context, _ string, _ ...slack.MsgOption) (string, string, error) {
	f.mu.Lock()
	f.postCalls++
	block := f.postBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-block:
		}
	}
	return "", "", nil
}

func (f *fakeSlackAPI) KickUserFromConversationContext(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kickCalls++
	if len(f.kickErrors) == 0 {
		return nil
	}
	err := f.kickErrors[0]
	f.kickErrors = f.kickErrors[1:]
	return err
}

func (f *fakeSlackAPI) InviteUsersToConversationContext(_ context.Context, _ string, _ ...string) (*slack.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inviteCalls++
	if len(f.inviteErrors) == 0 {
		return &slack.Channel{}, nil
	}
	err := f.inviteErrors[0]
	f.inviteErrors = f.inviteErrors[1:]
	if err != nil {
		return nil, err
	}
	return &slack.Channel{}, nil
}

func (f *fakeSlackAPI) counts() (kicks, invites, posts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kickCalls, f.inviteCalls, f.postCalls
}

type delayedKickAPI struct {
	fakeSlackAPI
	delay     time.Duration
	started   chan struct{}
	succeeded atomic.Bool
}

func (f *delayedKickAPI) KickUserFromConversationContext(ctx context.Context, _, _ string) error {
	f.mu.Lock()
	f.kickCalls++
	f.mu.Unlock()
	select {
	case f.started <- struct{}{}:
	default:
	}
	timer := time.NewTimer(f.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		f.succeeded.Store(true)
		return nil
	}
}

func newTestVibecheck(t *testing.T, api slackAPI) *Vibecheck {
	t.Helper()
	service, err := NewVibecheck(zap.NewNop(), Config{Enabled: true, DataDir: t.TempDir(), BanDuration: time.Minute}, api)
	require.NoError(t, err)
	return service
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	require.Eventually(t, condition, time.Second, time.Millisecond)
}

func addBan(t *testing.T, manager *kickedUsersManager, user, channel string, duration time.Duration) kickedUser {
	t.Helper()
	ban, err := manager.AddKickedUser(user, channel, duration)
	require.NoError(t, err)
	return ban
}

func TestConfigBanDuration(t *testing.T) {
	config := Config{BanDuration: 10 * time.Minute}
	require.Equal(t, 10*time.Minute, config.BanDuration)
}

func TestIsUserBanned(t *testing.T) {
	manager, err := newKickedUsersManager(zap.NewNop(), t.TempDir())
	require.NoError(t, err)

	_, banned := manager.IsUserBanned("user", "channel")
	require.False(t, banned)
	addBan(t, manager, "user", "channel", 5*time.Minute)
	user, banned := manager.IsUserBanned("user", "channel")
	require.True(t, banned)
	require.Equal(t, "user", user.UserID)
	require.Equal(t, "channel", user.ChannelID)

	addBan(t, manager, "expired", "channel", -time.Minute)
	_, banned = manager.IsUserBanned("expired", "channel")
	require.False(t, banned)
}

func TestReinviteRequiresSuccessConfirmation(t *testing.T) {
	manager, err := newKickedUsersManager(zap.NewNop(), t.TempDir())
	require.NoError(t, err)
	addBan(t, manager, "user", "channel", -time.Minute)
	require.Len(t, manager.GetUsersToReinvite(), 1)
	require.Len(t, manager.GetUsersToReinvite(), 1)

	pending := manager.GetUsersToReinvite()[0]
	require.NoError(t, manager.MarkReinvited(pending))
	require.Empty(t, manager.GetUsersToReinvite())
}

func TestMarkReinvitedDoesNotReplaceNewerBan(t *testing.T) {
	manager, err := newKickedUsersManager(zap.NewNop(), t.TempDir())
	require.NoError(t, err)
	addBan(t, manager, "user", "channel", -time.Minute)
	oldBan := manager.GetUsersToReinvite()[0]
	time.Sleep(time.Millisecond)
	addBan(t, manager, "user", "channel", time.Minute)
	require.NoError(t, manager.MarkReinvited(oldBan))
	_, banned := manager.IsUserBanned("user", "channel")
	require.True(t, banned)
}

func TestKickedUsersManagerLoadsNullState(t *testing.T) {
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, kickedUsersFile), []byte("null"), 0600))
	manager, err := newKickedUsersManager(zap.NewNop(), dataDir)
	require.NoError(t, err)
	addBan(t, manager, "user", "channel", time.Minute)
	_, banned := manager.IsUserBanned("user", "channel")
	require.True(t, banned)
}

func TestPersistenceFailureDoesNotPublishBan(t *testing.T) {
	store := &fakeKickedUsersStore{users: make(map[string]kickedUser)}
	manager, err := newKickedUsersManagerWithStore(zap.NewNop(), store)
	require.NoError(t, err)
	store.failNext(errors.New("disk full"))

	_, err = manager.AddKickedUser("user", "channel", time.Minute)
	require.ErrorContains(t, err, "disk full")
	_, banned := manager.IsUserBanned("user", "channel")
	require.False(t, banned)
}

func TestMarkReinvitedPersistenceFailureIsRetried(t *testing.T) {
	store := &fakeKickedUsersStore{users: make(map[string]kickedUser)}
	manager, err := newKickedUsersManagerWithStore(zap.NewNop(), store)
	require.NoError(t, err)
	addBan(t, manager, "user", "channel", -time.Minute)
	pending := manager.GetUsersToReinvite()[0]
	store.failNext(errors.New("disk full"))

	require.Error(t, manager.MarkReinvited(pending))
	require.Empty(t, manager.GetUsersToReinvite())
	require.NoError(t, manager.Flush())
	store.mu.Lock()
	persisted := store.users["user:channel"]
	store.mu.Unlock()
	require.True(t, persisted.Reinvited)
}

func TestKickedUsersManagerReturnsLoadFailure(t *testing.T) {
	_, err := newKickedUsersManagerWithStore(zap.NewNop(), &fakeKickedUsersStore{loadErr: errors.New("corrupt")})
	require.ErrorContains(t, err, "corrupt")
}

func TestVibecheckLifecycleIsRestartableAndConcurrentStopIsSafe(t *testing.T) {
	service := newTestVibecheck(t, &fakeSlackAPI{})
	require.NoError(t, service.Start(context.Background()))
	require.ErrorIs(t, service.Start(context.Background()), ErrAlreadyStarted)

	results := make(chan error, 2)
	go func() { results <- service.Stop(context.Background()) }()
	go func() { results <- service.Stop(context.Background()) }()
	require.NoError(t, <-results)
	require.NoError(t, <-results)

	require.NoError(t, service.Start(context.Background()))
	require.NoError(t, service.Stop(context.Background()))
}

func TestVibecheckCanRestartAfterRunContextCancellation(t *testing.T) {
	service := newTestVibecheck(t, &fakeSlackAPI{})
	runCtx, cancel := context.WithCancel(context.Background())
	require.NoError(t, service.Start(runCtx))
	firstRun := service.run.Load()
	cancel()
	select {
	case <-firstRun.done:
	case <-time.After(time.Second):
		t.Fatal("run did not finish after context cancellation")
	}

	require.NoError(t, service.Start(context.Background()))
	require.NoError(t, service.Stop(context.Background()))
}

func TestStaleStopCannotStopRestartedRun(t *testing.T) {
	service := newTestVibecheck(t, &fakeSlackAPI{})
	runCtx, cancel := context.WithCancel(context.Background())
	require.NoError(t, service.Start(runCtx))
	firstRun := service.run.Load()
	cancel()
	<-firstRun.done

	require.NoError(t, service.Start(context.Background()))
	secondRun := service.run.Load()
	require.NoError(t, service.stopRun(context.Background(), firstRun))
	require.Same(t, secondRun, service.run.Load())
	require.NoError(t, secondRun.ctx.Err())
	require.NoError(t, service.Stop(context.Background()))
}

func TestStopCancelsDelayedKick(t *testing.T) {
	api := &fakeSlackAPI{}
	service := newTestVibecheck(t, api)
	service.kickDelay = time.Hour
	require.NoError(t, service.Start(context.Background()))
	run := service.run.Load()
	ban := addBan(t, service.kickedUsers, "user", "channel", time.Minute)
	service.scheduleKick(run, service.kickDelay, ban, "test")
	require.NoError(t, service.Stop(context.Background()))

	kicks, _, _ := api.counts()
	require.Zero(t, kicks)
}

func TestScheduledKickRemainsPendingUntilComplete(t *testing.T) {
	api := &fakeSlackAPI{}
	service := newTestVibecheck(t, api)
	service.kickDelay = 20 * time.Millisecond
	require.NoError(t, service.Start(context.Background()))
	ban := addBan(t, service.kickedUsers, "user", "channel", time.Minute)
	service.scheduleKick(service.run.Load(), service.kickDelay, ban, "test")
	require.EqualValues(t, 1, service.PendingEvents())
	waitFor(t, func() bool {
		kicks, _, _ := api.counts()
		return kicks == 1 && service.PendingEvents() == 0
	})
	require.NoError(t, service.Stop(context.Background()))
}

func TestKickRetriesThenSucceeds(t *testing.T) {
	api := &fakeSlackAPI{kickErrors: []error{errors.New("temporary"), nil}}
	service := newTestVibecheck(t, api)
	service.kickRetryDelay = time.Millisecond
	require.NoError(t, service.Start(context.Background()))
	ban := addBan(t, service.kickedUsers, "user", "channel", time.Minute)
	service.scheduleKick(service.run.Load(), 0, ban, "test")
	waitFor(t, func() bool {
		kicks, _, _ := api.counts()
		return kicks == 2
	})
	require.NoError(t, service.Stop(context.Background()))
}

func TestKickStopsAfterMaximumAttempts(t *testing.T) {
	api := &fakeSlackAPI{kickErrors: []error{errors.New("one"), errors.New("two"), errors.New("three")}}
	service := newTestVibecheck(t, api)
	service.kickRetryDelay = time.Millisecond
	require.NoError(t, service.Start(context.Background()))
	ban := addBan(t, service.kickedUsers, "user", "channel", time.Minute)
	service.scheduleKick(service.run.Load(), 0, ban, "test")
	waitFor(t, func() bool {
		kicks, _, _ := api.counts()
		return kicks == defaultMaximumKickTries
	})
	time.Sleep(5 * time.Millisecond)
	kicks, _, _ := api.counts()
	require.Equal(t, defaultMaximumKickTries, kicks)
	require.NoError(t, service.Stop(context.Background()))
}

func TestStopCancelsKickRetry(t *testing.T) {
	api := &fakeSlackAPI{kickErrors: []error{errors.New("temporary")}}
	service := newTestVibecheck(t, api)
	service.kickRetryDelay = time.Hour
	require.NoError(t, service.Start(context.Background()))
	ban := addBan(t, service.kickedUsers, "user", "channel", time.Minute)
	service.scheduleKick(service.run.Load(), 0, ban, "test")
	waitFor(t, func() bool {
		kicks, _, _ := api.counts()
		return kicks == 1
	})
	require.NoError(t, service.Stop(context.Background()))
	kicks, _, _ := api.counts()
	require.Equal(t, 1, kicks)
}

func TestKickRetryStopsWhenBanExpires(t *testing.T) {
	api := &fakeSlackAPI{kickErrors: []error{errors.New("temporary"), nil}}
	service := newTestVibecheck(t, api)
	service.kickRetryDelay = 100 * time.Millisecond
	require.NoError(t, service.Start(context.Background()))
	ban := addBan(t, service.kickedUsers, "user", "channel", 50*time.Millisecond)
	service.scheduleKick(service.run.Load(), 0, ban, "test")
	waitFor(t, func() bool {
		kicks, _, _ := api.counts()
		return kicks == 1
	})
	time.Sleep(120 * time.Millisecond)
	kicks, _, _ := api.counts()
	require.Equal(t, 1, kicks)
	require.NoError(t, service.Stop(context.Background()))
}

func TestKickAttemptIsBoundedByBanDeadline(t *testing.T) {
	api := &delayedKickAPI{delay: 100 * time.Millisecond, started: make(chan struct{}, 1)}
	service := newTestVibecheck(t, api)
	require.NoError(t, service.Start(context.Background()))
	ban := addBan(t, service.kickedUsers, "user", "channel", 30*time.Millisecond)
	service.scheduleKick(service.run.Load(), 0, ban, "test")
	select {
	case <-api.started:
	case <-time.After(time.Second):
		t.Fatal("kick attempt did not start")
	}
	time.Sleep(50 * time.Millisecond)
	require.False(t, api.succeeded.Load())
	require.NoError(t, service.Stop(context.Background()))
}

func TestStopFlushesDirtyReinviteState(t *testing.T) {
	store := &fakeKickedUsersStore{users: make(map[string]kickedUser)}
	manager, err := newKickedUsersManagerWithStore(zap.NewNop(), store)
	require.NoError(t, err)
	pending := addBan(t, manager, "user", "channel", -time.Minute)
	store.failNext(errors.New("temporary"))
	require.Error(t, manager.MarkReinvited(pending))

	service := newTestVibecheck(t, &fakeSlackAPI{})
	service.kickedUsers = manager
	require.NoError(t, service.Start(context.Background()))
	require.NoError(t, service.Stop(context.Background()))
	store.mu.Lock()
	persisted := store.users["user:channel"]
	store.mu.Unlock()
	require.True(t, persisted.Reinvited)
}

func TestStopReturnsFinalPersistenceFailure(t *testing.T) {
	store := &fakeKickedUsersStore{users: make(map[string]kickedUser)}
	manager, err := newKickedUsersManagerWithStore(zap.NewNop(), store)
	require.NoError(t, err)
	pending := addBan(t, manager, "user", "channel", -time.Minute)
	store.failNext(errors.New("mark failed"))
	require.Error(t, manager.MarkReinvited(pending))
	store.failNext(errors.New("flush failed"))

	service := newTestVibecheck(t, &fakeSlackAPI{})
	service.kickedUsers = manager
	require.NoError(t, service.Start(context.Background()))
	require.ErrorContains(t, service.Stop(context.Background()), "flush failed")
}

func TestReinviteFailureRemainsPendingUntilSuccess(t *testing.T) {
	api := &fakeSlackAPI{inviteErrors: []error{errors.New("temporary"), nil}}
	service := newTestVibecheck(t, api)
	addBan(t, service.kickedUsers, "user", "channel", -time.Minute)

	service.processReinvites(context.Background())
	require.Len(t, service.kickedUsers.GetUsersToReinvite(), 1)
	service.processReinvites(context.Background())
	require.Empty(t, service.kickedUsers.GetUsersToReinvite())
	_, invites, _ := api.counts()
	require.Equal(t, 2, invites)
}

func TestEventQueueDoesNotDropMoreThanOriginalCapacity(t *testing.T) {
	block := make(chan struct{})
	api := &fakeSlackAPI{postBlock: block}
	service := newTestVibecheck(t, api)
	service.rejoinKickDelay = time.Hour
	const eventCount = 150
	for i := 0; i < eventCount; i++ {
		user := fmt.Sprintf("user-%d", i)
		addBan(t, service.kickedUsers, user, "channel", time.Minute)
	}
	require.NoError(t, service.Start(context.Background()))
	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		for i := 0; i < eventCount; i++ {
			_ = service.PushEvent(slackevents.EventsAPIEvent{
				Type: slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.MemberJoinedChannelEvent{
					User: fmt.Sprintf("user-%d", i), Channel: "channel",
				}},
			})
		}
	}()
	waitFor(t, func() bool {
		_, _, posts := api.counts()
		return posts == 1 && service.run.Load().pending() == eventQueueCapacity
	})
	select {
	case <-pushed:
		t.Fatal("producer should be backpressured while the queue is full")
	default:
	}
	close(block)
	select {
	case <-pushed:
	case <-time.After(time.Second):
		t.Fatal("producer did not resume after queue capacity became available")
	}
	waitFor(t, func() bool {
		_, _, posts := api.counts()
		return posts == eventCount
	})
	require.NoError(t, service.Stop(context.Background()))
}

func TestRandomResponseUsesConfigAndIgnoresEmptyEmoji(t *testing.T) {
	config := Config{GoodReactions: []string{"", "custom"}, GoodText: []string{"configured response"}}
	response := randomResponse(true, config)
	require.True(t, strings.Contains(response, ":custom:") && strings.Contains(response, "configured response"))
}
