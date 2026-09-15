package aichat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	_ "modernc.org/sqlite"
)

const historicalConversationSchema = `
	CREATE TABLE conversation_context (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		persona_name TEXT NOT NULL,
		message TEXT NOT NULL,
		role TEXT NOT NULL CHECK (role IN ('human', 'assistant')),
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX idx_user_channel_persona
		ON conversation_context (user_id, channel_id, persona_name);
	CREATE INDEX idx_timestamp ON conversation_context (timestamp);`

func TestContextStorageMigratesEveryHistoricalSchema(t *testing.T) {
	tests := []struct {
		name         string
		withPersonas bool
	}{
		{name: "conversation context schema"},
		{name: "conversation and persona schema", withPersonas: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			db := openMigrationTestDB(t, dataDir)
			if _, err := db.Exec(historicalConversationSchema); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			if _, err := db.Exec(`
				INSERT INTO conversation_context
					(user_id, channel_id, persona_name, message, role, timestamp)
				VALUES ('U1', 'C1', 'legacy', 'keep me', 'human', ?)`, now); err != nil {
				t.Fatal(err)
			}
			if tt.withPersonas {
				if _, err := db.Exec(`
					CREATE TABLE persona_assignment (
						conversation_scope TEXT PRIMARY KEY,
						persona_name TEXT NOT NULL,
						timestamp DATETIME NOT NULL
					);
					INSERT INTO persona_assignment
						(conversation_scope, persona_name, timestamp)
					VALUES ('C1', 'legacy', ?)`, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			storage, err := NewContextStorage(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = storage.Close() }()

			var version, migrationCount int
			if err := storage.db.QueryRow(`
				SELECT COALESCE(MAX(version), 0), COUNT(*) FROM schema_migrations`,
			).Scan(&version, &migrationCount); err != nil {
				t.Fatal(err)
			}
			if version != len(schemaMigrations) || migrationCount != len(schemaMigrations) {
				t.Fatalf("migration history = version %d, count %d", version, migrationCount)
			}

			contexts, err := storage.GetRecentContext("U1", "C1", &Config{MaxContextMessages: 10})
			if err != nil || len(contexts) != 1 || contexts[0].Message != "keep me" {
				t.Fatalf("migrated contexts = %#v, err = %v", contexts, err)
			}
			if tt.withPersonas {
				assignment, ok, err := storage.GetPersonaAssignment("C1")
				if err != nil || !ok || assignment.Name != "legacy" {
					t.Fatalf("migrated persona = %#v, ok = %v, err = %v", assignment, ok, err)
				}
			}
		})
	}
}

func TestContextStorageMigrationFailureRollsBack(t *testing.T) {
	dataDir := t.TempDir()
	createIncompatibleHistoricalDatabase(t, dataDir)

	_, err := NewContextStorage(dataDir)
	if err == nil || !strings.Contains(err.Error(), "schema migration 1 (create conversation context)") {
		t.Fatalf("NewContextStorage() error = %v", err)
	}

	db := openMigrationTestDB(t, dataDir)
	defer func() { _ = db.Close() }()
	var value string
	if err := db.QueryRow(`SELECT legacy_value FROM conversation_context`).Scan(&value); err != nil {
		t.Fatalf("prior schema is not usable after failed migration: %v", err)
	}
	if value != "keep me" {
		t.Fatalf("legacy value = %q", value)
	}
	var migrationTableCount int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'schema_migrations'`,
	).Scan(&migrationTableCount); err != nil {
		t.Fatal(err)
	}
	if migrationTableCount != 0 {
		t.Fatal("failed migration committed schema history")
	}
}

func TestNewAIChatReturnsMigrationFailure(t *testing.T) {
	dataDir := t.TempDir()
	createIncompatibleHistoricalDatabase(t, dataDir)

	chat, err := NewAIChat(zap.NewNop(), Config{DataDir: dataDir}, &mockSlack{}, nil)
	if chat != nil {
		t.Fatal("NewAIChat() returned a service after storage migration failed")
	}
	if err == nil || !strings.Contains(err.Error(), "schema migration 1 (create conversation context)") {
		t.Fatalf("NewAIChat() error = %v", err)
	}
}

func createIncompatibleHistoricalDatabase(t *testing.T, dataDir string) {
	t.Helper()
	db := openMigrationTestDB(t, dataDir)
	if _, err := db.Exec(`
		CREATE TABLE conversation_context (id INTEGER PRIMARY KEY, legacy_value TEXT NOT NULL);
		INSERT INTO conversation_context (legacy_value) VALUES ('keep me')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func openMigrationTestDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "aichat_context.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}
