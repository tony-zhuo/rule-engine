package kafka

import (
	"fmt"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

type KafkaConfig struct {
	Brokers       string `mapstructure:"brokers"`
	ConsumerGroup string `mapstructure:"consumer_group"`
	Topics        Topics `mapstructure:"topics"`
}

type Topics struct {
	Events  string `mapstructure:"events"`
	Results string `mapstructure:"results"`
}

func NewProducer(cfg KafkaConfig) (*kafka.Producer, error) {
	p, err := kafka.NewProducer(&kafka.ConfigMap{
		"bootstrap.servers": cfg.Brokers,
		"acks":              -1,
	})
	if err != nil {
		return nil, fmt.Errorf("kafka: new producer: %w", err)
	}
	return p, nil
}

func NewConsumer(cfg KafkaConfig) (*kafka.Consumer, error) {
	c, err := kafka.NewConsumer(&kafka.ConfigMap{
		"bootstrap.servers":        cfg.Brokers,
		"group.id":                 cfg.ConsumerGroup,
		"auto.offset.reset":        "earliest",
		"enable.auto.commit":       true,
		"auto.commit.interval.ms":  5000,
		"enable.auto.offset.store": false,
	})
	if err != nil {
		return nil, fmt.Errorf("kafka: new consumer: %w", err)
	}
	return c, nil
}

func Produce(p *kafka.Producer, topic, key string, value []byte) error {
	if p == nil {
		return nil
	}
	deliveryCh := make(chan kafka.Event, 1)
	if err := p.Produce(&kafka.Message{
		TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
		Key:            []byte(key),
		Value:          value,
	}, deliveryCh); err != nil {
		return fmt.Errorf("kafka: produce: %w", err)
	}

	e := <-deliveryCh
	m := e.(*kafka.Message)
	if m.TopicPartition.Error != nil {
		return fmt.Errorf("kafka: delivery failed: %w", m.TopicPartition.Error)
	}
	return nil
}
