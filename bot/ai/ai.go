package ai

import (
	"context"
	"fmt"

	"github.com/tmc/langchaingo/llms/openai"
	"go.uber.org/zap"
)

type Config struct {
	OpenAIAPIKey string
}

type AI struct {
	log    *zap.Logger
	config Config
	llm    *openai.LLM
}

func NewAI(log *zap.Logger, c Config) *AI {
	return &AI{
		log:    log,
		config: c,
	}
}

func (a *AI) Start(ctx context.Context) error {
	model, err := openai.New(openai.WithToken(a.config.OpenAIAPIKey))
	if err != nil {
		return fmt.Errorf("create OpenAI model: %w", err)
	}
	a.llm = model
	return nil
}

func (a *AI) Stop(ctx context.Context) error {
	return nil
}

func (a *AI) LLM() *openai.LLM {
	return a.llm
}
