package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"gorm.io/gorm"

	"github.com/tony-zhuo/rule-engine/service/base/cep/model"
)

var (
	_cepPatternRepoOnce sync.Once
	_cepPatternRepoObj  *CEPPatternRepo
)

var _ model.CEPPatternRepoInterface = (*CEPPatternRepo)(nil)

type CEPPatternRepo struct {
	db *gorm.DB
}

func NewCEPPatternRepo(db *gorm.DB) *CEPPatternRepo {
	_cepPatternRepoOnce.Do(func() {
		_cepPatternRepoObj = &CEPPatternRepo{db: db}
	})
	return _cepPatternRepoObj
}

// NewCEPPatternRepoWith creates a non-singleton instance (for testing with alternative DB connections).
func NewCEPPatternRepoWith(db *gorm.DB) *CEPPatternRepo {
	return &CEPPatternRepo{db: db}
}

func (r *CEPPatternRepo) ListActive(ctx context.Context) ([]model.CEPPattern, error) {
	var records []model.CEPPatternRecord
	if err := r.db.WithContext(ctx).
		Where("status = ?", model.CEPPatternStatusActive).
		Find(&records).Error; err != nil {
		return nil, fmt.Errorf("cep pattern repo list active: %w", err)
	}

	patterns := make([]model.CEPPattern, 0, len(records))
	for _, rec := range records {
		var states []model.PatternState
		if err := json.Unmarshal([]byte(rec.StatesJSON), &states); err != nil {
			return nil, fmt.Errorf("cep pattern repo unmarshal states for %s: %w", rec.ID, err)
		}
		patterns = append(patterns, model.CEPPattern{
			ID:     rec.ID,
			Name:   rec.Name,
			States: states,
		})
	}
	return patterns, nil
}
