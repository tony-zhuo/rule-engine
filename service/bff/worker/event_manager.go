package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/tony-zhuo/rule-engine/config"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
	workerUsecase "github.com/tony-zhuo/rule-engine/service/bff/worker/usecase"
)

type EventManager struct {
	ctx      context.Context
	name     string
	cfg      *config.Config
	handler  *workerUsecase.EventUsecase
	producer *kafka.Producer
}

func NewEventManager(ctx context.Context, cfg *config.Config, handler *workerUsecase.EventUsecase, producer *kafka.Producer) *EventManager {
	return &EventManager{
		ctx:      ctx,
		name:     "event_manager",
		cfg:      cfg,
		handler:  handler,
		producer: producer,
	}
}

func (m *EventManager) Name() string {
	return m.name
}

func (m *EventManager) Run() error {
	slog.Info("event manager running", "topic", m.cfg.Kafka.Topics.Events, "group", m.cfg.Kafka.ConsumerGroup)

	consumer, err := pkgkafka.NewConsumer(m.cfg.Kafka)
	if err != nil {
		return fmt.Errorf("create consumer: %w", err)
	}
	defer consumer.Close()

	if err := consumer.Subscribe(m.cfg.Kafka.Topics.Events, nil); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

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
				if err := m.handler.Execute(msg); err != nil {
					slog.Error("worker: execute failed", "error", err, "offset", msg.TopicPartition.Offset)
					continue
				}
				if _, err := consumer.CommitMessage(msg); err != nil {
					slog.Error("worker: commit failed", "error", err, "offset", msg.TopicPartition.Offset)
				}
			case kafka.Error:
				slog.Error("kafka consumer error", "error", msg)
			}
		}
	}
}

func (m *EventManager) Shutdown() error {
	remaining := m.producer.Flush(10000)
	if remaining > 0 {
		slog.Warn("producer flush incomplete", "remaining", remaining)
	}
	m.producer.Close()
	return nil
}

func (m *EventManager) Health() bool {
	return true
}
