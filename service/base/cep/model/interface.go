package model

import "context"

type ProgressStore interface {
	Get(ctx context.Context, id string) (*PatternProgress, error)
	Save(ctx context.Context, p *PatternProgress) error
	Delete(ctx context.Context, id string) error
	ListByMember(ctx context.Context, memberID string) ([]*PatternProgress, error)
}

type ProcessorInterface interface {
	AddPattern(pattern CEPPattern)
	ProcessEvent(ctx context.Context, event *Event) ([]*MatchResult, error)
}
