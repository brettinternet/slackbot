package vibecheck

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

const kickedUsersFile = "kicked_users.json"

// kickedUser represents a user who has been kicked from a channel.
type kickedUser struct {
	UserID     string    `json:"user_id"`
	ChannelID  string    `json:"channel_id"`
	KickedAt   time.Time `json:"kicked_at"`
	ReinviteAt time.Time `json:"reinvite_at"`
	Reinvited  bool      `json:"reinvited"`
}

type kickedUsersStore interface {
	Load() (map[string]kickedUser, error)
	Save(map[string]kickedUser) error
}

type fileKickedUsersStore struct {
	dataDir  string
	filePath string
}

func newFileKickedUsersStore(dataDir string) (*fileKickedUsersStore, error) {
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, fmt.Errorf("create kicked users data directory: %w", err)
	}
	return &fileKickedUsersStore{
		dataDir:  dataDir,
		filePath: filepath.Join(dataDir, kickedUsersFile),
	}, nil
}

func (s *fileKickedUsersStore) Load() (map[string]kickedUser, error) {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]kickedUser), nil
		}
		return nil, fmt.Errorf("read %s: %w", s.filePath, err)
	}

	users := make(map[string]kickedUser)
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("decode %s: %w", s.filePath, err)
	}
	if users == nil {
		users = make(map[string]kickedUser)
	}
	return users, nil
}

func (s *fileKickedUsersStore) Save(users map[string]kickedUser) error {
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return fmt.Errorf("encode kicked users: %w", err)
	}

	tempFile, err := os.CreateTemp(s.dataDir, ".kicked_users-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary kicked users file: %w", err)
	}
	tempPath := tempFile.Name()
	defer func() { _ = os.Remove(tempPath) }()

	if err = tempFile.Chmod(0600); err == nil {
		var written int
		written, err = tempFile.Write(data)
		if err == nil && written != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = tempFile.Sync()
	}
	closeErr := tempFile.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tempPath, s.filePath)
	}
	if err != nil {
		return fmt.Errorf("save %s: %w", s.filePath, err)
	}

	directory, err := os.Open(s.dataDir)
	if err != nil {
		return fmt.Errorf("open kicked users data directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil {
		return fmt.Errorf("sync kicked users data directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close kicked users data directory: %w", closeErr)
	}
	return nil
}

// kickedUsersManager manages kicked users and handles persistence.
type kickedUsersManager struct {
	log   *zap.Logger
	store kickedUsersStore
	users map[string]kickedUser
	dirty bool
	mu    sync.RWMutex
}

func newKickedUsersManager(log *zap.Logger, dataDir string) (*kickedUsersManager, error) {
	store, err := newFileKickedUsersStore(dataDir)
	if err != nil {
		return nil, err
	}
	return newKickedUsersManagerWithStore(log, store)
}

func newKickedUsersManagerWithStore(log *zap.Logger, store kickedUsersStore) (*kickedUsersManager, error) {
	users, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &kickedUsersManager{log: log, store: store, users: users}, nil
}

func (m *kickedUsersManager) generateKey(userID, channelID string) string {
	return userID + ":" + channelID
}

func (m *kickedUsersManager) IsUserBanned(userID, channelID string) (kickedUser, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	user, exists := m.users[m.generateKey(userID, channelID)]
	if !exists || user.Reinvited || time.Now().After(user.ReinviteAt) {
		return kickedUser{}, false
	}
	return user, true
}

// AddKickedUser durably records a ban before publishing it in memory.
func (m *kickedUsersManager) AddKickedUser(userID, channelID string, timeout time.Duration) (kickedUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	candidate := cloneKickedUsers(m.users)
	ban := kickedUser{
		UserID:     userID,
		ChannelID:  channelID,
		KickedAt:   now,
		ReinviteAt: now.Add(timeout),
	}
	candidate[m.generateKey(userID, channelID)] = ban
	if err := m.store.Save(candidate); err != nil {
		return kickedUser{}, err
	}
	m.users = candidate
	m.dirty = false
	return ban, nil
}

func (m *kickedUsersManager) GetUsersToReinvite() []kickedUser {
	m.mu.RLock()
	defer m.mu.RUnlock()

	usersToReinvite := make([]kickedUser, 0)
	now := time.Now()
	for _, user := range m.users {
		if !user.Reinvited && now.After(user.ReinviteAt) {
			usersToReinvite = append(usersToReinvite, user)
		}
	}
	return usersToReinvite
}

// MarkReinvited records Slack's successful reinvite in memory even if persistence fails.
// Flush retries any failed persistence on the next reconciliation cycle.
func (m *kickedUsersManager) MarkReinvited(reinvited kickedUser) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.generateKey(reinvited.UserID, reinvited.ChannelID)
	user, exists := m.users[key]
	if !exists || !user.KickedAt.Equal(reinvited.KickedAt) {
		return nil
	}
	user.Reinvited = true
	m.users[key] = user
	if err := m.store.Save(m.users); err != nil {
		m.dirty = true
		return err
	}
	m.dirty = false
	return nil
}

func (m *kickedUsersManager) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dirty {
		return nil
	}
	if err := m.store.Save(m.users); err != nil {
		return err
	}
	m.dirty = false
	return nil
}

func (m *kickedUsersManager) CleanupReinvitedUsers() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	oneDayAgo := time.Now().Add(-24 * time.Hour)
	candidate := cloneKickedUsers(m.users)
	changed := false
	for key, user := range candidate {
		if user.Reinvited && user.ReinviteAt.Before(oneDayAgo) {
			delete(candidate, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := m.store.Save(candidate); err != nil {
		return err
	}
	m.users = candidate
	m.dirty = false
	return nil
}

func cloneKickedUsers(users map[string]kickedUser) map[string]kickedUser {
	cloned := make(map[string]kickedUser, len(users))
	for key, user := range users {
		cloned[key] = user
	}
	return cloned
}
