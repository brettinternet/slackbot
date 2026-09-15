package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

// SlackEventMaxBodyBytes is the largest Slack Events request body accepted by the server.
const SlackEventMaxBodyBytes int64 = 1 << 20 // 1 MiB

// slackEventProcessor is an interface for components that want to process Slack events
type slackEventProcessor interface {
	PushEvent(slackevents.EventsAPIEvent) error
	ProcessorType() string
}

type slackEventPendingReporter interface {
	PendingEvents() int64
}

func (h *Server) RegisterEventProcessor(processor slackEventProcessor) {
	dispatcher := newSlackEventDispatcher(h.log, processor, h.metrics)

	h.dispatchMu.Lock()
	defer h.dispatchMu.Unlock()
	if !h.dispatchAccepting {
		h.log.Warn("Slack event processor unavailable during registration",
			zap.String("processor", processor.ProcessorType()))
		return
	}
	h.slackEventProcessors = append(h.slackEventProcessors, dispatcher)
	h.dispatchWG.Add(1)
	go func() {
		defer h.dispatchWG.Done()
		dispatcher.run()
	}()
	h.log.Info("Registered Slack event processor.",
		zap.String("type", processor.ProcessorType()),
		zap.Int("queue_capacity", SlackEventQueueCapacity),
		zap.String("overflow_policy", "drop_newest"))
}

// RegisterSlackEndpoints registers HTTP endpoints for handling Slack events
func (h *Server) registerSlackEndpoints() {
	path := "/api/slack/events"
	if h.config.SlackEventPath != "" {
		path = h.config.SlackEventPath
	}

	h.log.Info("Registering Slack events endpoint", zap.String("path", path))

	h.serveMux.HandleFunc(path, h.handleSlackEvents)
}

// handleSlackEvents processes Slack events
func (h *Server) handleSlackEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := readRequestBody(w, r)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			h.log.Warn("Slack event request body exceeds limit",
				zap.Int64("limitBytes", SlackEventMaxBodyBytes))
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		h.log.Error("Failed to read Slack event request body", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := h.slack.VerifyRequest(r.Header, body); err != nil {
		h.log.Error("Failed to verify request.", zap.Error(err))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	eventsAPIEvent, err := slackevents.ParseEvent(
		json.RawMessage(body),
		slackevents.OptionNoVerifyToken(),
	)
	if err != nil {
		h.log.Warn("Failed to parse Slack event", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Handle URL verification challenge
	if eventsAPIEvent.Type == slackevents.URLVerification {
		var challenge *slackevents.ChallengeResponse
		if err := json.Unmarshal(body, &challenge); err != nil {
			h.log.Warn("Failed to unmarshal Slack challenge", zap.Error(err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		response := map[string]string{"challenge": challenge.Challenge}
		responseBytes, err := json.Marshal(response)
		if err != nil {
			h.log.Error("Failed to marshal challenge response", zap.Error(err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(responseBytes)
		h.log.Info("Responded to URL verification challenge")
		return
	}

	eventID := slackEventID(eventsAPIEvent)
	duplicate, accepted := h.eventDeduplicator.accept(eventID, func() bool {
		return h.dispatchSlackEvent(eventsAPIEvent)
	})
	if duplicate {
		if h.metrics != nil {
			h.metrics.IncDeduplicatedEvent()
		}
		h.log.Debug("Ignoring duplicate Slack event", zap.String("event_id", eventID))
		w.WriteHeader(http.StatusOK)
		return
	}
	if !accepted {
		http.Error(w, "Slack event processing is unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func slackEventID(event slackevents.EventsAPIEvent) string {
	switch data := event.Data.(type) {
	case *slackevents.EventsAPICallbackEvent:
		return data.EventID
	case slackevents.EventsAPICallbackEvent:
		return data.EventID
	default:
		return ""
	}
}

func (h *Server) dispatchSlackEvent(event slackevents.EventsAPIEvent) bool {
	h.dispatchMu.RLock()
	defer h.dispatchMu.RUnlock()

	if len(h.slackEventProcessors) == 0 {
		h.log.Debug("No event processors registered, ignoring event")
		return true
	}
	if !h.dispatchAccepting {
		h.log.Warn("Slack event processors unavailable; event not dispatched",
			zap.Int("processor_count", len(h.slackEventProcessors)))
		return false
	}

	h.log.Debug("Received Slack event",
		zap.String("type", string(event.Type)),
		zap.Any("innerEvent", event.InnerEvent.Type))
	for _, dispatcher := range h.slackEventProcessors {
		if !dispatcher.enqueue(event) {
			if h.metrics != nil {
				h.metrics.IncProcessorQueueOverflow(dispatcher.processor.ProcessorType())
			}
			h.log.Warn("Slack event processor queue full; dropping event",
				zap.String("processor", dispatcher.processor.ProcessorType()),
				zap.Int("queue_capacity", SlackEventQueueCapacity),
				zap.String("overflow_policy", "drop_newest"))
		}
	}
	return true
}

func readRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, fmt.Errorf("request body is nil")
	}
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, SlackEventMaxBodyBytes))
}
