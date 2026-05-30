# In-Memory Rule Engine — 計畫缺漏評估

本文是 [in-memory-rule-engine-plan.md](./in-memory-rule-engine-plan.md) 的評估清單。

## Context：這是 Side Project

本計畫**不是 production 系統**，是探索性的 side project。沒有實際的業務 throughput 需求、沒有 SLA 約束、沒有 production 運維壓力。因此：

- 「目標 throughput」應該服務於「想學到 / 想證明什麼」，而不是業務需求
- 很多 production 視角的「缺漏」在 side project 視角下**根本不該叫缺漏**（例如 multi-region DR、TCO 對比）
- 真正值得花時間的是 **architectural / learning 類**的設計題

下面分類已按此 context 重新校準。

**使用方式**：一項一項看，決定「補進 plan / 列入 v2 / 標記為已知限制 / 不採用」。決策後在該項打勾並註記。

---

## 總體評估

| 面向 | 評分 | Side project 視角備註 |
|------|------|---------------------|
| 架構設計 | ★★★★★ | 跟 Flink 對照完整，借鏡論證紮實 |
| 實作可行性 | ★★★★☆ | 核心邏輯走得通 |
| Correctness | ★★★★☆ | Snapshot 跨檔原子性還要補 |
| Learning 價值 | ★★★★★ | 涵蓋 streaming、event sourcing、CEP、performance 等多領域 |

---

## 🔴 Critical：Correctness 不可妥協

### [x] #3 Snapshot 跨檔案的原子性沒講清楚

**問題**：§Checkpoint 講「每個 kg 一份檔案 atomic rename」「全部寫完才更新 KV 的 barrier_id」——但「全部寫完中途崩潰」的恢復語意不明確。

```
情境：dirty = {3, 7, 42, 99}
寫成功 kg_3、kg_7
寫到 kg_42 時 kill -9
KV 的 barrier 還是上一個

重啟時：
  - kg_3, kg_7 已經有 barrier_42 的 snap 檔（但不該用）
  - kg_42, kg_99 沒有
  → 該用哪個 barrier？
```

**決策**：採解法 3（Manifest 檔）✓

新增「barrier_XXX.manifest」檔當作整個 barrier 的單一 commit point：
- 寫完所有 .snap 檔後，atomic rename manifest 檔
- KV 為最終承諾點（manifest 存在但 KV 未更新 → 還是用上一個 barrier）
- 載入時掃磁碟清理 garbage：比 KV 新的 manifest、所有 .tmp 檔、沒被 manifest 引用的 .snap
- Manifest 含 sha256 verify 整合性
- 沒在 barrier_42.manifest 的 kg → 往前找最近一次 manifest（kg 沒 dirty 就沒新版本）

實作細節已寫入 plan：§Incremental、§Snapshot 格式、§Restart 流程。

---

## 🟡 Architectural / Learning：Side project 最有價值的部分

這些是值得花時間做的——每一項都是 distributed system / streaming engine 的經典設計題。

### [x] #2 熱點 Member 沒有對策（power-law 分布經典題）

**問題**：百萬級系統必有 power-law 流量分布。少數網紅 / 機器人 / 高頻交易帳戶占整體流量很大比例。

```
真實場景：1 個熱門帳戶占 shard 流量 30%
single goroutine 處理 → 該 member 序列化所有 event
→ 該 shard latency spike → 整個 shard 卡住
→ 連帶其他冷 member 一起慢
```

**決策**：採 Hot Member Quarantine（dedicated shard 隔離）✓

新增 §Hot Key Handling 章節（位於 §不停機升級到 Per-Shard Stream 之後）：
- 問題分析（power-law 對 single-goroutine 架構的衝擊）
- 偵測機制（per-member rolling counter + 閾值 5% 全 shard 流量）
- **Mitigation**：Hot Member Quarantine
  - Routing：producer 端 override table，從 NATS KV bucket 讀（watch 推播即時同步）
  - 搬遷流程：重用 §不停機升級到 Per-Shard Stream 的「雙寫 + drain」pattern
  - 保留 per-member 語意（CEP / aggregation 都對）
- 漸進式 3 階段（被動偵測 → 手動 quarantine → 自動 v2）
- 已知限制（churn、跨 shard 歷史、太多 hot、dedicated bottleneck、routing 切換）
- Metric 暴露 spec

備選方案（sub-key 拆分 / coarser aggregation / read fan-out）已從 plan 移除，不採用。

---

### [x] #4 可觀測性章節幾乎空白（prometheus + grafana 入門練習）

**問題**：整份計畫只有 §Backpressure 那 4 個 metric。Side project 想壓 100K events/s 沒 metric 等於盲跑。

**Side project 視角的簡化版**：不需要 production 級的 distributed tracing、audit log、合規。只要：

```
最小可用 metric set：
  □ events processed per second（per shard）
  □ process latency p50/p95/p99（per shard）
  □ memory usage（per shard）
  □ in_flight events (NATS MaxAckPending 距離)
  □ checkpoint duration + dirty kg 數量
  □ matched rules / patterns count（per rule_id）
  
+ 一個 simple HTTP endpoint：
  □ GET /debug/member/:id → 看某個 member 的 state（aggregations + progresses）
```

**建議**：補一節「## Observability」，含上述簡化 metric set + prometheus 整合 + 一個 debug HTTP endpoint。

**決策**：（待填）

---

### [ ] #5 Rule Hot-reload 的語意沒講

**問題**：`ruleCache atomic.Pointer[CompiledRuleSet]` 只說「換引用」，但業務語意完全沒交代：

- Rule 改了之後，**正在進行中的 CEP progress** 用舊 pattern 還是新 pattern 繼續？
- 規則刪除時，正在進行的 progress 立即 abort 還是讓它跑完？
- 新增規則時，**過去 N 分鐘已發生的 event** 算不算數？
- Rule 版本如何標記？matched_result 要記哪個版本的規則？

**Learning 價值**：State machine + 動態 config 切換的設計題，跟 Kubernetes operator、feature flag 系統的問題類似。

**建議**：補一節「## Rule Lifecycle」，定義 version、in-flight progress 處理、retroactive 評估策略。

**決策**：（待填）

---

### [ ] #9 NATS 跟 Core 故障模式沒列（chaos engineering 入門）

**問題**：§Risks 列了 5 個但都是 capacity 類風險，故障模式類風險空白。

**情境**：
- NATS cluster minority partition，Core 還能讀但寫 KV 失敗？
- Core 寫 snapshot 中途磁碟滿？
- Core process 之間時鐘漂移大（影響 watermark）？
- NATS 從某版本升級，stream 暫時不可寫？

**Learning 價值**：chaos engineering 是 distributed system 的必修課，side project 跑 chaos test 也很有趣。

**建議**：§Risks 加 1 節「故障模式」，列舉並給 mitigation；Phase 7 的混沌測試擴充具體 scenario list。

**決策**：（待填）

---

### [ ] #11 CPU 利用率：4 shard 在 32-core 機器上 28 core idle

**問題**：Single-goroutine-per-shard 模型下，shard 數量 = 並行度上限。如果硬體是 32-core 機器跑 4 shard，**28 core 一直閒著**。沒有 intra-shard parallelism 設計。

**Learning 價值**：這是 LMAX / actor model / single-writer principle 的核心 trade-off，值得深思。

**建議**：選一個方向：
- (a) Shard 數量必須 ≥ core 數量（規範化）
- (b) 設計 intra-shard worker pool（破壞 single-goroutine 的 FIFO 假設，需要 per-member ordering 機制）
- (c) 接受「single goroutine 就是極限」，靠加 shard 達到 parallelism

**決策**：（待填）

---

### [ ] #12 CEP 設計缺 Negative Pattern

**問題**：常見需求「event A 發生後 5 分鐘內沒有 event B」（例：「下單後 5 分鐘沒付款」「登入後 1 分鐘沒驗證」）。

目前 `compiledPattern` 只有正向序列。Negative pattern 需要 timer-driven 觸發（時間到還沒 B 就 trigger），跟現在的 event-driven 模型不同。

**Learning 價值**：CEP 完整性 + timer 機制設計是有趣的題目（Flink CEP 有完整解法可借鏡）。

**建議**：
- 要嘛**明確說「不支援 negative，已知限制」**
- 要嘛**設計 timer 機制**（per-progress timer 註冊、watermark 推進時掃 timer）

**決策**：（待填）

---

### [ ] #14 Memory Management 細節不足（想壓到 100K 就會碰到）

**問題**：`Risks §3` 只說「LRU + 7 天 threshold」，但：

- LRU 用什麼資料結構？（百萬 member 的 LRU 不是 toy 實作）
- Eviction 觸發條件？（記憶體用量上限？per-shard 限制？）
- Evicted member 再回來時的「冷啟動 NATS replay」效能 / 一致性？
- Go GC 對大 in-memory state 的影響？（百萬 map entry 的 GC pause）

對 100K events/s 的目標，**GC pause > 10ms 就會打掛 latency**。

**Learning 價值**：Go performance tuning + GC 行為理解，pprof 練習。

**建議**：擴充 §Risks §3 或新建 §Memory Management，含：
- LRU 實作選型（container/list + map / [hashicorp/golang-lru](https://github.com/hashicorp/golang-lru)）
- Eviction 觸發策略
- 冷啟動 replay 邊界
- Go GC 調優（GOGC、sync.Pool、object pool 使用）

**決策**：（待填）

---

### [ ] #16 ADR / 決策文件（portfolio 加分項）

**問題**：這份計畫的關鍵決策（為何 K=128、為何 single goroutine、為何不用 Flink）論證很完整，但**沒有獨立 ADR 文件**。

**Learning 價值**：side project 作為 portfolio piece，ADR 是展示「能做 architecture-level decision」的好證據。

**建議**：拆出 `docs/adr/` 目錄，至少寫這幾個 ADR：
- ADR-001: 採用 NATS JetStream 而非 Kafka
- ADR-002: K=128 key group 的選擇
- ADR-003: Single goroutine per shard
- ADR-004: 自刻 vs Flink

**決策**：（待填）

---

### [ ] #18 Testing Strategy（PBT 學習機會）

**問題**：
- 確定性測試只說「replay 兩次 state 一致」，沒講具體怎麼驗（diff snapshot？）
- Shadow traffic 比對的「容忍誤差範圍」？
- Property-based test 完全沒提（CEP 狀態機適合 PBT）

**Learning 價值**：Side project 學 property-based testing 是個好機會，CEP 狀態機剛好是 PBT 經典應用場景。

**建議**：擴充 §驗證方式，加：
- Determinism test 細節（snapshot binary diff / 結構化比對）
- PBT 工具選型（[gopter](https://github.com/leanovate/gopter)）+ CEP property 範例
- CI pipeline 整合

**決策**：（待填）

---

## 🟢 Nice to have

### [x] #1 Throughput 目標重新校準（改為 Benchmark Roadmap）

**校正後問題**：原本寫的「400K 超出單 stream 上限」是用 plan 過於保守的「100K 上限」推的；NATS 2.10+ 單 stream 實際可達 300K-500K msg/s。對 side project 來說「目標跟 capacity 衝突」**根本不是問題**——撞牆本身就是學習材料。

**決策**：採 5-milestone benchmark roadmap ✓

```
M1: 單 shard 10K events/s    （baseline，跟 Redis 版對比）
M2: 單 shard 50K events/s    （pprof 找熱點 + 中度優化）
M3: 單 shard 100K events/s   （plan stretch goal、GC 調優）
M4: 撞 NATS 單 stream 上限   （找出真實 capacity）
M5: 4 shard 500K events/s    （多 stream，線性 scale）
```

每個 milestone 含：目標、Setup、預期 bottleneck、優化手段、通過條件、學習主題。
已寫入 plan §Benchmark Roadmap（位於 §何時值得做 之後）。
其他相關位置同步更新：
- §預期效益 移除「100K+」硬指標，指向 roadmap
- §跟現有 Kafka worker 的差別 table 改為 M1→M3→M5 進展式表述
- §驗證方式 §效能 改為「按 milestone 推進」

---

### [ ] #7 Debug Endpoint（簡化版 admin API）

**問題**：純 consumer 架構沒有 sync API，debug 困難。

**Side project 簡化版**：不需要完整 admin API，一個 HTTP debug endpoint 就夠：

```
GET /debug/member/:id
  → 回傳該 member 當前的 aggregations + progresses

GET /debug/shard/:id/stats
  → 回傳該 shard 的 metric snapshot
  
POST /debug/replay/:member_id?from=<ts>
  → 強制 replay 某 member 的歷史（debug 用）
```

**建議**：併入 #4 Observability 的章節，列為 optional。

**決策**：（待填）

---

### [ ] #8 Sink Backpressure（Phase 5 才會碰到）

**問題**：2PC 解決了 correctness，但 sink 慢時 buffer 無上限會 OOM。

**Side project 視角**：Result sink 是 deferred 的，到 Phase 5 才實作，先不擔心。

**建議**：Phase 5 規格寫到時補上即可。

**決策**：（待填）

---

### [ ] #13 跨 Member Pattern（架構已知限制）

**問題**：所有設計假設 per-member。但「同一張卡在 3 個不同帳號上消費」「同 IP 1 分鐘內註冊 10 個新帳號」這類規則在「per-member single goroutine」架構下**無法實作**。

**Side project 視角**：直接接受為「已知範圍限制」並寫進 plan，當作未來探索題即可。

**建議**：在 §Risks 或 §Out of Scope 加一行：「Cross-member pattern 不在 v1 範圍，已知限制」。

**決策**：（待填）

---

### [ ] #15 Result Sink Interface Stub

**問題**：Phase 1 就要決定 result sink interface，否則後面要回頭改。

**建議**：Implementation Phase 1 加一條「定義 result sink interface stub」。

**決策**：（待填）

---

## ❌ 不適用 (Side Project)

以下項目在 production 系統很重要，但對 side project 不必處理。記錄起來避免重複討論：

- **#6 Multi-region / DR**：side project 不需要 cross-region 部署
- **#10 Cost / Capacity Sizing**：無業務需求 → 無 TCO 對比意義
- **#17 跟現有 Worker 程式碼的銜接**：side project 沒 legacy 包袱，不需要 cleanup checklist

---

## 🔬 第二輪缺口（面試準備複查時發現，plan 尚未記錄）

下面 #19–#26 是「拿 Notion 面試準備 §2.D 的疑問逐項對照 plan」時浮現的缺口——這些在第一輪 #1–#18 評估裡沒被點出來。多數是**改 plan 文件就能補**的設計層問題（不需要等實作）；#20、#26 連到 correctness / memory。

### [ ] #19 event_id 來源與穩定性保證沒寫（🔴 冪等的地基）

**問題**：§冪等性保證 假設 event 自帶 `event_id` 當 dedup key，但**沒寫「誰生成、怎麼保證唯一、為什麼必須跨 redelivery + replay 不變」**。若 `event_id` 是 Core 收到時才 `uuid.New()`，每次重送 / 重播都是新 id → 去重完全失效（「登入失敗 3 次」會被重複事件灌成 4 次誤判）。這是所有冪等保證的地基。

**建議**：plan 明確規範——`event_id` 由 **producer 在事件產生時分配**（業務唯一鍵 / UUID），進 NATS 前就帶著，跨 redelivery / replay 不變；補一句「Core 不生成 event_id，只消費」。

**對應**：Notion §2.D #2。

**決策**：（待填）

---

### [ ] #20 Snapshot worker 與 main loop 的並發 / Copy-on-write（🔴 latent Go panic）

**問題**：§Incremental 的 `onBarrier` 做 `refs := snapshotRefs(dirty)` 後 fork 背景 goroutine 序列化。若 `refs` 只是 map 指標，主 goroutine 繼續處理同一個 dirty key group 的新事件時，會跟背景序列化發生 **concurrent map read/write → Go runtime 直接 panic**（`fatal error: concurrent map read and map write`）。plan 宣稱「主路徑 pause ≈ µs 級」+「背景序列化 references」這兩件事**只在有 copy-on-write 時才成立**，但 plan 沒提 COW，兩段互相矛盾。

```
三選一：
(a) 抓指標就 fork      → µs pause，但 race / panic            ❌
(b) barrier 時 deep-copy dirty kg → 一致，但 pause 變 ms（與「µs」宣稱矛盾）
(c) copy-on-write map  → µs pause，但要自實作 COW（Flink heap backend 做法）
```

**為什麼跟 #3 不同**：#3 是「跨多個 .snap 檔的原子性」（用 manifest 解）；本項是「main loop 寫 vs snapshot worker 讀同一份 in-memory map」的記憶體並發，是不同層的問題。

**建議**：plan §Checkpoint 補「snapshot 一致性如何達成」：明確選 COW 或 deep-copy，並修正「µs pause」宣稱以對齊所選方案。

**對應**：plan §Checkpoint / §Incremental。

**決策**：（待填）

---

### [ ] #21 為何不用 NATS 原生 dedup + ack/redelivery 參數沒寫（🟡）

**問題**：plan 走 app 層 `event_id` 去重，但**沒記錄「為什麼不用 NATS 內建 `Nats-Msg-Id` + Duplicates window」**這個 deliberate 決策。理由其實清楚：NATS dedup 是 **publish 端、window 內、狀態在 server（不進 snapshot）**；而真正的重複來自 **consumer redelivery + 故意 replay**，且去重狀態必須跟 state 一起 snapshot 才能跨重啟存活。此外 `AckWait`（redelivery 觸發時間）、`MaxDeliver` 沒指定。

**建議**：plan §End-to-End Exactly-Once 補一段「為何不靠 NATS dedup（publish 端 vs consumer 端、window 跨度、狀態歸屬三點）」+ consumer ack 參數（`AckWait` / `MaxDeliver`；`MaxAckPending` 已有）。

**對應**：Notion §2.D #1。

**決策**：（待填）

---

### [ ] #22 Dedup retention 與 allowedLateness 的關係沒接起來（🟡）

**問題**：§冪等性保證 沒明寫「dedup 標記保留多久」（隱含 = bucket / progress 生命週期 = `maxWindow`）。更關鍵：§冪等 與 §Watermark 是**分開兩節寫的**，從沒把這條因果鏈接起來——「dedup 標記只需覆蓋 `allowedLateness` 範圍；比這更舊的重複，會被 watermark 當遲到擋在 `applyEvent` 之前，根本碰不到 bucket；所以『bucket 過期後重複又來』不會造成重算」。隱含的不變式 **`allowedLateness ≤ maxWindow`** 也沒寫明。

**建議**：在 §冪等 或 §Watermark 補一段「dedup 覆蓋範圍 vs watermark 覆蓋範圍互補、無縫，前提 `allowedLateness ≪ maxWindow`」，並把三個時間區（正常 / 容忍遲到 / 太遲 drop）跟 bucket 過期線畫在一起。

**對應**：Notion §2.D #2 + #4 的交互。

**決策**：（待填）

---

### [ ] #23 同 timestamp 事件定序沒定義（🟡）

**問題**：plan 通篇用 `event.OccurredAt`，但**兩個事件 `OccurredAt` 相同時誰先沒定義**。對 aggregation 無影響（進同一個 bucket）；對 **CEP 序列偵測有影響**（A→B 同時間戳，誰算先決定 pattern 推進）。實際 tie-break = NATS 到達順序（單 goroutine FIFO），但沒寫進 plan。

**建議**：plan §時間語意 補一行 tie-break 規則（用 NATS seq 當次序，或事件加單調 sequence number）。

**對應**：Notion §2.D #5。

**決策**：（待填）

---

### [ ] #24 `reached_live_position()` 判定機制未定義（🟡）

**問題**：§Restart 圖 5 step ⑤ 用 `reached_live_position()` 決定 replay → live 的切換，但**沒定義怎麼判**：consumer seq 比對 stream `LastSeq`？追到當下 `LastSeq` 就切？追的過程 `LastSeq` 還在長怎麼辦？

**建議**：plan §Restart 補判定機制。例：啟動時記下 `targetSeq = stream.LastSeq()`，consumer 追到 `targetSeq` 即 `replaying.Store(false)`；之後新事件自然走 live（追 target 期間新進的事件等切到 live 再處理）。

**對應**：Notion §2.D #3。

**決策**：（待填）

---

### [ ] #25 Evicted-member 冷啟動 replay 邊界（🟡）

**問題**：§Risks §3 說被 LRU evict 的 member 再有事件時「從 NATS replay 該 member 歷史重建」，但**沒定義回溯多遠、跟 `maxWindow` 的關係、replay 期間該 member 新事件怎麼辦**。這是跟「重啟 replay」不同的**第二種 replay**（per-member，不是 per-shard）。

**建議**：plan 補「per-member 冷啟動 replay」規格：回溯範圍 = `maxWindow`（更舊的 bucket 反正會被 prune，replay 無意義）；如何只 replay 該 member（subject filter）；replay 期間先 buffer 該 member 新事件。

**對應**：Notion §2.D #3。

**決策**：（待填）

---

### [ ] #26 ProcessedEventIDs 在 hot bucket 無上限成長（🟡，連 #14）

**問題**：`BucketData.ProcessedEventIDs` 是 per-bucket 的 `event_id` set。hot member 在單一 bucket 塞大量事件時，這個 set **線性成長，而且會被 snapshot**（同時放大 snapshot 體積 + 記憶體）。§CEP 記憶體成本估算（7KB/member）沒把這項算進去。

**建議**：併入 #14 Memory Management 一起評估；考慮替代去重——例如「bucket 只存 count/sum，不存每個 event_id」，改用 `(bucket_ts, last_processed_seq)` 為界配合 replay 的單調性做冪等；或限制 set 大小並接受極端情況的取捨。

**對應**：Notion §2.D #7 + 既有 #14。

**決策**：（待填）

---

### [ ] #27 Late-event 旁路政策與「風控不可漏」矛盾，且缺 backstop（🔴 政策矛盾）

**問題**：§遲到事件處理 寫「太遲 → 寫 audit log **或丟棄**，不影響主流程」，但這跟 plan 自己宣稱的「**風控不能漏，寧可晚不可漏**」**直接矛盾**——silent 丟棄 = 漏報。而且 `lateEventSink.Emit()` 之後**誰處理、怎麼補**完全沒定義，side output 變成只是換個地方丟。

**為什麼不能無條件補進任意遲到事件**（這部分 plan 也沒講清楚，是取捨的論證）：
1. 已關閉的 window、已發出的決策（例如已放行的提幣）**收不回** → 補算數字也救不了 side effect
2. 要補任意遲到事件 → 永不 prune bucket / 永留 dedup 標記 → **state 無上限**
3. 允許任意回溯重算 → 結果依到達時機而變 → **破壞 replay 確定性**（違反 §驗證方式）

所以 watermark 截止線是刻意取捨（state 有界 + 延遲有界 + 確定性），side output 是它的安全閥——但**安全閥接到哪、誰消化，plan 沒寫**。

**建議**：plan §遲到事件處理 補「late-event 政策」：
- 刪掉「或丟棄」，明確 side output **一律保留**（不對 fraud silent drop）
- 定義旁路下游：late-event stream → **review queue / 批次補偵測**（即時窗漏掉，離線補一刀）
- 補一段「即時層 best-effort 阻擋 + 離線批次全量對帳」的 **Lambda 式 backstop**，說明「漏即時窗 ≠ 永遠漏報」
- 點出「即時阻擋」對遲到事件本就無能為力（決策當下沒資訊），遲到事件的價值在「事後標記 / 複查」

**對應**：plan §時間語意與 Watermark §遲到事件處理；呼應 Notion §2.B「late event 走補償不丟棄」。

**決策**：（待填）

---

## 行動建議（按優先序）

對 side project 來說，建議的處理順序：

1. **#3 修 snapshot 原子性**（correctness 不可妥協，先解決）
2. **#1 把目標改寫成 benchmark roadmap**（消除 plan 內部矛盾，定學習方向）
3. **加 3 個 architectural / learning 章節**：
   - 「## Hot Key Handling」（#2）
   - 「## Observability」（#4 + #7）
   - 「## Rule Lifecycle」（#5）
4. **#16 開 ADR 目錄**（portfolio 加分）
5. **#14 補 Memory Management 細節**（壓到 100K 時會用到）
6. **#11、#12 是設計題**，可以放著 implementation 過程慢慢回答
7. **#18 在寫 test 時加入 PBT**

剩下的 (#7, #8, #13, #15) 列入 backlog，到對應 Phase 再處理。

---

## 進度追蹤

- 🔴 Critical: 1/1 完成 ✓
- 🟡 Architectural / Learning: 2/8 完成（#2 Hot Key、#4 Observability）
- 🟢 Nice to have: 1/5 完成（#1 採 5-milestone roadmap）
- ❌ 不適用: 3 項（已折疊）
- 🔬 第二輪缺口（面試準備複查）: 0/9 處理（#19–#27，全待決策；#19、#20、#27 為 🔴）

**總計**：4/23 項已處理
