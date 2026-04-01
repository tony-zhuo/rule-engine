package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
)

type EngineUsecaseInterface interface {
	// Rule management
	CreateRule(ctx context.Context, req *ruleModel.CreateRuleStrategyReq) (*ruleModel.RuleStrategy, error)
	GetRule(ctx context.Context, id uint64) (*ruleModel.RuleStrategy, error)
	ListRules(ctx context.Context, status *ruleModel.RuleStrategyStatus) ([]*ruleModel.RuleStrategy, error)
	UpdateRule(ctx context.Context, id uint64, req *ruleModel.UpdateRuleStrategyReq) error
	SetRuleStatus(ctx context.Context, id uint64, status ruleModel.RuleStrategyStatus) error
	// Event processing
	ProcessEvent(ctx context.Context, req *ProcessEventReq) error
}

type ProcessEventReq struct {
	MemberID   string                     `json:"member_id"   binding:"required"`
	PlatformID string                     `json:"platform_id"`
	Behavior   behaviorModel.BehaviorType `json:"behavior"    binding:"required"`
	Fields     map[string]any             `json:"fields"`
	OccurredAt time.Time                  `json:"occurred_at"`
}

var (
	_engineUCOnce sync.Once
	_engineUCObj  *EngineUsecase
)

var _ EngineUsecaseInterface = (*EngineUsecase)(nil)

type EngineUsecase struct {
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface
	producer       *kafka.Producer
	eventsTopic    string
}

func NewEngineUsecase(
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface,
	producer *kafka.Producer,
	eventsTopic string,
) *EngineUsecase {
	_engineUCOnce.Do(func() {
		_engineUCObj = &EngineUsecase{
			ruleStrategyUC: ruleStrategyUC,
			producer:       producer,
			eventsTopic:    eventsTopic,
		}
	})
	return _engineUCObj
}

func (uc *EngineUsecase) CreateRule(ctx context.Context, req *ruleModel.CreateRuleStrategyReq) (*ruleModel.RuleStrategy, error) {
	return uc.ruleStrategyUC.Create(ctx, req)
}

func (uc *EngineUsecase) GetRule(ctx context.Context, id uint64) (*ruleModel.RuleStrategy, error) {
	return uc.ruleStrategyUC.Get(ctx, id)
}

func (uc *EngineUsecase) ListRules(ctx context.Context, status *ruleModel.RuleStrategyStatus) ([]*ruleModel.RuleStrategy, error) {
	return uc.ruleStrategyUC.List(ctx, status)
}

func (uc *EngineUsecase) UpdateRule(ctx context.Context, id uint64, req *ruleModel.UpdateRuleStrategyReq) error {
	return uc.ruleStrategyUC.Update(ctx, id, req)
}

func (uc *EngineUsecase) SetRuleStatus(ctx context.Context, id uint64, status ruleModel.RuleStrategyStatus) error {
	return uc.ruleStrategyUC.SetStatus(ctx, id, status)
}

// ProcessEvent produces the event to Kafka for async processing by the worker.
func (uc *EngineUsecase) ProcessEvent(ctx context.Context, req *ProcessEventReq) error {
	if req.OccurredAt.IsZero() {
		req.OccurredAt = time.Now()
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("process event: marshal: %w", err)
	}

	if err := pkgkafka.Produce(uc.producer, uc.eventsTopic, req.MemberID, data); err != nil {
		return fmt.Errorf("process event: produce: %w", err)
	}
	return nil
}
