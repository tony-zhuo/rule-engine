# M1 Benchmark：NATS JetStream vs Kafka（單 shard baseline）

> 執行日期：2026-07-28
> 計畫：[docs/plans/2026-07-28-m1-benchmark-nats-vs-kafka.md](../plans/2026-07-28-m1-benchmark-nats-vs-kafka.md)
> Harness：`cmd/bench`（producer + consumer 同 process、同時鐘）

## TL;DR

1. **延遲**：舒適區間內 NATS 比 Kafka 低約 7 倍（10K events/s 下 p50 1.5ms vs 10–11.6ms；p99 3.5ms vs 17ms）。
2. **吞吐上限反轉**：Kafka 撐到 **80K/s**（上限 80K–160K 之間）；NATS 上限在 **40K–80K 之間**（80K 消化不完）。Kafka 的 batch 式消費贏吞吐，NATS 的逐訊息 iterator + ack 贏延遲。
3. **同步 snapshot 是高速率下的頭號延遲殺手**：40K 時 Kafka p99 從 18ms 惡化到 264ms（15×）、NATS 從 12ms 到 363ms（29×）；Kafka 80K 時到 3.57 秒。這是 async barrier + incremental checkpoint（plan §Checkpoint、gap #20）最直接的動機證據。
4. M1 目標（單 shard 10K）兩個 backend 都大幅超越；M3（100K）需要 async checkpoint + M2 的 pprof 優化。

## 環境

| 項目 | 規格 |
|------|------|
| 硬體 | Apple M2 Pro（12 core）/ 32GB，macOS 26.5.2 |
| 執行方式 | Docker Desktop 27.5.1（單節點 broker）；bench 於 host 執行 |
| Broker | `confluentinc/cp-kafka:7.6.0`（KRaft 單節點）、`nats:2-alpine`（JetStream file store） |
| Go / wire format | go1.25.0 / JSON |
| 負載 | 單 shard、MEMBER_POOL=100、18 條規則 + 3 個 CEP pattern（自 PG 載入，真實求值路徑） |
| 延遲定義 | ProcessEvent 完成時刻 −（producer enqueue 時刻）；replay 事件不計 |
| Snapshot | 現行同步 whole-shard gob dump，每 10s；on/off 對照 |
| 隔離 | 一次只跑一個 backend，另一個 container 停用 |

每組固定速率跑 60s；ladder 每階 60s。「未消化（not drained）」= 產完後 15s 內 consumer 未追平。

## 結果

### 固定 10K events/s（60s）

| Backend | Snapshot | p50 | p95 | p99 | max |
|---------|----------|-----|-----|-----|-----|
| Kafka | off | 11.6ms | 15.1ms | 17.1ms | 53.6ms |
| Kafka | on | 9.9ms | 13.8ms | 15.9ms | **85.0ms** |
| NATS | off | 1.47ms | 2.52ms | 3.49ms | 13.0ms |
| NATS | on | 1.48ms | 2.54ms | 3.83ms | **67.7ms** |

10K 時 snapshot 對 p99 幾乎無感（state 小、dump 快），但 **max 洩了底**：dump 當下整個 single-writer loop 停住，撞上的事件延遲直接 +50–70ms。（Kafka off/on 的 p50 差異在單次執行的 ±15% 變異範圍內，屬雜訊。）

### 吞吐階梯（60s/階，snapshot off）

| 速率 | Kafka p99 | Kafka 消化 | NATS p99 | NATS 消化 |
|------|-----------|-----------|----------|-----------|
| 10K | 14.7ms | ✅ | 3.2ms | ✅ |
| 20K | 15.5ms | ✅ | 3.6ms | ✅ |
| 40K | 17.8ms | ✅ | 12.4ms | ✅ |
| 80K | 59.0ms | ✅ | p50 19.8s | ❌ |
| 160K | p50 18.9s | ❌ | —（未跑） | — |

### 吞吐階梯（60s/階，snapshot on）

| 速率 | Kafka p99 | Kafka 消化 | NATS p99 | NATS 消化 |
|------|-----------|-----------|----------|-----------|
| 10K | 16.1ms | ✅ | 57.6ms | ✅ |
| 20K | 52.6ms | ✅ | 75.9ms | ✅ |
| 40K | **263.7ms** | ✅ | **362.5ms** | ✅ |
| 80K | **3.57s** | ✅（drain 3.5s） | p50 20.9s | ❌ |
| 160K | p50 19.7s | ❌ | — | — |

原始樣本（每事件一筆，ns）：`docs/benchmarks/raw/`（gitignored，僅本機保留）。

## 解讀

### (a) 兩個 MQ 在 10K 下的差異

NATS 端到端 p50 **1.5ms**，Kafka **~10ms**。差距主要來自 Kafka 生產/消費兩端的 batch 導向預設（franz-go produce batching + broker fetch 等待），不是引擎——引擎處理路徑兩邊完全相同。對「風控決策要快」的場景，NATS 的 out-of-box 延遲特性明顯較好；Kafka 要壓延遲需要調 `linger`/fetch 參數，屬 M2 課題。

### (b) 各自的吞吐上限

- **Kafka：80K/s 可持續**（p99 59ms），160K 崩潰 → 上限落在 80K–160K。
- **NATS：40K/s 可持續**（p99 12ms），80K 崩潰 → 上限落在 40K–80K。

關鍵推論：**引擎核心不是 40K–80K 的瓶頸**——同一個 ProcessEvent 在 Kafka 路徑能吃 80K/s，NATS 路徑 80K 卻不行，差在 consumer 端的訊息交付方式（`PollFetches` 一次抓上千筆 vs JetStream iterator 逐筆 `Next()` + 逐筆 `Ack()`）。NATS 若要上 80K+，方向是 batch fetch / AckAll 策略，或多 shard 平行。

### (c) Snapshot 的衝擊（async barrier checkpoint 的動機證據）

同步 whole-shard dump 的成本隨 state 成長：速率越高、member state 越大，每 10s 的 dump 停頓越長，而停頓期間 backlog 積壓又推高後續延遲——40K 時 p99 惡化 15–29 倍，Kafka 80K 時整體 p99 到 3.57s。**結論：進 M2/M3 之前，async barrier + per-key-group incremental checkpoint（plan §Checkpoint 機制，先解 gap #20 的 COW 決策）是必要投資，不是 nice-to-have。**

### Kafka partition 數量推算基礎（memory TODO 前半）

本機單 partition 單 shard 可持續 **~80K events/s**（sync snapshot off、JSON decode 在內）。推算公式：`partitions ≥ peak_rate / (per-partition 持續吞吐 × 目標利用率)`。以利用率 50% 計，100K/s 需要 ≥3 個 partition（留成長空間取 4–8）。**注意**：這是筆記本 + Docker 數字，production 機型要用同樣的 ladder 方法重量一次再定案。

## 已知限制（誠實聲明）

- macOS + Docker Desktop 虛擬化層壓低絕對數字；**只主張相對結論**。
- Producer 與 consumer 同 process 搶 CPU——160K 階時 producer 本身吃掉可觀核數，該階的「崩潰」含此因素（但 80K vs 160K 的量級差距仍成立）。
- 單節點 broker、預設 fsync/ack 設定，屬「out-of-box 對比」；replication 開啟後兩邊都會變慢且幅度不同。
- 低速率下單次執行變異約 ±15%（見 10K 的 off/on 倒掛）；本報告未做多次取樣平均，方向性結論不受影響。
- Kafka ladder 首次執行曾因「前一 run 的 topic 非同步刪除 + 立即灌量」觸發 franz-go produce 無限期 block；已在 bench 加 `RecordDeliveryTimeout(30s)` + settle 等待。重跑後正常。

## M2 建議（按預期投報排序）

1. **Async barrier checkpoint**（先決策 gap #20 COW vs deep-copy）——snapshot on 的數字已證明這是 100K 的硬前提
2. **NATS consumer batch 化**（fetch batch + AckAll）——把 NATS 上限從 40K 推向 Kafka 級
3. **pprof 熱點分析 @ 40K**——JSON decode 幾乎確定上榜；評估 protobuf / 手寫 decoder
4. Kafka 延遲調參（linger、fetch wait）——看 p50 10ms 能壓到多少
