package db

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"

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

// NewProcessedEventRepoWith creates a non-singleton instance (for testing with alternative DB connections).
func NewProcessedEventRepoWith(db *gorm.DB) *ProcessedEventRepo {
	return &ProcessedEventRepo{db: db}
}

func (r *ProcessedEventRepo) Upsert(ctx context.Context, eventID string) (*model.ProcessedEvent, error) {
	now := time.Now()

	var record model.ProcessedEvent
	err := r.db.WithContext(ctx).Raw(`
		INSERT INTO processed_events (event_id, attempts, status, created_at, updated_at)
		VALUES (?, 1, ?, ?, ?)
		ON CONFLICT (event_id) DO UPDATE SET
			attempts = processed_events.attempts + 1,
			updated_at = ?
		RETURNING event_id, attempts, status, created_at, updated_at
	`, eventID, model.ProcessedEventStatusPending, now, now, now).Scan(&record).Error
	if err != nil {
		return nil, fmt.Errorf("processed event upsert: %w", err)
	}
	return &record, nil
}

func (r *ProcessedEventRepo) UpsertWithBehaviorLog(ctx context.Context, eventID string, log *model.BehaviorLog) (*model.ProcessedEvent, error) {
	now := time.Now()

	var record model.ProcessedEvent
	err := r.db.WithContext(ctx).Raw(`
		WITH pe AS (
			INSERT INTO processed_events (event_id, attempts, status, created_at, updated_at)
			VALUES (?, 1, ?, ?, ?)
			ON CONFLICT (event_id) DO UPDATE SET
				attempts = CASE
					WHEN processed_events.status = 'pending' THEN processed_events.attempts + 1
					ELSE processed_events.attempts
				END,
				updated_at = CASE
					WHEN processed_events.status = 'pending' THEN ?
					ELSE processed_events.updated_at
				END
			RETURNING event_id, attempts, status, created_at, updated_at
		),
		bl AS (
			INSERT INTO behavior_logs (event_id, member_id, behavior, fields, occurred_at, created_at)
			SELECT ?, ?, ?, ?, ?, ?
			WHERE EXISTS (SELECT 1 FROM pe WHERE status = 'pending')
			ON CONFLICT (event_id) DO NOTHING
			RETURNING id
		)
		SELECT event_id, attempts, status, created_at, updated_at FROM pe
	`, eventID, model.ProcessedEventStatusPending, now, now, now,
		log.EventID, log.MemberID, log.Behavior, log.Fields, log.OccurredAt, now,
	).Scan(&record).Error
	if err != nil {
		return nil, fmt.Errorf("processed event upsert with behavior log: %w", err)
	}
	return &record, nil
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
