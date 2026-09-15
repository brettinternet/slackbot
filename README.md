# Slackbot

[![CI](https://github.com/brettinternet/slackbot/actions/workflows/ci.yml/badge.svg)](https://github.com/brettinternet/slackbot/actions/workflows/ci.yml)
[![Build](https://github.com/brettinternet/slackbot/actions/workflows/publish-bot.yaml/badge.svg)](https://github.com/brettinternet/slackbot/actions/workflows/publish-bot.yaml)

Workplace utilities for Slack.

![Slack user-change notifications](./demo.png)

## Features

- Notify a channel when workspace users are added, deactivated, or removed.
- Reply or react to configured message patterns.
- Run vibechecks and temporarily remove users who fail.
- Answer messages with OpenAI-backed, rotating personas and recent conversation context.
- Post an AI-generated shower thought during configured weekday hours.
- Send messages, invite users, or delete bot messages from the CLI.
- Expose health checks at `/health`, `/healthz`, and `/ready`.

## Setup

Requirements: Go 1.26 and [Task](https://taskfile.dev/). [mise](https://mise.jdx.dev/) can install the local toolchain.

```sh
task init # creates .env from example.env when absent
```

Edit `.env` and `config.yaml`, then start the bot:

```sh
task start
```

The bot requires `SLACK_TOKEN`. User monitoring is optional; enable it with a Slack conversation ID
such as `C0123456789` or `G0123456789` in `SLACK_USER_NOTIFY_CHANNEL` or `user.notify_channel`.
Grant `users:read`, `channels:read`, `groups:read`, and `chat:write`, and invite the bot to the destination.

For message, vibecheck, and AI chat events:

1. Set `SLACK_SIGNING_SECRET`.
2. Expose `POST /api/slack/events` publicly.
3. Set that URL under **Event Subscriptions** in the Slack app.
4. Subscribe to `app_mention` and the message events needed for your channels.

Add bot token scopes for the features you enable. Typical scopes include `users:read`, `chat:write`,
`reactions:write`, `app_mentions:read`, and channel history. Vibecheck and invite commands also need
permission to manage channel membership.

## Configuration

`config.yaml` contains complete examples for chat responses, vibecheck, AI personas, context limits,
and shower thoughts. Environment variables and CLI flags provide credentials and runtime settings:

```sh
task bot:run -- --help
```

Useful settings:

| Setting                   | Default             | Purpose                            |
| ------------------------- | ------------------- | ---------------------------------- |
| `CONFIG_FILE`             | `./config.yaml`     | YAML or JSON feature configuration |
| `DATA_DIR`                | `./`                | SQLite and user-state storage      |
| `SERVER_PORT`             | `4200`              | HTTP port                          |
| `SLACK_EVENTS_PATH`       | `/api/slack/events` | Slack Events API path              |
| `OPENAI_MODEL`            | application default | AI model                           |
| `OPENAI_REASONING_EFFORT` | application default | Model reasoning effort             |

The Slack Events endpoint accepts only `POST` requests and limits request bodies to 1 MiB. Slack
signature verification uses the original request bytes before JSON parsing.

Chat response changes in `config.yaml` reload while the bot runs. Restart after changing other feature
settings.

## Container

```yaml
services:
  slackbot:
    image: ghcr.io/brettinternet/slackbot:main
    ports:
      - "4200:4200"
    environment:
      CONFIG_FILE: /app/config.yaml
      DATA_DIR: /app/data
      SLACK_TOKEN: "${SLACK_TOKEN}"
      SLACK_SIGNING_SECRET: "${SLACK_SIGNING_SECRET}"
      SLACK_USER_NOTIFY_CHANNEL: C0123456789
      OPENAI_API_KEY: "${OPENAI_API_KEY}"
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - ./data:/app/data
```

Docker secrets are also read from `/run/secrets/slack_token`,
`/run/secrets/slack_signing_secret`, and `/run/secrets/openai_api_key`.

## CLI examples

```sh
# Send to one channel
task bot:run -- send-message --message "Hello" --channels C01234567

# Invite users to channels
task bot:run -- invite-channel --users U01234567 --channels C01234567

# Delete messages posted by bots in a channel
task bot:run -- delete-messages-from-channel --channel C01234567
```

## Development

```sh
task test   # tests
task check  # lint and security checks
task fix    # formatting and automatic fixes
task build  # build ./bin/bot
```
