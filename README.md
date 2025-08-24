# Slackbot

Utilities for the workplace.

![slack bot notifications when user is removed or added](./demo.png)

## Features

See example [config.yaml](config.yaml) or environment variables in [flag.go](./bot/config/flag.go) for feature configuration. Updates to this file will update the runtime configuration while it's running.

- Obituaries & user watch to get notified when users are removed or added from the Slack org (scopes: `channels:history`, `groups:history` and `chat:write`)
- Chat responses and reactions, requires `SLACK_SIGNING_SECRET`, configured responses, and a public event endpoint
- Vibecheck - failing a vibecheck will result in a temporary ban from the channel
- AI Chat with configurable prompts for sticky (assigned to users at random for 1 hour) personas, requires `SLACK_SIGNING_SECRET`, `OPENAI_API_KEY` and configuring a public event endpoint
- Deployable with a [container](https://github.com/brettinternet/slackbot/pkgs/container/slackbot)

## Setup

Create a Slack app: From "OAuth & Permissions" in the app's menu, you can "Install to workspace" and then get a "Bot User OAuth Token" which is the Slack token used in this service. Add necessary scopes per feature.

Manage the app via the CLI, run with `--help` to see options and valid environment variables. Requires `SLACK_TOKEN` or `SLACK_TOKEN_FILE`.

## Run

Here's how a minimal docker-compose service might look for the bot deployment. See also [docker-compose](./docker-compose.yaml).

```yaml
services:
  slackbot:
    image: ghcr.io/brettinternet/slackbot:main
    environment:
      LOG_LEVEL: debug
      SERVER_PORT: 4200
      DATA_DIR: /app/data
      CONFIG_FILE: /app/data/config.yaml
      SLACK_TOKEN: "${SLACK_TOKEN}"
      SLACK_USER_NOTIFY_CHANNEL: mybotchannel
      SLACK_SIGNING_SECRET: "${SLACK_SIGNING_SECRET}"
      SLACK_PREFERRED_USERS: ADMINUSERID
      OPENAI_API_KEY: "${OPENAI_API_KEY}"
    volumes:
      - "${CONFIG_DIR}/slackbot:/app/data"
```
