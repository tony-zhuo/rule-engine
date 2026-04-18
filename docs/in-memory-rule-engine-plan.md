# In-Memory Rule Engine Architecture Plan

**狀態**：Deferred — 計畫完整，尚未實作。

## Context

我們這個專案是 **rule engine**（風控 / 行為規則評估系統），不是交易撮合系統。此計畫只是**借用**交易所撮合引擎的架構 pattern（event sourcing + in-memory state machine + snapshot replay），套用到 rule engine 的 hot path 上。

### 問題

目前 rule engine 的 CheckEvent hot path 完全依賴 Redis，雖然已經透過 pipe-separated encoding（Phase 8）降低了 JSON GC 壓力，但**每次事件都要打 Redis 拿歷史資料**這件事是吞吐量的根本限制。

- Workers-1 on native Redis: ~12K events/s（fresh data）
- 隨 sorted set 累積降到 ~5K events/s
- 高併發（workers-32）受 TCP I/O 競爭限制到 ~1.6K events/s

### 目標

把 rule engine 的 hot path 改為 **event-sourced in-memory state machine**：

- **NATS JetStream** 當事件的 append-only log（source of truth）
- **Rule Engine Core**（in-memory stateful consumer）處理事件時所有聚合、CEP 都在 memory 完成
- **Snapshot + NATS replay** 實現秒級災難復原

### 預期效益

- 單 shard throughput **100K+ events/s**（pure in-memory）
- 聚合、CEP 都變 in-memory 運算，不打 Redis
- 處理延遲 ~10 µs per event（不含上游到 NATS 的網路）

### 取捨

- 架構級重寫（不是迭代優化）
- Operational complexity 增加（NATS cluster + snapshot + replay）
- 不提供 HTTP API 同步回應，是純 consumer 架構

## 何時值得做

- Throughput 目標 > 50K events/s per shard
- 需要強審計 / replay 能力（regulatory、production bug 重現）
- 團隊能承擔 event-sourcing 的心智模型

不滿足這些條件前，不應該做這個。

---

## 架構總覽

```
Event Source（上游系統 / API gateway / data pipeline）
  │ publish
  ▼
NATS JetStream
  Stream: rule-events (partitioned by shard_id, durable)
  │ consume (ordered per subject)
  ▼
Rule Engine Core (per shard, one instance)
  - in-memory state per member
  - single-goroutine per shard (保證 FIFO)
  - snapshot every N seconds → file + NATS KV
  - 評估規則、更新聚合、CEP state 全在 memory
  │
  └─ (Deferred) result output: publish to result stream / webhook / DB
```

### 資料流

1. 上游系統 publish event 到 `rule.events.{shard_id}`（按 member_id 決定 shard）
2. Rule Engine Core 按順序消費，in-memory 處理（規則評估、CEP、聚合更新）
3. 結果暫時不需要輸出（deferred），未來可以 publish 到 result stream 或 callback

### 跟現有 Kafka worker 的差別

| 項目 | 現有 Kafka Worker | In-Memory Rule Engine Core |
|------|-----------------|--------------------------|
| MQ | Kafka | NATS JetStream |
| 狀態儲存 | Redis（每個 event 打 Redis）| **In-memory**（零外部 I/O）|
| 災難復原 | Redis 自帶 persistence | Snapshot + NATS replay |
| Throughput | ~10K events/s | **100K+ events/s** |
| 複雜度 | 低 | 中（snapshot + replay + shard） |

---

## 核心元件設計

### 1. Shard 拓樸

按 `member_id` hash 到 N 個 shard。每個 shard 一個 Rule Engine Core instance。

```go
shardID := crc32(memberID) % numShards
natsSubject := fmt.Sprintf("rule.events.%d", shardID)
```

**初始**：3 shards，每個約負責 33 萬 member（總 100 萬）。
**擴充**：加 shard 需要 redistribute member state（同 Redis Cluster resharding 的概念）。

### 2. Rule Engine Core 內部結構

```go
package engine

type Core struct {
    shardID   int
    state     *ShardState
    replaying atomic.Bool      // replay 期間 true，壓制 side effect
    lastSeq   atomic.Uint64    // 已處理的最後一個 NATS sequence

    // Rule config
    ruleCache atomic.Pointer[CompiledRuleSet]  // 沿用現有 Phase 7 機制
    patternsByBehavior map[BehaviorType][]*compiledPattern  // CEP behavior pre-index

    // NATS
    js       jetstream.JetStream
    consumer jetstream.Consumer
    kv       jetstream.KeyValue  // 存 last_seq、snapshot metadata

    // Snapshot
    snapshotDir      string
    snapshotInterval time.Duration  // default 60s
}

type ShardState struct {
    members map[string]*MemberState  // memberID → state
}

type MemberState struct {
    MemberID     string
    Aggregations map[BehaviorType]*BehaviorAgg  // per-behavior time-bucket aggregations
    Progresses   map[string]*CEPProgress        // per-progress-id CEP state
    LastSeenAt   time.Time                      // LRU eviction reference
}

type BehaviorAgg struct {
    Buckets map[int64]*BucketData  // bucket_ts (unix secs, aligned) → data
}

type BucketData struct {
    Count             uint64
    Sums              map[string]float64  // field → sum
    Maxs              map[string]float64  // field → max
    Mins              map[string]float64  // field → min
    ProcessedEventIDs map[string]struct{} // 冪等重播用
}
```

### 3. 事件處理 loop

```go
func (c *Core) Run(ctx context.Context) error {
    // 1. Load snapshot (if exists)
    c.loadSnapshot()

    // 2. Replay from last_seq + 1
    c.replay(ctx)

    // 3. Start periodic snapshot goroutine
    go c.snapshotLoop(ctx)

    // 4. Start consuming live events
    msgs, _ := c.consumer.Messages()
    defer msgs.Stop()

    for {
        msg, err := msgs.Next()
        if err != nil { return err }

        event := decodeEvent(msg.Data())
        c.processEvent(event)

        msg.Ack()
        meta, _ := msg.Metadata()
        c.lastSeq.Store(meta.Sequence.Stream)
    }
}

func (c *Core) processEvent(event Event) {
    ms := c.state.getOrCreateMember(event.MemberID)

    // 1. Update aggregations (in-memory)
    c.updateAggregations(ms, event)

    // 2. Evaluate rules (in-memory compiled closures)
    matchedRules := c.evaluateRules(ms, event)

    // 3. Process CEP (in-memory state)
    matchedPatterns := c.processCEP(ms, event)

    ms.LastSeenAt = event.OccurredAt

    // (Deferred) 結果輸出：未來可以在這裡 publish matched results
    _ = matchedRules
    _ = matchedPatterns
}
```

### 4. Aggregation update（取代 Redis sorted set）

```go
func (c *Core) updateAggregations(ms *MemberState, event Event) {
    agg, ok := ms.Aggregations[event.Behavior]
    if !ok {
        agg = &BehaviorAgg{Buckets: make(map[int64]*BucketData)}
        ms.Aggregations[event.Behavior] = agg
    }

    bucketTs := event.OccurredAt.Truncate(bucketSize).Unix()
    bucket, ok := agg.Buckets[bucketTs]
    if !ok {
        bucket = newBucketData()
        agg.Buckets[bucketTs] = bucket
    }

    // 冪等：同 event_id 只算一次（replay safety）
    if _, dup := bucket.ProcessedEventIDs[event.EventID]; dup {
        return
    }
    bucket.ProcessedEventIDs[event.EventID] = struct{}{}

    bucket.Count++
    for field, value := range event.NumericFields {
        bucket.Sums[field] += value
        if value > bucket.Maxs[field] { bucket.Maxs[field] = value }
        if !isSet(bucket.Mins[field]) || value < bucket.Mins[field] {
            bucket.Mins[field] = value
        }
    }

    // 清理過期 bucket
    cutoff := event.OccurredAt.Add(-maxWindow).Truncate(bucketSize).Unix()
    for ts := range agg.Buckets {
        if ts < cutoff { delete(agg.Buckets, ts) }
    }
}
```

### 5. Rule evaluation（沿用現有 compiled rules）

```go
func (c *Core) evaluateRules(ms *MemberState, event Event) []MatchedRule {
    ruleSet := c.ruleCache.Load()

    // 從 in-memory buckets 組 aggregate cache（取代 Redis BatchAggregate）
    aggCache := make(map[string]any)
    for _, k := range ruleSet.AllKeys {
        aggCache[k.CacheKey()] = c.computeBucketAggregation(ms, k, event.OccurredAt)
    }

    fields := buildFieldsMap(event)
    var matched []MatchedRule
    for _, cs := range ruleSet.Strategies {
        evalCtx := ruleUsecase.NewPreloadedEvalContext(fields, aggCache)
        if ok, _ := cs.Eval(evalCtx); ok {
            matched = append(matched, MatchedRule{ID: cs.ID, Name: cs.Name})
        }
    }
    return matched
}
```

### 6. CEP（in-memory，用 behavior pre-index 優化）

```go
func (c *Core) processCEP(ms *MemberState, event Event) []MatchedPattern {
    var results []MatchedPattern

    // 1. 推進 in-progress
    for id, progress := range ms.Progresses {
        result := c.advanceProgress(ms, event, progress)
        if result.Completed {
            delete(ms.Progresses, id)
            results = append(results, result.Matched)
        }
    }

    // 2. 只看 state[0] 符合當前 event behavior 的 patterns（pre-index 優化）
    for _, pattern := range c.patternsByBehavior[event.Behavior] {
        if c.matchState(event, pattern.CompiledStates[0]) {
            ms.Progresses[uuid.NewString()] = newProgress(pattern, event)
        }
    }

    return results
}
```

---

## Snapshot + Replay 機制

### Snapshot 格式

用 **Protobuf** + **atomic rename**。按 shard 各自獨立。

```protobuf
// snapshot.proto
syntax = "proto3";

message SnapshotHeader {
    int32 shard_id = 1;
    uint64 last_nats_seq = 2;
    int64 snapshot_ts = 3;
    int32 schema_version = 4;
}

message MemberStatePB {
    string member_id = 1;
    repeated BehaviorAggPB aggregations = 2;
    repeated CEPProgressPB progresses = 3;
    int64 last_seen_at = 4;
}

message BehaviorAggPB {
    string behavior = 1;
    repeated BucketPB buckets = 2;
}

message BucketPB {
    int64 ts = 1;
    uint64 count = 2;
    map<string, double> sums = 3;
    map<string, double> maxs = 4;
    map<string, double> mins = 5;
    repeated string processed_event_ids = 6;
}
```

### 寫入流程

```go
// 1. Dump to .tmp file (streaming, length-prefixed frames)
f, _ := os.Create(snapshotPath + ".tmp")
w := bufio.NewWriter(f)
writeLengthPrefixed(w, &SnapshotHeader{...})
for _, ms := range c.state.members {
    writeLengthPrefixed(w, toProto(ms))
}
w.Flush(); f.Sync(); f.Close()

// 2. Atomic rename
os.Rename(snapshotPath+".tmp", snapshotPath)

// 3. 最後一步：更新 NATS KV（順序很重要，確保 crash safety）
c.kv.Put(ctx, fmt.Sprintf("shard_%d_last_seq", c.shardID), lastSeq)
```

### Restart 流程

```
啟動流程：
1. 載入 snapshot.bin（Protobuf 格式）→ 重建記憶體狀態
2. 從 NATS KV 讀取上次確認的 SeqID
3. 建立 Consumer，從 last_seq + 1 開始重播
4. 重播期間不對外發布結果（避免重複推送）
5. 追上後（NumPending=0），切換到 live 模式
```

### Restart Time Budget

目標：**100 萬 member、4 shards、每 60 秒 snapshot → restart < 3 秒**

```
Phase          | 時間         | 備註
────────────────────────────────────────────────────
Load snapshot  | ~1 s         | 1.3 GB per shard, protobuf streaming decode, 4 shards 並行
NATS replay    | ~0.75 s      | 600K events / 4 shards = 150K per shard, 重播速度 ~200K/s
Setup consumer | ~0.2 s       | NATS connection, consumer create
────────────────────────────────────────────────────
Total          | ~2 s ✓
```

---

## 冪等性保證

Replay 可能重播一段已經在 snapshot 裡的 events。所有 state 更新**必須冪等**。

| 操作 | 冪等策略 |
|------|---------|
| Aggregation update | `BucketData.ProcessedEventIDs` set，加入前先檢查 |
| CEP Progress advance | 已有 `ProcessedEvents []string` 欄位做 event_id dedup ✓ |
| 新 Progress 建立 | `(pattern_id, member_id, start_event_id)` 為 dedup key |

---

## Side Effect 壓制

Replay 期間**絕對不能**做：
- 寫入外部 system（audit log、notifications、webhook）
- 發 Kafka / Email / SMS

```go
if !c.replaying.Load() {
    c.emitResult(resp)     // 未來的 result output
    c.emitAuditLog(resp)
}
```

---

## 時間處理的陷阱

**絕對不能用 `time.Now()`** 做 window / 過期判斷，一律用 `event.OccurredAt`。

```go
// ❌ 錯（replay 時 wall clock 是現在，event 是過去）
if time.Now().Sub(progress.StartedAt) > maxWait { ... }

// ✅ 對
if event.OccurredAt.Sub(progress.StartedAt) > maxWait { ... }
```

---

## Sharding 考量

### 靜態 shard 數（v1）

第一階段：shard 數**固定**（例如 4），不支援 dynamic resharding。
- 加 shard 需要停機重新分配 member
- 大約 10 分鐘 downtime

### 動態 resharding（v2）

太複雜，**第一版不做**。

---

## 跟現有架構的比較

| 面向 | 現況（Phase 8 + Redis） | In-Memory Rule Engine |
|------|---------------------|-------------------|
| 單 shard throughput | ~10K events/s | **100K+** events/s |
| State 儲存 | Redis | In-memory + NATS log |
| CEP 成本 | SMEMBERS + MGET per event | 純 memory |
| Aggregation 成本 | ZRANGEBYSCORE + parse | In-memory bucket ops |
| 外部 I/O per event | 2-3 Redis round-trips | **0** |
| Restart 時間 | 即時（stateless）| ~2 s（snapshot + replay） |
| Source of truth | Redis | NATS JetStream log |
| 程式模型 | Sync API | Async consumer |
| Operational 複雜度 | Redis + API | NATS + snapshot + replay |

---

## Implementation Phases

### Phase 1: 單 shard 原型（2 週）

目標：單 shard Rule Engine Core 跑通到可 benchmark 狀態。

交付：
- [ ] `service/engine/core/` 新 package
- [ ] `cmd/rule-engine-core/main.go` entry point
- [ ] Protobuf schema + 程式碼生成
- [ ] 單 shard processEvent 邏輯（aggregation + rule eval + CEP 都 in-memory）
- [ ] NATS JetStream consumer 接入
- [ ] 簡易 snapshot dump + load

Benchmark 目標：單 shard > 50K events/s。

### Phase 2: Snapshot + Replay（1 週）

交付：
- [ ] Per-shard snapshot 檔案、atomic rename、NATS KV 寫 last_seq
- [ ] Replay 時壓制 side effect
- [ ] 冪等重播的 event_id dedup 機制
- [ ] 災難測試：`kill -9` + 重啟，驗證 state 一致
- [ ] Restart time benchmark（目標 < 3s）

### Phase 3: Multi-shard（1 週）

交付：
- [ ] NATS subject partition `rule.events.{shard_id}`
- [ ] 每 shard 獨立 Core instance（同 process 內多 goroutine 或分散部署）
- [ ] Per-shard metrics（throughput、lag、memory）

### Phase 4: Production hardening（2 週）

交付：
- [ ] Graceful shutdown（flush in-flight events、寫 snapshot、fsync）
- [ ] Monitoring / alerting（memory 使用率、NATS lag、snapshot 失敗率）
- [ ] Schema versioning 機制
- [ ] Load test：連續 24h 跑，確認 memory 不漏、snapshot 週期正常

### 總預估：**6 週**（1 位資深 Go engineer）

---

## Risks / Open Questions

### 1. NATS JetStream throughput 限制

單一 stream 的 publish rate 大約 **10K-100K msgs/s**（取決於硬體 / replication / message size）。如果超過，用 per-shard 獨立 stream 平行推進。

### 2. 單 shard single-goroutine 的 CPU 上限

每 event 處理花 10µs → 上限 100K events/s。
若不夠：把 shard 拆更細（4 → 32），或每 shard 用「worker pool 但保證 per-member ordering」。

### 3. 記憶體膨脹

Member 數量爆炸 → OOM。
防禦：LRU eviction + 最小 active threshold（只保留最近 7 天有事件的 member）。
被 evict 的 member 再次有事件時，從 NATS replay 該 member 歷史重建。

### 4. Snapshot 寫入停頓

Dump 1.3 GB 到 disk 要 2-3 秒，in-band 做會阻塞事件處理。
解法：copy-on-write（deep copy state → snapshot goroutine 寫，main goroutine 繼續處理）。

### 5. Schema Evolution

加欄位 OK（Protobuf backward compatible）。
破壞性變更 → snapshot header 裡有 `schema_version`，啟動時檢查。

---

## Result Output（Deferred）

目前暫不實作結果輸出。未來需要時的選項：

| 方式 | 適用情境 | 備註 |
|------|---------|------|
| Publish to NATS result stream | 下游 consumer 需要即時收到 matched rules/patterns | 最自然的 event-driven 做法 |
| Webhook callback | 通知外部系統（例如 block 帳戶、凍結交易）| 需要 retry + DLQ |
| 寫入 DB（PostgreSQL）| Audit log、報表查詢 | 類似現有 Worker write-back |
| API query on demand | Client 主動查某 member 的最新風控結果 | 從 in-memory state 直接讀 |

---

## Migration from Current Architecture

### Phase A: 並行跑（1 個月）

1. Rule Engine Core 架構起來但**不正式服務**
2. 現有 API + Redis 繼續跑
3. 同時把 event shadow copy 到 NATS
4. Core 處理 shadow traffic，比對結果跟 Redis 版本是否一致
5. 一致率 > 99.99% 維持 1 週 → 準備切換

### Phase B: 切換

1. 上游把 event source 從 API 改為 NATS
2. Core 成為 primary
3. 觀察 1 週

### Phase C: 清理

- 移除 Redis 上的 `rule_engine:events:*`、`rule_engine:progress:*`、`rule_engine:member:*`
- 保留 `rule_engine:active_rules`（規則 config 快取）或改從 DB 直接 load
- 移除舊 Worker 和 API 的 CheckEvent endpoint

---

## 驗證方式

### 正確性
- Unit test：單 event 輸入 → 預期 aggregation/CEP 輸出
- 確定性測試：同一個 event stream replay 兩次，state 完全一致
- Shadow traffic 比對：in-memory 結果 vs Redis 結果

### 效能
- Throughput benchmark：單 shard 壓 100K events/s 持續 1 小時
- Restart benchmark：`kill -9` → 起來 → 處理第一個新 event 的時間 < 3s
- Memory benchmark：100 萬 member × 7 天資料，memory < 10 GB

### 混沌測試
- Kill Core during snapshot
- Kill Core during replay
- NATS server restart 期間的事件丟失率
- Snapshot file 損壞的 fallback（自動用更舊的 snapshot）
