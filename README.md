# Slackbot

Utilities for the workplace.

## Setup

Create a Slack app. From "OAuth & Permissions" in the app's menu, you can "Install to <workspace>" and then get a "Bot User OAuth Token" which is the Slack token used in this service. Add necessary scopes per feature.

## Features

### Obituaries

Watches the workspace users to observe which users are no longer present.

Requires scopes: `channels:history`, `groups:history` and `chat:write`.

Requires `SLACK_OBITUARY_NOTIFY_CHANNEL` with the channel ID.
