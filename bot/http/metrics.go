package http

import (
	"net/http"
	"strconv"

	botmetrics "slackbot.arpa/bot/metrics"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (h *Server) registerMetricsEndpoint() {
	if h.metrics == nil || !h.config.Metrics.Enabled {
		return
	}
	path := h.config.Metrics.Path
	if path == "" {
		path = botmetrics.DefaultPath
	}
	h.serveMux.Handle(path, h.metrics.Handler())
}

func (h *Server) handler() http.Handler {
	if h.metrics == nil || !h.config.Metrics.Enabled {
		return h.serveMux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		h.serveMux.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		h.metrics.ObserveHTTPRequest(h.metricRoute(r.URL.Path), status)
	})
}

func (h *Server) metricRoute(path string) string {
	slackPath := h.config.SlackEventPath
	if slackPath == "" {
		slackPath = "/api/slack/events"
	}
	metricsPath := h.config.Metrics.Path
	if metricsPath == "" {
		metricsPath = botmetrics.DefaultPath
	}
	switch path {
	case slackPath:
		return "slack_events"
	case "/health", "/healthz":
		return "health"
	case "/ready":
		return "ready"
	case metricsPath:
		return "metrics"
	default:
		// Never expose arbitrary request paths as labels.
		return "other_" + strconv.Itoa(http.StatusNotFound)
	}
}
