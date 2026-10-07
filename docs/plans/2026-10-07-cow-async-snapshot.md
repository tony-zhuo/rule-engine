# Copy-on-Write State：讓 snapshot 改成背景非同步（gap #20）

> 建立日期：2026-10-07
> 狀態：Done
> 負責人：Tony / Claude

## 1. 背景與動機

現在 snapshot 是在主 goroutine 裡 inline 跑 gob encode 整個 shard（`service/engine/core/consumer_nats.go:151-156`、`consumer_kafka.go:164-171`）。之前測到 encode 約 12 秒，這段期間 shard 完全不處理事件。這跟 5 秒 RTO、p99 < 10ms 的目標都對不上。

直接把 encode 丟到背景 goroutine 不行。state 是一整棵指標樹（`state.go:16-81`），主迴圈同時在改 map，Go runtime 會直接 `fatal error: concurrent map read and map write`（gap #20）。

這次採用 gap #20 的選項 (c)：**copy-on-write**。snapshot 開始時只凍結指標，主迴圈第一次要改某個物件時，先複製一份再改。

## 2. Scope（要做什麼）

1. **State 改成以 key group 分桶**：`ShardState.Members map` 改成 `ShardState.KeyGroups [128]*keyGroupState`，每個 kg 底下各有一個 `Members map`。
2. **兩層 lazy COW**：
   - kg 層：snapshot 開始後，某個 kg 第一次被寫入時，用 `maps.Clone` 複製那個 kg 的 map
   - member 層：snapshot 開始後，某個 member 第一次被寫入時，深拷貝整個 `MemberState`
   - 用 epoch / version 判斷物件是不是還被 snapshot 引用
3. **寫入改走單一入口**：`memberForWrite(id)`。現有兩個寫入點要改過來：`core.go:97`、`negative_queue.go:64`。另外加一個唯讀的 `member(id)` 給測試和讀取路徑用。
4. **Snapshot API 拆成三段**：
   - `beginSnapshot()`：在主 goroutine 執行，µs 級，複製 1024 個 kg 指標（8 KB）、LastSeq、watermark
   - `frozen.encode()`：可以在任何 goroutine 執行
   - `endSnapshot()`：在主 goroutine 執行，解除凍結
   - 保留同步版的 `Snapshot()`（begin → encode → end），給 final snapshot 和測試用
5. **兩個 consumer 都改成背景 snapshot**：在 `consumer.go` 抽一個共用的 `asyncSnapshotter`，NATS 和 Kafka 的 loop 都改用它。同一時間最多只有一個 snapshot 在跑；上一個還沒結束時，跳過這次 interval。
6. **K 從 128 改成 1024**：`keygroup.go` 的 `NumKeyGroups` 和註解改成以「100M 會員、20% 活躍 = 20M」為依據。每個 kg 約 2 萬人，K=128 時是約 15.6 萬人，這樣 COW 的 map clone 才夠小；shard 也能擴到 50~70 個以上，負載依然平均。
7. **文件**：
   - `docs/in-memory-rule-engine-gaps.md`：更新 #20 的決策，並在 ADR-002 註記 K 改成 1024
   - `README.md:66,96`：把 128 改成 1024
   - `consumer.go` 頂部「為什麼不 async」的註解

## 3. Out of Scope（不做什麼）

- 不做 per-kg 分檔、manifest、dirty bitset、增量 checkpoint。還是整個 shard 寫成單一 gob 檔，只是內部結構換成以 kg 分桶。
- 不改序列化格式（gob → protobuf）
- 不做 ack 批次化、decode pipeline、遠端儲存、changelog
- 不加 snapshot 相關的 metrics（耗時、clone 次數），要的話另外開一份計畫
- 不動 `docs/in-memory-rule-engine-plan.md`。這個檔案你目前有還沒 commit 的修改，裡面還有很多處寫著 K=128，§Checkpoint 也還沒有 COW 的說明，要不要改、怎麼改，等你決定。
- 不相容舊的 snapshot 檔（見第 5 節）

## 4. 技術方案

**資料結構（`state.go`）：**

```go
type ShardState struct {
	KeyGroups [NumKeyGroups]*keyGroupState
	epoch     uint64 // 每次 beginSnapshot 就 +1（不匯出 → gob 會略過）
	frozen    uint64 // 進行中的 snapshot 的 epoch，0 代表沒有
}
type keyGroupState struct {
	Members map[string]*MemberState
	version uint64
}
// MemberState 加上 version uint64（不匯出）
```

**為什麼要以 kg 分桶**：把 member 換成 clone 出來的那份時，`kg.Members[id] = clone` 這個動作本身就是在寫 map。所以 map 本身也要 COW。如果不分桶，snapshot 開始後第一次寫入，就要一次 clone 一個有 2000 萬個 entry 的 map，主迴圈會卡住很久。分成 1024 個 kg 之後，每次只要 clone 被寫到的那一個 kg（約 2 萬個 entry），而且這些成本會分散到不同的事件上。之後的增量 checkpoint、以 kg 分檔、rescale，也都是直接以 kg 為單位操作。

**kg 的 map 延遲建立**：一個 shard 只擁有 1024/N 個 kg。其他位置保持 nil，等第一次寫入時才建立 map。

**判斷規則**：只有在 `frozen != 0 && obj.version < frozen` 時才 clone，clone 完的新物件 `version = epoch`。

- snapshot 之後才新建的 member，version 本來就等於 epoch，所以不會被 clone
- 沒有 snapshot 在跑的時候，完全不 clone
- Restore 回來的物件 version 都是 0，下一次 begin 時 epoch 會變成 1，所以判斷還是正確

**深拷貝**：`MemberState.clone()`、`BehaviorAgg.clone()`、`BucketData.clone()`、clone `PatternProgress`。

- 巢狀的 map 一律用 `maps.Clone`，`ProcessedEvents` 用 `slices.Clone`
- `Variables` 的值目前只有 string 和 float64（`snapshot.go:10-15`），所以淺複製這個 map 就夠了

**Consumer（`consumer.go`）：**

```go
type asyncSnapshotter struct { path string; interval time.Duration; last time.Time; done chan error; inFlight bool }
func (s *asyncSnapshotter) tick(core *Core)  // 每處理完一筆事件呼叫一次：先非阻塞收 done → endSnapshot；時間到了而且沒有 in-flight → begin + go encode/寫檔
func (s *asyncSnapshotter) drain(core *Core) // shutdown 時呼叫：等 in-flight 的那次結束，再做同步的 final snapshot
```

寫檔還是沿用「寫 tmp 再 rename」的 atomic 寫法。拆成 `writeSnapshotFile(path, data)`，背景和同步兩條路徑共用。

**會動到的檔案**：
- `state.go`、`core.go`、`negative_queue.go`、`snapshot.go`、`consumer.go`、`consumer_nats.go`、`consumer_kafka.go`
- 測試：新增 `state_test.go`；另外有 7 個測試檔直接存取 `State.Members[...]`，要改成走 `member()`
- 不新增任何依賴

## 5. 假設與待確認

- **舊的 snapshot 檔不相容**（Claude 自訂）：格式從 `Members` 變成 `KeyGroups`，舊檔會 decode 失敗，consumer 起不來。這是 side project，所以處理方式是手動刪掉舊檔，從 log 起點重新 replay，不寫 migration。**請確認可以接受。**
- **改了 K，NATS stream 和 Kafka topic 也要重建**：K 改了之後，`ShardOfMember` 算出來的 shard 也會變。以 N=4 為例，原本是用 crc 的第 5~6 bit 決定 shard，之後改用第 8~9 bit。舊事件是照舊的路由 publish 的，從頭 replay 的話，同一個 member 的 state 會被拆到兩個 shard。所以舊 snapshot、NATS stream、Kafka topic 都要一起清掉。N=1 時不受影響。producer 和 engine 也必須一起部署。**請確認可以接受。**
- **每處理完一筆事件才檢查一次 snapshot**（Claude 自訂）：NATS 的 `iter.Next()` 是阻塞呼叫，沒有事件進來的時候就不會觸發 snapshot，也不會收到 done。凍結狀態會一直維持到下一筆事件進來。這樣不影響正確性，只是可能多 clone 幾個 member。跟現在 inline 版本的行為一致（現在也是有事件才會檢查）。
- **跟 `2026-10-05-restore-benchmark.md` 的先後順序**：那份還在 Draft。如果這份先做完，benchmark 量到的 recovery 時間就不會再包含 inline snapshot 的暫停。**要先做哪一份？**

## 6. 驗收標準

- `go build ./...`、`go vet ./service/engine/core/` 通過
- `make test-short` 全綠，`go test -race ./service/engine/core/` 全綠
- 新的單元測試涵蓋以下情境（table-driven）：
  - begin 之後修改舊 member：frozen 看到的是舊值，live 看到的是新值
  - begin 之後新增 member：frozen 裡看不到
  - begin 之後刪除 progress：frozen 裡還在
  - 同一個 epoch 內，同一個 kg 和同一個 member 只會被 clone 一次（比對指標是否相同）
  - 沒有 snapshot 在跑的時候，完全不 clone
  - `clone()` 是深拷貝：改 clone 的每一層 map 和 slice，原本的物件都不受影響
- `keygroup_test.go`：`KeyGroupOf` 的結果落在 [0, 1024)；`ShardOf` 對 N=1、3、4、64 都會把 1024 個 kg 分配到 [0, N) 的連續區間，而且每個 shard 分到的 kg 數量最多只差 1
- Race 測試：背景 encode frozen 的同時，主迴圈持續寫入同一批 member。decode 回來的 state 要等於 begin 那一刻的 state。
- 既有的 snapshot、replay、consumer 的 restore 測試全部通過
- 有 Docker 的話跑 `make test`，Kafka 和 shadow 測試也要通過

## 7. 風險與權衡

- **記憶體峰值最多 2 倍**：一次 snapshot 期間，如果每個 member 都被寫到，等於整份 state 被複製一次。每個 shard 10 GB 的上限要預留這段空間。
- **成本會轉嫁到 hot path**：snapshot 期間，每個 member 第一次被寫入時要深拷貝。`ProcessedEventIDs` 越大，clone 越慢，p99 會在 snapshot 期間上升。這次不量，之後用 bench 量。
- **漏掉寫入入口就會出現 data race**：之後任何新的寫入路徑都必須經過 `memberForWrite`，也不能跨事件保存 `*MemberState` 或 `*PatternProgress` 的指標。這兩條會寫進 `state.go` 的註解，race 測試也會抓得到。
- **每個事件多算一次 crc32**：要拿 member 對應的 kg，所以每個事件多一次 `KeyGroupOf`，大約幾十 ns（憑印象）。
- **K 是上線後就不能改的承諾**：1024 是依照 100M 會員、20% 活躍的假設定的。如果之後的規模又放大一個量級，每個 kg 會變回 20 萬人，到時候就沒有辦法便宜地再改了。
- **背景寫檔失敗**：只記 log，下一個 interval 會再試一次。行為跟現在一樣。

## 8. 執行清單

- [x] 寫 `keygroup_test.go`（範圍和分配是否平均），把 `NumKeyGroups` 改成 1024 並更新註解，測試全綠
- [x] 寫 `state_test.go`：`member` / `memberForWrite` / COW 判斷 / clone 深拷貝的測試，確認紅燈
- [x] `state.go`：kg 分桶、version / epoch、`memberForWrite`、`member`、`clone()`，讓測試變綠
- [x] `core.go:97`、`negative_queue.go:64` 改走 `memberForWrite`；`snapshot.go` 的 `rebuildNegativeDeadlines` 改成遍歷 kg；7 個測試檔改用 `member()`；`make test-short` 全綠
- [x] 寫 race 測試（frozen encode 和寫入同時進行），確認紅燈
- [x] `snapshot.go`：`beginSnapshot` / `encode` / `endSnapshot`，`Snapshot()` 改成組合這三段；`-race` 全綠
- [x] `consumer.go`：`asyncSnapshotter` 和 `writeSnapshotFile`；NATS / Kafka 的 loop 改用它；既有的 consumer 測試全綠
- [x] 更新 gaps #20 的決策和 ADR-002、`README.md`、`consumer.go` 的註解
- [x] 最後驗證：`go vet`、`make test-short`、`go test -race ./service/engine/core/`，有 Docker 就再跑 `make test`
