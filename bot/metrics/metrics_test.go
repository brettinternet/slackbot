package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsRepresentativeValues(t *testing.T) {
	m := New()
	m.IncDeduplicatedEvent()
	m.SetProcessorQueueDepth("chat", 3)
	m.IncProcessorQueueOverflow("chat")
	m.SetProcessorQueueDepth("U123", 2)
	m.IncProcessorQueueOverflow("message-derived-value")
	m.ObserveExternalRequest("slack", time.Now(), true)
	m.AddOpenAITokens(12, 4)
	m.IncVibecheckOutcome(true)

	response := httptest.NewRecorder()
	m.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, DefaultPath, nil))
	body := response.Body.String()
	if strings.Contains(body, "U123") || strings.Contains(body, "message-derived-value") {
		t.Fatalf("metrics body exposed unbounded processor label:\n%s", body)
	}
	for _, want := range []string{
		"slackbot_slack_events_deduplicated_total 1",
		`slackbot_processor_queue_depth{processor="chat"} 3`,
		`slackbot_processor_queue_overflow_total{processor="chat"} 1`,
		`slackbot_processor_queue_depth{processor="unknown"} 2`,
		`slackbot_processor_queue_overflow_total{processor="unknown"} 1`,
		`slackbot_external_request_errors_total{service="slack"} 1`,
		`slackbot_openai_tokens_total{type="input"} 12`,
		`slackbot_openai_tokens_total{type="output"} 4`,
		`slackbot_vibecheck_outcomes_total{outcome="passed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
}
