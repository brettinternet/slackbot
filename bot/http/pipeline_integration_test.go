package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap/zaptest"
	botslack "slackbot.arpa/bot/slack"
)

const integrationSigningSecret = "integration-signing-secret"

type pipelineProcessor struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (p *pipelineProcessor) PushEvent(slackevents.EventsAPIEvent) error {
	p.calls.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	if p.release != nil {
		<-p.release
	}
	return nil
}

func (p *pipelineProcessor) ProcessorType() string { return "pipeline-integration" }

func newPipelineServer(t *testing.T, processor slackEventProcessor) (*Server, string) {
	t.Helper()
	verifier := botslack.NewSlack(zaptest.NewLogger(t), botslack.Config{
		SigningSecret: integrationSigningSecret,
	})
	server := NewServer(zaptest.NewLogger(t), Config{
		SlackEventPath:                "/slack/events",
		SlackEventDeduplicationWindow: time.Minute,
	}, verifier)
	if processor != nil {
		server.RegisterEventProcessor(processor)
	}
	httpServer := httptest.NewServer(server.handler())
	t.Cleanup(httpServer.Close)
	return server, httpServer.URL
}

func postSignedSlackFixture(t *testing.T, baseURL, body string) (int, string) {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(integrationSigningSecret))
	_, _ = mac.Write([]byte("v0:" + timestamp + ":" + body))

	request, err := stdhttp.NewRequest(
		stdhttp.MethodPost,
		baseURL+"/slack/events",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("create signed Slack request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Slack-Request-Timestamp", timestamp)
	request.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))

	response, err := stdhttp.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("submit signed Slack request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read Slack response: %v", err)
	}
	return response.StatusCode, string(responseBody)
}

func eventCallbackFixture(eventID string) string {
	return fmt.Sprintf(
		`{"type":"event_callback","team_id":"T123","api_app_id":"A123",`+
			`"event_id":%q,"event_time":1234567890,`+
			`"event":{"type":"message","channel":"C123","user":"U123",`+
			`"text":"hello","ts":"123.456"}}`,
		eventID,
	)
}

func TestSlackEventPipelineSignedFixtures(t *testing.T) {
	t.Run("URL verification", func(t *testing.T) {
		server, baseURL := newPipelineServer(t, nil)
		status, body := postSignedSlackFixture(
			t,
			baseURL,
			`{"type":"url_verification","challenge":"signed-challenge"}`,
		)
		if status != stdhttp.StatusOK || body != `{"challenge":"signed-challenge"}` {
			t.Fatalf("URL verification response = (%d, %q), want (200, challenge)", status, body)
		}
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown URL verification server: %v", err)
		}
	})

	t.Run("normal dispatch and duplicate retry", func(t *testing.T) {
		processor := &pipelineProcessor{started: make(chan struct{}, 1)}
		server, baseURL := newPipelineServer(t, processor)
		fixture := eventCallbackFixture("Ev-duplicate")

		for attempt := 0; attempt < 2; attempt++ {
			status, _ := postSignedSlackFixture(t, baseURL, fixture)
			if status != stdhttp.StatusOK {
				t.Fatalf("attempt %d status = %d, want 200", attempt+1, status)
			}
		}
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown duplicate server: %v", err)
		}
		if got := processor.calls.Load(); got != 1 {
			t.Fatalf("processor calls = %d, want 1", got)
		}
	})

	t.Run("saturated processor", func(t *testing.T) {
		processor := &pipelineProcessor{
			started: make(chan struct{}, 1),
			release: make(chan struct{}),
		}
		server, baseURL := newPipelineServer(t, processor)

		status, _ := postSignedSlackFixture(t, baseURL, eventCallbackFixture("Ev-blocking"))
		if status != stdhttp.StatusOK {
			t.Fatalf("blocking event status = %d, want 200", status)
		}
		select {
		case <-processor.started:
		case <-time.After(time.Second):
			t.Fatal("processor did not start blocking event")
		}
		for index := 0; index < SlackEventQueueCapacity; index++ {
			status, _ = postSignedSlackFixture(
				t,
				baseURL,
				eventCallbackFixture(fmt.Sprintf("Ev-queued-%d", index)),
			)
			if status != stdhttp.StatusOK {
				t.Fatalf("queued event %d status = %d, want 200", index, status)
			}
		}
		status, _ = postSignedSlackFixture(t, baseURL, eventCallbackFixture("Ev-overflow"))
		if status != stdhttp.StatusOK {
			t.Fatalf("overflow event status = %d, want 200", status)
		}

		close(processor.release)
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown saturated server: %v", err)
		}
		if got, want := processor.calls.Load(), int64(SlackEventQueueCapacity+1); got != want {
			t.Fatalf("processor calls = %d, want %d; overflow was not dropped", got, want)
		}
	})

	t.Run("shutdown drains accepted event", func(t *testing.T) {
		processor := &pipelineProcessor{
			started: make(chan struct{}, 1),
			release: make(chan struct{}),
		}
		server, baseURL := newPipelineServer(t, processor)
		status, _ := postSignedSlackFixture(t, baseURL, eventCallbackFixture("Ev-shutdown"))
		if status != stdhttp.StatusOK {
			t.Fatalf("shutdown fixture status = %d, want 200", status)
		}
		select {
		case <-processor.started:
		case <-time.After(time.Second):
			t.Fatal("processor did not start shutdown fixture")
		}

		shutdown := make(chan error, 1)
		go func() { shutdown <- server.Shutdown(context.Background()) }()
		deadline := time.Now().Add(time.Second)
		for {
			server.dispatchMu.RLock()
			accepting := server.dispatchAccepting
			server.dispatchMu.RUnlock()
			if !accepting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("shutdown did not stop event acceptance")
			}
		}
		select {
		case err := <-shutdown:
			t.Fatalf("shutdown returned before accepted event drained: %v", err)
		default:
		}

		close(processor.release)
		select {
		case err := <-shutdown:
			if err != nil {
				t.Fatalf("shutdown after drain: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown did not finish after accepted event drained")
		}
	})
}
