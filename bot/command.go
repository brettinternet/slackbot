package bot

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"
	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
)

type DeleteMessagesFromChannelCommandFlags struct {
	Channel string
}

func newDeleteMessagesFromChannelCommandFlags(cmd *cli.Command) *DeleteMessagesFromChannelCommandFlags {
	return &DeleteMessagesFromChannelCommandFlags{
		Channel: cmd.String("channel"),
	}
}

func newDeleteMessagesFromChannelCommand(s *Bot) *cli.Command {
	return &cli.Command{
		Name:   "delete-messages-from-channel",
		Usage:  "Delete messages from channel",
		Action: cmdWithBot(deleteMessagesFromChannel, s),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "channel",
				Usage:    "Channel ID to delete messages from",
				Required: true,
			},
		},
	}
}

func deleteMessagesFromChannel(ctx context.Context, cmd *cli.Command, s *Bot) error {
	f := newDeleteMessagesFromChannelCommandFlags(cmd)
	if f.Channel == "" {
		return fmt.Errorf("channel ID is required")
	}

	s.log.Info("Deleting bot messages from channel", zap.String("channel", f.Channel))

	client := s.slack.Client()
	if client == nil {
		return fmt.Errorf("slack client is unavailable")
	}

	// Get bot's own user ID
	authTest, err := client.AuthTest()
	if err != nil {
		s.log.Error("Failed to get bot user ID", zap.Error(err))
		return fmt.Errorf("failed to get bot user ID: %w", err)
	}
	botUserID := authTest.UserID
	s.log.Info("Bot user ID retrieved", zap.String("botUserID", botUserID))

	// Get conversation history
	params := &slack.GetConversationHistoryParameters{
		ChannelID: f.Channel,
		Limit:     1000, // Maximum allowed by Slack API
		Inclusive: true,
	}

	var messagesDeleted int
	for {
		history, err := client.GetConversationHistoryContext(ctx, params)
		if err != nil {
			s.log.Error("Failed to get conversation history", zap.Error(err))
			return fmt.Errorf("failed to get conversation history: %w", err)
		}

		for _, msg := range history.Messages {
			// Won't delete messages sent by msg.User == "USLACKBOT" 😢
			if msg.User == botUserID || msg.BotID != "" || msg.User == "USLACKBOT" {
				_, _, err := client.DeleteMessageContext(ctx, f.Channel, msg.Timestamp)
				if err != nil {
					s.log.Warn("Failed to delete message", zap.String("ts", msg.Timestamp), zap.Error(err))
					continue
				}
				messagesDeleted++
				s.log.Debug("Message deleted", zap.String("ts", msg.Timestamp))
			}
		}

		if !history.HasMore {
			break
		}

		params.Cursor = history.ResponseMetaData.NextCursor
	}

	s.log.Info("Finished deleting bot messages", zap.Int("messagesDeleted", messagesDeleted))
	return nil
}
