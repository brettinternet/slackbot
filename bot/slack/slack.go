package slack

import (
	"context"
	"fmt"
	"net/http"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

type Config struct {
	Token             string
	SigningSecret     string
	Debug             bool
	PreferredChannels []string
}

type Slack struct {
	log      *zap.Logger
	config   Config
	client   *slack.Client
	authResp *slack.AuthTestResponse
}

func NewSlack(log *zap.Logger, config Config) *Slack {
	return &Slack{
		log:    log,
		config: config,
	}
}

func (s *Slack) Setup(ctx context.Context) error {
	if s.config.Token == "" {
		return fmt.Errorf("no Slack authentication credentials provided")
	}

	clientOpts := []slack.Option{
		slack.OptionDebug(s.config.Debug),
	}

	s.client = slack.New(s.config.Token, clientOpts...)

	if resp, err := s.client.AuthTest(); err != nil {
		return fmt.Errorf("authenticate with Slack: %w", err)
	} else {
		s.authResp = resp
	}

	return nil
}

func (s *Slack) Start(ctx context.Context) error {
	if err := s.client.SetUserPresenceContext(ctx, "auto"); err != nil {
		return fmt.Errorf("user presence auto: %w", err)
	}

	for _, channel := range s.config.PreferredChannels {
		_, _, _, err := s.client.JoinConversationContext(ctx, channel)
		if err != nil {
			s.log.Error("Failed to join channel", zap.String("channel", channel), zap.Error(err))
		}
	}

	return nil
}

func (s *Slack) Stop(ctx context.Context) error {
	if err := s.client.SetUserPresenceContext(ctx, "away"); err != nil {
		return fmt.Errorf("user presence away: %w", err)
	}
	return nil
}

func (s *Slack) Client() *slack.Client {
	return s.client
}

// VerifyRequest validates the request body against the Slack signing secret
func (s *Slack) VerifyRequest(header http.Header, body []byte) error {
	sv, err := slack.NewSecretsVerifier(header, s.config.SigningSecret)
	if err != nil {
		return fmt.Errorf("create secrets verifier: %w", err)
	}

	if _, err := sv.Write(body); err != nil {
		return fmt.Errorf("write to secrets verifier: %w", err)
	}

	if err := sv.Ensure(); err != nil {
		return fmt.Errorf("verify request signature: %w", err)
	}

	return nil
}

func (s *Slack) OrgURL() string {
	return s.authResp.URL
}
