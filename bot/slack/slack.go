package slack

import (
	"context"
	"fmt"
	"net/http"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

type Config struct {
	ClientID     string
	ClientSecret string
	Debug        bool
}

type Slack struct {
	log    *zap.Logger
	config Config
	client *slack.Client
}

func NewSlack(log *zap.Logger, config Config) *Slack {
	return &Slack{
		log:    log,
		config: config,
	}
}

func (s *Slack) Start(ctx context.Context) error {
	if s.config.ClientID == "" || s.config.ClientSecret == "" {
		return fmt.Errorf("no Slack authentication credentials provided")
	}

	clientOpts := []slack.Option{
		slack.OptionDebug(s.config.Debug),
	}

	s.log.Info("Using Slack OAuth authentication with client ID and secret")
	oauthResp, err := slack.GetOAuthV2Response(
		http.DefaultClient,
		s.config.ClientID,
		s.config.ClientSecret,
		"", // code is empty for client_credentials grant
		"", // redirect URI is not needed for client_credentials grant
	)
	if err != nil {
		return fmt.Errorf("failed to authenticate with Slack: %w", err)
	}

	s.client = slack.New(oauthResp.AccessToken, clientOpts...)

	if oauthResp.RefreshToken != "" {
		s.log.Debug("Received refresh token, token will be automatically refreshed")
		// Here you could set up a background refresh process for the token
	}

	return nil
}

func (s *Slack) Stop(ctx context.Context) error {
	return nil
}

func (s *Slack) Client() *slack.Client {
	return s.client
}
