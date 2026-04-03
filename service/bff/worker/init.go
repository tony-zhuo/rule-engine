package worker

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"

	behaviorDB "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/db"
	behaviorUsecase "github.com/tony-zhuo/rule-engine/service/base/behavior/usecase"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"

	"github.com/tony-zhuo/rule-engine/config"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
	pkgredis "github.com/tony-zhuo/rule-engine/pkg/redis"
	workerUsecase "github.com/tony-zhuo/rule-engine/service/bff/worker/usecase"
)

// WorkerManager is the interface that all worker managers must implement.
type WorkerManager interface {
	Name() string
	Run() error
	Shutdown() error
	Health() bool
}

var (
	workerMgr = make(map[string]WorkerManager)
	wg        sync.WaitGroup
)

// Register adds a WorkerManager to the registry. Panics on duplicate names.
func Register(m WorkerManager) {
	if m == nil {
		return
	}
	if _, exists := workerMgr[m.Name()]; exists {
		panic(fmt.Errorf("worker [%s] already registered", m.Name()))
	}
	workerMgr[m.Name()] = m
}

// Run is the single entry point for the worker binary.
func Run() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	workerInit(ctx)
	enableWorker()
}

func workerInit(ctx context.Context) {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}

	pkgdb.Init(cfg.DB)
	pkgredis.Init(cfg.Redis)

	producer, err := pkgkafka.NewProducer(cfg.Kafka)
	if err != nil {
		log.Fatal("failed to create kafka producer: ", err)
	}

	db := pkgdb.GetDB()
	rdb := pkgredis.GetClient()

	behaviorRepo := behaviorDB.NewBehaviorRepo(db)
	behaviorUC := behaviorUsecase.NewBehaviorUsecase(behaviorRepo)
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC, rdb)

	handler := workerUsecase.NewEventUsecase(behaviorUC, ruleStrategyUC, producer, cfg.Kafka.Topics.Results)

	Register(NewEventManager(ctx, cfg, handler, producer))
}

func enableWorker() {
	for _, m := range workerMgr {
		wg.Add(1)
		go func(m WorkerManager) {
			defer wg.Done()
			slog.Info("worker started", "name", m.Name())
			if err := m.Run(); err != nil {
				slog.Error("worker error", "name", m.Name(), "error", err)
			}
			if err := m.Shutdown(); err != nil {
				slog.Error("worker shutdown error", "name", m.Name(), "error", err)
			}
			slog.Info("worker stopped", "name", m.Name())
		}(m)
	}
	wg.Wait()
}
