# Throughput Optimization Plan

## Context

### 現況與問題

系統目前的 event 處理是 **單線程 sequential**：Kafka consumer Poll 一條 → Execute 處理完 → CommitMessage → 再 Poll 下一條。

透過 `BenchmarkThroughput_Integration` 壓測發現：
- **1 worker**: 587 events/s, ~1.7ms/event
- **4+ workers（goroutine 並行呼叫 Execute）**: DB connection pool 爆掉，SQL 延遲飆到 200-400ms 後 crash

### 瓶頸分析

單筆 event 的 1.7ms 拆解：
1. INSERT behavior_log — ~0.3ms
2. Load active rules（Redis cache）— ~0.1ms
3. **5 個 aggregation query sequential 執行** — ~0.5-0.8ms（主要瓶頸）
4. AST rule evaluation — ~0.009ms（幾乎不花時間）
5. CEP pattern matching — ~0.1ms
6. **Kafka produce 同步等 ACK** — ~0.3-0.5ms

4+ workers crash 的原因：
- `pkg/db/db.go` 完全沒有設定 connection pool（無 MaxOpenConns、MaxIdleConns）
- Go `database/sql` 預設 MaxIdleConns=2，高併發時每個 goroutine 開新連線，瞬間打爆 PostgreSQL
- 多個連線同時做 aggregation，搶 PostgreSQL shared_buffers（預設 128MB），互相踢 cache

### 設計決策紀錄

**為什麼不開 worker pool（goroutine pool）？**
Kafka 是 offset-based commit。CommitMessage(offset N) 代表 1~N 全部完成。如果 4 個 goroutine 同時處理 msg 1-4，msg 4 先完成先 commit，但 msg 3 還在跑。此時 crash 重啟後從 offset 5 開始消費，msg 3 就丟了。雖然 behavior_logs 有 event_id unique constraint 保證冪等（重複處理不會重複寫入），但 offset 跳過的語意不乾淨。

**為什麼選 per-partition goroutine 而不是多 consumer instance？**
多 consumer instance（多 process，每個 sequential）可以解決問題，但沒有用到 Go 的 goroutine 優勢 — 跟開多個 Python process 沒差別。Per-partition goroutine 讓一個 process 內有 N 個 goroutine 並行處理 N 個 partition，充分利用 Go 的輕量級併發，同時每個 goroutine 內部 sequential 處理保證 offset 安全。

**為什麼用 Batch SQL 而不是 errgroup 並行 query？**
原本考慮用 errgroup 讓 5 個 aggregation query 並行發出。但並行只是讓等待時間重疊，DB 實際還是要處理 5 個 query、掃 5 次 behavior_logs、搶 shared_buffers。Batch SQL 用 PostgreSQL `FILTER` 語法把 5 個 query 合成 1 個，DB 只掃一次，從根本上減少 I/O 和 cache 壓力。

**為什麼繼續用 Kafka 而不是換 NATS JetStream / Redis Streams / RabbitMQ？**
評估過四種 MQ：
- Redis Streams：per-message XACK 但 failover 會丟訊息（async replication），風控不可接受
- RabbitMQ：沒有 message replay，無法重跑歷史事件 debug
- NATS JetStream：per-message ACK 最乾淨，但沒有原生 partition-key routing，多 process 時同一 member 的 event 可能被分到不同 process，破壞 CEP ordering
- Kafka：partition-key 原生保證 per-member ordering，replay 最強，且已在用

**為什麼 Notification Worker 獨立成新 process？**
Event Worker 要高吞吐（目標 10K+ events/s），Notification Worker 的 HTTP callback 有 retry、timeout、可能阻塞。混在一起會拖慢 event 處理。獨立部署可以各自擴展，event 處理不受下游 API 延遲影響。

---

## Phase 1: DB Connection Pool 設定（防 crash）

**檔案**: `pkg/db/db.go`

在 `DBConfig` 加入 pool 參數，`Init()` 裡 `gorm.Open` 後設定：
```go
sqlDB, _ := db.DB()
sqlDB.SetMaxOpenConns(20)
sqlDB.SetMaxIdleConns(10)
sqlDB.SetConnMaxLifetime(5 * time.Minute)
```

同時開啟 GORM PreparedStmt（減少重複 query 的 parse/plan 開銷）：
```go
gorm.Open(postgres.Open(config.DSN()), &gorm.Config{
    PrepareStmt: true,
})
```

**Config 變更**: `config.yaml` 的 `db:` 下加 `max_open_conns`, `max_idle_conns`, `conn_max_lifetime_sec`

**PostgreSQL 調參**（`docker-compose.yml`）：
```yaml
postgres:
  command: >
    -c shared_buffers=256MB
    -c effective_cache_size=512MB
    -c work_mem=4MB
```
- `shared_buffers`：快取空間從 128MB → 256MB，減少 aggregation query 的 cache miss
- `effective_cache_size`：告訴 query planner OS 有多少 cache，影響選擇 index scan 還是 seq scan
- `work_mem`：每個 query 的排序/hash 記憶體，aggregation 會用到

**預期效果**: 4+ workers 不再 crash，throughput 穩定在 ~2000 events/s

### Phase 1 實測結果（Apple M2 Pro, benchtime=10s）

| Workers | Baseline | Phase 1 | 改善 |
|---------|----------|---------|------|
| 1 | 587 events/s | 914 events/s | +56% |
| 4 | crash | 1865 events/s | 不再 crash |
| 8 | crash | 1732 events/s | 不再 crash |
| 16 | crash | 1596 events/s | 不再 crash，少量 SLOW SQL |
| 32 | crash | 1542 events/s | 不再 crash，SLOW SQL 增多 |

- 單 worker 提升來自 PreparedStmt + shared_buffers 調大
- 4 workers 為吞吐量甜蜜點，超過後因 25 條 DB 連線被多 goroutine 搶而下降
- Workers 16+ 出現 SLOW SQL（>200ms），瓶頸在 aggregation query 的 DB round-trip 次數 → Phase 2 解決

---

## Phase 2: Batch Aggregation SQL（合併 query）

**問題**：現在每個 event 對每個 aggregation key 發一條獨立 SQL（5 個 key = 5 次 DB round-trip），既慢又搶 shared_buffers。

**解法**：把 5 條 query 合併成 1 條，用 PostgreSQL 的 `FILTER` 語法：

```sql
-- 之前：5 條 query
SELECT COALESCE(COUNT(*), 0) FROM behavior_logs WHERE member_id=$1 AND behavior='Trade' AND occurred_at >= $2;
SELECT COALESCE(COUNT(*), 0) FROM behavior_logs WHERE member_id=$1 AND behavior='CryptoWithdraw' AND occurred_at >= $3;
SELECT COALESCE(SUM((fields->>'amount')::numeric), 0) FROM behavior_logs WHERE member_id=$1 AND behavior='CryptoWithdraw' AND occurred_at >= $4;
...

-- 之後：1 條 query
SELECT
  COALESCE(COUNT(*) FILTER (WHERE behavior='Trade' AND occurred_at >= $1), 0),
  COALESCE(COUNT(*) FILTER (WHERE behavior='CryptoWithdraw' AND occurred_at >= $2), 0),
  COALESCE(SUM((fields->>'amount')::numeric) FILTER (WHERE behavior='CryptoWithdraw' AND occurred_at >= $3), 0)
FROM behavior_logs
WHERE member_id = $4
```

DB 只掃一次 `behavior_logs`，shared_buffers 壓力大幅降低。

**要改的檔案**：

1. **`service/base/behavior/model/interface.go`** — interface 加 `BatchAggregate`：
```go
type BehaviorRepoInterface interface {
    Create(ctx context.Context, obj *BehaviorLog) error
    Aggregate(ctx context.Context, cond *AggregateCond) (float64, error)
    BatchAggregate(ctx context.Context, memberID string, keys []AggregateCond) (map[string]float64, error)
}

type BehaviorUsecaseInterface interface {
    Log(ctx context.Context, req *LogBehaviorReq) (*BehaviorLog, error)
    Aggregate(ctx context.Context, cond *AggregateCond) (float64, error)
    BatchAggregate(ctx context.Context, memberID string, keys []AggregateCond) (map[string]float64, error)
}
```

2. **`service/base/behavior/repository/db/behavior.go`** — 新增 `BatchAggregate` method：
   - 動態組 SELECT 欄位，每個 key 一個 `COALESCE(AGG(...) FILTER (WHERE ...), 0)`
   - 共用 `WHERE member_id = ?`
   - 用 `ValidateAggregation` + `ValidateFieldPath` 防 SQL injection（已有）
   - 回傳 `map[cacheKey]float64`

3. **`service/base/behavior/usecase/behavior.go`** — 加 `BatchAggregate` 透傳

4. **`service/bff/worker/usecase/event_usecase.go`** lines 113-122 — 把 for loop 換成一次呼叫：
```go
// 之前
for _, k := range allKeys {
    result, err := h.behaviorUC.Aggregate(ctx, cond)
    cache[k.CacheKey()] = result
}

// 之後
conds := buildAggregateConds(event.MemberID, allKeys, now)
cache, err := h.behaviorUC.BatchAggregate(ctx, event.MemberID, conds)
```

**依賴**: Phase 1（需要 connection pool）

**預期效果**: 5 次 DB round-trip → 1 次。單 event 延遲從 ~1.7ms 降到 ~0.5ms。同時大幅降低 DB 連線佔用和 cache miss

---

## Phase 3: Per-Partition Goroutine + 多 Process

**方案**: 改寫 `event_manager.go`，每個被分配的 partition 開一個 goroutine sequential 處理。搭配多 process 水平擴展。

```
4 個 worker process × 32 goroutines each = 128 partitions 全覆蓋

Process A (consumer group: rule-engine-worker)
  → rebalance 分配 partition 0-31
  → goroutine-0:  sequential 處理 partition 0
  → goroutine-1:  sequential 處理 partition 1
  → ...
  → goroutine-31: sequential 處理 partition 31

Process B/C/D 同理
```

**檔案**: `service/bff/worker/event_manager.go`

核心改動：

1. **Rebalance callback** — 被分配 partition 時建立 channel + goroutine，被撤銷時 close channel：
```go
consumer.Subscribe(topic, func(c *kafka.Consumer, ev kafka.Event) error {
    switch e := ev.(type) {
    case kafka.AssignedPartitions:
        for _, tp := range e.Partitions {
            ch := make(chan *kafka.Message, 64)
            channels[tp.Partition] = ch
            go func(ch chan *kafka.Message) {
                for msg := range ch {
                    handler.Execute(msg)
                    // offset commit 送回主 goroutine 統一處理
                    commitCh <- msg
                }
            }(ch)
        }
    case kafka.RevokedPartitions:
        for _, tp := range e.Partitions {
            close(channels[tp.Partition])
            delete(channels, tp.Partition)
        }
    }
    return nil
})
```

2. **Poll loop** — 只負責分發 message 到對應 partition 的 channel：
```go
for {
    ev := consumer.Poll(100)
    if msg, ok := ev.(*kafka.Message); ok {
        channels[msg.TopicPartition.Partition] <- msg
    }
}
```

3. **Offset commit** — `confluent-kafka-go` 的 `CommitMessage` 不是 thread-safe，需要統一在 Poll loop 的 goroutine commit。用一個 `commitCh` 收集處理完的 message，Poll loop 定期 batch commit：
```go
// 在 Poll loop 裡同時 select commitCh
select {
case msg := <-commitCh:
    consumer.StoreMessage(msg)  // thread-safe，標記 offset
default:
}
// 搭配 enable.auto.offset.store=false + enable.auto.commit=true
// librdkafka 會定期 commit 已 store 的 offset
```

**為什麼安全**:
- 同一 partition 的 message 只會在一個 goroutine 裡 sequential 處理
- `StoreMessage` 只標記「這個 offset 可以 commit」，不會跳過未完成的
- 同一 partition 內 msg 3 沒 store 的話，msg 4 store 了也不會被 commit（librdkafka 按 partition 追蹤最小未 store offset）

**Kafka consumer config 變更** (`pkg/kafka/kafka.go`):
```go
"enable.auto.commit":       true,
"auto.commit.interval.ms":  5000,
"enable.auto.offset.store": false,  // 手動 StoreMessage
```

**部署**:
```bash
docker compose up --scale worker=4
```
4 個 process，每個被分配 32 partitions，開 32 goroutines。共 128 goroutines 並行處理。

**DB connection pool 注意**: Phase 2 Batch SQL 後，每個 goroutine 處理一個 event 只需要 2 條 DB query（1 INSERT + 1 BatchAggregate）。32 goroutines 同時跑 = 最多 64 條同時連線。`MaxOpenConns` 設 50，多餘的排隊等連線即可（幾 ms 延遲可接受）。

**預期效果**: 4 process × 32 goroutines，每個 goroutine ~500-1000 events/s（受 DB 限制），總吞吐量 ~8000-15000 events/s

---

## Phase 4: 非同步 Kafka Produce + Result Notification Worker

分兩部分：

### 4a: Worker 端改為非同步 Produce

**問題**：現在 Worker produce 到 results topic 是同步等 broker ACK（~0.5-1ms），但 behavior_log 已在步驟 1 寫進 DB，result produce 失敗不會丟資料。

**檔案**: `pkg/kafka/kafka.go`

新增 `ProduceAsync` 函式 + 背景 delivery report goroutine：
```go
func ProduceAsync(p *kafka.Producer, topic, key string, value []byte) error {
    if p == nil { return nil }
    return p.Produce(&kafka.Message{
        TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
        Key:            []byte(key),
        Value:          value,
    }, nil)  // nil = 用 Events() channel，不阻塞
}

func StartDeliveryReporter(p *kafka.Producer) {
    go func() {
        for e := range p.Events() {
            if m, ok := e.(*kafka.Message); ok && m.TopicPartition.Error != nil {
                slog.Error("kafka: delivery failed", "error", m.TopicPartition.Error)
            }
        }
    }()
}
```

- `event_usecase.go` line 177 改呼叫 `ProduceAsync`
- 保留原本的 `Produce`（API server POST /v1/events 仍需同步確認）

**預期效果**: 每個 event 省 ~0.5-1ms

### 4b: 新增 Result Notification Worker

**目的**：消費 `rule-engine-results` topic，對匹配的結果發 REST callback 通知下游服務。

**為什麼獨立成一個 Worker**：
- Event Worker 保持簡單 — 只負責 evaluate + produce result
- Notification Worker 專門處理 retry、timeout、circuit breaker
- 兩者可以獨立擴展（event 處理需要高吞吐，通知不需要）

**架構**：
```
Event Worker                     Notification Worker
  evaluate rules                   consume results topic
  produce → results topic    →     POST callback URL
                                   失敗 → retry (exponential backoff)
                                   多次失敗 → dead letter queue / log
```

**新增檔案**：
- `service/bff/notifier/notification_manager.go` — 消費 results topic 的 WorkerManager
- `service/bff/notifier/usecase/notification_usecase.go` — callback 邏輯 + retry
- `service/bff/notifier/model/` — callback config（URL、retry policy）
- `cmd/notifier/main.go` — 新的 entry point

**NotificationManager 設計**：
```go
// 消費 results topic
func (m *NotificationManager) Run() error {
    consumer.Subscribe(cfg.Kafka.Topics.Results, nil)
    for {
        msg := consumer.Poll(100)
        // parse MatchResult
        // POST to callback URL with retry
        // commit offset
    }
}
```

**Callback 設計**：
```go
type CallbackConfig struct {
    URL            string        // 下游 API endpoint
    Timeout        time.Duration // 單次請求 timeout（e.g. 5s）
    MaxRetries     int           // 最大重試次數（e.g. 3）
    RetryInterval  time.Duration // 重試間隔（e.g. 1s, exponential backoff）
}
```

**Config 變更** (`config.yaml`)：
```yaml
notifier:
  callbacks:
    - url: "http://alert-service/v1/alerts"
      timeout: 5s
      max_retries: 3
    - url: "http://account-service/v1/freeze"
      timeout: 3s
      max_retries: 5
```

**部署**: docker-compose 加 notifier service，獨立於 event worker

---

## 實作順序與依賴

```
Phase 1 (DB Pool + PreparedStmt)       → 無依賴，先做
Phase 2 (Batch Aggregation SQL)        → 依賴 Phase 1
Phase 3 (Per-Partition Goroutine)      → 依賴 Phase 1
Phase 4a (Async Produce)               → 無依賴，可隨時做
Phase 4b (Notification Worker)         → 依賴 Phase 4a（需要 results topic 有資料）
```

Phase 1 → 2 → 3 → 4 逐步做，每個 phase 完成後跑一次 `BenchmarkThroughput_Integration` 驗證效果。

## 預期累計效果

| 完成階段 | 預估吞吐量 (4 workers) |
|---|---|
| Baseline | crash |
| Phase 1 | ~2000 events/s |
| Phase 1+2 | ~4000 events/s |
| Phase 1+2+3 | ~8000-12000 events/s |
| Phase 1+2+3+4 | ~10000-15000 events/s |

## 要改的檔案

| 檔案 | Phase | 改什麼 |
|---|---|---|
| `pkg/db/db.go` | 1 | Pool config + PreparedStmt |
| `config.yaml` / `config.example.yaml` | 1 | DB pool 參數 |
| `service/base/behavior/model/interface.go` | 2 | interface 加 BatchAggregate |
| `service/base/behavior/repository/db/behavior.go` | 2 | 新增 BatchAggregate method |
| `service/base/behavior/usecase/behavior.go` | 2 | 透傳 BatchAggregate |
| `service/bff/worker/usecase/event_usecase.go` | 2, 4a | 改用 BatchAggregate、改呼叫 ProduceAsync |
| `service/bff/worker/event_manager.go` | 3 | Per-partition goroutine dispatch、rebalance callback |
| `pkg/kafka/kafka.go` | 3, 4a | auto.commit + auto.offset.store 設定、ProduceAsync + StartDeliveryReporter |
| `service/bff/notifier/notification_manager.go` | 4b | 新增：消費 results topic 的 WorkerManager |
| `service/bff/notifier/usecase/notification_usecase.go` | 4b | 新增：callback + retry 邏輯 |
| `cmd/notifier/main.go` | 4b | 新增：notifier entry point |

## 驗證方式

每個 phase 完成後：
```bash
# 啟動 infra
make docker-up

# 跑 throughput benchmark
go test -bench=BenchmarkThroughput_Integration -benchtime=30s ./service/bff/worker/usecase/
```

觀察：
1. workers-4/8/16 不再 crash
2. events/s 數字逐步提升
3. 無 SLOW SQL 警告