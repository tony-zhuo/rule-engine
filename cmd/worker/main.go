package main

import (
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/tony-zhuo/rule-engine/config"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
	pkgredis "github.com/tony-zhuo/rule-engine/pkg/redis"
	behaviorDB "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/db"
	behaviorUsecase "github.com/tony-zhuo/rule-engine/service/base/behavior/usecase"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
	"github.com/tony-zhuo/rule-engine/service/bff/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}

	// Init infra.
	pkgdb.Init(cfg.DB)
	pkgredis.Init(cfg.Redis)
	db := pkgdb.GetDB()
	rdb := pkgredis.GetClient()

	// Build dependencies.
	behaviorRepo := behaviorDB.NewBehaviorRepo(db)
	behaviorUC := behaviorUsecase.NewBehaviorUsecase(behaviorRepo)
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC, rdb)

	producer, err := pkgkafka.NewProducer(cfg.Kafka)
	if err != nil {
		log.Fatal("failed to create kafka producer: ", err)
	}
	defer producer.Close()

	handler := worker.NewHandler(behaviorUC, ruleStrategyUC, producer, cfg.Kafka.Topics.Results)

	// Create consumer.
	consumer, err := pkgkafka.NewConsumer(cfg.Kafka)
	if err != nil {
		log.Fatal("failed to create kafka consumer: ", err)
	}
	defer consumer.Close()

	if err := consumer.Subscribe(cfg.Kafka.Topics.Events, nil); err != nil {
		log.Fatal("failed to subscribe: ", err)
	}

	slog.Info("worker started", "topic", cfg.Kafka.Topics.Events, "group", cfg.Kafka.ConsumerGroup)

	// Graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	run := true
	for run {
		select {
		case <-sigCh:
			slog.Info("worker shutting down")
			run = false
		default:
			ev := consumer.Poll(100)
			if ev == nil {
				continue
			}
			switch msg := ev.(type) {
			case *kafka.Message:
				handler.HandleMessage(msg)
			case kafka.Error:
				slog.Error("kafka consumer error", "error", msg)
			}
		}
	}
}
