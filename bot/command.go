package bot

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"
	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
)

type deleteMessagesFromChannelCommandFlags struct {
	Channel string
}

func newDeleteMessagesFromChannelCommandFlags(cmd *cli.Command) *deleteMessagesFromChannelCommandFlags {
	return &deleteMessagesFromChannelCommandFlags{
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

type inviteToChannelCommandFlags struct {
	Users    []string
	Channels []string
}

func newInviteToChannelCommandFlags(cmd *cli.Command) *inviteToChannelCommandFlags {
	return &inviteToChannelCommandFlags{
		Users:    cmd.StringSlice("users"),
		Channels: cmd.StringSlice("channels"),
	}
}

func newInviteToChannelCommand(s *Bot) *cli.Command {
	return &cli.Command{
		Name:   "invite-channel",
		Usage:  "Invite users to bot channels",
		Action: cmdWithBot(inviteToChannel, s),
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:     "users",
				Aliases:  []string{"u"},
				Usage:    "User ID to invite.",
				Required: true,
			},
			&cli.StringSliceFlag{
				Name:     "channels",
				Aliases:  []string{"c"},
				Usage:    "Channel ID to invite to. The bot must be a member of the channel.",
				Required: true,
			},
		},
	}
}

func inviteToChannel(ctx context.Context, cmd *cli.Command, s *Bot) error {
	f := newInviteToChannelCommandFlags(cmd)
	if f.Channels == nil {
		return fmt.Errorf("channel ID is required")
	}
	if f.Users == nil {
		return fmt.Errorf("user ID is required")
	}
	s.log.Info("Inviting users to channels", zap.Strings("channels", f.Channels), zap.Strings("users", f.Users))

	client := s.slack.Client()
	if client == nil {
		return fmt.Errorf("slack client is unavailable")
	}

	for _, channel := range f.Channels {
		for _, user := range f.Users {
			_, err := client.InviteUsersToConversationContext(ctx, channel, user)
			if err != nil {
				s.log.Error("Failed to invite user to channel", zap.String("channel", channel), zap.String("user", user), zap.Error(err))
				continue
			}
			s.log.Debug("User invited to channel", zap.String("channel", channel), zap.String("user", user))
		}
	}

	s.log.Info("Finished inviting users.", zap.Int("users", len(f.Users)), zap.Int("channels", len(f.Channels)))
	return nil
}

type sendMessageCommandFlags struct {
	Message  string
	Channels []string
}

func newSendMessageCommandFlags(cmd *cli.Command) *sendMessageCommandFlags {
	return &sendMessageCommandFlags{
		Message:  cmd.String("message"),
		Channels: cmd.StringSlice("channels"),
	}
}

func newSendMessageCommand(s *Bot) *cli.Command {
	return &cli.Command{
		Name:   "send-message",
		Usage:  "Send a message to specified channels or preferred channels",
		Action: cmdWithBot(sendMessage, s),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "message",
				Aliases:  []string{"m"},
				Usage:    "Message text to send",
				Required: true,
			},
			&cli.StringSliceFlag{
				Name:    "channels",
				Aliases: []string{"c"},
				Usage:   "Channel IDs to send message to. If not specified, uses preferred channels from config",
			},
		},
	}
}

func sendMessage(ctx context.Context, cmd *cli.Command, s *Bot) error {
	f := newSendMessageCommandFlags(cmd)
	if f.Message == "" {
		return fmt.Errorf("message text is required")
	}

	channels := f.Channels
	if len(channels) == 0 {
		config := s.configManager.GetConfig()
		if config == nil {
			return fmt.Errorf("configuration is unavailable")
		}
		channels = config.Slack.PreferredChannels
		if len(channels) == 0 {
			return fmt.Errorf("no channels specified and no preferred channels configured")
		}
		s.log.Info("Using preferred channels from config", zap.Strings("channels", channels))
	} else {
		s.log.Info("Using specified channels", zap.Strings("channels", channels))
	}

	s.log.Info("Sending message to channels", zap.String("message", f.Message), zap.Strings("channels", channels))

	client := s.slack.Client()
	if client == nil {
		return fmt.Errorf("slack client is unavailable")
	}

	var messagesSent int
	for _, channel := range channels {
		_, _, err := client.PostMessageContext(ctx, channel, slack.MsgOptionText(f.Message, false))
		if err != nil {
			s.log.Error("Failed to send message to channel", zap.String("channel", channel), zap.Error(err))
			continue
		}
		messagesSent++
		s.log.Debug("Message sent to channel", zap.String("channel", channel))
	}

	s.log.Info("Finished sending messages", zap.Int("messagesSent", messagesSent), zap.Int("totalChannels", len(channels)))
	return nil
}
