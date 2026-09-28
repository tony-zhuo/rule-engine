# M1 Benchmark：NATS JetStream vs Kafka 吞吐與延遲比較

> 建立日期：2026-07-28
> 狀態：Done
> 負責人：Tony / Claude

## 1. 背景與動機

引擎的兩個 MQ backend 在函式庫層面已對稱且經 shadow test 驗證 state 一致，但從未量過任何效能數字。本計畫執行 plan §Benchmark Roadmap 的 **M1（單 shard baseline）**，並回答兩個問題：

1. **NATS vs Kafka 在這個架構下的實際差異**（吞吐上限、端到端延遲分布）
2. **Kafka partition 數量規劃的依據**（memory TODO：先量 HandleMessage latency 才能推算 production partition 數）

比較方法：**一次跑一個 backend，同樣的負載跑兩次**。引擎邏輯兩邊完全共用，唯一變因是 MQ。

已確認的決策（Scope 問答）：
- 量測目標：**吞吐上限 + 固定速率下的延遲**兩者都要
- 延遲量測點：**端到端**（producer publish 時刻 → 引擎 ProcessEvent 完成）
- Snapshot：**開/關兩種都跑**（關 = 純 MQ 數字；開 = 真實運作 + 為 async barrier checkpoint 蒐集動機證據）

## 2. Scope（要做什麼）

### A. 前置：Kafka 接進引擎執行檔

`cmd/rule-engine-core` 目前硬寫 NATS。加 `BACKEND=nats|kafka` 環境變數開關（模式照抄 `cmd/event-producer`），Kafka 路徑建 `kgo.Client` + `NewKafkaConsumer`。

### B. 延遲量測儀器

- **Publish 時間戳**：producer 在發佈當下設 `OccurredAt = time.Now()`（現有欄位，無 schema 變更）。端到端延遲 = ProcessEvent 完成時刻 − `OccurredAt`。
- **量測掛鉤**：`NATSConfig` / `KafkaConfig` 各加一個可選的 `OnProcessed func(latency time.Duration)` callback（nil = 零開銷）。consumer loop 在 ProcessEvent 返回後呼叫。
- **統計**：callback 收進 in-memory int64 slice，結束時排序算 p50/p95/p99/max。不引入 histogram 依賴（10⁶ 級樣本 × 8 bytes 可接受）。
- **吞吐**：consumer 端以固定間隔記錄已處理事件數，穩態區間取平均。

### C. Benchmark runner

`cmd/bench/main.go`：單一 binary 同 process 跑 producer + engine consumer（避免跨 process 對時鐘；單機 benchmark 同鐘量差值最準）。參數：`BACKEND`、`RATE`、`DURATION`、`MEMBER_POOL`、`SNAPSHOT`（on/off）。輸出：stdout 摘要 + CSV 原始延遲樣本（丟 `docs/benchmarks/raw/`，gitignore）。

- **固定速率模式**：producer 按絕對時間表發送（schedule = start + n/rate），落後不補償間隔——避免 coordinated omission 低估延遲。
- **吞吐上限模式**：速率階梯（10K → 20K → 40K → …）每階 60s，判定「撐得住」= 消費 lag 有界且 p99 不隨時間發散；第一個撐不住的階梯即上限區間。

### D. 執行矩陣與報告

| 維度 | 值 |
|------|-----|
| Backend | NATS、Kafka（一次一個，另一個的 container 停掉） |
| Snapshot | off、on（10s 間隔，現行同步 dump） |
| 模式 | 固定 10K events/s 延遲量測（60s）、吞吐上限階梯 |

共 2×2×2 = 8 組。結果寫入 `docs/benchmarks/2026-07-28-m1-nats-vs-kafka.md`：環境規格、每組 p50/p95/p99/max + 吞吐、NATS vs Kafka 對照解讀、snapshot 開關的延遲衝擊（= async barrier checkpoint 的動機證據）、M2 的下一步建議。

## 3. Out of Scope（不做什麼）

- 不做 M2+ 的優化（pprof 調優、GC 調參）——M1 只量 baseline
- 不做 multi-shard、不動 checkpoint 機制
- 不比較 MQ 的 durability/replication 設定差異（兩邊都用單節點 Docker 預設）
- 不做正式的 Kafka partition 數量結論——只產出單 partition 的數字當推算基礎

## 4. 技術方案

- 量測程式碼放 `cmd/bench` + consumer config 的 optional callback，**引擎核心（A–G 檔案）零改動**
- 環境：本機 docker-compose（backend 互斥啟動：`docker compose stop kafka` / `stop nats`）
- 規則集：用現有 seed（003 rule + 005 CEP pattern），引擎照常從 PG 載入——benchmark 涵蓋真實規則求值路徑
- 已知限制（誠實記錄在報告）：macOS + Docker Desktop 的虛擬化層會壓低絕對數字；相對比較（NATS vs Kafka、snapshot on/off）仍有效

## 5. 假設與待確認

1. **同 process 跑 producer + consumer** 是我的預設（同鐘量差值最準）；代價是兩者搶 CPU，會在報告中註明。若你想分 process，跨 process 用同機 wall clock 也可接受
2. 固定速率取 **10K events/s**（M1 目標值）；上限階梯從 10K 起跳
3. `MEMBER_POOL=100`（現有 producer 預設）；member 數會影響 state 大小，M1 先不掃這個維度

## 6. 驗收標準

- `BACKEND=kafka go run ./cmd/rule-engine-core` 可啟動並消費（含一個煙霧測試）
- `cmd/bench` 兩個 backend 各跑完 8 組矩陣，數字進報告
- 報告能回答：(a) 兩個 MQ 在 10K 下的 p99 差多少 (b) 各自的吞吐上限區間 (c) snapshot 對 p99 的衝擊
- 現有 21 個測試維持全綠

## 7. 風險與權衡

- **單機 benchmark 的絕對數字不可外推 production**——報告只主張相對結論
- 吞吐上限可能先撞 producer 或 Docker 網路，而非 MQ 本身——runner 需記錄 producer 實際達成速率以識別
- Kafka container（cp-kafka 單節點）與 NATS 的 fsync/ack 預設不同，屬「out of box 對比」而非「同配置對比」——報告明示

## 8. 執行清單

- [x] 1. `cmd/rule-engine-core` 加 `BACKEND` 開關（含煙霧測試），模式照抄 event-producer
- [x] 2. consumer config 加 `OnProcessed` callback + 單元測試（驗證 nil 零開銷、非 nil 收到正確延遲）
- [x] 3. `cmd/bench`：固定速率模式（絕對時間表發送 + CSV 輸出）
- [x] 4. `cmd/bench`：吞吐上限階梯模式
- [x] 5. 跑 8 組矩陣（NATS/Kafka × snapshot on/off × 兩模式）
- [x] 6. 寫結果報告 + 更新 memory 的 partition TODO
- [x] 7. 全套驗收：現有測試全綠 + 報告完整
