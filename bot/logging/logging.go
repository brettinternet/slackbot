package logging

import (
	"context"

	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

const CorrelationIDKey = "correlation_id"

type contextKey struct{}

// Component adds the stable subsystem name expected on operational logs.
func Component(log *zap.Logger, component string) *zap.Logger {
	return log.With(zap.String("component", component))
}

// Operation identifies the bounded action that emitted an error or warning.
func Operation(operation string) zap.Field {
	return zap.String("operation", operation)
}

// SlackEventID returns Slack's safe, workspace-unique callback identifier when present.
func SlackEventID(event slackevents.EventsAPIEvent) string {
	switch data := event.Data.(type) {
	case *slackevents.EventsAPICallbackEvent:
		if data != nil {
			return data.EventID
		}
		return ""
	case slackevents.EventsAPICallbackEvent:
		return data.EventID
	default:
		return ""
	}
}

// ForSlackEvent correlates logs without copying message bodies or other event payload data.
func ForSlackEvent(log *zap.Logger, event slackevents.EventsAPIEvent) *zap.Logger {
	if eventID := SlackEventID(event); eventID != "" {
		return log.With(zap.String(CorrelationIDKey, eventID))
	}
	return log
}

// WithSlackEvent stores an event-scoped logger for downstream calls that already accept context.
func WithSlackEvent(ctx context.Context, log *zap.Logger, event slackevents.EventsAPIEvent) context.Context {
	return context.WithValue(ctx, contextKey{}, ForSlackEvent(log, event))
}

// FromContext returns the event-scoped logger, or fallback outside event processing.
func FromContext(ctx context.Context, fallback *zap.Logger) *zap.Logger {
	if ctx != nil {
		if log, ok := ctx.Value(contextKey{}).(*zap.Logger); ok {
			return log
		}
	}
	return fallback
}
