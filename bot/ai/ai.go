package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tmc/langchaingo/httputil"
	"github.com/tmc/langchaingo/llms/openai"
	"go.uber.org/zap"
)

const (
	DefaultModel           = "gpt-3.5-turbo"
	DefaultReasoningEffort = "medium"
)

const reasoningTokenAllowance = 1024

type Config struct {
	OpenAIAPIKey    string
	Model           string
	ReasoningEffort string
}

type AI struct {
	log    *zap.Logger
	config Config
	llm    *openai.LLM
}

func NewAI(log *zap.Logger, c Config) *AI {
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.ReasoningEffort == "" {
		c.ReasoningEffort = DefaultReasoningEffort
	}

	return &AI{
		log:    log,
		config: c,
	}
}

func (a *AI) Start(ctx context.Context) error {
	model, err := openai.New(
		openai.WithToken(a.config.OpenAIAPIKey),
		openai.WithModel(a.config.Model),
		openai.WithHTTPClient(&modelCompatibilityClient{
			client: httputil.DefaultClient,
			model:  a.config.Model,
			effort: a.config.ReasoningEffort,
		}),
	)
	if err != nil {
		return fmt.Errorf("create OpenAI model: %w", err)
	}

	reasoningEffort := a.config.ReasoningEffort
	if !supportsReasoningEffort(a.config.Model) {
		reasoningEffort = "not applicable"
	}
	a.log.Info("Configured OpenAI model",
		zap.String("model", a.config.Model),
		zap.String("reasoning_effort", reasoningEffort),
	)
	a.llm = model
	return nil
}

type modelCompatibilityClient struct {
	client interface {
		Do(*http.Request) (*http.Response, error)
	}
	model  string
	effort string
}

func (c *modelCompatibilityClient) Do(req *http.Request) (*http.Response, error) {
	if req.Body == nil ||
		!strings.HasSuffix(req.URL.Path, "/chat/completions") ||
		!supportsReasoningEffort(c.model) {
		return c.client.Do(req)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read OpenAI request body: %w", err)
	}
	if err := req.Body.Close(); err != nil {
		return nil, fmt.Errorf("close OpenAI request body: %w", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode OpenAI request body: %w", err)
	}
	effort, err := json.Marshal(c.effort)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI reasoning effort: %w", err)
	}
	payload["reasoning_effort"] = effort
	if rawMaxTokens, ok := payload["max_completion_tokens"]; ok {
		var maxTokens int
		if err := json.Unmarshal(rawMaxTokens, &maxTokens); err != nil {
			return nil, fmt.Errorf("decode OpenAI max completion tokens: %w", err)
		}
		payload["max_completion_tokens"], err = json.Marshal(maxTokens + reasoningTokenAllowance)
		if err != nil {
			return nil, fmt.Errorf("encode OpenAI max completion tokens: %w", err)
		}
	}
	delete(payload, "temperature")
	delete(payload, "top_p")
	delete(payload, "frequency_penalty")
	delete(payload, "presence_penalty")
	body, err = json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI request body: %w", err)
	}

	clonedReq := req.Clone(req.Context())
	clonedReq.Body = io.NopCloser(bytes.NewReader(body))
	clonedReq.ContentLength = int64(len(body))
	clonedReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return c.client.Do(clonedReq)
}

func supportsReasoningEffort(model string) bool {
	return strings.HasPrefix(model, "gpt-5") ||
		strings.HasPrefix(model, "o1") ||
		strings.HasPrefix(model, "o3") ||
		strings.HasPrefix(model, "o4")
}

func (a *AI) Stop(ctx context.Context) error {
	return nil
}

func (a *AI) LLM() *openai.LLM {
	return a.llm
}
