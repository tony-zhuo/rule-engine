# In-Memory Rule Engine Architecture Plan

**狀態**：Deferred — 計畫完整，尚未實作。

**相關文件**：
- [in-memory-rule-engine-references.md](./in-memory-rule-engine-references.md) — 借鏡產品清單、同規模真實系統、推薦研究資料

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

- 聚合、CEP 都變 in-memory 運算，不打 Redis（單 event 零外部 I/O）
- 處理延遲 ~10 µs per event（不含上游到 NATS 的網路）
- Throughput 目標：見 §Benchmark Roadmap（探索性 milestone，非業務硬指標）

### 取捨

- 架構級重寫（不是迭代優化）
- Operational complexity 增加（NATS cluster + snapshot + replay）
- 不提供 HTTP API 同步回應，是純 consumer 架構

## 何時值得做

- Throughput 目標 > 50K events/s per shard
- 需要強審計 / replay 能力（regulatory、production bug 重現）
- 團隊能承擔 event-sourcing 的心智模型
- 不能用 Flink / Spark Structured Streaming（部署環境限制、JVM 不能用、要極低延遲）

不滿足這些條件前，不應該做這個（特別是最後一條，Flink 早就把整套做完了，見下節）。

---

## Benchmark Roadmap

**本計畫為 side project**——探索 in-memory streaming + event sourcing 架構，**沒有實際業務 throughput 需求**。原本 §預期效益 寫的「100K events/s per shard」是 Redis 時代設定的「想要 10x」目標，對 in-memory 架構偏保守（業界類似 workload 如 Flink CEP 單 task 通常 100K~500K events/s）。

對 side project，throughput 不該是「必須達到的硬目標」，而是**探索性的 milestone**——每個階段壓到撞牆、找出 bottleneck、學習解法。這份 roadmap 取代「一個固定的 throughput 目標」。

### Milestones（按完成順序）

| # | 目標 | 學習主題 | 預期 bottleneck |
|---|------|---------|----------------|
| M1 | 單 shard 10K events/s | benchmark 框架、metric 接入 | 無（baseline） |
| M2 | 單 shard 50K events/s | pprof 找熱點 | map alloc、string key、proto decode |
| M3 | 單 shard 100K events/s | Go GC 調優、object pool | GC pause、cache miss |
| M4 | 撞單 stream NATS 上限 | NATS 真實 throughput | NATS server CPU / network |
| M5 | 4 shard 500K+ events/s | 跨 stream 協調、線性 scale | end-to-end latency |

### M1: 基準確認（單 shard 10K events/s）

**目標**：證明 in-memory 架構至少不輸 Redis 版本（Redis workers-1 為 12K events/s baseline）。

**Setup**：
- 1 個 Core process、1 個 NATS stream
- 預設 GC 設定、無優化
- Benchmark client 用簡單 Go producer 灌 events

**通過條件**：
- 連續 10 分鐘穩定處理 10K events/s
- p99 latency < 5ms
- 無 OOM、無 panic

**學會什麼**：整套 benchmark 流程、Prometheus + Grafana 接入、跟 Redis 版本成本對比方法

### M2: 中度優化（單 shard 50K events/s）

**目標**：用 pprof 找第一個 hotspot 並優化。

**預期 bottleneck**：
- `aggCache := make(map[string]any)` 每 event alloc → GC 壓力
- `map[string]*MemberState` 用 string key → hash 成本高
- Protobuf decode → 反射 / alloc 成本

**優化手段**：
- `sync.Pool` 重用 aggCache map / fields map
- Member ID 預雜湊成 uint64 → `map[uint64]*MemberState`
- Pre-computed rule aggregation paths（不每 event 重組）

**通過條件**：
- 單 shard 50K events/s p99 < 5ms
- pprof 看 alloc rate < 1 GB/s（GC 健康指標）

**學會什麼**：pprof CPU / heap profile 分析、Go alloc 模式、sync.Pool 正確用法

### M3: Plan stretch goal（單 shard 100K events/s）

**目標**：證明 §預期效益 的數字可達——也是 plan 原本的「主要目標」。

**預期 bottleneck**：
- GC pause（百萬 member map 的 GC scan 開銷）
- CPU cache miss（state 散佈各處）
- Single goroutine CPU 上限

**優化手段**：
- `GOGC` 調低 / `GOMEMLIMIT` 設定
- MemberState struct 排版優化（field 順序、避免 false sharing）
- 探討 off-heap 儲存（unsafe.Pointer + manual memory management）
- CPU 綁核（避免上下文切換）

**通過條件**：
- 單 shard 100K events/s p99 < 10ms
- GC pause p99 < 5ms（用 `runtime/metrics` 看）

**學會什麼**：Go GC 內部機制、low-latency Go 程式的調校技巧、mechanical sympathy

### M4: 撞 NATS 單 stream 上限

**目標**：找出 NATS JetStream 在實際硬體上的真實 throughput（不靠 plan 估計值）。

**Setup**：
- 4 shard 全部往單 stream publish
- 漸進增加總 publish rate：100K → 200K → 300K → 400K → 500K

**觀察**：
- NATS server CPU、disk I/O、network
- Stream replication lag（leader → follower）
- Consumer 端 latency 變化

**通過條件**：
- 找到「健康運作的最大 rate」（建議用 60% capacity 當 production 安全線）
- 文件化 NATS 在我們硬體上的真實 capacity number

**學會什麼**：NATS 真實 throughput vs marketing number、capacity planning 方法、margin 的重要性

### M5: 多 stream + 多 shard 線性 scale（500K+ events/s 總計）

**目標**：證明架構整體可以線性 scale（不是被單 stream 卡死）。

**Setup**：
- 4 個獨立 stream（per-shard，§不停機升級到 Per-Shard Stream 描述的目標狀態）
- 4 個 Core process
- 每 stream 灌 125K+ events/s

**通過條件**：
- 總吞吐 500K+ events/s 連續 1 小時
- Per-shard latency 跟 M3 一致（線性 scale，沒有 cross-stream 干擾）

**學會什麼**：
- Per-shard 隔離的實際效益
- 跨 stream checkpoint 管理的實作細節
- §不停機升級到 Per-Shard Stream 章節的實際操作（從 M1-4 的單 stream 升級到 M5 的多 stream）

### Roadmap 之外的說明

- **Milestone 不是必達**——「在 M2 卡住」也是有效學習，知道為什麼卡住更重要
- **每個 milestone 完成後寫一篇 retrospective**：學到什麼、踩到什麼坑、跟預期差在哪
- **若業務情境改變**（從 side project 變成有真實需求的 production），重新校準目標
- M3 完成後可考慮跳過 M4，直接進 M5（單 stream 已知會撞牆，多 stream 才是 production-grade 架構）

---

## 為什麼不直接用 Flink？借鏡 Flink 的設計

Apache Flink CEP 已經把「stateful streaming + checkpoint + rescaling + watermark + exactly-once」做成生產級 framework。**這個計畫等於是手工版的 Flink keyed state + CEP operator**。我們不直接用 Flink 的原因：

| 為什麼 DIY | 對應 trade-off |
|-----------|---------------|
| 目標 sync API p50 < 500µs | Flink barrier alignment 一次就 ~ms 級 |
| Go 技術棧、不引 JVM | Flink 是 JVM cluster |
| 想完全控制 snapshot 時機跟 sink commit 語意 | Flink 抽象層多，debug 要懂內部 |
| 學習 stateful streaming 內核 | Flink 把有趣的東西都藏起來了 |

**但 Flink 那些設計都是對的**，所以本計畫的關鍵機制都直接借過來：

| 機制 | Flink 名稱 | 本計畫採用 |
|------|-----------|-----------|
| Sharding 抽象 | Key group（KeyGroupAssignment）| §1 — 解耦 key 數量 vs shard 數量，rescaling 不用搬 1M member |
| 異步快照 | Async barrier snapshotting（Chandy-Lamport 變種）| §Checkpoint — 主路徑近 zero pause |
| 增量快照 | Incremental checkpoint（RocksDB backend 用）| §Checkpoint — dirty key group only |
| 時間語意 | Event time + Watermark + Allowed lateness | §Watermark — 處理 out-of-order 事件 |
| 結果一致性 | Two-phase commit sink (`TwoPhaseCommitSinkFunction`)| §Exactly-Once |
| 流控 | Network buffer-based natural backpressure | §Backpressure — NATS pull + `MaxAckPending` |
| State 演進 | State schema evolution | §Schema Evolution |

下面各章節詳述每個機制如何套用到我們的 NATS + Go 架構。

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
| Throughput | ~10K events/s | M1 10K → M3 100K → M5 500K（見 §Benchmark Roadmap）|
| 複雜度 | 低 | 中（snapshot + replay + shard） |

---

## 視覺化流程圖

讀的順序建議：**1 → 2 → 3 → 4 → 5 → 6**。圖 3 跟圖 4 是穩態跟例外的兩條主軸；圖 5 是把圖 4 的 snapshot 反過來用；圖 6 是把 sink 也納入這個語意保證。

### 圖 1: 整體架構（哪些 component、誰跟誰講話）

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ▶ 上游 (Event Producers)
     · API gateway / data pipeline / 外部系統 / shadow traffic
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
                          │
                          │  publish
                          │  subject: rule.events.{shard_id}.{member_id}
                          ▼
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ▶ NATS JetStream (Source of Truth)

     ┌─ Stream: rule-events
     │   · Partition by shard_id (subject filter)
     │   · Durable, replicated (R=3)
     │   · Retention ≥ 30 天（支援 schema migration replay）
     │   · Sequence: 單調遞增（被 checkpoint 記住的 cursor）
     │
     └─ KV Bucket: checkpoint_meta
         · key   = shard_id
         · value = { barrier_id, nats_seq, ts }
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
                          │
                          │  pull consumer
                          │  (AckExplicit, MaxAckPending=1000)
                          │  一個 shard = 一個 durable consumer
                          ▼
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ▶ Rule Engine Core (per shard 一個 process)

     [NATS Consumer]  ── pull batch ──▶  [Main Loop]
                                              │
                                              │  fork on barrier
                                              ▼
                                         [Snapshot Worker]
                                              │
                                              │  write dirty key groups
                                              ▼
                                         Local Disk:
                                         snapshots/shard_N/kg_NNN/*.snap

     Main Loop（single goroutine, FIFO 保證）做的事：
       1. decode msg
       2. 若是 barrier  → onBarrier()  → 觸發 snapshot worker
          若是 event    → processEvent() 走圖 3 流程
       3. update ShardState (純 in-memory)
       4. evaluate rules / CEP
       5. ack to NATS、記錄 lastSeq
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
                          │
                          │  (Deferred — Phase 5 才實作)
                          ▼
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ▶ Result Sink
     · NATS result stream（最自然的 event-driven 做法）
     · Webhook callback（通知外部系統，需要 retry + DLQ）
     · DB write-back（PostgreSQL，audit / 報表用）
     · API query on demand（client 主動查 in-memory state）
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

**重點**：
- 五層由上而下的單向資料流：Producers → NATS → Core → Disk / Sink
- NATS 是唯一可信來源，Core 的 in-memory state 跟 snapshot 都是衍生物
- Main Loop 跟 Snapshot Worker 分離：主路徑 single goroutine、無鎖、零外部 I/O；寫盤在背景
- Stream 存 event log，KV Bucket 存 checkpoint metadata（重啟時的地圖）

### 圖 2: Sharding / Key Group 分層（為什麼要兩段映射）

```
                    ┌──────────────────────┐
                    │  member_id（百萬級） │
                    └──────────┬───────────┘
                               │ crc32 % 128
                               │ ★ 這個 mapping 永遠不變
                               ▼
              ┌────────────────────────────────────┐
              │   Key Group ID  (固定 128 個)      │
              │   kg_000, kg_001, ..., kg_127      │
              └────────────────┬───────────────────┘
                               │ kgID * N / 128
                               │ ★ 這個 mapping 是執行期配置
                               │   rescaling 時只動這層
                               ▼
              ┌─────────────────────────────────┐
              │   Shard ID  (執行期可變)        │
              │   shard_0, shard_1, ...         │
              └─────────────────────────────────┘

範例：N=4 shard 時             範例：N=8 shard 時（rescale 後）
  shard_0  ← kg_000 ~ kg_031     shard_0  ← kg_000 ~ kg_015
  shard_1  ← kg_032 ~ kg_063     shard_1  ← kg_016 ~ kg_031
  shard_2  ← kg_064 ~ kg_095     shard_2  ← kg_032 ~ kg_047
  shard_3  ← kg_096 ~ kg_127     ...
                                 shard_7  ← kg_112 ~ kg_127

Rescaling 只需要：
  1. 更新 128 筆 kg→shard 映射
  2. 各 shard 重讀「自己現在擁有的 key group」snapshot 檔
  3. 從各 kg 對應的 nats_seq 開始 replay
                ↑
        ★ 不用搬 1M member 的 state、不用 rehash
```

**重點**：
- 128 是純邏輯數字（key group），NATS 完全感知不到
- 物理上 NATS subject、consumer、Core process 都只認 N 個 shard
- 兩段映射讓 rescale downtime 從 10 分鐘級 → < 1 分鐘

### 圖 3: 單一事件的 lifecycle（穩態流程）

```
NATS 訊息到達
       │
       ▼
┌─────────────────────────┐
│ msgs.Next() pull batch │  ← backpressure: MaxAckPending 卡住就停拉
└──────────┬──────────────┘
           ▼
┌─────────────────────────────────────────────────────┐
│  msg 是 barrier marker?                              │
│  ├─ Yes → 跳到圖 4 onBarrier 流程                   │
│  └─ No  → 繼續往下                                  │
└──────────┬──────────────────────────────────────────┘
           ▼
┌─────────────────────────┐
│ decodeEvent(msg.Data)   │
└──────────┬──────────────┘
           ▼
┌─────────────────────────────────────────────────────┐
│ Watermark / 遲到判斷（event time semantics）         │
│  ├─ event.OccurredAt < watermark → handleLateEvent  │
│  │    └─ 容忍範圍內：補進舊 bucket + 重算 window    │
│  │    └─ 太遲：side output（audit / 丟棄）          │
│  └─ 正常：推進 watermark = max(seen) - allowedLat   │
└──────────┬──────────────────────────────────────────┘
           ▼
┌─────────────────────────────────────────────────────┐
│ 冪等檢查：bucket.ProcessedEventIDs[event.EventID]?  │
│  └─ 已存在 → return（replay safety）                │
└──────────┬──────────────────────────────────────────┘
           ▼
┌─────────────────────────────────────────────────────┐
│ kgID = keyGroupOf(event.MemberID)                   │
│ dirtyKeyGroups.Set(kgID)   ← 這次 checkpoint 要寫   │
└──────────┬──────────────────────────────────────────┘
           ▼
┌─────────────────────────────────────────────────────┐
│ Update Aggregations (in-memory)                     │
│  ├─ 找 / 建 BucketData（按 bucketSize 對齊 ts）       │
│  ├─ count++, sums / max / min 更新                  │
│  └─ 清過期 bucket（OccurredAt - maxWindow 之前）    │
└──────────┬──────────────────────────────────────────┘
           ▼
┌─────────────────────────────────────────────────────┐
│ Evaluate Rules (in-memory compiled closures)        │
│  ├─ 從 in-memory buckets 組 aggCache                │
│  ├─ NewPreloadedEvalContext(fields, aggCache)       │
│  └─ 各 strategy.Eval → matchedRules                 │
└──────────┬──────────────────────────────────────────┘
           ▼
┌─────────────────────────────────────────────────────┐
│ Process CEP (in-memory progress state)              │
│  ├─ 推進所有 in-progress（advanceProgress）          │
│  └─ 用 patternsByBehavior 索引，看是否開新 progress │
└──────────┬──────────────────────────────────────────┘
           ▼
┌─────────────────────────────────────────────────────┐
│ if !replaying.Load():                               │
│    emitResult(matched)  ← (Deferred) sink output    │
└──────────┬──────────────────────────────────────────┘
           ▼
┌─────────────────────────┐
│ msg.Ack()               │
│ lastSeq.Store(msg.Seq)  │
└──────────┬──────────────┘
           │
           └─→ 回到 msgs.Next() 拉下一筆
```

**重點**：
- 整條路徑零外部 I/O（沒打 Redis、沒打 DB）
- Watermark 判斷在 aggregation 之前 → window 才能正確 trigger
- 冪等是「aggregate 之前查 ProcessedEventIDs」這一行保障的，replay 安全的根
- Ack 在最後 → 中途 crash 重啟會重發，配冪等就 at-least-once + 結果 exactly-once

### 圖 4: Checkpoint 流程（Barrier 如何切快照）

```
時間軸（NATS stream）：
  seq=100  evt   ┐
  seq=101  evt   ├─ 屬於 barrier_42 之前的事件
  seq=102  evt   ┘
  seq=103  BARRIER_42  ◀─── source（or 內部 timer）每 60s 注入
  seq=104  evt   ┐
  seq=105  evt   ├─ 屬於 barrier_43 之前
  seq=106  BARRIER_43


Core 主 goroutine（single-threaded，順序處理）：

   ┌──────┐ ┌──────┐ ┌──────┐  ┌──────────────┐ ┌──────┐ ┌──────┐ ...
   │ evt  │ │ evt  │ │ evt  │  │  BARRIER_42  │ │ evt  │ │ evt  │
   │ 100  │ │ 101  │ │ 102  │  │              │ │ 104  │ │ 105  │
   └──┬───┘ └──┬───┘ └──┬───┘  └──────┬───────┘ └──┬───┘ └──┬───┘
      │       │        │              │             │       │
      │ apply │ apply  │ apply        │             │ apply │ apply
      ▼       ▼        ▼              ▼             ▼       ▼
   ╔══════════════════════════╗       │           ╔════════════════╗
   ║   ShardState (in-mem)    ║       │           ║  繼續更新...   ║
   ║   dirty bitset: {3,7,42} ║       │           ║                ║
   ╚══════════════════════════╝       │           ╚════════════════╝
                                      │
                                      │ onBarrier(42, seq=103):
                                      │   ① 主 goroutine 做這幾件（µs 級）：
                                      │      a. dirty := bitset.SwapEmpty()
                                      │      b. refs := state.snapshotRefs(dirty)
                                      │      c. fork 背景 goroutine
                                      │   ② 立即 return → 繼續處理 seq=104
                                      │
                                      ▼
                          ┌──────────────────────────────┐
                          │  Snapshot Worker（背景）      │
                          │                               │
                          │  for kgID in dirty:           │
                          │    寫 kg_${kgID}/barrier_42  │
                          │       .snap.tmp              │
                          │    fsync + atomic rename     │
                          │                               │
                          │  全部成功後：                 │
                          │    KV.Put("shard_N",          │
                          │      {barrier: 42,            │
                          │       nats_seq: 103})         │
                          │                               │
                          │  (Phase 5) 通知 sink 做       │
                          │   phase-2 commit             │
                          └──────────────────────────────┘

時序保證：
  主路徑暫停時間 = SwapEmpty + fork goroutine ≈ µs 級
  寫盤、fsync、KV 更新都在背景，不阻塞 event 處理
```

**重點**：
- Barrier 只是個 marker，不帶資料；主 goroutine 看到 barrier 的瞬間 = 邏輯快照時間點
- Dirty bitset swap 是核心 trick：原子交換出本批變動清單，新事件繼續往新 bitset 累積，背景拿舊的去寫盤
- KV 最後才寫 → 中途 crash 時「最後成功 barrier」還是上一個，重啟從上一個對應 seq 開始 replay

### 圖 5: 重啟 / Replay 流程（崩潰恢復）

```
        ┌─────────────────────────────────────────┐
        │  Core 啟動（kill -9 後 / 部署重啟）     │
        └────────────────┬────────────────────────┘
                         ▼
        ┌─────────────────────────────────────────┐
        │ ① 讀 NATS KV: checkpoint_meta[shard_N]  │
        │    → {barrier: 42, nats_seq: 103}       │
        │   找不到 → 從 0 開始（fresh start）     │
        └────────────────┬────────────────────────┘
                         ▼
        ┌─────────────────────────────────────────┐
        │ ② 計算「我這個 shard 擁有哪些 kg」      │
        │   讀 assignment table（4 shard / 8 sh.）│
        │   假設 shard_0 → {kg_000, ..., kg_031}  │
        └────────────────┬────────────────────────┘
                         ▼
        ┌─────────────────────────────────────────┐
        │ ③ 並行載入 snapshot                     │
        │   for kg in owned_kgs:                  │
        │     read snapshots/shard_0/kg/          │
        │           barrier_000042.snap           │
        │     → 還原 MemberState 進 ShardState   │
        │   (snapshot 損壞 → fallback 更舊版本)   │
        └────────────────┬────────────────────────┘
                         ▼
        ┌─────────────────────────────────────────┐
        │ ④ replaying.Store(true) ← side effect   │
        │                          壓制 ON       │
        └────────────────┬────────────────────────┘
                         ▼
        ┌─────────────────────────────────────────┐
        │ ⑤ 建 NATS consumer，從 seq=104 開始拉  │
        │   (上次 ack 是 103，所以從 104)        │
        │                                         │
        │   for msg in messages:                  │
        │     processEvent(decode(msg))           │
        │     # 因為 emit 被壓制，只更新 state    │
        │     # 因為 ProcessedEventIDs 冪等檢查  │
        │     #   重播 snapshot 之後的 event 也  │
        │     #   不會重複算 aggregate            │
        │     if reached_live_position():         │
        │       break                             │
        └────────────────┬────────────────────────┘
                         ▼
        ┌─────────────────────────────────────────┐
        │ ⑥ replaying.Store(false)                │
        │                          壓制 OFF      │
        │   切換到 live mode                      │
        └────────────────┬────────────────────────┘
                         ▼
        ┌─────────────────────────────────────────┐
        │ ⑦ 正常處理（圖 3 的 lifecycle）         │
        │   每 60s 又會有新 barrier → 圖 4 流程   │
        └─────────────────────────────────────────┘

時間預算（從 kill -9 到追上 live）：
   Load snapshot     ~0.3 s   per-kg 並行讀
   NATS replay       ~0.5 s   兩次 barrier 間的事件量（60s × throughput）
   Consumer setup    ~0.2 s
   ─────────────────────────
   Total            ~1.0 s
```

**重點**：
- 為什麼能 1 秒回來：snapshot 大幅縮短 replay 距離（從 60s 前的 barrier 開始，不是 stream 起點）
- 為什麼安全：snapshot + nats_seq 是一對，比這個 seq 早的 effect 都在 snapshot 裡
- 為什麼可以重 replay 不出錯：ProcessedEventIDs + replay 期間壓 side effect 兩道防線

### 圖 6: Result Sink 的 Exactly-Once（Two-phase commit，Phase 5 才做）

```
                Barrier 流到 Sink 時觸發 2PC

────────────────────────────────────────────────────────────────
Phase 1: Pre-commit（barrier 到 sink）
────────────────────────────────────────────────────────────────

   Core ──BARRIER_42──▶ Sink
                          │
                          ▼
                 ┌──────────────────────┐
                 │ Sink 把當前 batch 寫 │
                 │ 到 "staging"，不對外 │
                 │ 發布                 │
                 │                      │
                 │ 例：Kafka txn write  │
                 │     但不 commit       │
                 │     DB → staging tbl │
                 │     webhook → buffer │
                 └──────────┬───────────┘
                            │ 回報「pre-commit OK」
                            ▼
                  Coordinator 等待：
                    ├─ state snapshot 完成？
                    ├─ sink pre-commit 完成？
                    └─ 全部 OK → 進 Phase 2

────────────────────────────────────────────────────────────────
Phase 2: Commit
────────────────────────────────────────────────────────────────

   Coordinator ──"commit barrier 42"──▶ Sink
                                          │
                                          ▼
                                 ┌──────────────────┐
                                 │ Sink 正式發布     │
                                 │ Kafka txn commit │
                                 │ staging → live   │
                                 │ flush webhook    │
                                 └──────────────────┘

────────────────────────────────────────────────────────────────
崩潰恢復
────────────────────────────────────────────────────────────────

  Phase 1 之後、Phase 2 之前掛：
    重啟 → 從上一個成功 barrier 還原 state
        → 對應 in-flight transaction 由 broker abort
        → 重 replay 該批 → 重新 pre-commit → 重新 commit
        → 下游零重複、零遺失

  Phase 2 進行中掛：
    重啟 → 檢查 broker，已 commit 的不重發
        → 未 commit 的補完
```

**重點**：
- Sink 端的 exactly-once 不靠下游冪等也能做到（前提是 sink 支援 transaction）
- 對不支援 transaction 的 sink（webhook）退回到「冪等 key + 下游 dedup」

### 圖之間的關係

```
        圖 1 (整體架構)
            │
            │ 拆 sharding
            ▼
        圖 2 (Key group → shard 兩段映射)
            │
            │ 進入 Core 內部
            ▼
        圖 3 (穩態：單 event lifecycle) ◀───┐
            │                                │
            │ 每 60s 來一次 barrier         │
            ▼                                │
        圖 4 (Checkpoint：barrier + 異步snap)│
            │                                │
            │ 如果掛了重啟                  │
            ▼                                │
        圖 5 (Replay：snapshot + 補事件) ────┘
            │
            │ Phase 5 加上
            ▼
        圖 6 (Sink 2PC：end-to-end exactly-once)
```

圖 3 跟圖 4 是穩態跟例外的兩條主軸；圖 5 是把圖 4 的 snapshot 反過來用；圖 6 是把 sink 也納入這個語意保證。

---

## 核心元件設計

### 1. Shard 拓樸（Flink-inspired Key Group 設計）

借用 Flink 的 **key group** 抽象，把「key 數量」跟「shard 數量」解耦：

```
member_id ──hash──→ key_group_id (固定 K=128 個) ──assign──→ shard_id (可變 N 個)
```

```go
const numKeyGroups = 128  // 一旦上線就固定不變

keyGroupID := crc32(memberID) % numKeyGroups
shardID    := keyGroupID * numShards / numKeyGroups
natsSubject := fmt.Sprintf("rule.events.%d.%s", shardID, memberID)
```

**為什麼分兩層**：
- `member_id → key_group` 永遠固定（不受 shard 數變動影響）
- `key_group → shard` 是執行期配置，rescaling 時只重新分配 128 個 key group，不用搬 1M member 的 state
- Snapshot 也按 key group 切（見 §Checkpoint），rescale 時各 shard 讀「自己現在擁有的 key group」對應的 snapshot 段就好

**初始**：4 shards，128 key group → 每 shard 32 個 key group，每 group 約 7800 member。
**擴充**：8 shards 時每 shard 16 個 key group，重新分配只動 128 筆映射、搬對應 snapshot 段。Downtime 目標 **< 1 分鐘**（vs 沒 key group 時的 10 分鐘級）。

#### Key Group 不是 NATS 提供的概念

容易誤會的點：**key group 不是 NATS 的 feature**。NATS 完全不知道 key group 的存在。它純粹是我們程式碼裡算出來的一個 Go int（0~127），從 Flink 的 `KeyGroupAssignment` 借過來的設計概念。

- 它不會出現在 NATS subject 裡
- 它不會出現在 NATS message header 裡
- NATS 看到的世界只有 N 個 shard subject

Key group 只在兩個地方被用到：
1. **Core 內部的 dirty bitset 跟 snapshot 分檔**（`snapshots/shard_N/kg_NNN/*.snap`）
2. **Rescaling 時重新計算 `kg → shard` 對應表**

#### 三層概念的物理對應

| 層次 | 「shard」的意義 | 物理隔離？ |
|------|---------------|-----------|
| **NATS Stream** | subject 字串前綴（路由標籤） | ❌ 一份 log 混在一起 |
| **NATS Consumer** | 每 shard 一個 durable consumer | ✓ 邏輯隔離 |
| **Core process** | 每 shard 一個 process / goroutine | ✓ 不同機器 / 不同記憶體 |
| **本地 snapshot** | 每 shard 一個目錄、每 kg 一個子目錄 | ✓ 不同檔案路徑 |

**NATS 那層只是邏輯隔離**——所有 shard 的訊息實體上寫在同一份 log file，按 publish 時序混合 append；consumer 用 subject filter（例如 `rule.events.1.>`）從混合 log 撈自己負責的訊息。**Core process + Snapshot 那層才是真的物理隔離**。

(如果未來流量大到單 stream 撐不住，可以升級成「每 shard 一個獨立 Stream」就變成物理隔離。)

#### Producer 端實際發生的事

```go
// Producer 收到 event
memberID := "user_abc"

// Step 1（純 CPU 計算）：member → key group
kgID := crc32("user_abc") % 128       // = e.g. 42  ← 永遠不變

// Step 2（純 CPU 計算）：key group → shard
shardID := kgID * numShards / 128     // 假設 N=4 → shardID = 1
                                       // ★ rescale 時只有這行的結果會變

// Step 3（這才是真的 NATS publish）
subject := "rule.events.1.user_abc"
nats.Publish(subject, eventBytes)
```

**Producer 直接 publish 到 N 個 shard subject 之一**。128 只是中間算出來的數字，連 NATS 訊息 header 都不會帶。

#### 端到端資料流

```
   程式概念                NATS 內部                  磁碟實體
───────────────         ──────────────────         ──────────────

 memberID = "user_abc"
        │
        │ crc32 % 128
        ▼
 keyGroupID = 42  ◀── 純粹 Go int
        │                NATS 完全不知道有 42 這個東西
        │ * N / 128
        ▼
 shardID = 1  ──────────▶  publish 到 subject:
                           "rule.events.1.user_abc"
                                │
                          ┌─────┴────────────────┐
                          │ Stream log (1 份)：  │
                          │   seq=1 subj=...0... │
                          │   seq=2 subj=...2... │
                          │   seq=3 subj=...1...│←user_abc 那筆
                          │   seq=4 subj=...0... │
                          └──────┬───────────────┘
                                 │ filter "rule.events.1.>"
                                 ▼
                          Consumer 1  ──▶  Core process #1
                                              │
                                              │ kgID = 42（重算）
                                              ▼
                                          /var/lib/rule-engine/
                                            snapshots/shard_1/
                                              kg_042/barrier_*.snap
                                              ↑
                                       這裡才看到 42 的存在
```

**重點**：
- 「128」這個數字只活在 Producer 計算邏輯跟 Core 自己的磁碟路徑裡，NATS 從頭到尾不認識它
- Producer 算出 shardID 之後直接 publish 到對應 subject
- NATS Stream 是一份 log，所有 shard 的訊息混在一起按時序 append
- Consumer 用 subject filter（`rule.events.{shard_id}.>`）撈自己那份
- Core process 收到事件後，**自己重新算一次** `kgID = crc32(memberID) % 128`，用來決定 dirty bitset 跟 snapshot 路徑

#### 為什麼不直接 `crc32 % N` 就好？

這就是 key group 存在的全部意義。對比兩種做法：

```
做法 A（沒 key group，直接分 shard）：
  N=4 時：  user_abc → crc32 → % 4 → shard 1
  N=8 時：  user_abc → crc32 → % 8 → shard 3   ★ 變了！

  → rescale 時所有 1M member 重洗一次
  → snapshot 全部要 reshuffle 重組
  → downtime 10 分鐘以上


做法 B（用 key group，本計畫採用）：
  N=4 時：  user_abc → kg_42 → shard 1   (42 * 4 / 128 = 1)
  N=8 時：  user_abc → kg_42 → shard 2   (42 * 8 / 128 = 2)
              ↑
          kg 永遠是 42

  → member → kg 的 hash 永遠不變
  → snapshot 是按 kg 分檔的，所以 rescale 只是
    「shard_2 改去讀 kg_42 那一份檔」，不用解壓重組
  → downtime < 1 分鐘
```

**做法 B 的代價**：多寫 ~50 行 Go code、snapshot 路徑多一層目錄、選一個一開始就要釘住的 magic number（128）。換來 rescale downtime 從分鐘級 → 秒級。對「未來可能要 scale」的系統幾乎是免費的保險。

#### 為什麼選 128？

128 = key group 的總數，一旦上線**不能改**（改了就所有 member 重 hash，等於做法 A 的災難）。選擇考量：

- **比預期 shard 數量大很多**：這樣每個 shard 才能拿到「一把」key group，rescale 才彈性。預期最多 ~32 shard，每 shard 拿 4 個 kg 還有空間
- **不要太大**：每個 key group 一份 snapshot 檔。128 = 128 個檔案可接受；如果 10000 個 key group → 10000 個檔，IO 變很碎

Flink 預設也是 128（`maxParallelism`），算是個經過驗證的甜蜜點。

#### 128 是 day-1 commitment，跟 member 數量無關

容易誤會：「等 member 變千萬級時要不要改 K？」**不用**。`crc32 % 128` 是從第一天就釘住的常數，跟 member 總數無關，只跟「未來最多會有幾個 shard」有關。

| 情境 | K=128 是否要動 |
|------|--------------|
| Member 從 1M 長到 100M | ✗ 不動 |
| Shard 從 4 加到 16、32 | ✗ 不動 |
| 機器規格升級 / 換 CPU | ✗ 不動 |
| Snapshot 格式版本升級 | ✗ 不動 |
| **未來需要 > 128 shard** | ✓ 這時才考慮（門檻 ≈ 12.8M events/s）|

```
member 數量變動時，每個 kg 「變肥」是預期內的事：
  member 1M    → 每 kg ≈ 7800 members
  member 10M   → 每 kg ≈ 78,000 members
  member 100M  → 每 kg ≈ 780,000 members

只要 hash 分布均勻，所有 kg 同步變肥，沒有 hotspot 問題。
```

K=128 的真正天花板是 **最多 128 個 shard ≈ 12.8M events/s**（按單 shard 100K events/s 算）。對百萬到千萬級用戶綽綽有餘，唯有逼近全球頂級支付 / 交易所規模才會撞到——那時面對的問題遠不止 K 不夠。

**千萬級時真正要處理的是這些**（都跟 K 無關）：

| 影響面 | 處理方式 |
|--------|---------|
| 單 shard memory 變大 | 加 shard（4 → 8 → 16）→ 每 shard 分到的 kg 變少 → memory 降回 |
| 單 stream throughput 撞牆 | 升級到 per-shard 獨立 stream（見 §不停機升級到 Per-Shard Stream） |
| 熱點 member（例如網紅、機器人）| 應用層拆 member ID，或該 shard 配更多資源 |

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

#### 設計核心：每 member 一台「並行狀態機集合」

CEP（Complex Event Processing）的本質：**偵測「按特定順序 + 在時間窗內發生的事件組合」**。例如：

```
Pattern: "FraudAttempt"（疑似撞庫攻擊）
  State 1: login_failed
  State 2: login_failed     (距 State 1 ≤ 5 分鐘)
  State 3: login_failed     (距 State 2 ≤ 3 分鐘)
  State 4: login_success    (距 State 3 ≤ 1 分鐘，同 IP)
```

每個 member 隨時可能在多個 pattern 的不同階段，**也可能同時跑同一個 pattern 的多個獨立 progress**（例如不同時間點各自開始一輪嘗試）。

```
Member user_abc 的 Progresses map：

  "uuid-1": { PatternID:"FraudAttempt",  CurrentState:2,  ... }
  "uuid-2": { PatternID:"FraudAttempt",  CurrentState:1,  ... }  ← 同 pattern 但獨立
  "uuid-3": { PatternID:"UnusualBuy",    CurrentState:0,  ... }
```

#### CEPProgress 完整欄位

```go
type CEPProgress struct {
    ProgressID      string       // 唯一 ID (uuid)
    PatternID       string       // 對應哪個 pattern
    CurrentState    int          // 已推進到第幾個 state
    StartedAt       time.Time    // 第一個 event 的 OccurredAt
    LastEventAt     time.Time    // 最近一個匹配 event 的 OccurredAt
    MatchedEvents   []EventRef   // 已匹配的 event 引用
    ProcessedEvents []string     // event_id 集合（冪等用）
}
```

#### Pattern 編譯（啟動時做一次）

CEP 規則用 DSL / config 定義，啟動時編譯成可執行的狀態機：

```go
type compiledPattern struct {
    PatternID      string
    CompiledStates []compiledState  // 順序：state[0] → state[1] → ...
    MaxDuration    time.Duration    // 整個 pattern 的總超時
}

type compiledState struct {
    Behavior   BehaviorType
    Predicates []func(Event) bool   // 額外條件（同 IP、金額 > X 等）
    MaxGap     time.Duration        // 距離前一個 state 的最大時間差
}
```

Pre-index `patternsByBehavior` 讓查詢「哪些 pattern 的 state[0] 是這個 behavior」變成 O(1)：

```
天真做法：每 event 掃所有 P 個 pattern → O(P)
Pre-index： patternsByBehavior[event.Behavior] → O(K)，K 通常 < 10

實務上 P=1000、K=5~20 → 50~200 倍加速
```

#### 每個 event 做兩件事

```
動作 A: 推進現有 progress
  for each progress in ms.Progresses:
    - 冪等檢查（event_id 在 ProcessedEvents 裡？跳過）
    - 超時檢查（用 event time，不用 wall clock）
    - 行為 + predicates 匹配？
      ├─ 完整匹配（推進到最後）→ 輸出 result，delete progress
      ├─ 推進（state++）→ 留著繼續等
      ├─ 過期 → delete progress
      └─ 不匹配 → progress 不動

動作 B: 起始新 progress
  for each pattern in patternsByBehavior[event.Behavior]:
    - state[0] 匹配？→ 建新 progress，CurrentState=1
```

#### 走一遍 FraudAttempt 完整流程

```
時間  Event                          Progresses 變化
────  ─────────────────────────     ─────────────────────────────────
T=0   login_failed (evt_100)        +uuid-1: state=1
T=1   login_failed (evt_105)        uuid-1: state=2、+uuid-2: state=1
T=2   browse_page (evt_110)         （不影響任何 progress）
T=3   login_failed (evt_120)        uuid-1: state=3、uuid-2: state=2、+uuid-3: state=1
T=4   login_success (evt_130)       uuid-1: 完整匹配 → 輸出 result + delete
                                    uuid-2/uuid-3: 不匹配（需要 login_failed），不動
```

#### 純超時的 progress：watermark-driven sweep

如果 progress 卡在中間，再也沒對的 event 來推進——單靠 event 觸發清不掉。解法是 **watermark 推進時順便掃過期 progress**：

```go
func (c *Core) sweepExpiredProgresses(watermark time.Time) {
    for _, kg := range c.state.keyGroups {
        for _, ms := range kg.members {
            for id, p := range ms.Progresses {
                pattern := c.compiledPatternByID(p.PatternID)
                if watermark.Sub(p.StartedAt) > pattern.MaxDuration {
                    delete(ms.Progresses, id)
                }
            }
        }
    }
}
```

「watermark 推到 T」≈ 「保證 T 之前不會再有更晚的 event」——這時用 watermark 跟 `progress.StartedAt` 比可以安全判定過期。

#### Snapshot / Replay 處理

CEP progress 跟 aggregation 一起按 kg 分桶、按 barrier snapshot。Replay 安全的兩個冪等保證：

| 操作 | 冪等手段 |
|------|---------|
| 推進現有 progress | `ProcessedEvents` 已含 event_id → 跳過 |
| 起始新 progress | `(pattern_id, member_id, first_event_id)` dedup key |

Snapshot 也包含 progresses：

```protobuf
message MemberStatePB {
    string member_id = 1;
    repeated BehaviorAggPB aggregations = 2;
    repeated CEPProgressPB progresses = 3;   // ★ 連同 progresses 一起 snapshot
    int64 last_seen_at = 4;
}
```

#### 跟 Redis 版本的對比

| 操作 | Redis 版本 | In-Memory 版本 |
|------|-----------|--------------|
| 推進 progress | `SMEMBERS` + `MGET` | `for range ms.Progresses` |
| 寫回 progress | `SET` + `SADD` | `ms.Progresses[id] = p` |
| Pattern 查詢 | 從 Redis 讀 active patterns | in-memory `patternsByBehavior` |
| 超時 sweep | TTL on Redis key（replay 時不對）| watermark-driven sweep |
| **Per event 成本** | 3-5 個 Redis round-trip | 純 memory，~1µs |

CEP 路徑完全沒有 Redis I/O——這就是 throughput 從 10K 跳到 100K events/s 的主要來源之一。

#### 記憶體成本估算

```
單 progress ≈ 1 KB（含 MatchedEvents + ProcessedEvents）
典型 active member 同時 2~5 個 progress ≈ 5 KB CEP 開銷
+ Aggregation 開銷 ≈ 2 KB
≈ 7 KB / active member 總計

100 萬 active member × 7 KB ≈ 7 GB
4 shard 分擔 → 每 shard 1.75 GB  ✓ 符合 §Memory「< 10 GB」目標
```

控制 progress 數量爆炸的兩個機制：
1. **MaxDuration 超時** → progress 卡住自動清
2. **完整匹配後 delete** → 成功的 progress 立即清

---

## Checkpoint + Replay 機制（Flink-inspired）

### 設計原則

借用 Flink 兩個關鍵 idea，取代「每 60 秒整個 dump」的天真做法：

1. **Async barrier snapshotting**：source 把 barrier 注入事件流，barrier 流過時各 component 異步快照，**不停止事件處理**（Chandy-Lamport 分散式快照演算法的單機簡化版）
2. **Incremental checkpoint**：只快照變動的 key group，不是每次 dump 全部 state

### Barrier 機制

```
NATS 訊息流被 source 標記了 barrier：
  evt evt evt [BARRIER_42] evt evt [BARRIER_43] evt ...

Core 處理到 [BARRIER_42] 時：
  1. 記下「barrier 42 對應 NATS seq = X」
  2. 把 barrier 往下游 result sink 傳（exactly-once 兩階段提交用）
  3. fork 一個 snapshot goroutine，異步寫 dirty key groups
  4. 主 goroutine 立即繼續處理後續 event（不等寫檔）
```

主處理路徑**幾乎不停頓**（barrier 觸發 + atomic dirty bitset swap，~µs 級），實際寫檔在背景 goroutine。

### Incremental（dirty key group only）

```go
type Core struct {
    state          *ShardState
    dirtyKeyGroups bitset.BitSet  // 自上次 checkpoint 以來變動的 key group
    barrierID      atomic.Uint64
}

func (c *Core) processEvent(event Event) {
    kgID := keyGroupOf(event.MemberID)
    c.dirtyKeyGroups.Set(kgID)
    c.applyEvent(event)
}

func (c *Core) onBarrier(barrierID uint64, seq uint64) {
    // 主 goroutine: atomic swap dirty bitset + 拿 state references
    dirty := c.dirtyKeyGroups.SwapEmpty()
    refs := c.state.snapshotRefs(dirty)

    // 背景: 寫 dirty kg、寫 manifest、更新 KV
    go c.commitBarrier(barrierID, seq, dirty, refs)
}

func (c *Core) commitBarrier(barrierID, seq uint64, dirty bitset.BitSet, refs StateRefs) {
    // Step 1: 寫所有 dirty kg 的 .snap 檔 + 計算 sha256
    entries := make([]ManifestEntry, 0, dirty.Count())
    for kgID := range dirty.Iter() {
        path := fmt.Sprintf("snapshots/shard_%d/kg_%03d/barrier_%06d.snap",
                            c.shardID, kgID, barrierID)
        sha := writeKeyGroupSnapshot(path, refs.keyGroup(kgID))  // 寫 + fsync
        entries = append(entries, ManifestEntry{
            KeyGroupID: kgID,
            FilePath:   path,
            Sha256:     sha,
        })
    }

    // Step 2: 寫 manifest 檔（atomic rename）
    //   manifest 存在 = 「這個 barrier 的所有 .snap 檔都寫完了」
    manifestPath := fmt.Sprintf("snapshots/shard_%d/barrier_%06d.manifest",
                                 c.shardID, barrierID)
    writeManifest(manifestPath, Manifest{
        BarrierID:     barrierID,
        NatsSeq:       seq,
        SchemaVersion: currentSchemaVersion,
        Entries:       entries,
    })  // 寫 .tmp → fsync → atomic rename
    // ★ manifest 寫成功 = 整個 barrier 在磁碟上完整

    // Step 3: 更新 NATS KV（最終 commit point）
    c.kv.Put(ctx, fmt.Sprintf("shard_%d", c.shardID),
             CheckpointMeta{BarrierID: barrierID, NatsSeq: seq})

    // Step 4: 觸發 sink 的 phase-2 commit
    c.notifySinkCommit(barrierID)
}
```

**三層 commit 的時序保證**：

```
階段                  完成代表的事
──────────────────   ──────────────────────────────────
.snap 檔寫完          單一 kg 的 state 落地
manifest 寫完 ★       整個 barrier 的所有 kg 都已落地
KV 更新               對外正式承諾「這個 barrier 可用」
```

預期效果：典型一次 checkpoint 只寫 ~5% key group（流量分布不均），1.3 GB → ~65 MB / checkpoint。

### Snapshot 格式（per key group + manifest）

**設計目標**：解決「跨多個 .snap 檔的原子性」問題。單一檔案 atomic rename 只保證該檔案要嘛舊版要嘛新版，但 barrier 涉及多個 kg 檔，需要「整套 commit / 整套 rollback」的語意。**解法是用 manifest 檔當作整個 barrier 的單一 commit point**。

#### 檔案結構

```
snapshots/
  shard_0/
    barrier_000041.manifest    ← 上一個成功的 barrier
    barrier_000042.manifest    ← 最新成功的 barrier ★
    kg_000/
      barrier_000041.snap
      barrier_000042.snap
    kg_001/
      barrier_000041.snap
      barrier_000042.snap      ← 只有 dirty kg 才有新版本
    kg_002/
      barrier_000041.snap      ← 沒 dirty，沒 barrier_42 檔
    ...
```

- **`.snap` 檔**：每 kg 一份 protobuf
- **`.manifest` 檔**：每 barrier 一份（shard 層級），列出該 barrier 涵蓋哪些 .snap 檔
- **`.snap.tmp` / `.manifest.tmp`**：寫入中的暫存檔，啟動時清掉

#### Protobuf schema

```protobuf
// snapshot.proto
syntax = "proto3";

message KeyGroupSnapshotHeader {
    int32 shard_id = 1;
    int32 key_group_id = 2;
    uint64 barrier_id = 3;
    uint64 nats_seq = 4;
    int64 snapshot_ts = 5;
    int32 schema_version = 6;
}

message MemberStatePB {
    string member_id = 1;
    repeated BehaviorAggPB aggregations = 2;
    repeated CEPProgressPB progresses = 3;
    int64 last_seen_at = 4;
}

message BehaviorAggPB { ... }
message BucketPB { ... }

// Manifest 描述「barrier X 對應哪些 .snap 檔」
message Manifest {
    uint64 barrier_id = 1;
    uint64 nats_seq = 2;
    int64 created_at = 3;
    int32 schema_version = 4;
    repeated ManifestEntry entries = 5;
}

message ManifestEntry {
    int32 key_group_id = 1;
    string file_path = 2;        // 相對於 snapshot root
    bytes sha256 = 3;            // 內容雜湊（loader 驗證用）
    uint64 file_size = 4;
}
```

#### 為什麼 manifest 解決原子性問題

```
情境：barrier 42 寫到一半 kill -9（dirty = {3, 7, 42}）

寫了：kg_3/barrier_42.snap     ✓
      kg_7/barrier_42.snap     ✓
      kg_42/barrier_42.snap    ✗ (沒寫到)
      barrier_42.manifest      ✗ (還沒開始寫)

重啟時：
  1. 讀 KV → barrier=41
  2. 看磁碟有 barrier_42.manifest 嗎？沒有
  3. → 視為「barrier 42 失敗」，忽略所有 barrier_42.snap 檔
  4. 用 barrier 41 的 manifest 載入該 barrier 對應的 .snap 檔
  5. barrier_42.snap 殘骸排 GC 時清掉
```

```
情境：barrier 42 全寫完，KV 還沒更新就 kill

寫了：kg_*/barrier_42.snap     ✓ (全部)
      barrier_42.manifest      ✓
      KV.Put                   ✗ (沒成功)

重啟時：
  1. 讀 KV → barrier=41（KV 沒成功更新）
  2. 看磁碟有 barrier_42.manifest 嗎？有，且完整
  3. → 雖然磁碟有 barrier_42，但 KV 沒承諾 → 還是用 barrier 41
  4. 從 nats_seq=96 開始 replay → 自然會重新觸發 barrier 42
  5. barrier_42.manifest 跟對應 .snap 排 GC 時清掉

★ 這裡的選擇：「以 KV 為準」而不是「以磁碟最新 manifest 為準」
  原因：KV 寫成功 = sink 的 phase-2 commit 也應該觸發了
        如果用磁碟 barrier 42 但 sink 沒 commit → 兩邊不一致
        以 KV 為準，sink commit 跟 state 都從同一個點重新走
```

#### 每 barrier 一個 manifest 的清理策略

```
保留近 N 份 manifest（預設 3，跟 .snap 保留策略對齊）：
  barrier_000040.manifest   ← 可清
  barrier_000041.manifest   ← 保留（KV 之前的 barrier）
  barrier_000042.manifest   ← 保留（KV 當前的 barrier）
  barrier_000043.manifest   ← 保留（最新成功的）

清理 manifest 時，順便清「該 manifest 引用的 .snap 檔」
  → 確保沒有 orphan .snap 檔
```

### Restart 流程

```
Step 1: 從 NATS KV 讀「最後成功 barrier ID + 對應 NATS seq」
        → 例：barrier=42, nats_seq=103

Step 2: 清理本地 garbage 檔
        ┌─ 刪掉所有 .snap.tmp、.manifest.tmp（寫到一半的殘骸）
        ├─ 刪掉所有 barrier_id > 42 的 .manifest 檔
        │  （比 KV 還新的 manifest = 寫完但未承諾，視為未發生）
        └─ 刪掉所有「沒被任何 manifest 引用」的 .snap 檔
           （包括 barrier 42 沒涵蓋的舊 kg 版本、orphan 殘骸）

Step 3: 讀 barrier_42.manifest
        → 拿到該 barrier 對應的所有 (kg, file_path, sha256) 清單

Step 4: 並行載入每個 .snap 檔
        ├─ 驗證 sha256（內容是否完好）
        ├─ 反序列化 protobuf
        └─ 塞進對應的 ShardState.keyGroups[kg]
        
        ★ 哪些 kg 沒在 manifest 裡？
          表示「barrier 42 時這個 kg 沒 dirty」
          → 從更早的 manifest（barrier 41、40...）找該 kg 的最後版本載入

Step 5: 從 NATS seq = 104 開始 replay
        ├─ replaying.Store(true) 壓制 side effect
        ├─ 處理 event（ProcessedEventIDs 保證冪等）
        └─ 追上 live position → replaying.Store(false)

Step 6: 切到 live mode，正常處理
```

#### Step 4 的「找該 kg 的最後版本」是什麼意思

```
情境：barrier 42 的 manifest 只列了 {kg_3, kg_7, kg_42}
      但系統有 128 個 kg，其他 125 個 kg 的 state 怎麼來？

答：往前找最近一次「該 kg 有出現在 manifest 的 barrier」

例如 kg_5：
  barrier_42.manifest:  沒列 kg_5
  barrier_41.manifest:  沒列 kg_5
  barrier_40.manifest:  列了 kg_5  ★
  → 載入 kg_5/barrier_40.snap

實作上不用每次都搜：載入時建一個 in-memory index：
  "kg_5 → barrier_40", "kg_7 → barrier_42", ...
  
比較粗暴的做法：每個 manifest 都記「該 barrier 時所有 kg 的最新版本指針」
（manifest 變大，但載入更直接，跟 LSM tree 的 manifest 一樣的做法）
```

**第一版用「往前找」**，簡單實作；觀察到載入慢再升級成「manifest 記全 kg 指針」。

### Restart Time Budget

```
Phase          | 時間        | 備註
──────────────────────────────────────────────────
Load snapshot  | ~0.3 s      | per-key-group 並行載入，每份很小
NATS replay    | ~0.5 s      | 兩次 barrier 間的事件量 << 全 stream
Setup consumer | ~0.2 s      | NATS connection, consumer create
──────────────────────────────────────────────────
Total          | ~1 s ✓
```

比原本「整 dump + 60s 內全 replay」更快，因為 incremental snapshot 不用解壓 1.3 GB，replay 區間也短。

---

## Event Identity Contract

引擎的所有冪等保證(下一章 §冪等性保證)都建立在「**event_id 是個合格的身份識別**」這個前提上。這個合約由 **producer 端**保證,我們的 engine 純粹是 consumer——consumer 無法驗證,只能假設;一旦違反,state 會**靜默損壞**(不是 panic、不是 error log,就是計數錯、誤判,還很難 debug)。

故此合約是**外部協議**,不是實作細節——值得獨立一章,放在依賴它的 §冪等、§CEP、§Exactly-Once 之前。

### 六條硬不變式

| # | 規範 | 違反後果(具體場景) |
|---|------|------------------|
| **1.** 由 producer 在事件**產生當下**分配,綁進 payload | 中間層(API gateway / MQ broker / 我們 engine)生成的話,producer retry 時拿到新 id → `Bucket.ProcessedEventIDs` 視為兩筆 → user 過去 5 分鐘 trade 數從 4 → 5,**誤觸發風控** |
| **2.** 跨 redelivery / replay **保持完全一致**(同一邏輯事件無論被傳幾次,id 不變) | 引擎重啟後從 NATS replay 全部 event,每筆 id 不同 → state 翻倍 → **所有 user 的 aggregation 都錯了** |
| **3.** 不同邏輯事件 id **必須不同**(全域唯一,UUID 等級) | 兩個不同 event 被當同一個 → 第二個被 dedup 丟掉 → **該觸發的詐騙偵測沒觸發** |
| **4.** **不**依賴 transport-side metadata(NATS seq / Kafka offset / 網路 timestamp) | 同筆事件 retry 時 transport metadata 會變,身份就崩了(同 #1) |
| **5.** **不**用 content hash(`sha256(payload)` 不行) | 兩筆**業務上不同但欄位剛好一樣**的事件(同 user 同金額兩次點擊)會碰撞 → silent 合併成一筆 |
| **6.** 字串型 + 合理長度(UUID 36 chars 或 ULID 26 chars) | 我們存進 `map[string]struct{}`,hot bucket 累積很多份 → 字串長度直接影響記憶體(連 §gap #26) |

### 推薦實作:UUIDv7(優於 v4)

```
v7 = 48-bit unix millisecond timestamp + 74-bit randomness
     ▲                                    ▲
     │                                    │
   時間排序友善                        全域唯一性
```

比 v4 好在:
- **時間可讀**:看 id 直接知道大概什麼時候產生(audit / debug 友善)
- **排序友善**:DB index、log grep、ProcessedEventIDs map 的 hash 分佈更均勻
- 跟 v4 同樣 36 chars,沒有額外成本

Go SDK:`github.com/google/uuid` v1.6+ 的 `uuid.NewV7()`。其他語言類似。

### 我們系統內 event_id 的三個 dedup 點(都依賴此合約)

```
producer ──event_id 帶在 payload──▶ NATS / Kafka
                                          │
                                          ▼
                              consumer 拿到 event.EventID
                                          │
                                          ▼
                       ① Bucket.ProcessedEventIDs[id]            ← aggregation dedup
                       ② CEPProgress.ProcessedEvents[]           ← CEP advance dedup
                       ③ CEPProgress.ID = pattern|member|startEventID  ← 開新 progress 的去重 key
```

三個點都假設這 6 條成立。**任一條破了三個地方就會悄悄出錯**。

### Operational 規範

- **Producer SDK 統一 helper**:`producer.NewEvent(member, behavior, fields)` 內部自動 `uuid.NewV7()`,避免每個 producer 各自亂寫(這也是 `cmd/event-producer` 範本)
- **Audit trail**:event_id 寫進上游系統 log,讓「user 在 10:30 那筆 trade」可以被 grep 追進 engine 的 `ProcessedEventIDs`
- **不要 namespace**:不需要 `engine-v1-uuid` 這種前綴(無意義增加長度);id **本體**就應該全域唯一
- **不要 recycle**:UUID 理論上永不重複,但**監控不該假設它真的不會**——可週期取樣統計 id 碰撞率當健康指標

### 為什麼 consumer 端不去驗證

理論上 consumer 可以做最小驗證(例如:檢查 id 是否為合法 UUID 格式),但這些是**形狀檢查**,擋不住違反語意的 case(例如 broker 生成的合法 UUID 也是 UUID)。**真正的不變式(#1 / #2 / #4)是時間維度的——consumer 在收到事件當下沒有資訊判斷**。所以這合約必須在 producer 側遵守 + 在組織層面做為協議,不是 engine 能技術性強制的。

---

## 冪等性保證

Replay 可能重播一段已經在 snapshot 裡的 events。所有 state 更新**必須冪等**。

**前提**:event_id 滿足 §Event Identity Contract(上一章)——下面所有策略以此為地基,合約若破,冪等全失效。

| 操作 | 冪等策略 |
|------|---------|
| Aggregation update | `BucketData.ProcessedEventIDs` set,加入前先檢查 |
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

## End-to-End Exactly-Once（Flink-inspired）

### 現況：at-least-once + 冪等

事件處理層面已透過 `event_id` dedup 保證冪等（見 §冪等性保證）。但**結果輸出**的 exactly-once 還沒設計。

### Two-phase commit on result sink

借用 Flink `TwoPhaseCommitSinkFunction` pattern：

```
階段 1: pre-commit（barrier 流到 sink）
  Sink 把當前批次結果寫到「臨時區」，不對外發布
  例：Kafka transactional producer 寫但不 commit、DB 寫到 staging table

階段 2: commit（checkpoint 完全成功後，coordinator 通知）
  Sink 才正式發布
  例：commit Kafka transaction、staging → live table

崩潰恢復：
  ├ 階段 1 後、階段 2 前掛了
  │  → 重啟後從 last checkpoint 還原狀態
  │  → 對應 barrier 的 in-flight transaction 由 broker 端 abort（Kafka）
  │  → 從 NATS replay 重跑該批次，重新 pre-commit
  └ 階段 2 期間掛了
     → 重啟後檢查 broker 狀態，未 commit 的 transaction 補完
     → 已 commit 的不重發
```

### 等冪 sink 替代方案

不是所有下游都支援 transaction（webhook、外部 REST API）。對這類 sink：
- 帶 `(barrier_id, member_id, rule_id)` 為冪等 key
- 下游收到時自己 dedup
- Sink 端維護 in-flight buffer，barrier commit 後才 release 出去

### Source 側：NATS offset 納入 checkpoint

Consumer 的 ack policy 設為 `Explicit`，barrier 對應的 `nats_seq` 跟 state snapshot 一起寫進 checkpoint。Restart 時從 checkpoint 的 `nats_seq + 1` 開始消費，不依賴 NATS 自己的 ack 狀態 → 避免「ack 了但 state 沒寫進 snapshot」這種斷裂。

---

## Observability

### Side Project 的取捨

Production observability 服務的對象是「不認識系統的 oncall」，所以要 distributed tracing、APM、audit log、SLA alerting... 對 side project 都是 overkill。本節是 **self-service observability**——服務「你自己」，只解決 4 個問題：

| 服務目的 | Side project 是否需要 |
|---------|--------------------|
| 驗證 correctness | ✓ 學習：架構是否真的對 |
| 診斷 bug | ✓ 你會撞 bug |
| Capacity planning | ✓ Benchmark roadmap 需要 |
| Performance optimization | ✓ M2+ milestone 都靠這個 |
| 合規 / audit | ✗ 沒合規需求 |
| SLA 監控 | ✗ 沒 SLA、沒用戶 |
| Customer support | ✗ 沒客戶 |

**工具棧**：Prometheus + Grafana + Go slog + Go pprof。全部免費、開箱即用、約 200 行 Go code 全部建好。

**不做的事**：distributed tracing（單 Core process trace 沒意義）、APM 整合、log aggregation cluster、SLA alerting + pager rotation。

### Metric 清單（核心 18 個）

按來源分組。Label `shard_id` 預設都帶，下面省略不重複寫。

#### Throughput / Latency

| Metric | 類型 | 額外 Labels | 意義 |
|--------|------|-----------|------|
| `core_events_processed_total` | counter | — | 處理過的 event 總數 |
| `core_event_latency_seconds` | histogram | phase | 處理單 event 的耗時，phase = decode\|aggregate\|rule\|cep |
| `core_in_flight_events` | gauge | — | NATS 已 deliver 未 ack 的事件數 |

#### State / Memory

| Metric | 類型 | 額外 Labels | 意義 |
|--------|------|-----------|------|
| `core_member_count` | gauge | — | 該 shard 當前 in-memory member 數 |
| `core_progress_count` | gauge | — | 該 shard 當前 in-flight CEP progress 數 |
| `core_goroutine_count` | gauge | — | per-shard goroutine 數（含 snapshot worker）|

#### Checkpoint

| Metric | 類型 | 額外 Labels | 意義 |
|--------|------|-----------|------|
| `checkpoint_duration_seconds` | histogram | — | barrier → KV commit 的總耗時 |
| `checkpoint_dirty_kg_count` | histogram | — | 該次 checkpoint 寫了幾個 dirty kg |
| `checkpoint_last_success_age_seconds` | gauge | — | 距離上次成功 checkpoint 多久 |
| `checkpoint_failures_total` | counter | reason | 失敗次數（reason = disk_full\|kv_timeout\|...）|

#### Watermark / Time

| Metric | 類型 | 額外 Labels | 意義 |
|--------|------|-----------|------|
| `watermark_lag_seconds` | gauge | — | `now - watermark` |
| `late_events_total` | counter | action | 遲到 event 數（action = apply\|drop）|

#### NATS

| Metric | 類型 | 額外 Labels | 意義 |
|--------|------|-----------|------|
| `nats_consumer_pending` | gauge | — | NATS 端累積未送出的訊息 |
| `nats_publish_failures_total` | counter | stream | publish 失敗次數 |

#### Rule / Pattern

| Metric | 類型 | 額外 Labels | 意義 |
|--------|------|-----------|------|
| `rule_match_total` | counter | rule_id | 該 rule 累計命中次數 |
| `pattern_match_total` | counter | pattern_id | pattern 累計完整匹配次數 |

#### Hot Key（來自 §Hot Key Handling）

| Metric | 類型 | 額外 Labels | 意義 |
|--------|------|-----------|------|
| `hotkey_member_rate` | gauge | member_id | Top-N hot member 的 rate |
| `hotkey_quarantined_count` | gauge | — | 已 quarantine 的 member 數 |
| `hotkey_shard_skew_ratio` | gauge | — | max(member_rate) / avg(member_rate) |

### 基本告警規則

不接 pager rotation，用 Grafana / email / Discord webhook 通知就好：

| Alert | 觸發條件 | 含義 |
|-------|---------|------|
| HighProcessLatency | `core_event_latency_seconds p99 > 10ms` 持續 5min | shard 跟不上 |
| HighInFlight | `core_in_flight_events > 800` 持續 1min（假設 MaxAckPending=1000）| 接近 backpressure |
| CheckpointStale | `checkpoint_last_success_age_seconds > 300` | checkpoint 卡住 |
| WatermarkStall | `watermark_lag_seconds > 60` | event 流停滯 or 跟不上 |
| HighShardSkew | `hotkey_shard_skew_ratio > 100` | 流量分布嚴重不均 |

### Structured Logging（Go slog）

用 Go 1.21+ `log/slog`，所有 log 結構化 JSON 出 stdout：

```go
slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelInfo,
})))

// Event 處理 log
slog.Info("event_processed",
    slog.Int("shard_id", c.shardID),
    slog.String("event_id", event.EventID),
    slog.String("member_id", event.MemberID),
    slog.Duration("latency", time.Since(start)),
)

// Checkpoint commit log
slog.Info("checkpoint_committed",
    slog.Int("shard_id", c.shardID),
    slog.Uint64("barrier_id", barrierID),
    slog.Uint64("nats_seq", seq),
    slog.Int("dirty_kg_count", dirty.Count()),
    slog.Duration("duration", time.Since(start)),
)
```

**最小欄位 spec**：

- 所有 log 必帶 `shard_id`
- 所有 event-related log 必帶 `event_id`, `member_id`
- 所有耗時操作必帶 `duration`
- 錯誤 log 必帶 `error`（用 `slog.Any("error", err)`）

**Log level 使用**：

| Level | 何時用 |
|-------|--------|
| `Debug` | 單 event 處理詳情、CEP state transition |
| `Info` | Checkpoint commit、watermark advance、shard lifecycle |
| `Warn` | Late event drop、quarantine 觸發、rule eval timeout |
| `Error` | NATS 失敗、snapshot 寫失敗、panic 救回來 |

### HTTP Debug + Admin Endpoint

每個 Core process 跑一個簡單 HTTP server（不同 port），暴露：

```
GET  /metrics                       Prometheus exposition
GET  /debug/pprof/*                 Go 內建 pprof（CPU/heap/goroutine）
GET  /debug/member/:id              看單 member 完整 state
GET  /debug/shard/stats             該 shard 統計快照
GET  /debug/hot                     Top-N hot member（從 detector）
POST /admin/quarantine/:id          手動觸發 quarantine（hot key handling）
POST /admin/replay/:member_id       強制 replay 某 member 歷史（debug）
```

實作骨架（用 chi router）：

```go
func (h *DebugHandler) GetMember(w http.ResponseWriter, r *http.Request) {
    memberID := chi.URLParam(r, "id")
    ms, ok := h.core.GetMemberSnapshot(memberID)
    if !ok {
        http.Error(w, "not found", 404)
        return
    }
    json.NewEncoder(w).Encode(ms)
}

func (h *DebugHandler) GetShardStats(w http.ResponseWriter, r *http.Request) {
    json.NewEncoder(w).Encode(map[string]any{
        "shard_id":         h.core.ShardID,
        "member_count":     h.core.MemberCount(),
        "progress_count":   h.core.ProgressCount(),
        "watermark":        h.core.Watermark(),
        "last_checkpoint":  h.core.LastCheckpoint(),
        "in_flight_events": h.core.InFlightCount(),
    })
}
```

`GET /debug/member/:id` 回應範例：

```json
{
  "member_id": "user_abc",
  "last_seen_at": "2026-05-21T15:30:42Z",
  "aggregations": {
    "login_failed": {
      "buckets": {
        "1716305400": {"count": 3, "sums": {}},
        "1716305460": {"count": 5, "sums": {}}
      }
    }
  },
  "progresses": [
    {
      "progress_id": "abc-123",
      "pattern_id": "FraudAttempt",
      "current_state": 2,
      "started_at": "2026-05-21T15:29:00Z",
      "matched_events": ["evt_100", "evt_105"]
    }
  ]
}
```

### Grafana Dashboard 三分區

```
┌────────────────────────────────────────────────────┐
│ 1. Throughput & Latency                             │
│  ─ events/s per shard (line chart)                  │
│  ─ p50/p95/p99 latency per shard                    │
│  ─ NATS in_flight 趨勢                              │
└────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────┐
│ 2. Health                                           │
│  ─ Memory usage per shard                           │
│  ─ Watermark lag per shard                          │
│  ─ Checkpoint duration / last success age           │
│  ─ Goroutine count                                  │
└────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────┐
│ 3. Rules & Patterns                                 │
│  ─ Top 10 rules by match rate (bar)                 │
│  ─ Pattern progress count over time                 │
│  ─ Hot member top 10 (table)                        │
└────────────────────────────────────────────────────┘
```

用 Grafana Cloud 免費方案就夠。Local docker-compose 跑 prometheus + grafana 也行。

### 漸進式建置（對應 Benchmark Roadmap）

不一次到位，按 milestone 漸進：

- **M1**：接 Prometheus + 基本 metric（events/s, latency, memory, in_flight）+ 一個 dashboard + `/metrics` + `/debug/pprof/*`
- **M2**：加 checkpoint metrics + watermark metrics + slog 結構化 log
- **M3**：加 GC pause metric（用 `runtime/metrics`）+ scheduler lag
- **M4**：加 NATS server-side metric（需要架 nats-exporter）+ stream replication lag
- **M5**：加 cross-shard 對比 dashboard（線性 scale 驗證）+ hot key handling 整套 metric

---

## Backpressure

### 問題

Consumer 從 NATS 拉訊息速度 > Core 處理速度時，記憶體裡 in-flight events 累積 → OOM。原計畫沒提這件事。

### 做法：Pull-based 流控

Flink 整條 pipeline 用 blocking queue 串接，下游慢上游就停拉。NATS pull consumer 天然支援：

```go
// Pull consumer，限制同時 in-flight 訊息數
consumer, _ := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
    AckPolicy:     jetstream.AckExplicitPolicy,
    MaxAckPending: 1000,  // 未 ack 上限，超過 NATS 不再送新的
})

msgs, _ := consumer.Messages(jetstream.PullMaxMessages(100))
for {
    msg, _ := msgs.Next()  // 處理完才 Next() 拉下一批
    c.processEvent(decode(msg))
    msg.Ack()              // ack 後 NATS 才補新訊息
}
```

### 監控指標

Backpressure 相關 metric 已整合到 §Observability 的「Throughput / Latency」「NATS」分組（`core_in_flight_events`、`nats_consumer_pending`、`core_event_latency_seconds`、`watermark_lag_seconds`）。對應的告警規則見 §Observability「基本告警規則」。

**判讀**：當 `core_in_flight_events` 持續貼到 `MaxAckPending` → Core 跟不上，需要加 shard 或優化處理（按 §Benchmark Roadmap 進到下個 milestone）。

---

## Schema Evolution & State Migration

Flink 對 state schema 變更有正式的 migration 機制。我們套用簡化版：

### 加欄位

Protobuf backward compatible，舊 snapshot 直接讀，新欄位填 zero value。**無需特殊處理**。

### 改型別 / 刪欄位

破壞性變更。流程：
1. Snapshot header 有 `schema_version`，啟動時檢查
2. 寫 migration code：`v1 → v2` 函式，啟動時掃所有 key group snapshot 轉換
3. 轉換完寫回新 version snapshot，原檔備份保留 N 天
4. 多版本 schema 必要時用 chain：v1 → v2 → v3 各寫一個 migrator

### Operator 邏輯變更（不改 schema）

例如改變聚合計算公式。這種無法靠 snapshot 修復，**需要從 NATS 重 replay 全歷史**重建狀態。
這也是為什麼 NATS stream 的 retention 要設足夠長（建議 ≥ 30 天）。

---

## 時間語意與 Watermark（Flink-inspired）

### 三個時間概念，要分清楚

- **Event time**: `event.OccurredAt`（事件實際發生時間）→ 規則 / window 唯一可信來源
- **Ingestion time**: 事件進 NATS 的時間 → 監控用
- **Processing time**: `time.Now()` 在 Core 內 → **絕對不能用於規則邏輯**

```go
// ❌ 錯（replay 時 wall clock 是現在，event 是過去）
if time.Now().Sub(progress.StartedAt) > maxWait { ... }

// ✅ 對
if currentWatermark.Sub(progress.StartedAt) > maxWait { ... }
```

### Watermark 機制

跨 producer / 跨 NATS subject 的事件不保證 OccurredAt 順序到達。如果不處理，「最近 1 分鐘事件數」這種 window 會錯算（遲到事件被丟到後一個 bucket，或先觸發了還沒收齊資料的 window）。

借用 Flink 做法：

```go
type Core struct {
    watermark      atomic.Int64  // unix nano: "之後不會再有 OccurredAt < W 的事件"
    allowedLateness time.Duration // 預設 5s
}

func (c *Core) processEvent(event Event) {
    eventTs := event.OccurredAt.UnixNano()
    
    // 1. 遲到判斷
    if eventTs < c.watermark.Load() {
        c.handleLateEvent(event)
        return
    }
    
    // 2. 推進 watermark = max(seen) - allowedLateness
    candidate := eventTs - c.allowedLateness.Nanoseconds()
    c.advanceWatermark(candidate)
    
    // 3. 觸發 ready window（OccurredAt 整個 < watermark 的 bucket 才結算）
    c.triggerReadyWindows(c.watermark.Load())
    
    // 4. 應用事件
    c.applyEvent(event)
}
```

### Allowed lateness

預設 `allowedLateness = 5s`：watermark 永遠落後最大 OccurredAt 5 秒，給遲到事件容忍空間。可調，越大延遲越高、容忍越強。

### 遲到事件處理

```go
func (c *Core) handleLateEvent(event Event) {
    // 還在容忍範圍 → 補進對應 bucket，重觸發該 bucket 的 window 評估
    if event.OccurredAt.After(time.Unix(0, c.watermark.Load()-maxLateness.Nanoseconds())) {
        c.applyEvent(event)
        c.recomputeAffectedWindows(event)
        return
    }
    // 太遲 → side output（寫 audit log 或丟棄，不影響主流程）
    c.lateEventSink.Emit(event)
}
```

### Watermark 來源策略

| 策略 | 描述 | 適用 |
|------|------|------|
| Per-shard 自推 | 各 shard 用 `max(OccurredAt seen) - allowedLateness` | 簡單，適用 producer 端時序穩定的場景 |
| Source-driven | 上游定期送 watermark heartbeat 訊息 | 上游能保證 watermark 推進時 |
| Idle source 處理 | 該 shard 沒事件時 watermark 卡住 → 用 timer 推進 | 流量不均時必要 |

第一版用 per-shard 自推，加 idle timer fallback。

---

## Sharding 與 Rescaling

### Key group 帶來的能力

因為採用 key group 機制（見 §1），rescaling 不再需要重 hash 1M member。

```
從 4 → 8 shard 的步驟：
1. 暫停 producer（or 上游 buffer）
2. 等現有 shard 處理完 NATS 已有訊息（並寫 final checkpoint）
3. 重算 key_group → shard assignment（128 個 key group → 8 shard，每 shard 16 個）
4. 各 shard 讀取「自己現在負責的 key group」的 snapshot 段
5. 啟動，從 NATS 上次 checkpoint 對應的 seq replay
6. 恢復 producer
```

**目標 downtime: < 1 分鐘**。

### Snapshot 按 key group 分檔

每個 key group 一份 snapshot 子檔，shard 啟動時讀取屬於自己的那批：

```
snapshots/
  ├─ kg_000/barrier_*.snap   ← 哪個 shard 負責就由哪個讀
  ├─ kg_001/barrier_*.snap
  ├─ ...
  └─ kg_127/barrier_*.snap
```

### 動態 rescaling（不停機）— v2

不停機 rescaling 仍然複雜（要 mid-stream 切 ownership、處理 in-flight events 的歸屬交接），第一版不做。但 **key group 設計讓未來實作這件事的成本降一個數量級**：只需要協調 128 個 key group 的 ownership 切換，不用搬 1M member 的 state。

Flink 的 rescaling 也是 key group 級別的協調，可參考其 `KeyGroupAssignment` 演算法。

---

## 不停機升級到 Per-Shard Stream

初版採用「單 stream + subject filter」模式（見 §1 三層概念的物理對應）。當撞到單 stream 吞吐上限時（見 §Risks §1），要升級成「每 shard 一個獨立 stream」做物理隔離。本節說明**如何不停機完成這個升級**。

### 觸發升級的條件

| 訊號 | 意義 |
|------|------|
| Stream publish rate > 80% theoretical max | 已經貼上限，要 scale out |
| Hot shard 拖累其他 shard 的 delivery latency | I/O 競爭已經有實質影響 |
| 需要不同 shard 不同 retention | 需要 config 層級隔離 |
| 想跨 region 部署不同 shard | 物理放置需要分開 |

沒撞到這些前，**單 stream + subject filter 是 sweet spot**：簡單、rescale 容易、checkpoint 模型乾淨。

### 升級的可行性基礎

剛好可以重用本計畫已經設計好的兩件事：
1. **`event_id` 冪等性**（§冪等性保證）：同一個 event 處理兩次結果一樣
2. **Per-shard 獨立 checkpoint**（§Checkpoint）：每個 shard 互不影響

這兩個讓「一次搬一個 shard、新舊 stream 重疊期間有少量重複處理也沒關係」變成可能。

### 三種可行做法

| 做法 | 機制 | 適用 |
|------|------|------|
| **A. Producer 雙寫 + 滾動切換** | Application 層 | 最常用、控制最多（推薦）|
| **B. NATS Stream Sourcing** | NATS 內建 server-side 複製 | 不能改 producer 時 |
| **C. Subject Mapping** | NATS 2.10+ subject 重寫 | Producer 完全 transparent |

下面詳細說明做法 A，做法 B/C 在最後對照。

### 做法 A：Producer 雙寫 + 滾動切換（推薦）

#### Step 1: 建好新 streams（被動接收，無 consumer）

```
舊： Stream "rule-events"           (existing, active)
新： Stream "rule-events-shard-0"   (created, idle)
     Stream "rule-events-shard-1"
     Stream "rule-events-shard-2"
     Stream "rule-events-shard-3"

Consumer 都還在舊 stream 上跑
```

#### Step 2: Producer 開始雙寫

```go
func Publish(event Event) error {
    shardID := computeShard(event.MemberID)

    // 寫舊 stream（current source of truth）
    if err := publishOld(event); err != nil {
        return err  // 失敗就 retry，新 stream 也不寫
    }

    // 寫新 stream（best-effort）
    if err := publishNew(event); err != nil {
        // 不 fail 整個請求，只記 metric
        // 之後對帳腳本會發現少訊息，可從舊 stream 補
        metrics.NewStreamPublishFailed.Inc()
    }
    return nil
}
```

**舊 stream 永遠是 source of truth**，直到 step 4 切換為止。雙寫的新 stream 即使偶爾失敗也 OK，可以用對帳腳本後補。

#### Step 3: 滾動切換 shard（一次切一個）

```
for shardID in [0, 1, 2, 3]:

  ① 在新 stream 上記下「當前 live position」
     liveSeq := newStream.LastSeq()  // 例如 8745

  ② 觸發 shard N 寫 final checkpoint
     core_old.TriggerBarrier()
     wait until barrier_done
     → 這時 shard N 的 in-memory state 已涵蓋舊 stream 所有訊息

  ③ 停掉舊 consumer
     core_old.Stop()

  ④ 啟動新 consumer（讀新 stream，從 liveSeq 之後開始）
     core_new := NewCore(
         stream: "rule-events-shard-N",
         startFrom: liveSeq + 1,    ★ 不從 1 開始，避免重 replay 幾天份
     )
     core_new.LoadSnapshot()        // 讀剛剛 step ② 的 snapshot
     core_new.Run()

  ⑤ 觀察 5-10 分鐘確認穩定 → 進下個 shard

每個 shard pause 期間（③→④）≈ 秒級，其他 shard 完全不受影響
```

#### Step 4: 停止雙寫

```
所有 shard 切完後，producer 停止寫舊 stream，只寫新 stream
```

#### Step 5: 清理

```
舊 stream "rule-events" 等 30 天 retention 過期後 delete
（或確認新架構穩定 1 週後直接 delete）
```

### 為什麼不會丟訊息也不會重複算

```
舊 stream 訊息流（shard 0 視角）：
  seq=1000 ← 舊 consumer 處理到這
  seq=1001 ← final barrier 觸發
  seq=1002 ← 還沒處理，但這時舊 consumer 停了

新 stream 訊息流（shard 0 視角）：
  seq=5000   |
  seq=5001   ├─ 雙寫期累積的訊息
  seq=5002   |
  seq=5003 ← liveSeq 記錄這個點
  seq=5004 ← 新 consumer 從這開始
  seq=5005

關鍵：
  ─ 舊 stream seq=1002 = 新 stream seq=5004（同一個 event_id）
    這筆事件實際上 publish 了兩次（雙寫），event_id 一樣
  ─ 舊 consumer 沒處理 → snapshot 不含這筆
  ─ 新 consumer 從 5004 開始拉 → 處理這筆 → ProcessedEventIDs 標記
  ─ 不會遺失 ✓
  ─ 不會重複算（event_id 第一次處理）✓
```

### 系統整體時序

```
時間 ─────────────────────────────────────────────────────▶

舊 stream:        ─────────────────────────[stop write]──aged out───
                          ▲                       ▲
                     producer 雙寫            producer 停雙寫

新 streams:        ──────[created]──────[shard-3 切完]────onwards──
                          ▼                       ▼
                     producer 雙寫           成為唯一 source

shard-0 consumer: ═══[old]═════[pause]═[new]═══════════════════════
shard-1 consumer: ═══[old]═════════════[pause]═[new]═══════════════
shard-2 consumer: ═══[old]═══════════════════[pause]═[new]═════════
shard-3 consumer: ═══[old]═══════════════════════════[pause]═[new]═

                  ↑pause 區間 ≈ 秒級（停舊 + 起新）
                  每 shard 切換期間，其他 shard 完全不受影響
```

**整個系統視角**：永遠有 3/4 的 shard 在正常服務，只有切換中的那 1 個 shard 短暫 pause（producer 雙寫所以訊息不丟，只是延遲 ~秒級處理）。

### 做法 B：NATS Stream Sourcing

NATS JetStream 內建 **Stream Sourcing**——server 端自動把訊息從一個 stream 複製到另一個：

```
建新 stream 時宣告 source：

  Stream rule-events-shard-0:
    sources:
      - name: "rule-events"
        filter_subject: "rule.events.0.>"
        opt_start_seq: 1   # 或 opt_start_time
```

**好處**：Producer 一行不用改、不影響原 stream 寫入效能。  
**壞處 / 注意點**：
- Source 過來的訊息**會得到新 stream 的新 seq**（原 seq 保留在 message metadata 裡 `Nats-Sequence-Source`）
- Sourcing 是 best-effort，切換時要 wait 新 stream `LastSeq` 追到舊的
- 無法永久代替 producer cutover；最終 producer 還是要直接寫新 stream

實務上是「**用 sourcing 填歷史 + 做法 A 滾動切換**」的混合做法：
1. 建新 stream，配 sourcing 從舊 stream 抽歷史
2. 等新 stream 追上 live
3. 走做法 A Step 3-5 的滾動切換

### 做法 C：Subject Mapping（NATS 2.10+）

利用 server-side subject mapping：

```
# server config
mappings:
  - src: "rule.events.0.>"
    dest: "rule.events.shard0.>"   # 路由到新 stream
```

Producer 還是發 `rule.events.0.user_a`，server 自動改寫 subject 落到新 stream。

**好處**：完全 transparent，producer 不感知。  
**壞處**：適合「就改一次然後永遠用 mapping」的場景，不適合滾動遷移；mapping 切換瞬間舊訊息可能還在舊 stream 裡，要配合 sourcing 才完整。

對本計畫場景做法 A 比較直白，C 不太合適。

### 風險清單

| 風險 | 緩解 |
|------|------|
| 雙寫期間兩個 stream 訊息順序不一致 | 不靠 stream 順序判斷，靠 `event.OccurredAt` + event_id |
| 新 consumer 從 `liveSeq+1` 開始，但邊界訊息已被舊 consumer 處理 | event_id 冪等保證重複也沒事 |
| 雙寫期間磁碟用量翻倍 | 短期內可接受；切換後舊 stream age out |
| Producer 雙寫成本 | 每 publish 多一次 NATS round-trip；可改 async publish 攤平 |
| 某 shard 切換後出問題 | 立即 revert：恢復舊 consumer（舊 stream 還有訊息）|

### 升級期間的可觀測性

切換期間額外監控：

| Metric | 健康範圍 |
|--------|---------|
| `migration.dual_write_lag` | 新舊 stream LastSeq 差異 < 100 |
| `migration.new_stream_publish_fail_rate` | < 0.01% |
| `migration.duplicate_events_dedup_rate` | 切換瞬間有 spike 正常，5min 內回 0 |
| `core.process_latency_p99` (per shard) | 切換後 5min 內回到切換前水平 |

### TL;DR

- 不需要停機，靠「**滾動切換 + event_id 冪等**」達成
- 每個 shard 切換期間只有秒級 pause，其他 shard 持續服務
- 失敗可立即 revert（舊 stream 還在）
- 推薦做法 A（Producer 雙寫），需要時混搭做法 B（Sourcing）填歷史

---

## Hot Key Handling

### 問題

百萬級系統的流量幾乎一定是 **power-law 分布**（Zipfian）：少數熱門 member 占整體流量很大比例。對 single-goroutine-per-shard 架構這是致命傷。

```
真實場景模擬：

某 shard 流量 25K events/s，其中：
  熱門 member user_X：     8K events/s（占 32%）
  熱門 member user_Y：     3K events/s（占 12%）
  其他 ~10000 個 member： 14K events/s 平均分散

Single goroutine 處理：
  ─ user_X 一個 member 序列化 32% 的處理時間
  ─ user_X 的 event 處理 latency 變高
    （state 大、CEP progress 多、aggregation bucket 多）
  ─ 其他 9999 個 member 陪跑，被 user_X 拖累
  ─ 全 shard p99 latency 被 hot member 推高
```

**根本原因**：single goroutine 是 §核心元件設計 的關鍵假設（保 FIFO + 無鎖），熱門 member 直接撞這個瓶頸。

### 偵測機制

最小可用版本：per-member rolling counter（每 shard 自己維護）。

```go
type HotKeyDetector struct {
    counters  map[uint64]*atomic.Int64  // memberID hash → 本視窗 event 數
    threshold int64                      // events/sec 閾值
    window    time.Duration              // 滑動視窗，預設 60s
}

func (d *HotKeyDetector) Track(memberID uint64) {
    cnt, ok := d.counters[memberID]
    if !ok {
        cnt = &atomic.Int64{}
        d.counters[memberID] = cnt
    }
    cnt.Add(1)
}

func (d *HotKeyDetector) Hot() []HotMember {
    var hot []HotMember
    for id, cnt := range d.counters {
        rate := cnt.Load() / int64(d.window.Seconds())
        if rate > d.threshold {
            hot = append(hot, HotMember{ID: id, Rate: rate})
        }
    }
    return hot
}
```

**閾值定法**：

```
shard 目標 throughput = T events/s
hot threshold = T × 5%
（per-member 流量超過全 shard 5% 就視為 hot）

例：shard 25K events/s → threshold = 1250 events/s/member
```

Production 版可用 **Count-Min Sketch** 省記憶體；side project 用 plain map 夠（百萬 member 才 ~50MB）。

### Mitigation：Hot Member Quarantine

把 hot member 隔離到「專屬 shard」（dedicated process）：

```
偵測到 user_X 流量 > 5% threshold
  ↓
觸發 quarantine：把 user_X 從原 shard 搬到 dedicated shard
  ↓
原 shard 解脫；user_X 獨佔一個 process
dedicated shard 可以：
  ─ 用更多 CPU / memory
  ─ 跑 intra-shard worker pool（因為只剩一個 member，沒 ordering 衝突）
  ─ 客製化 GC 設定
```

**為什麼選這個做法**：完全保留 per-member 規則語意（CEP / aggregation 都對），對其他 cold member 零影響。代價是需要 control plane 維護 routing override + dedicated process 的資源成本——可接受。

#### Routing 機制

Producer 端維護一個 hot-member override table：

```go
func computeShard(memberID string) int {
    // 1. 查 hot-member override
    if shardID, ok := hotMemberOverride[memberID]; ok {
        return shardID  // 走 dedicated shard
    }
    // 2. 正常 kg → shard mapping
    kgID := crc32(memberID) % 128
    return kgID * numShards / 128
}
```

Override table 從 control plane（NATS KV bucket `hot_member_routing`）讀，KV watch 推播變更，所有 producer instance 即時同步。

#### 搬遷流程

直接重用 §不停機升級到 Per-Shard Stream 的「Producer 雙寫 + drain」pattern：

1. Producer 把該 member 雙寫到 old shard + dedicated shard
2. 等 old shard 處理完該 member 的所有 in-flight events，寫 final snapshot 把該 member 的 state 包進去
3. Dedicated shard 載入該 member state（用 §Snapshot 格式 的 manifest 機制）
4. Producer 停雙寫，只寫 dedicated shard

每次搬遷期間，該 member 的 event 有短暫雙寫但不丟、不亂序（靠 event_id 冪等 + drain 順序保證）。

### 漸進式三階段路徑

對 side project，建議分階段做：

```
Phase 1（必做）：加 detector，被動偵測，只 emit metric
  ─ 學 power-law 分布在實際資料上長什麼樣
  ─ 不做任何 mitigation，先觀察 1~2 週
  ─ 收集真實 threshold 應該定多少的依據

Phase 2（M2 milestone 之後）：手動 quarantine
  ─ 看到 metric 告警 → 手動執行 quarantine command
  ─ 學 hot member 搬遷的具體實作（snapshot transfer + routing override）

Phase 3（v2，可選）：自動 quarantine
  ─ Detector 直接寫 override table 觸發搬遷
  ─ Control plane 自動協調
  ─ 學 control plane 設計、auto-balance 演算法
```

```
Phase 1（必做）：加 detector，被動偵測，只 emit metric
  ─ 學 power-law 分布在實際資料上長什麼樣
  ─ 不做任何 mitigation，先觀察 1~2 週
  ─ 收集真實 threshold 應該定多少的依據

Phase 2（M2 milestone 之後）：手動 quarantine
  ─ 看到 metric 告警 → 手動執行 quarantine command
  ─ 學 hot member 搬遷的具體實作（snapshot transfer + routing override）

Phase 3（v2，可選）：自動 quarantine
  ─ Detector 直接寫 override table 觸發搬遷
  ─ Control plane 自動協調
  ─ 學 control plane 設計、auto-balance 演算法
```

**不要一開始就做 Phase 3**——先觀察才知道 threshold 怎麼定、搬遷頻率合不合理。

### 已知限制與邊界

| 情境 | 處理方式 |
|------|---------|
| Hot member churn 高（不斷新出現 / 消失） | Quarantine 頻繁觸發 → oscillation 風險；加 hysteresis：連續 N 分鐘超過閾值才搬，連續 M 分鐘低於閾值才收回 |
| Hot member 跨 shard 的歷史合併 | 一個 member 進 dedicated shard 後，歷史 state 跟著走；要「收回」原 shard 需要反向 snapshot transfer，第一版不做 |
| 同時太多 hot member | 每個都 quarantine → 太多 dedicated process；限制 `max_quarantined_count`（例如 8 個），超過就告警等人工處理 |
| Dedicated shard 自己變 bottleneck | 只能靠 intra-shard parallelism（dedicated 內因為只有一個 member，可以 worker pool）或最後手段 sub-key 拆分（接受語意代價） |
| Quarantine 期間 producer routing 切換不一致 | Producer 用 NATS KV watch 即時推播，配合 §不停機升級的「雙寫 + drain」確保切換期 0 訊息丟失 |

### Metric 暴露

```
Metric                                  意義
─────────────────────────────────────   ─────────────────
hotkey.member_rate{member_id}            Top-N hot member 的 rate
hotkey.threshold                         當前閾值
hotkey.quarantined_count                 已 quarantine 的 member 數
hotkey.quarantine_triggered_total        累計觸發次數（觀察 churn）
hotkey.shard_skew_ratio{shard_id}        max(member_rate) / avg(member_rate)
                                         → 越大代表流量越不均
```

---

## 跟現有架構的比較

| 面向 | 現況（Phase 8 + Redis） | In-Memory Rule Engine |
|------|---------------------|-------------------|
| 單 shard throughput | ~10K events/s | **100K+** events/s |
| State 儲存 | Redis | In-memory + NATS log |
| CEP 成本 | SMEMBERS + MGET per event | 純 memory |
| Aggregation 成本 | ZRANGEBYSCORE + parse | In-memory bucket ops |
| 外部 I/O per event | 2-3 Redis round-trips | **0** |
| Restart 時間 | 即時（stateless）| **~1 s**（incremental snapshot + replay）|
| Source of truth | Redis | NATS JetStream log |
| 程式模型 | Sync API | Async consumer |
| Sharding 抽象 | N/A | Key group（128 個 fixed）→ shard 動態映射 |
| Snapshot 機制 | N/A | Async barrier + incremental（dirty key group only）|
| Out-of-order 事件 | 不處理（隱含信任）| Watermark + allowed lateness + 遲到 path |
| Result 輸出語意 | At-least-once（client 自己 dedup）| **End-to-end exactly-once**（two-phase commit sink）|
| Backpressure | N/A | Pull consumer + `MaxAckPending` |
| Schema 變更 | N/A | `schema_version` + migration |
| Operational 複雜度 | Redis + API | NATS + checkpoint + watermark + sink coord |

---

## Implementation Phases

### Phase 1: 單 shard 原型 + Key group 抽象（2 週）

目標：單 shard Rule Engine Core 跑通到可 benchmark 狀態，**從第一天就有 key group 抽象**。

交付：
- [ ] `service/engine/core/` 新 package
- [ ] `cmd/rule-engine-core/main.go` entry point
- [ ] Key group 抽象（`KeyGroupID`、`memberID → keyGroup → shard` 兩段映射）
- [ ] Protobuf schema + 程式碼生成（含 `key_group_id` 欄位）
- [ ] 單 shard processEvent 邏輯（aggregation + rule eval + CEP 都 in-memory）
- [ ] NATS JetStream consumer 接入（subject pattern: `rule.events.{shard_id}.{member_id}`）
- [ ] 簡易 per-key-group snapshot dump + load

Benchmark 目標：單 shard > 50K events/s。

### Phase 2: Async Barrier Checkpointing + Replay（2 週）

目標：實作 Flink-style async barrier snapshotting + incremental checkpoint。

交付：
- [ ] Barrier 注入機制（source 定期送 barrier marker 到 NATS，or in-process 觸發）
- [ ] Dirty key group bitset 追蹤
- [ ] 背景 snapshot goroutine（atomic swap + 寫 dirty key groups）
- [ ] Per-key-group snapshot 檔案、atomic rename、NATS KV 寫 `(barrier_id, nats_seq)`
- [ ] Replay 時壓制 side effect
- [ ] 冪等重播的 event_id dedup
- [ ] 災難測試：`kill -9` 在 barrier 處理中、寫 snapshot 中各種時點，驗證一致性
- [ ] Restart time benchmark（目標 < 1.5s）

### Phase 3: Watermark + 遲到事件處理（1 週）

目標：建立 event-time 語意，正確處理 out-of-order events。

交付：
- [ ] Watermark 推進邏輯（per-shard `max(OccurredAt) - allowedLateness`）
- [ ] Idle source timer fallback
- [ ] Window 觸發改為 watermark-driven（不是 wall clock）
- [ ] 遲到事件 path：補進 bucket + window 重算 / side output
- [ ] 測試：注入 out-of-order 事件流，驗證 window 結果跟 in-order 一致

### Phase 4: Multi-shard + Backpressure（1 週）

交付：
- [ ] 4 個 shard 同 process 跑（goroutine 隔離 + 各自 NATS consumer）
- [ ] Pull consumer + `MaxAckPending` 配置
- [ ] Per-shard metrics（throughput、lag、memory、watermark_lag）
- [ ] Backpressure 觸發測試：人為慢化 Core，驗證 NATS 端正確擋住

### Phase 5: End-to-End Exactly-Once Sink（1 週，僅在啟用 result output 時）

交付：
- [ ] Two-phase commit sink interface
- [ ] Kafka transactional producer sink（範例實作）
- [ ] 等冪 webhook sink（範例實作）
- [ ] 測試：在 pre-commit / commit 之間 kill，驗證下游零重複、零遺失

### Phase 6: Rescaling 工具（1 週）

交付：
- [ ] `key_group → shard` assignment 重算工具
- [ ] Snapshot 段搬移腳本
- [ ] 4 → 8 shard 演練腳本（停機 < 1 分鐘）

### Phase 7: Production hardening（2 週）

交付：
- [ ] Graceful shutdown（flush in-flight events、寫 final checkpoint、fsync）
- [ ] Monitoring / alerting（memory、NATS lag、checkpoint 失敗率、watermark stall）
- [ ] Schema versioning + migration 工具
- [ ] Load test：連續 24h 跑，確認 memory 不漏、checkpoint 週期正常
- [ ] 混沌測試（snapshot 寫入中 kill、NATS server restart、磁碟滿）

### 總預估：**10 週**（1 位資深 Go engineer）

> 比原本估的 6 週多出 4 週，主要在 watermark、async barrier checkpointing、exactly-once sink 這三塊——這也正是「自己刻 streaming runtime」相對於用 Flink 的真實成本。

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

已採 Flink async barrier snapshotting + incremental checkpoint（見 §Checkpoint 機制）：
- 主 goroutine 看到 barrier 只 swap dirty bitset，不阻塞
- 背景 goroutine 寫只變動的 key group（典型 ~5% 全 state）
- 預期單次 checkpoint 寫入 < 100 ms 背景時間，主路徑近 zero pause

### 5. Schema Evolution

見 §Schema Evolution & State Migration。

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
- Throughput benchmark：按 §Benchmark Roadmap 的 M1~M5 milestone 推進，每階段壓到通過條件
- Restart benchmark：`kill -9` → 起來 → 處理第一個新 event 的時間 < 3s
- Memory benchmark：100 萬 member × 7 天資料，memory < 10 GB

### 混沌測試
- Kill Core during snapshot
- Kill Core during replay
- NATS server restart 期間的事件丟失率
- Snapshot file 損壞的 fallback（自動用更舊的 snapshot）
