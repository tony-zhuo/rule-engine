package db

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

var (
	_processedEventRepoOnce sync.Once
	_processedEventRepoObj  *ProcessedEventRepo
)

var _ model.ProcessedEventRepoInterface = (*ProcessedEventRepo)(nil)

type ProcessedEventRepo struct {
	db *gorm.DB
}

func NewProcessedEventRepo(db *gorm.DB) *ProcessedEventRepo {
	_processedEventRepoOnce.Do(func() {
		_processedEventRepoObj = &ProcessedEventRepo{db: db}
	})
	return _processedEventRepoObj
}

func (r *ProcessedEventRepo) Upsert(ctx context.Context, eventID string) (*model.ProcessedEvent, error) {
	now := time.Now()
	record := &model.ProcessedEvent{
		EventID:   eventID,
		Attempts:  1,
		Status:    model.ProcessedEventStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}

	result := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "event_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"attempts":   gorm.Expr("processed_events.attempts + 1"),
				"updated_at": now,
			}),
		}).
		Create(record)
	if result.Error != nil {
		return nil, fmt.Errorf("processed event upsert: %w", result.Error)
	}

	// Read back current state (UPSERT returning clause not portable in GORM).
	var current model.ProcessedEvent
	if err := r.db.WithContext(ctx).Where("event_id = ?", eventID).First(&current).Error; err != nil {
		return nil, fmt.Errorf("processed event upsert read back: %w", err)
	}
	return &current, nil
}

func (r *ProcessedEventRepo) MarkCompleted(ctx context.Context, eventID string) error {
	result := r.db.WithContext(ctx).
		Model(&model.ProcessedEvent{}).
		Where("event_id = ?", eventID).
		Updates(map[string]any{
			"status":     model.ProcessedEventStatusCompleted,
			"updated_at": time.Now(),
		})
	if result.Error != nil {
		return fmt.Errorf("processed event mark completed: %w", result.Error)
	}
	return nil
}

func (r *ProcessedEventRepo) MarkFailed(ctx context.Context, eventID string) error {
	result := r.db.WithContext(ctx).
		Model(&model.ProcessedEvent{}).
		Where("event_id = ?", eventID).
		Updates(map[string]any{
			"status":     model.ProcessedEventStatusFailed,
			"updated_at": time.Now(),
		})
	if result.Error != nil {
		return fmt.Errorf("processed event mark failed: %w", result.Error)
	}
	return nil
}
