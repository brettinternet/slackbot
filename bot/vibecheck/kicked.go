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

// newKickedUsersManager creates a new manager for kicked users
func newKickedUsersManager(log *zap.Logger, dataDir string) *kickedUsersManager {
	filePath := filepath.Join(dataDir, kickedUsersFile)

	log.Debug("Initializing kicked users manager",
		zap.String("data_dir", dataDir),
		zap.String("file_path", filePath),
	)

	manager := &kickedUsersManager{
		log:      log,
		users:    make(map[string]kickedUser),
		dataDir:  dataDir,
		filePath: filePath,
	}

	// Ensure data directory exists
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		log.Error("Failed to create data directory", zap.Error(err), zap.String("path", dataDir))
	} else {
		log.Debug("Ensured data directory exists", zap.String("path", dataDir))
	}

	manager.loadFromDisk()
	return manager
}

// generateKey creates a unique key for the user+channel combination
func (m *kickedUsersManager) generateKey(userID, channelID string) string {
	return userID + ":" + channelID
}

// IsUserBanned checks if a user is currently banned from a channel
func (k *kickedUsersManager) IsUserBanned(userID, channelID string) (kickedUser, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	
	key := k.generateKey(userID, channelID)
	user, exists := k.users[key]
	if !exists || user.Reinvited {
		return kickedUser{}, false
	}
	
	// Check if ban time has expired
	if time.Now().After(user.ReinviteAt) {
		return kickedUser{}, false
	}
	
	return user, true
}

// AddKickedUser adds a user to the kicked list with a reinvite time
func (m *kickedUsersManager) AddKickedUser(userID, channelID string, timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	key := m.generateKey(userID, channelID)

	m.log.Debug("Adding user to kicked users list",
		zap.String("user_id", userID),
		zap.String("channel_id", channelID),
		zap.Time("kicked_at", now),
		zap.Time("reinvite_at", now.Add(timeout)),
	)

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
			m.log.Debug("Found user ready for reinvite",
				zap.String("user_id", user.UserID),
				zap.String("channel_id", user.ChannelID),
				zap.Time("kicked_at", user.KickedAt),
				zap.Time("reinvite_at", user.ReinviteAt),
			)

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
	removedCount := 0

	for key, user := range m.users {
		if user.Reinvited && user.ReinviteAt.Before(oneDayAgo) {
			delete(m.users, key)
			needSave = true
			removedCount++
		}
	}

	if needSave {
		m.log.Debug("Cleaned up old reinvited users", zap.Int("removed_count", removedCount))
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

	err = os.WriteFile(m.filePath, data, 0600)
	if err != nil {
		m.log.Error("Failed to save kicked users data", zap.Error(err), zap.String("path", m.filePath))
	} else {
		m.log.Debug("Saved kicked users data to disk",
			zap.String("path", m.filePath),
			zap.Int("num_users", len(m.users)),
		)
	}
}

// loadFromDisk loads kicked users data from the JSON file
func (m *kickedUsersManager) loadFromDisk() {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			m.log.Error("Failed to read kicked users data", zap.Error(err), zap.String("path", m.filePath))
		} else {
			m.log.Debug("No kicked users file exists yet", zap.String("path", m.filePath))
		}
		return
	}

	err = json.Unmarshal(data, &m.users)
	if err != nil {
		m.log.Error("Failed to unmarshal kicked users data", zap.Error(err))
	} else {
		m.log.Debug("Loaded kicked users data from disk",
			zap.String("path", m.filePath),
			zap.Int("num_users", len(m.users)),
		)
	}
}
