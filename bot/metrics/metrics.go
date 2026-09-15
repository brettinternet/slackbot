package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const DefaultPath = "/metrics"

type Config struct {
	Enabled bool
	Path    string
}

type FileConfig struct {
	Enabled *bool   `json:"enabled" yaml:"enabled"`
	Path    *string `json:"path" yaml:"path"`
}

// Metrics owns a private Prometheus registry so tests and multiple Bot instances
// cannot collide through the process-global registry.
type Metrics struct {
	registry *prometheus.Registry

	httpRequests      *prometheus.CounterVec
	deduplicated      prometheus.Counter
	queueDepth        *prometheus.GaugeVec
	queueOverflow     *prometheus.CounterVec
	externalDuration  *prometheus.HistogramVec
	externalErrors    *prometheus.CounterVec
	openAITokens      *prometheus.CounterVec
	vibecheckOutcomes *prometheus.CounterVec
}

func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slackbot_http_requests_total",
			Help: "HTTP requests handled by route and status code.",
		}, []string{"route", "status"}),
		deduplicated: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "slackbot_slack_events_deduplicated_total",
			Help: "Slack retry deliveries suppressed by event ID.",
		}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "slackbot_processor_queue_depth",
			Help: "Events waiting or in flight for each processor.",
		}, []string{"processor"}),
		queueOverflow: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slackbot_processor_queue_overflow_total",
			Help: "Events dropped because a processor queue was full.",
		}, []string{"processor"}),
		externalDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "slackbot_external_request_duration_seconds",
			Help:    "Slack and OpenAI HTTP request latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"service", "outcome"}),
		externalErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slackbot_external_request_errors_total",
			Help: "Failed Slack and OpenAI HTTP requests.",
		}, []string{"service"}),
		openAITokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slackbot_openai_tokens_total",
			Help: "OpenAI tokens reported by the API.",
		}, []string{"type"}),
		vibecheckOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slackbot_vibecheck_outcomes_total",
			Help: "Vibecheck pass and fail outcomes.",
		}, []string{"outcome"}),
	}
	m.registry.MustRegister(
		m.httpRequests,
		m.deduplicated,
		m.queueDepth,
		m.queueOverflow,
		m.externalDuration,
		m.externalErrors,
		m.openAITokens,
		m.vibecheckOutcomes,
	)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) ObserveHTTPRequest(route string, status int) {
	m.httpRequests.WithLabelValues(route, strconv.Itoa(status)).Inc()
}

func (m *Metrics) IncDeduplicatedEvent() { m.deduplicated.Inc() }

func (m *Metrics) SetProcessorQueueDepth(processor string, depth int64) {
	m.queueDepth.WithLabelValues(processorLabel(processor)).Set(float64(depth))
}

func (m *Metrics) IncProcessorQueueOverflow(processor string) {
	m.queueOverflow.WithLabelValues(processorLabel(processor)).Inc()
}

func processorLabel(processor string) string {
	switch processor {
	case "chat", "vibecheck", "user-watch", "aichat":
		return processor
	default:
		return "unknown"
	}
}

func (m *Metrics) ObserveExternalRequest(service string, started time.Time, failed bool) {
	outcome := "success"
	if failed {
		outcome = "error"
		m.externalErrors.WithLabelValues(service).Inc()
	}
	m.externalDuration.WithLabelValues(service, outcome).Observe(time.Since(started).Seconds())
}

func (m *Metrics) AddOpenAITokens(input, output int) {
	if input > 0 {
		m.openAITokens.WithLabelValues("input").Add(float64(input))
	}
	if output > 0 {
		m.openAITokens.WithLabelValues("output").Add(float64(output))
	}
}

func (m *Metrics) IncVibecheckOutcome(passed bool) {
	outcome := "failed"
	if passed {
		outcome = "passed"
	}
	m.vibecheckOutcomes.WithLabelValues(outcome).Inc()
}
