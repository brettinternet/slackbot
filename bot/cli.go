package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"
	"slackbot.arpa/bot/config"
)

var currentProcessName string = filepath.Base(os.Args[0])

type cmdWithArgs func(ctx context.Context, cmd *cli.Command, s *Bot) error

// Wrap subcommands to inject the server dependency
func cmdWithServer(action cmdWithArgs, server *Bot) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		return action(ctx, cmd, server)
	}
}

type setupWithArgs func(ctx context.Context, cmd *cli.Command) (context.Context, error)

func setup(setup setupWithArgs) cli.BeforeFunc {
	return func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
		return setup(ctx, cmd)
	}
}

func NewCommandRoot(s *Bot) (*bool, *cli.Command) {
	opts := s.BuildOpts
	version := fmt.Sprintf("%s (%s)", opts.BuildVersion, opts.BuildTime)
	if opts.BuildTime == "" {
		version = opts.BuildVersion
	}
	start := new(bool)
	return start, &cli.Command{
		Name:    "slackbot",
		Usage:   "Multifunctional operating slack bot system for blah blah",
		Version: version,
		Before:  setup(s.Setup), // runs before any command to initialize the server
		Action: func(ctx context.Context, c *cli.Command) error {
			*start = true
			return nil
		},
		Commands:               Commands(s),
		Flags:                  config.Flags(),
		MutuallyExclusiveFlags: config.MutuallyExclusiveFlags(),
	}
}

func Commands(s *Bot) []*cli.Command {
	return []*cli.Command{}
}
