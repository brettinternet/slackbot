package user

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

func TestUserWatchFailedNotificationsAreDurableAndRetried(t *testing.T) {
	api := &watcherAPI{team: "T1", users: []slack.User{{ID: "U1", Name: "one"}}, failPosts: true, validChannels: map[string]bool{"C1234567890": true}}
	watch := newHardeningWatch(t, api)
	if err := watch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watch.Stop(context.Background()) }()
	state, err := watch.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || len(state.Pending) != 1 {
		t.Fatalf("pending notifications = %v, want one", state)
	}
	api.failPosts = false
	watch.retryPending(context.Background())
	watch.stateMu.Lock()
	pending := len(watch.pending)
	watch.stateMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending notifications after retry = %d", pending)
	}
	state, err = watch.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("durable pending queue not cleared: %d", len(state.Pending))
	}
}

func TestUserWatchLifecycleRejectsRepeatedStartAndAllowsRepeatedStop(t *testing.T) {
	api := &watcherAPI{team: "T1", validChannels: map[string]bool{"C1234567890": true}}
	watch := newHardeningWatch(t, api)
	if err := watch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := watch.Start(context.Background()); err != ErrAlreadyStarted {
		t.Fatalf("second Start error = %v", err)
	}
	if err := watch.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := watch.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUserWatchRestartReplacesDurablePendingQueue(t *testing.T) {
	api := &watcherAPI{team: "T1", users: []slack.User{{ID: "U1", Name: "one"}}, failPosts: true, validChannels: map[string]bool{"C1234567890": true}}
	watch := newHardeningWatch(t, api)
	if err := watch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := watch.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := watch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watch.Stop(context.Background()) }()
	watch.stateMu.Lock()
	pending := len(watch.pending)
	watch.stateMu.Unlock()
	if pending != 1 {
		t.Fatalf("pending notifications after restart = %d, want 1", pending)
	}
}

func TestUserWatchReadsLegacyUsersArray(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(`[{"id":"U1","name":"one","real_name":"One"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	watch := NewUserWatch(zap.NewNop(), Config{DataDir: dir}, &watcherAPI{})
	users, err := watch.loadUsersFromDisk()
	if err != nil {
		t.Fatal(err)
	}
	if users["U1"].RealName != "One" {
		t.Fatalf("legacy user = %#v", users["U1"])
	}
}

func TestUserWatchStateHasSchemaAndDeterministicUsers(t *testing.T) {
	api := &watcherAPI{team: "T1", users: []slack.User{{ID: "U2", Name: "two"}, {ID: "U1", Name: "one"}}, validChannels: map[string]bool{"C1234567890": true}}
	watch := newHardeningWatch(t, api)
	if err := watch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watch.Stop(context.Background()) }()
	state, err := watch.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != stateSchemaVersion || state.WorkspaceID != "T1" {
		t.Fatalf("state identity = %#v", state)
	}
	data, err := json.Marshal(state.Users)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `[{"id":"U1","name":"one","real_name":""},{"id":"U2","name":"two","real_name":""}]` {
		t.Fatalf("users are not deterministic: %s", data)
	}
}
