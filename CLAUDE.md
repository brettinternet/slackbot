# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Slackbot is a Go application providing workplace utilities for Slack organizations, including user monitoring, AI chat, vibecheck functionality, and automated responses. Built with Go 1.24.2 using the Slack Go SDK and OpenAI integration via LangChain.

## Development Commands

### Setup

```bash
task init          # Install dependencies and setup environment
```

### Running

```bash
task start         # Start server with hot reload (air)
task bot:run       # Run with custom arguments
```

### Testing

```bash
task test          # Run tests with gotestsum formatting
go test -race -coverprofile=coverage.out -covermode=atomic ./...  # Tests with coverage
```

### Code Quality

```bash
task check         # Run all checks (lint, security, format)
task fix           # Auto-fix issues where possible
```

Individual tools:

- `go vet ./...` - Go static analysis
- `golangci-lint run --timeout 5m` - Comprehensive linting
- `gosec ./...` - Security scanning
- `errcheck ./...` - Error checking
- `dprint check` / `dprint fmt` - Config file formatting

### Building

```bash
task bot:build     # Build binary with version info
go build ./cmd/bot # Simple build
```

## Architecture

```
cmd/bot/           # Main application entry point
bot/               # Core business logic
├── config/        # Configuration management
├── slack/         # Slack API integration  
├── aichat/        # AI chat functionality
├── user/          # User management
├── vibecheck/     # Vibecheck feature
├── http/          # HTTP server
├── ai/            # AI utilities
└── chat/          # Chat handling
```

The application follows a modular architecture with feature-based packages. Configuration is centralized and supports hot-reloading. The Bot struct (`bot/bot.go`) serves as the main orchestrator.

## Code Conventions

- Standard Go formatting (gofmt)
- Tabs for Go code indentation, 2 spaces for config files
- 100 character line limit
- Structured logging with zap
- Standard Go error handling patterns
- Test coverage target: 80%+

## Task Completion Requirements

Always run after making changes:

1. `task check` - Ensures code quality and security
2. `task test` - Verifies functionality
3. `task fix` - Auto-fixes common issues if needed

Pre-commit hooks (lefthook) automatically run checks and secret scanning.

## Key Dependencies

- `github.com/slack-go/slack` - Slack API client
- `github.com/tmc/langchaingo` - OpenAI integration
- `github.com/urfave/cli/v3` - CLI framework
- `modernc.org/sqlite` - Database
- `go.uber.org/zap` - Structured logging
