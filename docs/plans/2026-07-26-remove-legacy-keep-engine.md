# 移除 Legacy 殘留，只保留 In-Memory Engine + Rule CRUD Control Plane

> 建立日期：2026-07-26
> 狀態：Done
> 負責人：Tony / Claude

## 1. 背景與動機

Task M/N/Q 已經拆掉 CheckEvent endpoint、legacy Kafka worker 和 Redis 依賴，但 repo 裡仍留有一批「沒人用但還在編譯」的 legacy 程式碼、過期的 config / Makefile / Dockerfile 條目，以及 README 的雙架構敘事。本次把這些一次清乾淨，讓 repo 只講一個故事：**in-memory engine（資料面）+ rule CRUD API（控制面）**。

已確認的決策（Scope 問答）：
- **`cmd/apis` 保留**：它是規則唯一的寫入路徑（control plane），不算 legacy。
- **README 完全抹掉 legacy 敘事**：不保留「兩代架構演進」的故事。

## 2. Scope(要做什麼)

### A. 刪除死碼（已逐一驗證無人引用）

| 目標 | 內容 |
|------|------|
| `service/base/rule/usecase/db_context.go` | 整檔刪除。`DBEvalContext` 是 legacy DB-backed 求值路徑，全 repo 無引用 |
| `service/base/rule/usecase/aggregate.go` | 刪 `BuildAggregateConds` / `buildAggregateCond`（只被 db_context 用）。保留 `CollectUniqueAggregateKeys`、`BuildBehaviorSchemas`、`MaxWindowFromKeys`（引擎在用） |
| `service/base/behavior/model/interface.go` | 整檔刪除。4 個 interface（BehaviorRepo / BehaviorUsecase / BehaviorEventStore / ProcessedEventRepo）全是 legacy 路徑遺物 |
| `service/base/behavior/model/request.go` | 刪 `LogBehaviorReq`、`AggregateCond`（隨上述死碼一起死） |
| `service/base/behavior/model/entity.go` | 刪 `BehaviorLog`、`ProcessedEvent`（對應的資料表 migration 早已移除）。**保留 `BehaviorEvent`**（引擎核心型別） |

### B. 改名去 legacy 化

- `service/bff/apis/usecase/engine.go`：`EngineUsecase` → `RuleAdminUsecase`（code 註解裡本來就註記了這個 follow-up）。連動修改 controller / wire / wire_gen 的引用與檔名。

### C. 設定與建置檔案對齊

- `config.yaml` + `config.example.yaml`：刪 `worker:`、`redis:` 區塊及 `consumer_group: rule-engine-worker`（`config/config.go` 早已不讀這些 key），對齊 config struct 實際欄位。
- `Makefile`：刪 `build`/`run-worker` 裡對已不存在的 `cmd/worker` 的引用；新增 `run-core`（`cmd/rule-engine-core`）、`run-producer`（`cmd/event-producer`）targets；`kafka-*` helper 的預設 group 改為引擎用的名稱。
- `Dockerfile`：目前只 build `cmd/apis`。改為 build 兩個 binary：`rule-engine-api`（control plane）+ `rule-engine-core`（engine）。

### D. README 重寫

- 移除「Legacy Pipeline」整節、雙架構開場、對照表中的 legacy 欄位。
- 重寫為單一架構敘事：in-memory engine 為主體，`cmd/apis` 以「rule admin API（control plane）」身分出現。
- Bottleneck 動機段落改寫為不依賴「repo 裡有 legacy code」的說法（效能論證仍可引用 plan 文件）。

### E. 註解掃尾

- 更新引用到已刪除符號/檔案的註解（如 `engine.go` package comment、wire 裡的 Task M/Q 說明），使其描述現狀而非歷史。

## 3. Out of Scope（不做什麼）

- `docs/in-memory-rule-engine-*.md` 四份設計文件**不動**——其中的 Migration Phase A/B/C、legacy 對照是設計史料，保留。
- 引擎行為零改動：`service/engine/core/` 不碰（除非編譯需要）。
- 不做 benchmark、不做新功能、不動 DB migration（現存 4 個 migration 都是引擎需要的）。
- 不重命名 `service/bff/` 目錄結構本身（只改 usecase 型別名）。

## 4. 技術方案

- 純刪除 + 改名的 refactor，無新依賴、無行為變更。
- 安全網 = 既有 21 個測試 + `go build ./...` + `go vet ./...`。刪碼順序按依賴反向（先刪引用者 db_context，再刪被引用的 model 符號），每步保持可編譯。
- wire_gen.go 一併手動修改（repo 慣例是 checked-in generated code；若 `wire` 指令可用則重新生成）。

## 5. 假設與待確認

1. **Dockerfile 改為 build 雙 binary** 是我的預設——如果你想維持 image 只有 API，跟我說。
2. `BehaviorLog` / `ProcessedEvent` 型別假設無人引用（已 grep 過 interface 層，實作階段會再以編譯驗證）。
3. README 重寫後長度會明顯縮短，「Design decisions worth interviewing on」表格保留（都是引擎相關）。

## 6. 驗收標準

- `go build ./...`、`go vet ./...` 通過。
- `go test -short ./service/engine/core/` 全綠；有 Docker 時跑完整 `go test ./...` 全綠。
- `grep -ri "worker\|redis\|CheckEvent\|BehaviorLog\|ProcessedEvent\|DBEvalContext" --include="*.go" service cmd config` 無殘留（docs 除外）。
- README 無任何 legacy 敘事；`cmd/apis` 以 control plane 身分描述。

## 7. 風險與權衡

- **wire_gen.go 手改風險**：改名若漏改會編譯錯，風險由編譯器兜底。
- **README 抹掉 legacy 的代價**：面試時「為什麼重寫」的對照故事只剩 docs/ 裡有——已由你拍板接受。
- 刪 `AggregateCond` 會讓 `behavior/model/request.go` 幾乎清空——若清空就整檔刪除。

## 8. 執行清單

- [x] 1. 刪 `db_context.go` + `aggregate.go` 死函式，編譯驗證
- [x] 2. 刪 `behavior/model` 的 interface.go / request.go 死符號 / entity.go 的 BehaviorLog、ProcessedEvent，編譯驗證
- [x] 3. `EngineUsecase` → `RuleAdminUsecase` 改名（含 controller / wire / wire_gen / 檔名），編譯 + 測試
- [x] 4. config.yaml / config.example.yaml / Makefile / Dockerfile 對齊
- [x] 5. 註解掃尾（引用已刪符號的註解更新為現狀描述）
- [x] 6. README 重寫（單一架構敘事）
- [x] 7. 全套驗收：build + vet + 完整測試 + grep 殘留檢查

## 9. 實作時的計畫外調整

實作中發現的、原計畫沒涵蓋但直接服務於「只留 in-memory engine」的項目：

1. **`FieldSchema` 整條鏈一併移除**（計畫原本說「保留 `BuildBehaviorSchemas`（引擎在用）」，是錯的）。
   `strategy.go` 確實**呼叫** `BuildBehaviorSchemas`，但結果寫進 `CompiledRuleSet.Schemas` 後**沒有任何 reader**——`FieldSchema` 是 Redis 時代 pipe-separated 編碼的產物。刪除範圍：`behavior/model/schema.go` 整檔、`BuildBehaviorSchemas`、`CompiledRuleSet.Schemas` 欄位及其賦值。

2. **`Makefile` 的 `migrate-down` 是壞的**：它 drop `behavior_logs`（migration 早已移除、資料表不存在），卻漏掉實際存在的 `cep_patterns`。已改為 drop `cep_patterns, rule_strategies`。

3. **移除 `kafka-lag` target**：引擎手動 pin partition、自己用 snapshot 管 offset，不加入 consumer group，所以 `kafka-consumer-groups` 永遠查不到東西。連帶移除 `KAFKA_GROUP` 變數；`KAFKA_TOPIC` 預設改為 `rule-events` 以對齊 producer。

4. **`config.yaml` 的 `kafka:` 區塊一併刪除**（計畫只寫了 worker + redis）。`config.Config` 只有 App/DB/Log 三個欄位，kafka 區塊同樣沒人讀；引擎的 Kafka 設定走環境變數。

5. **`docker-compose.yml` 新增 `engine` service**：Dockerfile 改 build 雙 binary 後，若 compose 沒有對應 service，`rule-engine-core` 這顆 binary 進了 image 卻沒人跑。新增的 service 以 entrypoint 覆寫指向引擎，並掛一個 `engine_snapshots` volume。

6. **新增 `test` / `test-short` Makefile targets**：README 的驗證指令改走 make，避免 README 與 Makefile 各講一套。

### 已知未處理

`gofmt -l` 目前列出 8 個檔案（import 排序與 gofmt 慣例不同）。**這些在本次改動前就已存在**（用 HEAD worktree 比對確認），且多數落在本次沒碰的檔案（`consumer.go`、`negative_queue.go`、數個 test 檔）。修它會在無關檔案產生 diff 噪音，故不在本次處理，留作獨立的 formatting commit。
