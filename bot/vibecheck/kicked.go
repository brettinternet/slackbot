package vibecheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

const kickedUsersFile = "kicked_users.json"

// kickedUser represents a user who has been kicked from a channel
type kickedUser struct {
	UserID     string    `json:"user_id"`
	ChannelID  string    `json:"channel_id"`
	KickedAt   time.Time `json:"kicked_at"`
	ReinviteAt time.Time `json:"reinvite_at"`
	Reinvited  bool      `json:"reinvited"`
}

// kickedUsersManager manages kicked users and handles persistence
type kickedUsersManager struct {
	log      *zap.Logger
	users    map[string]kickedUser // key is userID+channelID
	dataDir  string
	filePath string
	mu       sync.RWMutex
}

// NewKickedUsersManager creates a new manager for kicked users
func newKickedUsersManager(log *zap.Logger, dataDir string) *kickedUsersManager {
	filePath := filepath.Join(dataDir, kickedUsersFile)

	manager := &kickedUsersManager{
		log:      log,
		users:    make(map[string]kickedUser),
		dataDir:  dataDir,
		filePath: filePath,
	}

	manager.loadFromDisk()
	return manager
}

// generateKey creates a unique key for the user+channel combination
func (m *kickedUsersManager) generateKey(userID, channelID string) string {
	return userID + ":" + channelID
}

// AddKickedUser adds a user to the kicked list with a reinvite time
func (m *kickedUsersManager) AddKickedUser(userID, channelID string, timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	key := m.generateKey(userID, channelID)

	m.users[key] = kickedUser{
		UserID:     userID,
		ChannelID:  channelID,
		KickedAt:   now,
		ReinviteAt: now.Add(timeout),
		Reinvited:  false,
	}

	m.saveToDisk()
}

// GetUsersToReinvite returns all users who should be reinvited now
func (m *kickedUsersManager) GetUsersToReinvite() []kickedUser {
	m.mu.Lock()
	defer m.mu.Unlock()

	var usersToReinvite []kickedUser
	now := time.Now()

	for key, user := range m.users {
		if !user.Reinvited && now.After(user.ReinviteAt) {
			usersToReinvite = append(usersToReinvite, user)

			// Mark as reinvited
			user.Reinvited = true
			m.users[key] = user
		}
	}

	if len(usersToReinvite) > 0 {
		m.saveToDisk()
	}

	return usersToReinvite
}

// CleanupReinvitedUsers removes users who have been reinvited for more than a day
func (m *kickedUsersManager) CleanupReinvitedUsers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	oneDayAgo := time.Now().Add(-24 * time.Hour)
	needSave := false

	for key, user := range m.users {
		if user.Reinvited && user.ReinviteAt.Before(oneDayAgo) {
			delete(m.users, key)
			needSave = true
		}
	}

	if needSave {
		m.saveToDisk()
	}
}

// saveToDisk saves the kicked users data to a JSON file
func (m *kickedUsersManager) saveToDisk() {
	data, err := json.MarshalIndent(m.users, "", "  ")
	if err != nil {
		m.log.Error("Failed to marshal kicked users data", zap.Error(err))
		return
	}

	err = os.WriteFile(m.filePath, data, 0644)
	if err != nil {
		m.log.Error("Failed to save kicked users data", zap.Error(err), zap.String("path", m.filePath))
	}
}

// loadFromDisk loads kicked users data from the JSON file
func (m *kickedUsersManager) loadFromDisk() {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			m.log.Error("Failed to read kicked users data", zap.Error(err), zap.String("path", m.filePath))
		}
		return
	}

	err = json.Unmarshal(data, &m.users)
	if err != nil {
		m.log.Error("Failed to unmarshal kicked users data", zap.Error(err))
	}
}
