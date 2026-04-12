package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/tony-zhuo/rule-engine/config"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
	workerUsecase "github.com/tony-zhuo/rule-engine/service/bff/worker/usecase"
)

const (
	partitionChannelBuf = 64
	commitChannelBuf    = 4096
)

type partitionWorker struct {
	msgCh chan *kafka.Message
	done  chan struct{}
}

type EventManager struct {
	ctx      context.Context
	name     string
	cfg      *config.Config
	handler  *workerUsecase.EventUsecase
	producer *kafka.Producer

	consumer   *kafka.Consumer
	partitions map[int32]*partitionWorker
	commitCh   chan *kafka.Message
	wg         sync.WaitGroup
}

func NewEventManager(ctx context.Context, cfg *config.Config, handler *workerUsecase.EventUsecase, producer *kafka.Producer) *EventManager {
	return &EventManager{
		ctx:        ctx,
		name:       "event_manager",
		cfg:        cfg,
		handler:    handler,
		producer:   producer,
		partitions: make(map[int32]*partitionWorker),
		commitCh:   make(chan *kafka.Message, commitChannelBuf),
	}
}

func (m *EventManager) Name() string {
	return m.name
}

func (m *EventManager) Run() error {
	slog.Info("event manager running", "topic", m.cfg.Kafka.Topics.Events, "group", m.cfg.Kafka.ConsumerGroup)

	// Consumer 故意延遲到 Run() 才建立：整條 Poll / rebalance / partition
	// goroutine 的生命週期都綁在這個 goroutine 上，避免跨 goroutine 管理
	// librdkafka 資源；而且 NewConsumer 一呼叫就會開背景執行緒與 broker
	// heartbeat，放在 constructor 會讓「建好但從未 Poll」的 consumer 佔用
	// group coordinator 的 member 名額。
	consumer, err := pkgkafka.NewConsumer(m.cfg.Kafka)
	if err != nil {
		return fmt.Errorf("create consumer: %w", err)
	}
	m.consumer = consumer
	defer func() {
		if err := consumer.Close(); err != nil {
			slog.Error("worker: consumer close failed", "error", err)
		}
		m.consumer = nil
	}()

	if err := consumer.Subscribe(m.cfg.Kafka.Topics.Events, m.rebalanceCallback); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	for {
		select {
		case <-m.ctx.Done():
			m.shutdownPartitions()
			m.drainCommits()
			return nil
		default:
		}

		m.drainCommits()

		ev := consumer.Poll(100)
		if ev == nil {
			continue
		}
		switch e := ev.(type) {
		case *kafka.Message:
			pw, ok := m.partitions[e.TopicPartition.Partition]
			if !ok {
				slog.Warn("worker: message for unassigned partition", "partition", e.TopicPartition.Partition)
				continue
			}
			if !m.dispatch(pw, e) {
				m.shutdownPartitions()
				m.drainCommits()
				return nil
			}
		case kafka.Error:
			slog.Error("kafka consumer error", "error", e)
		}
	}
}

// rebalanceCallback is invoked from the Poll goroutine, so it is safe to
// mutate partitions / drain commitCh without extra synchronization.
func (m *EventManager) rebalanceCallback(c *kafka.Consumer, ev kafka.Event) error {
	switch e := ev.(type) {
	case kafka.AssignedPartitions:
		slog.Info("partitions assigned", "partitions", e.Partitions)
		if err := c.Assign(e.Partitions); err != nil {
			return fmt.Errorf("assign: %w", err)
		}
		for _, tp := range e.Partitions {
			m.startPartitionWorker(tp.Partition)
		}
	case kafka.RevokedPartitions:
		slog.Info("partitions revoked", "partitions", e.Partitions)
		for _, tp := range e.Partitions {
			m.stopPartitionWorker(tp.Partition)
		}
		// flush remaining stored offsets before the next owner picks up
		m.drainCommits()
		if err := c.Unassign(); err != nil {
			return fmt.Errorf("unassign: %w", err)
		}
	}
	return nil
}

func (m *EventManager) startPartitionWorker(partition int32) {
	if _, exists := m.partitions[partition]; exists {
		return
	}
	pw := &partitionWorker{
		msgCh: make(chan *kafka.Message, partitionChannelBuf),
		done:  make(chan struct{}),
	}
	m.partitions[partition] = pw
	m.wg.Add(1)
	go m.runPartitionWorker(partition, pw)
}

func (m *EventManager) runPartitionWorker(partition int32, pw *partitionWorker) {
	defer m.wg.Done()
	defer close(pw.done)
	for msg := range pw.msgCh {
		if err := m.handler.Execute(msg); err != nil {
			slog.Error("worker: execute failed", "error", err, "partition", partition, "offset", msg.TopicPartition.Offset)
			continue
		}
		select {
		case m.commitCh <- msg:
		case <-m.ctx.Done():
			return
		}
	}
}

// stopPartitionWorker closes the worker's msg channel and waits for it to
// drain. While waiting it keeps draining commitCh so a full commit buffer
// cannot deadlock the worker during rebalance.
func (m *EventManager) stopPartitionWorker(partition int32) {
	pw, ok := m.partitions[partition]
	if !ok {
		return
	}
	close(pw.msgCh)
	for {
		select {
		case <-pw.done:
			delete(m.partitions, partition)
			return
		case msg := <-m.commitCh:
			m.storeOffset(msg)
		}
	}
}

func (m *EventManager) shutdownPartitions() {
	for p, pw := range m.partitions {
		close(pw.msgCh)
		for {
			done := false
			select {
			case <-pw.done:
				done = true
			case msg := <-m.commitCh:
				m.storeOffset(msg)
			}
			if done {
				break
			}
		}
		delete(m.partitions, p)
	}
	m.wg.Wait()
}

func (m *EventManager) dispatch(pw *partitionWorker, msg *kafka.Message) bool {
	for {
		select {
		case pw.msgCh <- msg:
			return true
		case cmsg := <-m.commitCh:
			m.storeOffset(cmsg)
		case <-m.ctx.Done():
			return false
		}
	}
}

func (m *EventManager) drainCommits() {
	for {
		select {
		case msg := <-m.commitCh:
			m.storeOffset(msg)
		default:
			return
		}
	}
}

func (m *EventManager) storeOffset(msg *kafka.Message) {
	if m.consumer == nil {
		return
	}
	if _, err := m.consumer.StoreMessage(msg); err != nil {
		slog.Error("worker: store offset failed", "error", err, "offset", msg.TopicPartition.Offset)
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
