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

- **Message queue（NATS JetStream）** 當事件的 append-only log（source of truth）
- **Rule Engine Core**（in-memory stateful process）處理事件時所有聚合、CEP 都在 memory 完成
- **Snapshot + NATS replay** 實現秒級災難復原

### 預期效益

- 單 shard throughput 從 ~10K events/s → **100K+ events/s**
- 聚合、CEP 都變 in-memory 運算，不打 Redis
- P50 latency（純 in-memory 處理）~10 µs，含 NATS 往返約 1-2 ms

### 取捨

- 架構級重寫（不是迭代優化）
- Operational complexity 顯著提升（NATS cluster + snapshot + replay）
- API 從同步請求 → publish to NATS → 等 response（透過 request_id 關聯）

## 何時值得做

- 當 time-bucket pre-aggregation + Redis Cluster sharding 都做完後仍然不夠
- Throughput 目標 > 50K events/s per shard
- 需要強審計 / replay 能力（regulatory、production bug 重現）
- 團隊能承擔 event-sourcing 的心智模型

不滿足這些條件前，不應該做這個。

---

## 架構總覽

```
Client
  │ HTTP POST /v1/events/check (sync)
  ▼
API Gateway (stateless)
  │ 1. publish event to INPUT stream (subject = rule.events.{shard_id})
  │ 2. wait on RESPONSE inbox (subject = rule.responses.{request_id})
  ▼
NATS JetStream
  ├─ Stream: rule-events      (partitioned by shard_id, durable)
  └─ Stream: rule-responses   (partitioned by request_id, short retention)
  ▲                                       │
  │ consume (ordered per subject)         │ publish response
  │                                       │
Rule Engine Core (per shard, one instance)
  │ - in-memory state per member
  │ - single-goroutine per shard (保證 FIFO)
  │ - snapshot every N seconds → file + NATS KV
```

### 資料流

1. Client 呼叫 `POST /v1/events/check` → API 產生 `request_id`
2. API 把 event 包成 NATS message publish 到 `rule.events.{shard_id}`（按 member_id 決定 shard）
3. API 開啟 inbox subscribe `rule.responses.{request_id}`
4. Rule Engine Core 按順序消費 `rule.events.{shard_id}`，in-memory 處理（規則評估、CEP、聚合更新）
5. Core publish response 到 `rule.responses.{request_id}`
6. API 收到 response → HTTP 回給 client

---

## 核心元件設計

### 1. Shard 拓樸

按 `member_id` hash 到 N 個 shard。每個 shard 一個 Rule Engine Core instance。

```go
shardID := crc32(memberID) % numShards
natsSubject := fmt.Sprintf("rule.events.%d", shardID)
```

**初始**：3 shards，每個約負責 33 萬 member（總 100 萬）。
**擴充**：加 shard 要 redistribute member state（參考 Redis Cluster resharding 的概念）。

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
    patternsByBehavior map[behaviorModel.BehaviorType][]*compiledPattern

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
    Count    uint64
    Sums     map[string]float64  // field → sum
    Maxs     map[string]float64  // field → max
    Mins     map[string]float64  // field → min
    // 冪等重播用：已納入此 bucket 的 event_id
    ProcessedEventIDs map[string]struct{}
}
```

### 3. 事件處理 loop

```go
func (c *Core) Run(ctx context.Context) error {
    msgs, err := c.consumer.Messages()
    defer msgs.Stop()

    for {
        msg, err := msgs.Next()
        if err != nil { return err }

        event, err := decodeEvent(msg.Data())
        if err != nil {
            msg.Nak()  // 或送 DLQ
            continue
        }

        resp := c.processEvent(event)

        if !c.replaying.Load() {
            c.publishResponse(resp)
        }

        msg.Ack()
        meta, _ := msg.Metadata()
        c.lastSeq.Store(meta.Sequence.Stream)
    }
}

func (c *Core) processEvent(event Event) Response {
    ms := c.state.getOrCreateMember(event.MemberID)

    // 1. Update aggregations (in-memory)
    c.updateAggregations(ms, event)

    // 2. Evaluate rules (in-memory compiled closures, 沿用 Phase 4)
    matchedRules := c.evaluateRules(ms, event)

    // 3. Process CEP (in-memory, 不再打 Redis)
    matchedPatterns := c.processCEP(ms, event)

    ms.LastSeenAt = event.OccurredAt

    return Response{
        RequestID:       event.RequestID,
        EventID:         event.EventID,
        MatchedRules:    matchedRules,
        MatchedPatterns: matchedPatterns,
    }
}
```

### 4. Aggregation update（取代 Redis sorted set + ZRANGEBYSCORE）

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

    // 清理過期 bucket（超過 maxWindow）
    cutoff := event.OccurredAt.Add(-maxWindow).Truncate(bucketSize).Unix()
    for ts := range agg.Buckets {
        if ts < cutoff { delete(agg.Buckets, ts) }
    }
}
```

### 5. Rule evaluation（沿用現有 compiled rules）

CompiledRuleSet 從 DB 載入，Pub/Sub 通知各 shard reload。

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

// 從 in-memory buckets 計算指定 window 的 SUM/COUNT/AVG/MAX/MIN
func (c *Core) computeBucketAggregation(ms *MemberState, key AggregateKey, now time.Time) float64 {
    agg := ms.Aggregations[key.Behavior]
    if agg == nil { return 0 }

    since := now.Add(-key.Window).Truncate(bucketSize).Unix()
    var count uint64
    var sum, maxv, minv float64
    found := false

    for ts, bucket := range agg.Buckets {
        if ts < since { continue }
        count += bucket.Count
        if key.Field == "" { continue }  // COUNT only
        sum += bucket.Sums[key.Field]
        if !found || bucket.Maxs[key.Field] > maxv { maxv = bucket.Maxs[key.Field] }
        if !found || bucket.Mins[key.Field] < minv { minv = bucket.Mins[key.Field] }
        found = true
    }

    switch key.Aggregation {
    case "COUNT": return float64(count)
    case "SUM":   return sum
    case "AVG":   if count == 0 { return 0 }; return sum / float64(count)
    case "MAX":   return maxv
    case "MIN":   return minv
    }
    return 0
}
```

### 6. CEP（in-memory state）

CEP 的 `PatternProgress` 直接掛在 `MemberState.Progresses`。沒有 Redis 呼叫。

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

    // 2. 嘗試啟動新 progress（用 behavior pre-index 優化：只看 state[0] 符合當前 event behavior 的 pattern）
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

### 寫入流程（streaming，避免一次性建整個 message）

```go
// 1. Open snapshot.tmp
f, _ := os.Create(snapshotPath + ".tmp")
w := bufio.NewWriter(f)

// 2. 先寫 header
header := &SnapshotHeader{ShardId: int32(c.shardID), LastNatsSeq: c.lastSeq.Load(), ...}
writeLengthPrefixed(w, header)

// 3. 逐個 member 寫（length-prefixed frames）
for _, ms := range c.state.members {
    pb := toProto(ms)
    writeLengthPrefixed(w, pb)
}

// 4. fsync + close
w.Flush()
f.Sync()
f.Close()

// 5. Atomic rename
os.Rename(snapshotPath+".tmp", snapshotPath)

// 6. 最後一步：更新 NATS KV 的 last_seq
c.kv.Put(ctx, fmt.Sprintf("shard_%d_last_seq", c.shardID), c.lastSeq.Load())
```

**為什麼先寫檔再寫 KV**：
- 若 step 5 完成但 step 6 crash → 下次啟動用舊的 `last_seq`，會 replay 一小段已經在 snapshot 裡的 events → **冪等處理**不會出錯
- 若 step 5 crash → snapshot 還是舊版，last_seq 也還是舊的 → 完全一致

### Snapshot 載入

```go
func (c *Core) loadSnapshot() error {
    f, err := os.Open(snapshotPath)
    if err != nil { return nil }  // cold start, no snapshot
    defer f.Close()

    r := bufio.NewReader(f)

    // 1. Read header
    var header SnapshotHeader
    readLengthPrefixed(r, &header)

    if header.SchemaVersion != currentSchemaVersion {
        return fmt.Errorf("schema mismatch, need migration")
    }

    c.lastSeq.Store(header.LastNatsSeq)

    // 2. Stream decode members
    for {
        var ms MemberStatePB
        err := readLengthPrefixed(r, &ms)
        if err == io.EOF { break }
        if err != nil { return err }
        c.state.members[ms.MemberId] = fromProto(&ms)
    }

    return nil
}
```

### Replay

```go
func (c *Core) startReplay(ctx context.Context) error {
    c.replaying.Store(true)
    defer c.replaying.Store(false)

    // 從 NATS KV 讀 last_seq（可能比 snapshot header 的新，取較大者為準）
    entry, _ := c.kv.Get(ctx, fmt.Sprintf("shard_%d_last_seq", c.shardID))
    kvSeq := decodeUint64(entry.Value())
    startSeq := max(c.lastSeq.Load(), kvSeq) + 1

    consumer, _ := c.js.CreateConsumer(ctx, ...,
        jetstream.ConsumerConfig{
            DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
            OptStartSeq:   startSeq,
        })

    // 消費直到「追上」
    caughtUp := false
    for !caughtUp {
        msg, _ := consumer.Next()
        event, _ := decodeEvent(msg.Data())
        c.processEvent(event)  // side effect 壓制著（replaying=true）
        msg.Ack()

        meta, _ := msg.Metadata()
        c.lastSeq.Store(meta.Sequence.Stream)

        // 如果沒更新的 msg（NumPending=0），視為追上
        if meta.NumPending == 0 { caughtUp = true }
    }

    slog.Info("replay complete", "shard", c.shardID, "events", c.lastSeq.Load()-startSeq+1)
    return nil
}
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

Replay 可能重播一段已經在 snapshot 裡的 events（snapshot 寫完但 KV 還沒更新就 crash）。所有 state 更新**必須冪等**。

| 操作 | 冪等策略 |
|------|---------|
| Aggregation update | `BucketData.ProcessedEventIDs` set，加入前先檢查 |
| CEP Progress advance | 已有 `ProcessedEvents []string` 欄位做 event_id dedup ✓ |
| 新 Progress 建立 | `(pattern_id, member_id, start_event_id)` 為 dedup key |

`ProcessedEventIDs` 空間估算：
- 10K events/s × 60s（一個 bucket size）= 60 萬 events
- 60 萬 × ~40 bytes event_id = **~24 MB per shard**
- 隨 bucket 過期自動清除，不會膨脹

---

## Side Effect 壓制

Replay 期間**絕對不能**做這些：
- Publish response 到 `rule.responses.{request_id}`（client 早就 timeout）
- 寫入外部 system（audit log、notifications、webhook）
- 發 Kafka / Email / SMS

```go
if !c.replaying.Load() {
    c.publishResponse(resp)
    c.emitAuditLog(resp)
}
```

---

## 時間處理的陷阱

**絕對不能用 `time.Now()`** 做 window 判斷，一律用 `event.OccurredAt`。

```go
// ❌ 錯（replay 時 wall clock 是現在，event 是過去）
if time.Now().Sub(progress.StartedAt) > maxWait { ... }

// ✅ 對
if event.OccurredAt.Sub(progress.StartedAt) > maxWait { ... }
```

Snapshot 寫入時機可以用 `time.Now()`（不影響業務邏輯）。

---

## Sharding 考量

### 靜態 shard 數（v1）

第一階段：shard 數**固定**（例如 4），不支援 dynamic resharding。
- 加 shard 需要停機：舊 shard state 倒到 NATS、新拓樸啟動、replay
- 大約 10 分鐘 downtime（取決於 state 大小）

### 動態 resharding（v2）

參考 Kafka partition reassignment / Redis Cluster resharding：
- 計算新 slot ownership
- 從 source shard 匯出特定 member state
- 新 shard 從 NATS 特定 seq 開始 replay
- 期間該 member 的請求走 "MOVED redirect" 到新 shard

太複雜，**第一版不做**。

---

## 跟現有架構的比較

| 面向 | 現況（Phase 8） | In-Memory Rule Engine |
|------|---------------|-------------------|
| 單 shard throughput | ~10K events/s | **100K+** events/s |
| P50 latency | ~500 µs | ~1-2 ms（多 NATS 往返） |
| State 儲存 | Redis | In-memory + NATS log |
| CEP 成本 | SMEMBERS + MGET per event | 純 memory |
| Aggregation 成本 | ZRANGEBYSCORE + parse | O(window/bucket_size) memory ops |
| Restart 時間 | 即時（stateless）| ~3 秒（含 replay） |
| Cross-instance 一致性 | Redis 是 source of truth | NATS log 是 source of truth |
| 程式模型 | Request/Response 同步 | Async（透過 NATS 關聯 request_id）|
| Operational 複雜度 | Redis + API | Redis + NATS + Rule Engine Core + snapshot + replay |

---

## Implementation Phases

### Phase 1: 單 shard 原型（2 週）

目標：單 shard Rule Engine Core 跑通到可測試狀態。

交付：
- [ ] `service/engine/core/` 新 package
- [ ] `cmd/rule-engine-core/main.go` entry point
- [ ] Protobuf schema + 程式碼生成
- [ ] 單 shard 的 processEvent 邏輯（aggregation + rule + CEP 都 in-memory）
- [ ] NATS JetStream 接入（input stream + response stream + KV store）
- [ ] 簡易 snapshot dump + load
- [ ] API 端改為 publish/subscribe 模式，`request_id` 關聯 HTTP response

Benchmark 目標：單 shard > 50K events/s，latency < 2ms。

### Phase 2: Snapshot + Replay（1 週）

交付：
- [ ] Per-shard snapshot 檔案、atomic rename、NATS KV 寫 last_seq
- [ ] Replay 時壓制 side effect
- [ ] 冪等重播的 event_id dedup 機制
- [ ] 災難測試：`kill -9` + 重啟，驗證 state 一致
- [ ] Restart time benchmark（目標 < 3s）

### Phase 3: Multi-shard（1 週）

交付：
- [ ] API 端按 memberID hash 決定 shard
- [ ] NATS subject partition `rule.events.{shard_id}`
- [ ] 每 shard 獨立 Core instance
- [ ] Per-shard metrics（throughput、lag、memory）

### Phase 4: Production hardening（2 週）

交付：
- [ ] Graceful shutdown（flush in-flight events、寫 snapshot、fsync）
- [ ] Monitoring / alerting（memory 使用率、NATS lag、snapshot 失敗率）
- [ ] Schema versioning 機制
- [ ] Runbook（擴 shard、replay 驗證、snapshot corruption 恢復）
- [ ] Load test：連續 24h 跑，確認 memory 不漏、snapshot 週期正常

### 總預估：**6 週**（1 位資深 Go engineer）

---

## Risks / Open Questions

### 1. NATS JetStream 單機 throughput 限制

NATS JetStream 單一 stream 的 publish rate 大約 **10K-100K msgs/s**（取決於硬體 / replication factor / message size）。如果我們的 throughput 超過，要用多個 stream（per shard 一個 stream）平行推進。

**待驗證**：NATS JetStream 在我們 message size（~1KB）下的實際上限。

### 2. HTTP request/response correlation 成本

每個 HTTP request 要：
1. 生成 UUID request_id
2. 在 API 開 ephemeral subscribe on `rule.responses.{request_id}`
3. Publish event
4. 等 response or timeout
5. Unsubscribe

Ephemeral subscribe 在 NATS 是便宜的（不持久），但每個 request 開/關的成本需要實測。

**替代方案**：API 用持久 consumer 收 `rule.responses.*`，內部 channel map 派發給等待的 goroutine。

### 3. 單 shard single-thread 的 CPU 成本

單 goroutine 處理所有事件保證 FIFO，但 CPU 使用率有上限。若每個 event 處理花 10µs → 上限 100K events/s。

若不夠：把 shard 拆更細（例如從 4 個拆到 32 個），或每 shard 用「worker pool 但保證 per-member ordering」（複雜）。

### 4. 記憶體膨脹

- Member 數量爆炸（例如殭屍 bot 創千萬 member_id）→ memory OOM
- 防禦：LRU eviction + 最小 active threshold（例如只保留最近 7 天有事件的 member）
- 被 evict 的 member 再次有事件時，從 NATS replay 該 member 歷史重建

### 5. Snapshot 寫入期間的停頓

Dump 1.3 GB 到 disk 要 2-3 秒。如果是 in-band 做，會阻塞事件處理 2-3 秒。

**解法**：copy-on-write snapshot
- 觸發 snapshot：fork state（Go 沒真 fork，用 deep copy 或 persistent data structure）
- 原 state 繼續處理事件
- Snapshot goroutine 慢慢寫

或更簡單：事件處理期間定期 yield，允許 snapshot 分批序列化。

### 6. Schema Evolution

加欄位 OK（Protobuf backward compatible），但：
- 改欄位語意（例如 bucket 從 1 分鐘改 5 分鐘）→ 需要 migration
- 破壞性變更 → 版本號 + 兩版並存一段時間 → 切換

每個 snapshot 檔頂有 `schema_version`，啟動時檢查。

---

## Migration from Current Architecture

### Phase A: 並行跑（1 個月）

1. Rule Engine Core 架構起來但**不正式服務**
2. API 收到 CheckEvent 後：
   - 走現有 Redis 邏輯（正式服務 client）
   - **同時** publish 到 NATS（shadow traffic）
3. Core 處理 shadow traffic，把結果跟 Redis 結果比對
4. 發現不一致 → debug、修
5. 一致率 > 99.99% 維持 1 週 → 準備切換

### Phase B: 切換（Cutover）

1. Flag gate：`use_in_memory_engine: true`
2. API 走 in-memory engine 為 primary、Redis 為 fallback
3. 觀察 1 週
4. 移除 Redis primary path

### Phase C: 清理（1 週）

- 移除 Redis 上的 `rule_engine:events:*`、`rule_engine:progress:*`、`rule_engine:member:*` keys
- 保留 `rule_engine:active_rules`（規則 config 快取）
- Worker 移除（已經不做 write-back）
- Docker compose 加 NATS service

---

## 驗證方式

### 正確性
- Unit test：單 event 輸入 → 預期 aggregation/CEP 輸出
- 確定性測試：同一個 event stream replay 兩次，state 完全一致
- Shadow traffic 比對：in-memory 結果 vs Redis 結果（驗證邏輯等價）

### 效能
- Throughput benchmark：單 shard 壓 100K events/s 持續 1 小時
- Latency benchmark：p50/p99/p999 < 2ms / 5ms / 20ms
- Restart benchmark：`kill -9` → 起來 → 處理第一個新 event 的時間 < 3s
- Memory benchmark：100 萬 member × 7 天資料，memory < 10 GB

### 混沌測試
- Kill Core during snapshot
- Kill Core during replay
- NATS server restart 期間的事件丟失率
- Snapshot file 損壞的 fallback（自動用更舊的 snapshot）
