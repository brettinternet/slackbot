package aichat

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func storePrivacyTestContext(t *testing.T, storage *ContextStorage, scope, message string, timestamp time.Time) {
	t.Helper()
	if err := storage.StoreContext(ConversationContext{
		UserID: "U1", ChannelID: scope, PersonaName: "test",
		Message: message, Role: "human", Timestamp: timestamp,
	}); err != nil {
		t.Fatalf("StoreContext() error = %v", err)
	}
}

func TestContextStorageDeleteConversationScope(t *testing.T) {
	storage, err := NewContextStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close() }()

	now := time.Now()
	storePrivacyTestContext(t, storage, "C1", "channel", now)
	storePrivacyTestContext(t, storage, "C1:thread:1.0", "thread", now)
	if err := storage.StorePersonaAssignment("C1", personaAssignment{Name: "one", Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	if err := storage.StorePersonaAssignment("C1:thread:1.0", personaAssignment{Name: "two", Timestamp: now}); err != nil {
		t.Fatal(err)
	}

	counts, err := storage.DeleteConversationScope("C1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Contexts != 1 || counts.Personas != 1 {
		t.Fatalf("counts = %#v, want one context and one persona", counts)
	}
	cfg := &Config{MaxContextMessages: 10, MaxContextTokens: 1000}
	contexts, err := storage.GetRecentContext("U1", "C1:thread:1.0", cfg)
	if err != nil || len(contexts) != 1 || contexts[0].Message != "thread" {
		t.Fatalf("unrelated thread context = %#v, err = %v", contexts, err)
	}
	if _, ok, err := storage.GetPersonaAssignment("C1:thread:1.0"); err != nil || !ok {
		t.Fatalf("unrelated persona missing: ok = %v, err = %v", ok, err)
	}
}

func TestContextStorageCleanExpiredIncludesPersonas(t *testing.T) {
	storage, err := NewContextStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close() }()

	now := time.Now()
	storePrivacyTestContext(t, storage, "old", "old", now.Add(-2*time.Hour))
	storePrivacyTestContext(t, storage, "new", "new", now)
	if err := storage.StorePersonaAssignment("old", personaAssignment{Name: "old", Timestamp: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := storage.StorePersonaAssignment("new", personaAssignment{Name: "new", Timestamp: now}); err != nil {
		t.Fatal(err)
	}

	counts, err := storage.CleanExpired(time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Contexts != 1 || counts.Personas != 1 {
		t.Fatalf("counts = %#v, want one context and one persona", counts)
	}
	cfg := &Config{MaxContextMessages: 10, MaxContextTokens: 1000}
	contexts, err := storage.GetRecentContext("U1", "new", cfg)
	if err != nil || len(contexts) != 1 {
		t.Fatalf("recent context = %#v, err = %v", contexts, err)
	}
	if _, ok, err := storage.GetPersonaAssignment("new"); err != nil || !ok {
		t.Fatalf("recent persona missing: ok = %v, err = %v", ok, err)
	}
}

func TestAIChatStartAutomaticallyCleansExpiredData(t *testing.T) {
	a, storage := newTestAIChatWithStorage(t, Config{
		MaxContextAge:  time.Hour,
		StickyDuration: time.Hour,
	})
	old := time.Now().Add(-2 * time.Hour)
	storePrivacyTestContext(t, storage, "C1", "old", old)
	if err := storage.StorePersonaAssignment("C1", personaAssignment{Name: "old", Timestamp: old}); err != nil {
		t.Fatal(err)
	}
	a.stickyPersonas["C1"] = personaAssignment{Name: "old", Timestamp: old}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		contexts, err := storage.GetRecentContext("U1", "C1", &Config{MaxContextMessages: 10, MaxContextTokens: 1000})
		if err != nil {
			t.Fatal(err)
		}
		_, personaExists, err := storage.GetPersonaAssignment("C1")
		if err != nil {
			t.Fatal(err)
		}
		a.mutex.Lock()
		_, cached := a.stickyPersonas["C1"]
		a.mutex.Unlock()
		if len(contexts) == 0 && !personaExists && !cached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("automatic cleanup did not remove expired data")
		}
		time.Sleep(time.Millisecond)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestContextStorageConcurrentReadsAndCleanup(t *testing.T) {
	storage, err := NewContextStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close() }()

	for i := 0; i < 20; i++ {
		storePrivacyTestContext(t, storage, "C1", fmt.Sprintf("message-%d", i), time.Now().Add(-2*time.Hour))
	}
	cfg := &Config{MaxContextMessages: 50, MaxContextTokens: 10000}
	var wg sync.WaitGroup
	errs := make(chan error, 21)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, readErr := storage.GetRecentContext("U1", "C1", cfg)
			errs <- readErr
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, cleanupErr := storage.CleanExpired(time.Hour, time.Hour)
		errs <- cleanupErr
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
}

func TestPersonaCacheObservesExternalClear(t *testing.T) {
	a, storage := newTestAIChatWithStorage(t, Config{Personas: map[string]string{"one": "prompt"}})
	old := time.Now().Add(-time.Minute)
	assignment := personaAssignment{Name: "one", Timestamp: old}
	if err := storage.StorePersonaAssignment("C1", assignment); err != nil {
		t.Fatal(err)
	}
	a.stickyPersonas["C1"] = assignment
	if _, err := storage.DeleteConversationScope("C1"); err != nil {
		t.Fatal(err)
	}

	if got := a.userPersona("C1"); got != "one" {
		t.Fatalf("persona = %q, want one", got)
	}
	refreshed, ok, err := storage.GetPersonaAssignment("C1")
	if err != nil || !ok {
		t.Fatalf("refreshed assignment missing: ok = %v, err = %v", ok, err)
	}
	if !refreshed.Timestamp.After(old) {
		t.Fatalf("assignment timestamp = %v, want after %v", refreshed.Timestamp, old)
	}
}

func TestAIChatClearContextEvictsCachedPersona(t *testing.T) {
	a, storage := newTestAIChatWithStorage(t, Config{})
	now := time.Now()
	storePrivacyTestContext(t, storage, "C1", "message", now)
	if err := storage.StorePersonaAssignment("C1", personaAssignment{Name: "one", Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	a.stickyPersonas["C1"] = personaAssignment{Name: "one", Timestamp: now}

	counts, err := a.ClearContext("C1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Contexts != 1 || counts.Personas != 1 {
		t.Fatalf("counts = %#v", counts)
	}
	if _, ok := a.stickyPersonas["C1"]; ok {
		t.Fatal("cached persona was not evicted")
	}
}
