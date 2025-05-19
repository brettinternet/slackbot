package obituary

import (
	"context"

	"go.uber.org/zap"
)

type Obituary struct {
	log *zap.Logger
}

func NewObituary(log *zap.Logger) *Obituary {
	return &Obituary{
		log: log,
	}
}

func (o *Obituary) Start(ctx context.Context) error {
	return nil
}

func (o *Obituary) Stop(ctx context.Context) error {
	return nil
}
