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
	workerWire "github.com/tony-zhuo/rule-engine/service/bff/worker/wire"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}

	pkgdb.Init(cfg.DB)
	pkgredis.Init(cfg.Redis)

	handler := workerWire.InitializeHandler(cfg)

	consumer, err := pkgkafka.NewConsumer(cfg.Kafka)
	if err != nil {
		log.Fatal("failed to create kafka consumer: ", err)
	}
	defer consumer.Close()

	if err := consumer.Subscribe(cfg.Kafka.Topics.Events, nil); err != nil {
		log.Fatal("failed to subscribe: ", err)
	}

	slog.Info("worker started", "topic", cfg.Kafka.Topics.Events, "group", cfg.Kafka.ConsumerGroup)

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
