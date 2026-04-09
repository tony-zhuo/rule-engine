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
- Kafka：partition-key 原生保證 per-member ordering，replay 最強

**為什麼 Notification Worker 獨立成新 process？**
Event Worker 要高吞吐（目標 10K+ events/s），Notification Worker 的 HTTP callback 有 retry、timeout、可能阻塞。混在一起會拖慢 event 處理。獨立部署可以各自擴展，event 處理不受下游 API 延遲影響。

---

## Phase 1: DB Connection Pool 設定（防 crash）

**檔案**: `pkg/db/db.go`

在 `DBConfig` 加入 pool 參數，`Init()` 裡 `gorm.Open` 後設定：

```go
sqlDB, _ := db.DB()
sqlDB.SetMaxOpenConns(25)
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


| Workers | Baseline     | Phase 1       | 改善                      |
| ------- | ------------ | ------------- | ------------------------- |
| 1       | 587 events/s | 914 events/s  | +56%                      |
| 4       | crash        | 1865 events/s | 不再 crash                |
| 8       | crash        | 1732 events/s | 不再 crash                |
| 16      | crash        | 1596 events/s | 不再 crash，少量 SLOW SQL |
| 32      | crash        | 1542 events/s | 不再 crash，SLOW SQL 增多 |

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

### Bug Fix + Phase 2 實測結果（Apple M2 Pro, benchtime=10s）

此次 benchmark 同時包含 Bug Fix（processed_events 去重）和 Phase 2（Batch Aggregation SQL）的改動。

| Workers | Phase 1       | Bug Fix + Phase 2 | 變化  |
| ------- | ------------- | ------------------ | ----- |
| 1       | 914 events/s  | 723 events/s       | -21%  |
| 4       | 1865 events/s | 1138 events/s      | -39%  |
| 8       | 1732 events/s | 1142 events/s      | -34%  |
| 16      | 1596 events/s | 1032 events/s      | -35%  |
| 32      | 1542 events/s | 1070 events/s      | -31%  |

**吞吐量下降原因**：Bug Fix 的 `processed_events` 每次 event 新增了 3 次 DB round-trip（UPSERT + SELECT read-back + UPDATE），抵消了 Batch SQL 從 5 次 aggregation query 合併成 1 次省下的 4 次 round-trip。淨效果是每次 event 的 DB round-trip 反而比 Phase 1 多了。

SLOW SQL log 顯示瓶頸在 `processed_event.go:58` 的 SELECT read-back（UPSERT 後讀回完整 row 以取得 status 和 attempts）。

**追加優化**：`Upsert` 改用 PostgreSQL `INSERT ... ON CONFLICT ... RETURNING *` 省掉 SELECT read-back，減少 1 次 DB round-trip。實測結果：

| Workers | UPSERT + SELECT | RETURNING | 變化        |
| ------- | --------------- | --------- | ----------- |
| 1       | 723 events/s    | 790 events/s  | +9%     |
| 4       | 1138 events/s   | 1103 events/s | 誤差範圍 |
| 8       | 1142 events/s   | 1073 events/s | 誤差範圍 |
| 16      | 1032 events/s   | 1093 events/s | 誤差範圍 |
| 32      | 1070 events/s   | 971 events/s  | 誤差範圍 |

省掉 SELECT read-back 的效果不顯著，瓶頸不在那次 SELECT，而是 `processed_events` 整體多出的 2 次寫入 round-trip（UPSERT + UPDATE）。這部分是 Bug Fix 正確性保證的必要成本，無法再省。

### EXPLAIN ANALYZE：Batch SQL vs 分開 5 條 query

測試對象：`member-1`（4679 筆 behavior_logs）

**分開 5 條 query（每條完整走 `(member_id, behavior, occurred_at)` 三欄 index）：**

| Query | Index 使用 | DB 執行時間 |
|-------|-----------|------------|
| Trade COUNT (3 days) | Index Scan ✅ 三欄全中 | 0.09ms |
| CryptoWithdraw COUNT (24h) | Index Scan ✅ 三欄全中 | 2.13ms |
| CryptoWithdraw SUM amount (7d) | Index Scan ✅ 三欄全中 | 9.35ms |
| FiatDeposit COUNT (1h) | Index Scan ✅ 三欄全中 | 0.03ms |
| FiatDeposit MAX amount (1h) | Index Scan ✅ 三欄全中 | 0.02ms |
| **合計（DB 端）** | | **11.6ms** |
| **+ 5 次 round-trip（~1ms/次）** | | **~16.6ms** |

**Batch 1 條 query（FILTER 語法，只用到 `member_id` 前綴）：**

| Query | Index 使用 | DB 執行時間 |
|-------|-----------|------------|
| Batch FILTER | Bitmap Index Scan ⚠️ 只中 `member_id`，撈出 4679 筆後 heap filter | **29ms** |
| **+ 1 次 round-trip** | | **~30ms** |

**EXPLAIN ANALYZE 結論**：單看 DB 執行時間，分開的 query（11.6ms）比 Batch（29ms）快一倍，因為每條都精準走三欄 index。Batch query 只用到 `member_id` 前綴，必須掃該 member 全部 row 再做 FILTER。

### 實際 Benchmark：分開 query vs Batch SQL

但 EXPLAIN ANALYZE 只測 DB 端執行時間，不含連線取得開銷。在高併發下，分開 5 條 query 要從 connection pool（MaxOpenConns=25）取 5 次連線，連線爭搶成為瓶頸。

| Workers | Phase 1       | Batch SQL     | 分開 5 條 query |
| ------- | ------------- | ------------- | --------------- |
| 1       | 914 events/s  | 790 events/s  | 636 events/s    |
| 4       | 1865 events/s | 1103 events/s | 712 events/s    |
| 8       | 1732 events/s | 1073 events/s | 744 events/s    |
| 16      | 1596 events/s | 1093 events/s | 882 events/s    |
| 32      | 1542 events/s | 971 events/s  | 701 events/s    |

分開 query 比 Batch 更慢 20-35%。原因：每次 `Aggregate` 都要從 pool 取連線 → 發 query → 等回應 → 歸還，5 次 round-trip 放大了連線爭搶。Batch 雖然 DB 端慢，但只佔 1 條連線 1 次。

**決定**：維持 Batch SQL。在 connection pool 有限的情況下，減少 round-trip 次數比優化單條 query 的 index 利用率更重要。

### CTE 合併 round-trip：UPSERT processed_events + INSERT behavior_log

原本每條 event 有 4 次 DB round-trip：

```
1. UPSERT processed_events  (寫)
2. INSERT behavior_log       (寫)
3. BatchAggregate            (讀)
4. UPDATE processed_events   (寫)
```

用 PostgreSQL CTE 把步驟 1+2 合成一條 SQL，減少到 3 次：

```sql
WITH pe AS (
    INSERT INTO processed_events ... ON CONFLICT ... RETURNING *
),
bl AS (
    INSERT INTO behavior_logs ...
    WHERE EXISTS (SELECT 1 FROM pe WHERE status = 'pending')
    ON CONFLICT (event_id) DO NOTHING
    RETURNING id
)
SELECT * FROM pe;
```

CTE 額外好處：已完成/已失敗的 event 不會執行 behavior_log INSERT（WHERE EXISTS 過濾），對 completed/failed 的 event 也省了一次無謂的寫入。

| Workers | 4 round-trips | CTE 3 round-trips | 變化   |
| ------- | ------------- | ------------------ | ------ |
| 1       | 790 events/s  | 745 events/s       | -6%    |
| 4       | 1103 events/s | 1149 events/s      | +4%    |
| 8       | 1073 events/s | 1115 events/s      | +4%    |
| 16      | 1093 events/s | 1073 events/s      | -2%    |
| 32      | 971 events/s  | 1071 events/s      | **+10%** |

高併發（workers 32）改善最明顯（+10%），因為少 1 次 round-trip = 少 1 次連線爭搶。低併發無顯著差異。

### Phase 2 總結

| 優化項目 | DB round-trips | 效果 |
|---------|---------------|------|
| Baseline (Phase 1) | 7 次（1 INSERT + 5 Aggregate + 1 Produce） | 914 events/s (1 worker) |
| + Bug Fix (processed_events) | +3 次 = 10 次 | 吞吐量下降（正確性成本） |
| + Batch SQL (5→1 Aggregate) | -4 次 = 6 次 | 部分抵消 Bug Fix 的開銷 |
| + RETURNING (省 SELECT) | -1 次 = 5 次 | 效果不顯著 |
| + CTE 合併 (2→1 寫入) | -1 次 = 4 次 | 高併發 +10% |

目前每條 event 4 次 DB round-trip（CTE 寫入 + BatchAggregate + UPDATE + Kafka produce），已接近單連線處理的極限。進一步提升需靠 Phase 3 並行處理。

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

## Bug Fix: Retry 時 Duplicate Event 被跳過

### 問題

現在的 Execute pipeline（`event_usecase.go:71-181`）在 retry 時有 bug：

```
第一次消費 msg：
  1. INSERT behavior_log → 成功（ON CONFLICT DO NOTHING，RowsAffected=1）
  2. ListActive rules → Redis 斷線 → return err
  3. Kafka 不 commit

第二次消費同一條 msg（retry）：
  1. INSERT behavior_log → RowsAffected=0 → return ErrDuplicateEvent
  2. event_usecase.go:87-89 收到 ErrDuplicateEvent → return nil（直接跳過）
  3. Kafka commit → 這條 event 永遠沒被 evaluate
```

### 為什麼不能用 DB transaction 或 status 欄位解決

**DB transaction 不可行**：Pipeline 裡不只有 DB 操作 — load rules 是 Redis、CEP progress 是 Redis、produce 是 Kafka。DB transaction 無法包住這些外部系統，rollback DB 時 Redis/Kafka 的副作用已經發生。

**behavior_logs 加 status 欄位不可行**：

- `pending → completed` 狀態機要求中間步驟的副作用可 rollback 或追蹤，但 CEP progress（Redis）、Kafka produce 都是不可逆的外部操作
- UPDATE status='completed' 的時機兩難：放在 Kafka produce 之前，produce 失敗就丟事件；放在之後，Phase 4a async produce 拿不到同步結果
- 把流程控制欄位（status）混進業務資料表（behavior_logs）是職責混淆

### 解法：分離去重 + 每步冪等

核心思路：不做 rollback，讓 retry 安全地重跑整條 pipeline。用獨立的 `processed_events` 表標記「完整跑完」，pipeline 中每個有副作用的步驟各自做冪等。

#### 流程

```
Execute(event):
  1. UPSERT processed_events: INSERT ON CONFLICT SET attempts = attempts + 1
     → status = 'completed' → return nil（已成功，跳過）
     → status = 'failed' → return nil（已放棄，跳過）
     → attempts > max_retries → UPDATE status='failed', return nil
     → 否則繼續（status 維持 'pending'）

  2. INSERT behavior_log（ON CONFLICT DO NOTHING）
     → 不管第幾次都安全，不再 return error
     → aggregation query 需要這筆資料在 DB 裡

  3. Load rules（Redis cache，純讀取）

  4. Aggregation query（純讀取）

  5. Rule evaluation（純運算，無副作用）

  6. CEP ProcessEvent（Redis 寫入 — 需要冪等化，見下方）

  7. Kafka produce results

  8. UPDATE processed_events SET status='completed'

  9. return nil → Kafka commit offset
```

#### Retry 場景

```
第一次消費：
  1. UPSERT processed_events → 新建（attempts=1, status='pending'）→ 繼續
  2. INSERT behavior_log → 成功
  3-5. Load rules, aggregate, evaluate → 成功
  6. CEP ProcessEvent → 推進 progress → Redis 斷線 → return err
  7-9. 沒跑到
  → Kafka 不 commit

第二次消費（retry）：
  1. UPSERT processed_events → attempts=2, status='pending' → 繼續
  2. INSERT behavior_log → ON CONFLICT DO NOTHING（已存在，靜默跳過）
  3-5. Load rules, aggregate, evaluate → 重跑，結果相同
  6. CEP ProcessEvent → 用 event_id 去重，跳過已處理的推進
  7. Kafka produce → 成功
  8. UPDATE processed_events SET status='completed'
  9. return nil → Kafka commit
```

#### 各步驟冪等策略

| 步驟 | 副作用 | 冪等方式 |
|------|--------|---------|
| INSERT behavior_log | DB 寫入 | `ON CONFLICT DO NOTHING`，不回傳 error |
| Load rules | 無（純讀取） | 天生冪等 |
| Aggregation | 無（純讀取） | 天生冪等 |
| Rule evaluation | 無（純運算） | 天生冪等 |
| CEP ProcessEvent | Redis 寫入 | event_id 去重（見下方） |
| Kafka produce | 訊息發送 | 下游用 event_id 去重 |
| UPSERT/UPDATE processed_events | DB 寫入 | UPSERT 原子操作；UPDATE status 是冪等的（completed → completed 無影響） |

#### CEP 冪等化

CEP progress 存在 Redis（`rule_engine:progress:<id>`），目前 `advanceProgress`（`cep.go:117-170`）收到 event 就無條件推進 `CurrentStep`。retry 時同一條 event 會重複推進，導致 pattern 誤觸發。

**做法**：在 `PatternProgress` 加 `ProcessedEvents []string` 欄位，記錄已處理的 event_id：

```go
type PatternProgress struct {
    // ... 現有欄位
    ProcessedEvents []string `json:"processed_events"` // 已處理的 event_id
}
```

`advanceProgress` 開頭檢查：

```go
func (p *CEPUsecase) advanceProgress(ctx context.Context, progress *model.PatternProgress, event *model.Event) (*model.MatchResult, error) {
    // 冪等檢查：這個 progress 已經處理過這條 event
    if slices.Contains(progress.ProcessedEvents, event.EventID) {
        return nil, nil
    }

    // ... 現有推進邏輯

    // 推進成功後記錄
    progress.ProcessedEvents = append(progress.ProcessedEvents, event.EventID)
}
```

因為 progress 有 TTL（`ExpiresAt`），`ProcessedEvents` 會隨 progress 過期自動清理，不需要額外清理機制。

#### Retry 上限與 `processed_events` 的 status 欄位

如果某條 event 因為 bug 永遠無法成功（例如 rule 引用不存在的 field），會無限 retry 阻塞整個 partition。

**為什麼需要在 application layer 追蹤 retry？**

Kafka consumer 沒有 per-message retry 上限的機制。Kafka 的 retry 行為是：consumer 不 commit offset → 下次 poll 拿到同一條 message → 無條件重試。沒有原生設定可以限制單條 message 的重試次數。

因此 `processed_events` 除了作為 completion marker，還必須追蹤 attempts 和 status，才能在 application layer 實現 retry 上限：

- **`attempts`**：每次 Execute 開頭 UPSERT +1，記錄這條 event 被嘗試了幾次
- **`status`**：區分三種終態 — `pending`（進行中）、`completed`（成功）、`failed`（超過重試上限，放棄）

```sql
CREATE TABLE processed_events (
    event_id    VARCHAR(64) PRIMARY KEY,
    attempts    INT         NOT NULL DEFAULT 0,
    status      VARCHAR(16) NOT NULL DEFAULT 'pending',  -- pending / completed / failed
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

**完整 Pipeline（含 retry 上限）：**

```
Execute(event):
  1. UPSERT processed_events: INSERT ON CONFLICT SET attempts = attempts + 1
     → status = 'completed' → return nil（已成功，跳過）
     → status = 'failed' → return nil（已放棄，跳過）
     → attempts > max_retries → UPDATE status='failed', return nil（超過上限，放棄）
     → 否則繼續

  2-7. 原有 pipeline（每步冪等）

  8. UPDATE processed_events SET status='completed'
  9. return nil → Kafka commit
```

超過 `max_retries` 被標記為 `failed` 的 event，後續可透過 monitoring 告警或人工介入重新處理。

### 要改的檔案

1. **新增 migration file** `database/migrate/init/005_create_processed_events.up.sql`：

```sql
CREATE TABLE IF NOT EXISTS processed_events (
    event_id    VARCHAR(64)  PRIMARY KEY,
    attempts    INT          NOT NULL DEFAULT 0,
    status      VARCHAR(16)  NOT NULL DEFAULT 'pending',
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
```

2. **`service/base/behavior/repository/db/behavior.go`** — `Create` method：
   - 移除 `ErrDuplicateEvent` 回傳，`ON CONFLICT DO NOTHING` 後不管 RowsAffected，直接 return nil

3. **新增 `processed_events` 的 repository + model**：
   - `service/base/behavior/model/entity.go` — 新增 `ProcessedEvent` struct
   - `service/base/behavior/model/interface.go` — interface 加 `Upsert` / `MarkCompleted` / `MarkFailed`
   - `service/base/behavior/repository/db/processed_event.go` — UPSERT + UPDATE 實作

4. **`service/bff/worker/usecase/event_usecase.go`** — Execute 改寫：
   - 開頭：UPSERT processed_events，檢查 status 和 attempts
   - 結尾：UPDATE status='completed'
   - 移除 `ErrDuplicateEvent` 的判斷邏輯（line 87-89）

5. **`service/base/cep/model/entity.go`** — `PatternProgress` 加 `ProcessedEvents` 欄位

6. **`service/base/cep/usecase/cep.go`** — `advanceProgress` 加 event_id 去重檢查

### 與其他 Phase 的關係

- **不再影響 behavior_logs schema** — behavior_logs 保持純業務資料表
- **Phase 4a 相容** — async produce 不影響此設計，因為 processed_events 的 completed 標記可以放在 produce 呼叫之後（async produce 只是不等 ACK，呼叫本身是同步的）
- **Phase 3 安全** — rebalance 時同一 event 可能被兩個 consumer 處理，但 UPSERT processed_events 是原子操作，attempts 計數準確；pipeline 每步冪等，重複執行不會產生錯誤結果
- 此修法應在 Phase 2 之前做，因為它影響 Execute 的流程和 error handling

---

## 實作順序與依賴

```
Bug Fix (Retry Duplicate Event)        → 無依賴，最優先
Phase 1 (DB Pool + PreparedStmt)       → 無依賴，已完成 ✅
Phase 2 (Batch Aggregation SQL)        → 依賴 Phase 1 + Bug Fix
Phase 3 (Per-Partition Goroutine)      → 依賴 Phase 1
Phase 4a (Async Produce)               → 無依賴，可隨時做
Phase 4b (Notification Worker)         → 依賴 Phase 4a（需要 results topic 有資料）
```

Phase 1 → 2 → 3 → 4 逐步做，每個 phase 完成後跑一次 `BenchmarkThroughput_Integration` 驗證效果。

## 預期累計效果


| 完成階段      | 預估吞吐量 (4 workers) |
| ------------- | ---------------------- |
| Baseline      | crash                  |
| Phase 1       | ~2000 events/s         |
| Phase 1+2     | ~4000 events/s         |
| Phase 1+2+3   | ~8000-12000 events/s   |
| Phase 1+2+3+4 | ~10000-15000 events/s  |

## 要改的檔案


| 檔案                                                   | Phase | 改什麼                                                                     |
| ------------------------------------------------------ | ----- | -------------------------------------------------------------------------- |
| `database/migrate/init/005_create_processed_events.up.sql` | Bug Fix | 新增 processed_events 表                                              |
| `service/base/behavior/model/entity.go`                | Bug Fix | 新增 ProcessedEvent struct                                                |
| `service/base/behavior/model/interface.go`             | Bug Fix | interface 加 Upsert / MarkCompleted / MarkFailed                          |
| `service/base/behavior/repository/db/processed_event.go` | Bug Fix | processed_events UPSERT + UPDATE 實作                                   |
| `service/base/behavior/repository/db/behavior.go`      | Bug Fix | Create 移除 ErrDuplicateEvent 回傳                                        |
| `service/bff/worker/usecase/event_usecase.go`          | Bug Fix | 改用 processed_events 去重 + 移除 ErrDuplicateEvent 邏輯                  |
| `service/base/cep/model/entity.go`                     | Bug Fix | PatternProgress 加 ProcessedEvents 欄位                                   |
| `service/base/cep/usecase/cep.go`                      | Bug Fix | advanceProgress 加 event_id 冪等檢查                                      |
| `pkg/db/db.go`                                         | 1     | Pool config + PreparedStmt                                                 |
| `config.yaml` / `config.example.yaml`                  | 1     | DB pool 參數                                                               |
| `service/base/behavior/model/interface.go`             | 2     | interface 加 BatchAggregate                                                |
| `service/base/behavior/repository/db/behavior.go`      | 2     | 新增 BatchAggregate method                                                 |
| `service/base/behavior/usecase/behavior.go`            | 2     | 透傳 BatchAggregate                                                        |
| `service/bff/worker/usecase/event_usecase.go`          | 2, 4a | 改用 BatchAggregate、改呼叫 ProduceAsync                                   |
| `service/bff/worker/event_manager.go`                  | 3     | Per-partition goroutine dispatch、rebalance callback                       |
| `pkg/kafka/kafka.go`                                   | 3, 4a | auto.commit + auto.offset.store 設定、ProduceAsync + StartDeliveryReporter |
| `service/bff/notifier/notification_manager.go`         | 4b    | 新增：消費 results topic 的 WorkerManager                                  |
| `service/bff/notifier/usecase/notification_usecase.go` | 4b    | 新增：callback + retry 邏輯                                                |
| `cmd/notifier/main.go`                                 | 4b    | 新增：notifier entry point                                                 |

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
