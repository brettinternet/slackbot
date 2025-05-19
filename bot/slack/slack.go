package slack

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

type Config struct {
	Token string
	Debug bool
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
	if s.config.Token == "" {
		return fmt.Errorf("no Slack authentication credentials provided")
	}

	clientOpts := []slack.Option{
		slack.OptionDebug(s.config.Debug),
	}

	s.client = slack.New(s.config.Token, clientOpts...)

	if _, err := s.client.AuthTest(); err != nil {
		return fmt.Errorf("failed to authenticate with Slack: %w", err)
	}

	return nil
}

func (s *Slack) Stop(ctx context.Context) error {
	return nil
}

func (s *Slack) Client() *slack.Client {
	return s.client
}
