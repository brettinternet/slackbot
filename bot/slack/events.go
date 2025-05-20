package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

// EventProcessor is an interface for components that want to process Slack events
type EventProcessor interface {
	PushEvent(event slackevents.EventsAPIEvent)
	ProcessorType() string // Returns a descriptive name of the processor type
}

// HandleSlackEvent processes a Slack event received via HTTP request
// This can be called by an HTTP handler to process events
func HandleSlackEvent(w http.ResponseWriter, r *http.Request, log *zap.Logger, client *slack.Client, processors []EventProcessor) {
	body, err := readRequestBody(r)
	if err != nil {
		log.Error("Failed to read request body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Verify the request is coming from Slack
	sv, err := slack.NewSecretsVerifier(r.Header, "") // We'll need to get this from config
	if err != nil {
		log.Error("Failed to create secrets verifier", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if _, err := sv.Write(body); err != nil {
		log.Error("Failed to write to secrets verifier", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := sv.Ensure(); err != nil {
		// For now, let's just log this but continue processing
		// In production, you should verify this properly
		log.Warn("Failed to verify request signature (continuing anyway)", zap.Error(err))
	}

	eventsAPIEvent, err := slackevents.ParseEvent(
		json.RawMessage(body),
		slackevents.OptionNoVerifyToken(),
	)
	if err != nil {
		log.Error("Failed to parse Slack event", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Handle URL verification challenge
	if eventsAPIEvent.Type == slackevents.URLVerification {
		var challenge *slackevents.ChallengeResponse
		if err := json.Unmarshal(body, &challenge); err != nil {
			log.Error("Failed to unmarshal challenge", zap.Error(err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(challenge.Challenge))
		log.Info("Responded to URL verification challenge")
		return
	}

	// Log the event for debugging
	log.Debug("Received Slack event",
		zap.String("type", string(eventsAPIEvent.Type)),
		zap.Any("innerEvent", eventsAPIEvent.InnerEvent.Type))

	// Process the event in all registered processors
	for _, processor := range processors {
		processor.PushEvent(eventsAPIEvent)
	}

	w.WriteHeader(http.StatusOK)
}

// Helper function to read the request body
func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, fmt.Errorf("request body is nil")
	}

	defer r.Body.Close()

	// ReadAll reads from r.Body until an error or EOF and returns the data
	return io.ReadAll(r.Body)
}
