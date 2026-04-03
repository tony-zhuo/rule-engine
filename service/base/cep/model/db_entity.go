package model

import "time"

type CEPPatternStatus int

const (
	CEPPatternStatusActive   CEPPatternStatus = 1
	CEPPatternStatusInactive CEPPatternStatus = 2
)

type CEPPatternRecord struct {
	ID          string           `gorm:"primaryKey;size:64"`
	Name        string           `gorm:"size:255;not null"`
	Description string           `gorm:"type:text"`
	StatesJSON  string           `gorm:"column:states;type:jsonb;not null"`
	Status      CEPPatternStatus `gorm:"not null;default:1"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (CEPPatternRecord) TableName() string { return "cep_patterns" }
