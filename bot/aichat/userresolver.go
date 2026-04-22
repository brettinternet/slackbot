package aichat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/slack-go/slack"
)

type userNameResolver struct {
	names  map[string]string // userID -> first name
	client *slack.Client
	log    *zap.Logger
}

type usersFileEntry struct {
	ID       string `json:"id"`
	RealName string `json:"real_name"`
}

func newUserNameResolver(dataDir string, client *slack.Client, log *zap.Logger) *userNameResolver {
	r := &userNameResolver{
		names:  make(map[string]string),
		client: client,
		log:    log,
	}
	if dataDir != "" {
		r.loadFromFile(filepath.Join(dataDir, "users.json"))
	}
	return r
}

func (r *userNameResolver) loadFromFile(path string) {
	data, err := os.ReadFile(path) //#nosec G304 -- path is constructed from configured DataDir
	if err != nil {
		return
	}
	var entries []usersFileEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	for _, e := range entries {
		if e.ID != "" && e.RealName != "" {
			r.names[e.ID] = firstNameFrom(e.RealName)
		}
	}
}

func (r *userNameResolver) resolve(ctx context.Context, userID string) string {
	if name, ok := r.names[userID]; ok {
		return name
	}
	if r.client == nil {
		return ""
	}
	user, err := r.client.GetUserInfoContext(ctx, userID)
	if err != nil {
		r.log.Warn("Failed to resolve user name", zap.String("user", userID), zap.Error(err))
		r.names[userID] = "" // cache miss to avoid repeated calls
		return ""
	}
	name := firstNameFrom(user.Profile.RealName)
	if name == "" {
		name = firstNameFrom(user.Profile.DisplayName)
	}
	if name == "" {
		name = user.Name
	}
	r.names[userID] = name
	return name
}

// firstNameFrom derives a first name from a real name by taking the first whitespace-delimited token.
func firstNameFrom(realName string) string {
	parts := strings.Fields(realName)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}
