package aichat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ConversationContext represents a stored conversation context
type ConversationContext struct {
	UserID      string
	ChannelID   string
	PersonaName string
	Message     string
	Role        string // "human" or "assistant"
	Timestamp   time.Time
}

// ContextStorage handles conversation context persistence
type ContextStorage struct {
	db *sql.DB
}

// NewContextStorage creates a new context storage instance
func NewContextStorage(dataDir string) (*ContextStorage, error) {
	dbPath := filepath.Join(dataDir, "aichat_context.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	// A single connection serializes writes from conversation shards. The busy
	// timeout also tolerates short-lived locks held by another process.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure database: %w", err)
	}

	storage := &ContextStorage{db: db}
	if err := storage.initSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return storage, nil
}

// Close closes the database connection
func (cs *ContextStorage) Close() error {
	return cs.db.Close()
}

type schemaMigration struct {
	version int
	name    string
	query   string
}

var schemaMigrations = []schemaMigration{
	{
		version: 1,
		name:    "create conversation context",
		query: `
			CREATE TABLE IF NOT EXISTS conversation_context (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id TEXT NOT NULL,
				channel_id TEXT NOT NULL,
				persona_name TEXT NOT NULL,
				message TEXT NOT NULL,
				role TEXT NOT NULL CHECK (role IN ('human', 'assistant')),
				timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
			);
			CREATE INDEX IF NOT EXISTS idx_user_channel_persona
				ON conversation_context (user_id, channel_id, persona_name);
			CREATE INDEX IF NOT EXISTS idx_timestamp ON conversation_context (timestamp);`,
	},
	{
		version: 2,
		name:    "create persona assignments",
		query: `
			CREATE TABLE IF NOT EXISTS persona_assignment (
				conversation_scope TEXT PRIMARY KEY,
				persona_name TEXT NOT NULL,
				timestamp DATETIME NOT NULL
			);`,
	},
}

// initSchema transactionally upgrades both new and pre-migration databases.
// The migrations use idempotent DDL so databases created by older releases are
// adopted without rewriting their conversation or persona data.
func (cs *ContextStorage) initSchema() (err error) {
	tx, err := cs.db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		return fmt.Errorf("create schema migration history: %w", err)
	}

	rows, err := tx.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read schema migration history: %w", err)
	}
	var applied int
	for rows.Next() {
		var version int
		if scanErr := rows.Scan(&version); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("read schema migration version: %w", scanErr)
		}
		applied++
		if version != applied {
			_ = rows.Close()
			return fmt.Errorf("schema migration history has a gap before version %d", version)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		_ = rows.Close()
		return fmt.Errorf("read schema migration history: %w", rowsErr)
	}
	if closeErr := rows.Close(); closeErr != nil {
		return fmt.Errorf("close schema migration history: %w", closeErr)
	}
	if applied > len(schemaMigrations) {
		return fmt.Errorf(
			"database schema version %d is newer than supported version %d",
			applied,
			len(schemaMigrations),
		)
	}

	for _, migration := range schemaMigrations[applied:] {
		if migration.version != applied+1 {
			return fmt.Errorf("internal schema migration order is invalid at version %d", migration.version)
		}
		if _, err = tx.Exec(migration.query); err != nil {
			return fmt.Errorf("apply schema migration %d (%s): %w", migration.version, migration.name, err)
		}
		if _, err = tx.Exec(
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
			migration.version,
			migration.name,
		); err != nil {
			return fmt.Errorf("record schema migration %d (%s): %w", migration.version, migration.name, err)
		}
		applied = migration.version
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migrations: %w", err)
	}
	return nil
}

// StoreContext stores a conversation message in the database
func (cs *ContextStorage) StoreContext(ctx ConversationContext) error {
	query := `
	INSERT INTO conversation_context (user_id, channel_id, persona_name, message, role, timestamp)
	VALUES (?, ?, ?, ?, ?, ?)`

	_, err := cs.db.Exec(query, ctx.UserID, ctx.ChannelID, ctx.PersonaName, ctx.Message, ctx.Role, ctx.Timestamp)
	return err
}

// GetRecentContext retrieves recent context for a user and channel. Memory is shared
// across persona assignments so changing style does not erase conversation continuity.
func (cs *ContextStorage) GetRecentContext(userID, channelID string, config *Config) ([]ConversationContext, error) {
	// Apply context limits from config
	maxMessages := config.MaxContextMessages
	if maxMessages <= 0 {
		maxMessages = 50 // default fallback
	}

	// Calculate the minimum timestamp based on MaxContextAge
	var minTimestamp time.Time
	if config.MaxContextAge > 0 {
		minTimestamp = time.Now().Add(-config.MaxContextAge)
	}

	query := `
	SELECT user_id, channel_id, persona_name, message, role, timestamp
	FROM conversation_context
	WHERE user_id = ? AND channel_id = ?`

	args := []any{userID, channelID}

	// Add timestamp filter if MaxContextAge is configured
	if !minTimestamp.IsZero() {
		query += ` AND timestamp >= ?`
		args = append(args, minTimestamp)
	}

	query += `
	ORDER BY timestamp DESC
	LIMIT ?`
	args = append(args, maxMessages)

	rows, err := cs.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var contexts []ConversationContext
	totalTokens := 0
	maxTokens := config.MaxContextTokens
	counter := config.modelTokenCounter()

	for rows.Next() {
		var ctx ConversationContext
		err := rows.Scan(&ctx.UserID, &ctx.ChannelID, &ctx.PersonaName, &ctx.Message, &ctx.Role, &ctx.Timestamp)
		if err != nil {
			return nil, err
		}

		messageTokens := counter.messageTokens(ctx.Message)
		if maxTokens > 0 && totalTokens+messageTokens > maxTokens {
			break // Stop adding messages if we exceed token limit
		}

		contexts = append(contexts, ctx)
		totalTokens += messageTokens
	}

	// Reverse the slice to get chronological order (oldest first)
	for i, j := 0, len(contexts)-1; i < j; i, j = i+1, j-1 {
		contexts[i], contexts[j] = contexts[j], contexts[i]
	}

	return contexts, rows.Err()
}

// StorePersonaAssignment persists the current persona for a conversation scope.
func (cs *ContextStorage) StorePersonaAssignment(scope string, assignment personaAssignment) error {
	_, err := cs.db.Exec(`
		INSERT INTO persona_assignment (conversation_scope, persona_name, timestamp)
		VALUES (?, ?, ?)
		ON CONFLICT(conversation_scope) DO UPDATE SET
			persona_name = excluded.persona_name,
			timestamp = excluded.timestamp`, scope, assignment.Name, assignment.Timestamp)
	return err
}

// GetPersonaAssignment retrieves a persisted persona for a conversation scope.
func (cs *ContextStorage) GetPersonaAssignment(scope string) (personaAssignment, bool, error) {
	var assignment personaAssignment
	err := cs.db.QueryRow(`
		SELECT persona_name, timestamp
		FROM persona_assignment
		WHERE conversation_scope = ?`, scope).Scan(&assignment.Name, &assignment.Timestamp)
	if err == sql.ErrNoRows {
		return personaAssignment{}, false, nil
	}
	if err != nil {
		return personaAssignment{}, false, err
	}
	return assignment, true, nil
}

// TouchPersonaAssignment atomically retrieves and refreshes a non-expired
// assignment. The single statement prevents a concurrent administrative clear
// from being followed by a stale refresh upsert.
func (cs *ContextStorage) TouchPersonaAssignment(
	scope string,
	now time.Time,
	maxAge time.Duration,
) (personaAssignment, bool, error) {
	query := `
		UPDATE persona_assignment
		SET timestamp = ?
		WHERE conversation_scope = ?`
	args := []any{now, scope}
	if maxAge > 0 {
		query += ` AND timestamp >= ?`
		args = append(args, now.Add(-maxAge))
	}
	query += ` RETURNING persona_name, timestamp`

	var assignment personaAssignment
	err := cs.db.QueryRow(query, args...).Scan(&assignment.Name, &assignment.Timestamp)
	if err == sql.ErrNoRows {
		return personaAssignment{}, false, nil
	}
	if err != nil {
		return personaAssignment{}, false, err
	}
	return assignment, true, nil
}

// DeletionCounts reports how many persisted records were removed.
type DeletionCounts struct {
	Contexts int64
	Personas int64
}

// DeleteConversationScope removes all stored messages and the persona assignment
// for one channel or thread scope in a single transaction.
func (cs *ContextStorage) DeleteConversationScope(scope string) (DeletionCounts, error) {
	return cs.deleteWhere(
		`DELETE FROM conversation_context WHERE channel_id = ?`, []any{scope},
		`DELETE FROM persona_assignment WHERE conversation_scope = ?`, []any{scope},
	)
}

// CleanExpired removes conversation context older than contextMaxAge and persona
// assignments older than personaMaxAge. A non-positive age leaves that data unchanged.
func (cs *ContextStorage) CleanExpired(contextMaxAge, personaMaxAge time.Duration) (DeletionCounts, error) {
	var contextQuery, personaQuery string
	var contextArgs, personaArgs []any
	if contextMaxAge > 0 {
		contextQuery = `DELETE FROM conversation_context WHERE timestamp < ?`
		contextArgs = []any{time.Now().Add(-contextMaxAge)}
	}
	if personaMaxAge > 0 {
		personaQuery = `DELETE FROM persona_assignment WHERE timestamp < ?`
		personaArgs = []any{time.Now().Add(-personaMaxAge)}
	}
	return cs.deleteWhere(contextQuery, contextArgs, personaQuery, personaArgs)
}

func (cs *ContextStorage) deleteWhere(
	contextQuery string,
	contextArgs []any,
	personaQuery string,
	personaArgs []any,
) (counts DeletionCounts, err error) {
	tx, err := cs.db.Begin()
	if err != nil {
		return counts, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if contextQuery != "" {
		result, execErr := tx.Exec(contextQuery, contextArgs...)
		if execErr != nil {
			return counts, execErr
		}
		counts.Contexts, err = result.RowsAffected()
		if err != nil {
			return counts, err
		}
	}
	if personaQuery != "" {
		result, execErr := tx.Exec(personaQuery, personaArgs...)
		if execErr != nil {
			return counts, execErr
		}
		counts.Personas, err = result.RowsAffected()
		if err != nil {
			return counts, err
		}
	}
	if err = tx.Commit(); err != nil {
		return DeletionCounts{}, err
	}
	return counts, nil
}

// CleanOldContext removes conversation context older than the specified duration.
func (cs *ContextStorage) CleanOldContext(maxAge time.Duration) error {
	_, err := cs.CleanExpired(maxAge, 0)
	return err
}
