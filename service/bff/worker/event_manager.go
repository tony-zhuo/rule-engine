package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/gammazero/workerpool"
	"github.com/tony-zhuo/rule-engine/config"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
	workerUsecase "github.com/tony-zhuo/rule-engine/service/bff/worker/usecase"
)

type EventManager struct {
	ctx      context.Context
	name     string
	cfg      *config.Config
	handler  *workerUsecase.EventUsecase
	poolSize int
}

func NewEventManager(ctx context.Context, cfg *config.Config, handler *workerUsecase.EventUsecase, poolSize int) *EventManager {
	return &EventManager{
		ctx:      ctx,
		name:     "event_manager",
		cfg:      cfg,
		handler:  handler,
		poolSize: poolSize,
	}
}

func (m *EventManager) Name() string {
	return m.name
}

func (m *EventManager) Run() error {
	slog.Info("event manager running", "pool_size", m.poolSize, "topic", m.cfg.Kafka.Topics.Events, "group", m.cfg.Kafka.ConsumerGroup)

	consumer, err := pkgkafka.NewConsumer(m.cfg.Kafka)
	if err != nil {
		return fmt.Errorf("create consumer: %w", err)
	}
	defer consumer.Close()

	if err := consumer.Subscribe(m.cfg.Kafka.Topics.Events, nil); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	wp := workerpool.New(m.poolSize)
	defer wp.StopWait()

	for {
		select {
		case <-m.ctx.Done():
			return nil
		default:
			ev := consumer.Poll(100)
			if ev == nil {
				continue
			}
			switch msg := ev.(type) {
			case *kafka.Message:
				captured := msg
				wp.Submit(func() {
					m.handler.Execute(captured)
				})
			case kafka.Error:
				slog.Error("kafka consumer error", "error", msg)
			}
		}
	}
}

func (m *EventManager) Shutdown() error {
	return nil
}

func (m *EventManager) Health() bool {
	return true
}
