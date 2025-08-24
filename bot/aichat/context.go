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

	storage := &ContextStorage{db: db}
	if err := storage.initSchema(); err != nil {
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return storage, nil
}

// Close closes the database connection
func (cs *ContextStorage) Close() error {
	return cs.db.Close()
}

// initSchema creates the necessary database tables
func (cs *ContextStorage) initSchema() error {
	query := `
	CREATE TABLE IF NOT EXISTS conversation_context (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		persona_name TEXT NOT NULL,
		message TEXT NOT NULL,
		role TEXT NOT NULL CHECK (role IN ('human', 'assistant')),
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	_, err := cs.db.Exec(query)
	if err != nil {
		return err
	}

	// Create indexes separately
	indexQueries := []string{
		`CREATE INDEX IF NOT EXISTS idx_user_channel_persona ON conversation_context (user_id, channel_id, persona_name);`,
		`CREATE INDEX IF NOT EXISTS idx_timestamp ON conversation_context (timestamp);`,
	}

	for _, indexQuery := range indexQueries {
		_, err := cs.db.Exec(indexQuery)
		if err != nil {
			return err
		}
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

// GetRecentContext retrieves recent conversation context for a user/channel/persona
func (cs *ContextStorage) GetRecentContext(userID, channelID, personaName string, config *Config) ([]ConversationContext, error) {
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
	WHERE user_id = ? AND channel_id = ? AND persona_name = ?`

	args := []any{userID, channelID, personaName}

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

	for rows.Next() {
		var ctx ConversationContext
		err := rows.Scan(&ctx.UserID, &ctx.ChannelID, &ctx.PersonaName, &ctx.Message, &ctx.Role, &ctx.Timestamp)
		if err != nil {
			return nil, err
		}

		// Rough token estimation (4 characters ≈ 1 token)
		messageTokens := len(ctx.Message) / 4
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

// CleanOldContext removes conversation context older than the specified duration
func (cs *ContextStorage) CleanOldContext(maxAge time.Duration) error {
	query := `DELETE FROM conversation_context WHERE timestamp < ?`
	cutoff := time.Now().Add(-maxAge)
	_, err := cs.db.Exec(query, cutoff)
	return err
}
