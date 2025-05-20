package http

import (
	"net/http"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
	botSlack "slackbot.arpa/bot/slack"
)

// RegisterEventProcessor adds a Slack event processor to the HTTP server
func (h *Server) RegisterEventProcessor(processor botSlack.EventProcessor) {
	h.eventProcessors = append(h.eventProcessors, processor)
	h.log.Info("Registered Slack event processor",
		zap.String("type", processor.ProcessorType()))
}

// SetSlackClient sets the Slack client for the HTTP server
func (h *Server) SetSlackClient(client *slack.Client) {
	h.slackClient = client
}

// RegisterSlackEndpoints registers HTTP endpoints for handling Slack events
func (h *Server) RegisterSlackEndpoints() {
	// Use the configured path or default to /api/events
	path := "/api/events"
	if h.config.SlackEventPath != "" {
		path = h.config.SlackEventPath
	}

	h.log.Info("Registering Slack events endpoint", zap.String("path", path))

	// Register the Slack events endpoint
	h.serveMux.HandleFunc(path, h.handleSlackEvents)
}

// handleSlackEvents processes Slack events
func (h *Server) handleSlackEvents(w http.ResponseWriter, r *http.Request) {
	if len(h.eventProcessors) == 0 {
		h.log.Debug("No event processors registered, ignoring event")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Forward the request to the Slack handler
	botSlack.HandleSlackEvent(w, r, h.log, h.slackClient, h.eventProcessors)
}
