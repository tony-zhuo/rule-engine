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

### PgBouncer 實測（Apple M2 Pro, benchtime=10s）

嘗試在 app 和 PostgreSQL 之間加 PgBouncer（transaction mode），將 app 側 MaxOpenConns 從 25 提高到 100，PgBouncer 維持 25 條實際連線到 PostgreSQL。

注意：PgBouncer transaction mode 下必須關閉 GORM `PrepareStmt`（prepared statement 綁定 server connection，PgBouncer 在 transaction 結束後會重新分配連線）。

| Workers | 直連 PostgreSQL (MaxOpen=25, PrepareStmt=true) | PgBouncer (MaxOpen=100, PrepareStmt=false) | 變化 |
| ------- | ----------------------------------------------- | ------------------------------------------ | ---- |
| 1       | 745 events/s                                    | 718 events/s                               | -4%  |
| 4       | 1149 events/s                                   | 1032 events/s                              | -10% |
| 8       | 1115 events/s                                   | 1041 events/s                              | -7%  |
| 16      | 1073 events/s                                   | 1012 events/s                              | -6%  |
| 32      | 1071 events/s                                   | 992 events/s                               | -7%  |

**PgBouncer 在 localhost 環境反而更慢**，原因：

1. **代理 overhead**：每個 query 多一次 TCP 轉發（app → PgBouncer → PostgreSQL），在 localhost 下延遲本來就趨近零，多一跳反而增加開銷
2. **失去 PrepareStmt**：PgBouncer transaction mode 不支援 prepared statement，每條 SQL 都要重新 parse/plan，抵消了連線池的收益
3. **localhost 無連線爭搶瓶頸**：直連時連線取得幾乎零成本，PgBouncer 的連線複用優勢無法體現

**結論**：PgBouncer 的優勢在 production 環境（app 與 DB 分機器、有真實網路延遲）才會顯現。localhost benchmark 下不適用，暫不納入優化路徑。

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

**為什麼 `auto.commit` 要改回 `true`**

`offset.store` 和 `auto.commit` 是兩個不同層級的概念，Phase 3 把它們拆開各司其職：

- `offset.store`：把 offset 標記為「已處理完、可以 commit」，只寫到本地記憶體
- `auto.commit`：把已 store 的 offset 真正送到 broker，做 consumer group 協調

Phase 3 的流程：

1. partition goroutine 處理完 event 後把 msg 丟進 `commitCh`
2. Poll goroutine 單執行緒呼叫 `StoreMessage(msg)` → 只是標記該 offset 可 commit
3. librdkafka 按 partition 追蹤「最小未 store 的 offset」，同一 partition 內 msg 3 沒完成時，msg 4 就算 store 了也不會被 commit → 保證 at-least-once
4. 真正送到 broker 的動作交給 librdkafka 背景執行緒每 5 秒做一次（`auto.commit.interval.ms=5000`）

為什麼不沿用 Phase 1/2 的手動 `CommitMessage`：

- `confluent-kafka-go` 的 `CommitMessage` 是同步 RPC，每條 event commit 一次會變成新的瓶頸
- `CommitMessage` 不是 thread-safe，多個 partition goroutine 同時呼叫會出問題
- 改成「worker 只 Store、librdkafka 背景 batch commit」後，commit 成本從 per-event 變成每 5 秒一次，吞吐量才能吃滿

結論：`auto.commit=true` 負責「批次送出」，`offset.store=false` 讓我們保有「什麼時候算完成」的控制權，兩者合起來才是安全又高吞吐的組合。

**部署**:

```bash
docker compose up --scale worker=4
```

4 個 process，每個被分配 32 partitions，開 32 goroutines。共 128 goroutines 並行處理。

**DB connection pool 注意**: Phase 2 Batch SQL 後，每個 goroutine 處理一個 event 只需要 2 條 DB query（1 INSERT + 1 BatchAggregate）。32 goroutines 同時跑 = 最多 64 條同時連線。`MaxOpenConns` 設 50，多餘的排隊等連線即可（幾 ms 延遲可接受）。

**預期效果**: 4 process × 32 goroutines，每個 goroutine ~500-1000 events/s（受 DB 限制），總吞吐量 ~8000-15000 events/s

---

## Phase 4: Rule Strategy Compilation（消除 GC 壓力）

### 問題：遞迴 tree walk 的 heap escape

目前 `Evaluate(node RuleNode, ctx EvalContext)` 每個 event 都遞迴走一遍 RuleNode tree，造成大量 heap escape：

1. **RuleNode struct 逃逸** — `Evaluate` 接收 `model.RuleNode`（含 `[]RuleNode` children slice、`any` Value、`*TimeWindow` pointer），遞迴呼叫時 Go compiler 無法證明這些值不逃逸，全部分配到 heap
2. **`any` interface boxing** — `resolveValue` 和 `resolveRHS` 回傳 `any`，每次比較都要 box/unbox，產生 heap allocation
3. **`fmt.Sprintf("%v", actual)`** — string comparison path 每次呼叫都 allocate 新 string
4. **`strings.ToUpper(node.Operator)`** — 每次 eval 都 allocate 新 string
5. **`fmt.Errorf(...)`** — 每個 error wrap 都是 heap allocation
6. **ListActive Redis GET** — 每個 event 都反序列化 `[]*RuleStrategy`，產生大量短命 object

在高吞吐量場景（10K+ QPS），每個 event 評估多條 rule，每條 rule 有多個 node，**每秒產生數萬個短命 heap object**，GC 頻繁觸發 stop-the-world。

### 解法：compile-time 一次性分配，eval-time 零分配

將 `RuleNode` tree 在 application 啟動時編譯為 function closure：
- Closure 本身只在 compile time 分配一次，長生命週期住在 heap 上，不會被 GC 回收
- Eval time 直接呼叫 closure，**不傳遞 RuleNode struct、不做 type switch、不做 string 操作**
- 附帶好處：hot path 不再需要 Redis GET / DB query 取 rule 定義

### Architecture

```
Startup:
  DB → []*RuleStrategy → Compile(RuleNode) → []CompiledRule → atomic.Pointer
  (one-time heap allocation, lives until next Reload)

Hot path (per event):
  atomic.Pointer.Load() → []CompiledRule    ← no alloc, pointer read
  ├─ pre-extracted AggregateKeys iteration  ← no alloc, slice read
  ├─ BatchAggregate (DB, unavoidable)
  └─ cr.EvalFn(evalCtx)                    ← closure call, no tree walk
      ├─ LHS: map lookup (fields/cache)    ← no alloc
      ├─ RHS: pre-coerced literal          ← no alloc (captured in closure)
      └─ compare: function pointer call    ← no alloc

Rule CRUD (API process):
  Create/Update/SetStatus → invalidateCache()
  └─ Redis Publish("rule_engine:rule_reload")
  └─ Worker subscriber → Reload() → re-compile → atomic swap
```

### 實作細節

#### 4.1: 新增 CompiledRule model

**新檔案**: `service/base/rule/model/compiled.go`

```go
type CompiledRule struct {
    ID            uint64
    Name          string
    EvalFn        func(EvalContext) (bool, error)  // compiled closure
    AggregateKeys []AggregateKey                    // pre-extracted at compile time
}
```

**修改**: `service/base/rule/model/interface.go` — 新增：

```go
type RuleRegistryInterface interface {
    GetCompiledRules() []CompiledRule
}
```

#### 4.2: Compile function — 消除 eval-time allocation

**新檔案**: `service/base/rule/usecase/compiler.go`

`Compile(node RuleNode) (func(EvalContext) (bool, error), error)` — 遞迴只在 compile time 發生（啟動時），不在 hot path。

**compileAnd/compileOr/compileNot**：compile time 建立 `[]func(EvalContext)(bool,error)` slice（一次性 heap alloc），回傳的 closure capture 這個 slice，eval time 只做 range + call。

**compileCondition** — 核心，以下全部在 compile time 完成：

| 目前 eval-time 的操作 | 改為 compile-time 預計算 | 避免的 alloc |
|---|---|---|
| `strings.Contains(field, ":")` | bool flag captured in closure | string scan |
| `strings.ToUpper(node.Operator)` | pre-uppercased string | string alloc |
| `switch op { case ">": ... }` | function pointer `func(float64,float64)bool` | switch overhead |
| `toFloat64(expected)` for literal | pre-coerced `float64` captured | `any` unboxing |
| `fmt.Sprintf("%v", actual)` | 只在確定是 string path 時才做 | 減少不必要 alloc |
| `AggregateKey{}.CacheKey()` | pre-computed string | string concat |
| `strings.HasPrefix(field, "$")` | bool flag | string check |
| `[]any` for IN list | pre-built `map[string]struct{}` | O(n)→O(1) + 避免 Sprintf |

**Zero-alloc eval path 設計**：

```go
func compileCondition(node RuleNode) (func(EvalContext) (bool, error), error) {
    // compile time: 預計算所有能預計算的東西
    // 1. field 分類（simple / aggregation / $variable）
    // 2. operator → function pointer
    // 3. RHS literal coercion
    // 4. aggregation cache key

    // 回傳的 closure 只做：
    //   a. map lookup 取 LHS value（fields[key] 或 cache[key]）
    //   b. toFloat64(lhs) — 只有 LHS 需要 runtime coerce（type switch, no alloc）
    //   c. 呼叫 pre-resolved comparator function pointer
    return func(ctx EvalContext) (bool, error) {
        lhs := fields[fieldKey]           // map lookup, no alloc
        lhsF, ok := toFloat64(lhs)        // type switch, no alloc
        return cmpFn(lhsF, rhsFloat), nil // function pointer call, no alloc
    }, nil
}
```

#### 4.3: RuleRegistry — lock-free read

**同檔案**: `service/base/rule/usecase/compiler.go`

```go
type RuleRegistry struct {
    rules     atomic.Pointer[[]CompiledRule]  // lock-free read on hot path
    repo      RuleStrategyRepoInterface
    rdb       *redis.Client
}
```

- `NewRuleRegistry(ctx, repo, rdb)` — startup 時 load + compile，fail-fast（DB 不通就不啟動）
- `GetCompiledRules() []CompiledRule` — `*r.rules.Load()`，single atomic pointer read
- `Reload(ctx)` — load from DB → compile each → build new slice → atomic swap
  - 舊 slice 由 GC 自然回收（等所有 in-flight goroutine 釋放 reference 後）
- `startSubscriber(ctx)` — Redis Pub/Sub on `rule_engine:rule_reload`
  - 收到訊息 → `Reload(ctx)`
  - 5 分鐘 periodic fallback（防 pub/sub message 遺失）

#### 4.4: API side signaling

**修改**: `service/base/rule/usecase/strategy.go`

`invalidateCache` 加入 `rdb.Publish(ctx, ruleReloadChannel, "reload")`。
保留現有 `rdb.Del`（API process 自己的 ListActive 可能還在用）。

#### 4.5: Wire into worker

**修改**: `service/bff/worker/init.go`

```go
ruleRegistry, err := ruleUsecase.NewRuleRegistry(ctx, ruleRepo, rdb)
if err != nil {
    log.Fatal("failed to initialize rule registry: ", err)
}
slog.Info("Rule registry loaded", "count", len(ruleRegistry.GetCompiledRules()))
```

Pass `ruleRegistry` to `NewEventUsecase`。

#### 4.6: Hot path changes

**修改**: `service/bff/worker/usecase/event_usecase.go`

- `EventUsecase` 加 `ruleRegistry ruleModel.RuleRegistryInterface`
- 移除 `ruleStrategyUC`（hot path 不再使用）
- `Execute()` 改為：

```go
compiled := h.ruleRegistry.GetCompiledRules()  // atomic read, no I/O

// Pre-computed aggregate keys — 只是 slice iteration，不走 tree
for _, cr := range compiled {
    for _, k := range cr.AggregateKeys { ... }
}

// Evaluate — direct closure call，不走遞迴 tree walk
for _, cr := range compiled {
    evalCtx := ruleUsecase.NewPreloadedEvalContext(fields, cache)
    ok, err := cr.EvalFn(evalCtx)  // zero-alloc closure call
}
```

### Per-event allocation: Before vs After

| Before (per event, per rule) | After |
|---|---|
| `RuleNode` struct + children slices 逃逸到 heap | 零：closure 在 compile time 已分配 |
| `any` boxing for resolveValue/resolveRHS | 零：LHS 只做 `toFloat64` type switch |
| `strings.ToUpper(op)` — string alloc | 零：compile time 已完成 |
| `fmt.Sprintf("%v", actual/expected)` — string alloc | 僅在 string path 且無法避免時 |
| `AggregateKey.CacheKey()` — string concat | 零：compile time 已計算 |
| Redis GET / JSON unmarshal | 零：atomic pointer load |
| CollectAggregateKeys tree walk | 零：pre-computed slice |

### 要改的檔案

| 檔案 | 改什麼 |
|------|--------|
| `service/base/rule/model/compiled.go` | 新增：CompiledRule struct |
| `service/base/rule/model/interface.go` | 新增：RuleRegistryInterface |
| `service/base/rule/usecase/compiler.go` | 新增：Compile function + RuleRegistry |
| `service/base/rule/usecase/compiler_test.go` | 新增：unit test + benchmark |
| `service/base/rule/usecase/strategy.go` | 修改：invalidateCache 加 Publish |
| `service/bff/worker/init.go` | 修改：建立 RuleRegistry，傳給 EventUsecase |
| `service/bff/worker/usecase/event_usecase.go` | 修改：用 compiled rules 取代 ListActive + Evaluate |

### 驗證方式

1. **Unit tests** (`compiler_test.go`): 對每種 node type / operator / field type 驗證 `Compile(tree)(ctx) == Evaluate(tree, ctx)`
2. **Benchmark** (`compiler_test.go`): `BenchmarkCompiled` vs `BenchmarkTreeWalk`，比較 ns/op 和 **allocs/op**
3. **Escape analysis**: `go build -gcflags='-m' ./service/base/rule/usecase/` 確認 closure eval path 無非預期逃逸
4. **Integration**: `docker compose up` → API create rule → worker 透過 pub/sub 秒級 reload

---

## Phase 5: 非同步 Kafka Produce + Result Notification Worker

分兩部分：

### 5a: Worker 端改為非同步 Produce

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

### 5b: 新增 Result Notification Worker

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
- UPDATE status='completed' 的時機兩難：放在 Kafka produce 之前，produce 失敗就丟事件；放在之後，Phase 5a async produce 拿不到同步結果
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
- **Phase 5a 相容** — async produce 不影響此設計，因為 processed_events 的 completed 標記可以放在 produce 呼叫之後（async produce 只是不等 ACK，呼叫本身是同步的）
- **Phase 3 安全** — rebalance 時同一 event 可能被兩個 consumer 處理，但 UPSERT processed_events 是原子操作，attempts 計數準確；pipeline 每步冪等，重複執行不會產生錯誤結果
- 此修法應在 Phase 2 之前做，因為它影響 Execute 的流程和 error handling

---

## Phase 6: 架構重構 — Sync API + Redis Hot Path（已完成 ✅）

### 問題

Phase 1-4 優化後，PostgreSQL connection pool 仍然是吞吐量的天花板。每個事件在 hot path 上有 3 次 DB round-trip（UpsertWithBehaviorLog、BatchAggregate、MarkCompleted）。

### 架構變更

```
Before:  Client → API (202) → Kafka → Worker → [PG×3] → Kafka results
After:   Client → API (200) → [Redis only] → Response + Kafka → Worker → [PG×1]
```

- **新增 `POST /v1/events/check`** — 同步評估規則 + CEP，回傳 matched rules/patterns
- **保留 `POST /v1/events`** — 原有 async 端點，向後相容
- **Worker 簡化為 PG write-back** — 只做 `INSERT behavior_logs ON CONFLICT DO NOTHING`
- **CEP 移到 API** — CEP 已用 Redis 存 state，直接在 API 同步處理

### 實作內容

#### 6.1 Redis Behavior Event Store

**新檔案**: `service/base/behavior/repository/redis/behavior.go`

- Redis Sorted Set 儲存事件（key: `rule_engine:events:{member_id}:{behavior}`）
- Score: `occurred_at.UnixMilli()`，Member: `{event_id}\x00{json_fields}`
- `StoreEvent` — Lua script: ZADD NX + ZREMRANGEBYSCORE（清理過期）+ EXPIRE
- `BatchAggregate` — Pipeline 多個 ZRANGEBYSCORE，Go 端計算 COUNT/SUM/AVG/MAX/MIN
- `StoreAndAggregate` — 合併 StoreEvent + BatchAggregate 為單一 Pipeline（1 round-trip）
- maxWindow 由實際規則的最長時間窗口決定（`TimeWindow.Duration()` + `MaxWindowFromKeys()`）

**單元測試**: `service/base/behavior/repository/redis/behavior_test.go`（9 個測試，使用 miniredis）

#### 6.2 Rule Strategy Compilation 增強

- `CompiledRuleSet` struct — 包含 `Strategies []CompiledStrategy` + `MaxWindow time.Duration`
- `ListActiveCompiled` 回傳 `*CompiledRuleSet`，在 compile 階段預計算 maxWindow
- `TimeWindow.Duration()` — 新增方法轉換為 `time.Duration`

#### 6.3 CEP Pattern Compilation

- CEP 的 `PatternState.Condition` 在 `AddPattern` 時預編譯為 `CompiledRule` closure
- `matchState` 改為直接呼叫 compiled closure，不走 `ruleUC.Evaluate()` 遞迴
- 編譯失敗時 fallback 回 interpreted 模式

#### 6.4 共用邏輯提取

- `CollectUniqueAggregateKeys()` — 從 compiled strategies 提取去重的 aggregate keys
- `BuildAggregateConds()` — 將 aggregate keys 轉為 behavior aggregate conditions
- 以上從 worker 的 `event_usecase.go` 提取到 `service/base/rule/usecase/aggregate.go`

#### 6.5 Sync API Endpoint

- `CheckEvent` handler — `POST /v1/events/check`，同步評估規則 + CEP
- Hot path: Redis StoreAndAggregate (pipeline) → Compiled rule evaluation → CEP → Response
- Fire-and-forget goroutine: produce event to Kafka for PG write-back

#### 6.6 Worker 簡化

- `EventUsecase` 簡化為只做 `behaviorRepo.Create()`（INSERT ON CONFLICT DO NOTHING）
- 移除: `ruleStrategyUC`, `cepProcessor`, `processedEventRepo`, `producer`, `resultsTopic`

### Benchmark 結果（BenchmarkCheckEvent_Throughput_Integration，Apple M2 Pro）

測試環境：12 active compiled rules, 3 CEP patterns, real Redis + real PostgreSQL (for rule loading)

| Concurrency | events/s | latency/op |
|-------------|----------|-----------|
| 1 | 2,228 | 449 µs |
| 4 | 1,813 | 552 µs |
| 8 | 1,433 | 698 µs |
| 16 | 1,184 | 844 µs |
| 32 | 977 | 1.02 ms |

---

## Phase 7: Inverse Scaling 調查與優化（已完成 ✅）

### 問題

Phase 6 的 benchmark 顯示**反向擴展**：concurrency 從 1 增加到 32 時，throughput 從 2,228 events/s 下降到 977 events/s。

### 調查過程

#### 排除的嫌疑犯

1. **`sync.Once` lock contention** — 排除。`Once.Do()` 完成後只做 `atomic.LoadUint32`（~1ns），不拿 mutex。且 benchmark 用非 singleton 建構。
2. **Redis connection pool size** — 排除。Pool size 從 120 調到 1000，效能差異在 ±5% 誤差範圍內，不是瓶頸。

| Pool Config | 1 goroutine | 8 goroutines | 32 goroutines |
|-------------|-------------|--------------|---------------|
| default (120) | 430 events/s | 566 events/s | 531 events/s |
| pool=50 idle=10 | 375 | 530 | 489 |
| pool=100 idle=20 | 364 | 499 | 462 |
| pool=200 idle=50 | 329 | 431 | 412 |

#### 確認的根本原因

**Redis single-threaded 是物理瓶頸。** 每個 CheckEvent 產生多次 Redis command（StoreEvent Lua + ZRANGEBYSCORE pipeline + CEP SMEMBERS + MGET），Redis server 排隊處理。32 個 goroutine 同時送 command，latency 線性增加。

附帶發現：benchmark 累積資料導致 sorted set 膨脹到 11,091 筆，ZRANGEBYSCORE 掃描 + Go 端 `json.Unmarshal` × N 成為顯著開銷。

### 實施的優化

#### 7.1 In-Memory Rule Cache（`atomic.Pointer`）

**問題**：`ListActiveCompiled()` 每次呼叫都 `rdb.Get → json.Unmarshal → Compile`。

**修復**：`strategy.go` 新增 `cached atomic.Pointer[CompiledRuleSet]`。
- Hot path: `atomic.Pointer.Load()`（~1ns，zero alloc）
- Cache miss 時 lazy compile + `atomic.Store`
- `invalidateCache()` 同時清除 in-memory cache + Redis cache

#### 7.2 CEP Progress MGET 批次查詢

**問題**：`ListByMember` 做 SMEMBERS + N 個獨立 GET，N 次 Redis round-trip。

**修復**：`cep.go` 改用 SMEMBERS + MGET（2 次 round-trip）。

#### 7.3 Pipeline 合併 StoreEvent + BatchAggregate

**問題**：StoreEvent（Lua）和 BatchAggregate（Pipeline）是 2 次 Redis round-trip。

**修復**：`StoreAndAggregate` 方法將 Lua script + 所有 ZRANGEBYSCORE 放進同一個 Pipeline.Exec()（1 次 round-trip）。

### 優化後 Benchmark 結果

| Concurrency | Phase 6 (before) | Phase 7 (after) |
|-------------|-----------------|-----------------|
| 1 | 2,228 | 2,225 |
| 4 | 1,813 | 1,670 |
| 8 | 1,433 | 1,382 |
| 16 | 1,184 | 1,158 |
| 32 | 977 | 968 |

Inverse scaling 曲線基本不變。**證實瓶頸是 Redis server single-threaded 的物理限制**，不是 Go 端的鎖或 GC 問題。

### 初步結論（後來證實是錯的）

原本判斷：「單台 Redis 架構下，~2,000 events/s 到 ~1,000 events/s 是物理天花板。」

**這個結論是錯的。** 如果真是 Redis CPU 打滿，曲線應該是 **plateau**（併發 4 = 1800，併發 32 = 2000 持平），而不是 **inverse scaling**（併發 32 = 977 比併發 1 少了一半）。

### pprof 實測證據（workers-32, 15 秒 benchmark）

#### CPU Profile（1056 秒 CPU 時間）
```
encoding/json.(*decodeState).object   22.63%  ← JSON 解碼
runtime.mallocgc                       14.50%  ← 記憶體分配
runtime.pthread_cond_wait               6.71%  ← 等條件變數
runtime.scanobject + GC 相關           ~10%   ← GC scan 物件
```

#### Memory Profile（15 秒 benchmark 期間共 allocate 492 GB！）
```
encoding/json.Unmarshal                78.26%  ← 主犯
reflect.mapassign_faststr0             37.66%  ← unmarshal 填 map[string]any
StoreAndAggregate                      99.56%  ← 幾乎所有 alloc 都從這來
```

#### Block Profile（goroutine 共阻塞 12.67 小時）
```
(*baseClient)._getConn                 98.76%  ← 全部卡在「拿 Redis 連線」
Pipeline.Exec                          47.98%
Process                                50.79%
```

### 真正的根本原因

**Go 端的 GC 風暴 + 連線池阻塞的惡性循環**，不是 Redis 物理極限：

1. ZRANGEBYSCORE 回傳 N 筆 entries（benchmark 累積到幾千筆）
2. 每筆都 `json.Unmarshal` → 產生 `map[string]any` → heap allocation
3. 32 goroutine 同時做這件事 → 每秒幾十萬個 alloc → GC STW 暫停
4. GC 暫停期間，Redis 連線被 goroutine 持有但沒人在讀 → 其他 goroutine 在 `_getConn` 排隊
5. 越多 goroutine → 越多 alloc → 越多 GC STW → 越多連線等待 → **inverse scaling**

---

## Phase 8: 消除 JSON Unmarshal — Pipe-Separated Member 編碼（已完成 ✅）

### 問題

Phase 7 的 pprof 已經指出 `encoding/json.Unmarshal` 是 inverse scaling 的主因：每筆 ZRANGEBYSCORE 回來的 entry 都要解析成 `map[string]any`，32 goroutine 同時做會瘋狂產生 heap allocation，引發 GC STW 風暴，連帶讓 goroutine 都卡在 Redis 連線池上。

### 設計

把 sorted set member 從 JSON 改為 **pipe-separated** 格式：

```
Before: member = "evt-001\x00{\"amount\":5000,\"address\":\"0x...\"}"
After:  member = "evt-001|5000|10.5"
```

Member 的 slot 順序由 **per-behavior FieldSchema** 決定，schema 從 compiled rules 的 `AggregateKeys` 動態推導：

- Rule `CryptoWithdraw:SUM:amount` + `CryptoWithdraw:AVG:fee`
  → CryptoWithdraw schema: `Fields=["amount","fee"]`
- Rule `Login:COUNT` → Login schema 沒有欄位（COUNT 不需要）

讀取時用 **zero-allocation** 的 `strings.Cut` + `strconv.ParseFloat`：
```go
for _, m := range members {
    val, ok := parseFieldAt(m, pos)  // zero alloc
    if ok { sum += val }
}
```

### 實作重點

1. **`behavior/model/schema.go`** — 新增 `FieldSchema` 型別（Fields + Index）
2. **`rule/usecase/aggregate.go`** — 新增 `BuildBehaviorSchemas()` 從 compiled rules 推導
3. **`rule/model/CompiledRuleSet`** — 加 `Schemas map[BehaviorType]*FieldSchema` 欄位，在 `compileAndCache` 時一併算好
4. **`BehaviorEventStoreInterface`** — 三個方法都改為接受 `map[BehaviorType]*FieldSchema`（支援跨 behavior 聚合）
5. **`behavior/repository/redis/behavior.go`** — 重寫 `encodeMember` / `parseFieldAt` / `aggregateField`；移除 `json.Unmarshal` + `map[string]any`
6. **`CheckEvent`** — 傳 `ruleSet.Schemas` 進 `StoreAndAggregate`

**動態 schema 處理**：規則變更 → 規則重編譯 → 新 schema → 舊資料按舊 schema 存的最多撐 maxWindow 後自然過期，不需要遷移。

### Benchmark 對比（workers-32, 15 秒, 無任何 mock）

| 指標 | Phase 7 (JSON) | Phase 8 (Flatten) | 變化 |
|------|---------------|-------------------|------|
| alloc_space（bench 期間） | 492 GB | **46 GB** | **-90%** |
| `encoding/json` CPU% | 22.63% | **0%** | 完全消失 |
| `reflect.mapassign_faststr0` alloc% | 37.66% | **0%** | 完全消失 |
| CPU 主要熱點 | encoding/json + GC | `syscall.syscall` (網路 I/O) | 瓶頸轉移 |

### Throughput 對比

| Concurrency | Phase 7 | Phase 8 | 變化 |
|-------------|---------|---------|------|
| 1 | 2,225 | **3,582** | **+61%** |
| 4 | 1,670 | 1,889 | +13% |
| 8 | 1,382 | 1,331 | -4% |
| 16 | 1,158 | 1,214 | +5% |
| 32 | 968 | **1,211** | +25% |

### 分析

- **Phase 8 目標達成**：JSON 解析完全消失，GC 壓力降低 90%。
- **Inverse scaling 未完全消除**：workers-32 仍然比 workers-1 低。CPU profile 顯示新的熱點是 `syscall.syscall`（40.90%）和 `runtime.kevent`（14.72%），這代表瓶頸已經從 Go 端轉移到 **Redis client 的 TCP 通訊** / Redis server 真正的序列化處理。
- 若要繼續優化 inverse scaling，方向變成 **Redis 水平擴展**（Cluster 分 shard、讀寫分離）或**減少 Redis 依賴**（in-memory LRU cache 聚合結果對冷資料），不再是 Go 端能解決的。

---

## 實作順序與依賴

```
Bug Fix (Retry Duplicate Event)        → 無依賴，最優先
Phase 1 (DB Pool + PreparedStmt)       → 無依賴，已完成 ✅
Phase 2 (Batch Aggregation SQL)        → 依賴 Phase 1 + Bug Fix，已完成 ✅
Phase 3 (Per-Partition Goroutine)      → 依賴 Phase 1，已完成 ✅
Phase 4 (Rule Strategy Compilation)    → 依賴 Phase 3，已完成 ✅
Phase 5a (Async Produce)               → 無依賴，可隨時做
Phase 5b (Notification Worker)         → 依賴 Phase 5a
Phase 6 (Sync API + Redis Hot Path)    → 已完成 ✅（架構重構，PG 移出 hot path）
Phase 7 (Inverse Scaling 調查+優化)     → 已完成 ✅（in-memory cache, MGET, pipeline 合併）
Phase 8 (消除 JSON unmarshal)           → 已完成 ✅（pipe-separated member, alloc -90%）
```

## 累計效果

| 完成階段 | 架構 | 吞吐量 |
|---------|------|--------|
| Baseline | Worker (PG) | crash at 4+ workers |
| Phase 1 | Worker (PG) | ~2,000 events/s (4 workers) |
| Phase 1+2 | Worker (PG) | ~4,000 events/s |
| Phase 1+2+3 | Worker (PG) | ~8,000-12,000 events/s |
| Phase 4 | Worker (PG) | +18% rule eval speedup |
| **Phase 6** | **Sync API (Redis)** | **~2,200 events/s (1 concurrency), p50 ~450µs** |
| **Phase 7** | **Sync API (Redis)** | **same throughput, reduced Redis round-trips** |
| **Phase 8** | **Sync API (Redis)** | **3,582 events/s (1 concurrency), alloc -90%** |

Phase 6 的吞吐量看起來比 Phase 3 低，但本質不同：
- Phase 3 是 async worker，client 拿不到即時結果
- Phase 6 是 sync API，client 在 **450µs 內拿到完整風控結果**（matched rules + CEP patterns）

**Phase 7 inverse scaling 調查**：原本以為是 Redis 物理極限（錯誤判斷）。pprof 實測確認真兇是 Go 端 `json.Unmarshal` 引發的 GC 風暴（15 秒 benchmark 內 allocate 492 GB，JSON 解碼佔 CPU 22%），導致 goroutine 在 Redis 連線池上排隊（98.76% 的阻塞時間）。

**Phase 8 從根本解決**：把 sorted set member 從 JSON 改為 pipe-separated 格式（`"event_id|val0|val1|..."`），聚合用 `strings.Cut` + `strconv.ParseFloat` zero-alloc 解析。結果：workers-1 從 2,225 提升到 3,582 events/s（+61%），alloc_space 從 492 GB 降到 46 GB（-90%），`encoding/json` 和 `reflect.mapassign_faststr0` 在 profile 中完全消失。殘餘的 inverse scaling 來自 Redis client 的 TCP 通訊 / Redis server 序列化處理，已無法在 Go 端解決。

## 驗證方式

```bash
# 啟動 infra
docker compose up -d

# Worker write-back benchmark
go test -bench=BenchmarkWriteBack -benchtime=10s ./service/bff/worker/usecase/

# API CheckEvent benchmark（需要乾淨 Redis）
docker exec rule-engine-redis-master-1 redis-cli FLUSHDB
go test -bench=BenchmarkCheckEvent_Throughput_Integration -benchtime=10s ./service/bff/apis/usecase/

# Rule compilation benchmark
go test -bench=BenchmarkCompile_vs_Interpret ./service/base/rule/usecase/

# Redis behavior store unit tests
go test -v ./service/base/behavior/repository/redis/
```
