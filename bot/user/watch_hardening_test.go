package user

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

type watcherAPI struct {
	users         []slack.User
	team          string
	posts         int
	failPosts     bool
	failUsers     bool
	validChannels map[string]bool
	channelInfo   map[string]slack.Channel
	blockUsers    <-chan struct{}
	usersStarted  chan<- struct{}
	blockPosts    <-chan struct{}
	postsStarted  chan<- struct{}
}

func (f *watcherAPI) OrgURL() string { return "https://example.slack.com/" }
func (f *watcherAPI) GetUsersContext(ctx context.Context) ([]slack.User, error) {
	if f.usersStarted != nil {
		select {
		case f.usersStarted <- struct{}{}:
		default:
		}
	}
	if f.blockUsers != nil {
		select {
		case <-f.blockUsers:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failUsers {
		return nil, errors.New("users unavailable")
	}
	return f.users, nil
}
func (f *watcherAPI) PostMessageContext(ctx context.Context, _ string, _ ...slack.MsgOption) (string, string, error) {
	f.posts++
	if f.postsStarted != nil {
		select {
		case f.postsStarted <- struct{}{}:
		default:
		}
	}
	if f.blockPosts != nil {
		select {
		case <-f.blockPosts:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	if f.failPosts {
		return "", "", errors.New("post failed")
	}
	return "C", "1", nil
}
func (f *watcherAPI) GetConversationInfoContext(_ context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error) {
	if !f.validChannels[input.ChannelID] {
		return nil, errors.New("channel unavailable")
	}
	if info, ok := f.channelInfo[input.ChannelID]; ok {
		return &info, nil
	}
	return &slack.Channel{IsMember: true}, nil
}
func (f *watcherAPI) AuthTestContext(context.Context) (*slack.AuthTestResponse, error) {
	return &slack.AuthTestResponse{TeamID: f.team}, nil
}

func newHardeningWatch(t *testing.T, api *watcherAPI) *UserWatch {
	t.Helper()
	return NewUserWatch(zap.NewNop(), Config{NotifyChannel: "C1234567890", DataDir: t.TempDir()}, api)
}

func TestUserWatchEmptyChannelDisablesWithoutSlack(t *testing.T) {
	api := &watcherAPI{team: "T1"}
	watch := NewUserWatch(zap.NewNop(), Config{}, api)
	if err := watch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.posts != 0 {
		t.Fatalf("disabled watcher posted %d messages", api.posts)
	}
}

func TestUserWatchDoesNotOverwriteBaselineWhenInitialFetchFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	original := `{"schema_version":2,"workspace_id":"T1","users":[{"id":"U1","name":"one"}]}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	api := &watcherAPI{team: "T1", failUsers: true, validChannels: map[string]bool{"C1234567890": true}}
	watch := NewUserWatch(zap.NewNop(), Config{NotifyChannel: "C1234567890", DataDir: dir}, api)
	if err := watch.Start(context.Background()); err == nil {
		t.Fatal("expected initial fetch error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("baseline was overwritten: %s", got)
	}
}

func TestUserWatchWorkspaceMismatchResetsWithoutChangeAlerts(t *testing.T) {
	dir := t.TempDir()
	state := `{"schema_version":2,"workspace_id":"OLD","users":[{"id":"U-old","name":"old"}]}`
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(state), 0600); err != nil {
		t.Fatal(err)
	}
	api := &watcherAPI{team: "NEW", users: []slack.User{{ID: "U-new", Name: "new"}}, validChannels: map[string]bool{"C1234567890": true}}
	watch := NewUserWatch(zap.NewNop(), Config{NotifyChannel: "C1234567890", DataDir: dir}, api)
	if err := watch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watch.Stop(context.Background()) }()
	if api.posts != 1 {
		t.Fatalf("workspace reset emitted %d posts, want only startup", api.posts)
	}
}

func TestUserWatchRejectsInvalidChannelReplacement(t *testing.T) {
	api := &watcherAPI{team: "T1", validChannels: map[string]bool{"C1234567890": true}}
	watch := newHardeningWatch(t, api)
	if err := watch.UpdateNotifyChannel(context.Background(), "C9999999999"); err == nil {
		t.Fatal("expected invalid replacement error")
	}
	watch.stateMu.Lock()
	got := watch.notifyChannel
	watch.stateMu.Unlock()
	if got != "C1234567890" {
		t.Fatalf("channel changed after failed validation: %q", got)
	}
}

func TestUserWatchValidatesPrivateArchivedAndNonMemberChannels(t *testing.T) {
	api := &watcherAPI{
		validChannels: map[string]bool{"G1234567890": true, "C1234567891": true, "C1234567892": true},
		channelInfo: map[string]slack.Channel{
			"G1234567890": {IsMember: true},
			"C1234567891": {IsMember: false},
			"C1234567892": {IsMember: true, GroupConversation: slack.GroupConversation{IsArchived: true}},
		},
	}
	watch := NewUserWatch(zap.NewNop(), Config{}, api)
	if !watch.validateChannel(context.Background(), "G1234567890") {
		t.Fatal("valid private channel was rejected")
	}
	if watch.validateChannel(context.Background(), "C1234567891") {
		t.Fatal("non-member channel was accepted")
	}
	if watch.validateChannel(context.Background(), "C1234567892") {
		t.Fatal("archived channel was accepted")
	}
}

func TestUserWatchHotEnableUsesStartContext(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	api := &watcherAPI{
		team: "T1", users: []slack.User{{ID: "U1", Name: "one"}},
		validChannels: map[string]bool{"C1234567890": true}, blockUsers: release, usersStarted: started,
	}
	watch := NewUserWatch(zap.NewNop(), Config{DataDir: t.TempDir()}, api)
	parent := context.Background()
	if err := watch.Start(parent); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- watch.UpdateNotifyChannel(context.Background(), "C1234567890")
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("hot enable did not begin reconciliation")
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("hot enable did not finish")
	}
	if err := watch.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUserWatchConcurrentStartHasOneWorker(t *testing.T) {
	api := &watcherAPI{team: "T1", validChannels: map[string]bool{"C1234567890": true}}
	watch := newHardeningWatch(t, api)
	results := make(chan error, 2)
	go func() { results <- watch.Start(context.Background()) }()
	go func() { results <- watch.Start(context.Background()) }()
	var successes, alreadyStarted int
	for range 2 {
		switch err := <-results; err {
		case nil:
			successes++
		case ErrAlreadyStarted:
			alreadyStarted++
		default:
			t.Fatalf("unexpected Start error: %v", err)
		}
	}
	if successes != 1 || alreadyStarted != 1 {
		t.Fatalf("Start results = %d successes, %d already started", successes, alreadyStarted)
	}
	if err := watch.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUserWatchStartCancellationDoesNotLeakLifecycle(t *testing.T) {
	release := make(chan struct{})
	api := &watcherAPI{team: "T1", validChannels: map[string]bool{"C1234567890": true}, blockUsers: release}
	watch := newHardeningWatch(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := watch.Start(ctx); err == nil {
		t.Fatal("expected cancelled Start to fail")
	}
	if watch.isStarted() {
		t.Fatal("cancelled Start left watcher active")
	}
	close(release)
}

func TestUserWatchStopClearsStateWhenSaveFails(t *testing.T) {
	api := &watcherAPI{team: "T1", validChannels: map[string]bool{"C1234567890": true}}
	watch := newHardeningWatch(t, api)
	if err := watch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	badUsersFile := filepath.Join(filepath.Dir(watch.usersFile), "users-directory")
	watch.usersFile = badUsersFile
	if err := os.Mkdir(badUsersFile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := watch.Stop(context.Background()); err == nil {
		t.Fatal("expected final save failure")
	}
	if watch.isStarted() {
		t.Fatal("Stop left watcher active after save failure")
	}
}

func TestUserWatchCanRestartAfterRunContextCancellation(t *testing.T) {
	api := &watcherAPI{team: "T1", validChannels: map[string]bool{"C1234567890": true}}
	watch := newHardeningWatch(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	if err := watch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	watch.stateMu.Lock()
	done := watch.workerDone
	watch.stateMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after run context cancellation")
	}
	if err := watch.Start(context.Background()); err != nil {
		t.Fatalf("restart after cancellation: %v", err)
	}
	if err := watch.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUserWatchStopCancelsBlockedStartupPost(t *testing.T) {
	postsStarted := make(chan struct{}, 1)
	api := &watcherAPI{
		team: "T1", validChannels: map[string]bool{"C1234567890": true},
		blockPosts: make(chan struct{}), postsStarted: postsStarted,
	}
	watch := newHardeningWatch(t, api)
	startResult := make(chan error, 1)
	go func() { startResult <- watch.Start(context.Background()) }()
	select {
	case <-postsStarted:
	case <-time.After(time.Second):
		t.Fatal("startup did not reach blocked post")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := watch.Stop(stopCtx); err != nil {
		t.Fatalf("Stop did not cancel blocked startup: %v", err)
	}
	select {
	case <-startResult:
	case <-time.After(time.Second):
		t.Fatal("Start remained blocked after Stop")
	}
}

func TestUserWatchLegacyBaselineRebaselinesWithoutAlerts(t *testing.T) {
	dir := t.TempDir()
	legacy := `[{"id":"U-guest","name":"guest"}]`
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	api := &watcherAPI{team: "T1", validChannels: map[string]bool{"C1234567890": true}}
	watch := NewUserWatch(zap.NewNop(), Config{NotifyChannel: "C1234567890", DataDir: dir}, api)
	if err := watch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watch.Stop(context.Background()) }()
	if api.posts != 1 {
		t.Fatalf("legacy migration emitted change alerts: posts = %d, want startup only", api.posts)
	}
}

func TestEscapeMrkdwnOnlyEscapesSlackControlCharacters(t *testing.T) {
	got := escapeMrkdwn(`a&<>'"`)
	if got != `a&amp;&lt;&gt;'"` {
		t.Fatalf("escaped text = %q", got)
	}
}
