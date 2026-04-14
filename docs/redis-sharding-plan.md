# Redis Cluster Sharding Plan

**狀態**：Deferred — 計畫完整，尚未實作。

## Context

**問題**：benchmark 顯示單台 Redis（Sentinel 架構）是吞吐量天花板。在 production 規模下，member 數量持續增長，每秒請求數也持續增長，**單台 Redis 的總 QPS 會成為瓶頸**。

**目標**：把目前的 Sentinel 主從架構（1 master + 2 replicas + 3 sentinels）改為 **Redis Cluster**，讓寫入/讀取可以水平擴展到 N 台 master 上，不同 member 的事件分散到不同 shard 處理。

**非目標**：解決「單一 VIP member 的 sorted set 過大」問題。Sharding 對這個場景無效（該 member 的所有資料還是在同一個 shard）。這種 hot member 問題要靠另外的方法（time-bucket 預聚合、window 縮減）處理。

---

## 現況分析（好消息）

所有 hot path 上的 Redis key **都已經以 `member_id` 為 scope**，天然適合 sharding：

| Key Pattern | 範圍 | 操作 |
|---|---|---|
| `rule_engine:events:{member_id}:{behavior}` | per-member | ZADD, ZRANGEBYSCORE, EXPIRE |
| `rule_engine:member:{member_id}` | per-member | SADD, SMEMBERS, SREM |
| `rule_engine:progress:{uuid}` | **per-progress（非 per-member）** | SET, GET, DEL |
| `rule_engine:active_rules` | **global** | GET, SET, DEL |

**所有 pipeline 也都只在同一個 member 的 keys 上操作**（`StoreAndAggregate`、`BatchAggregate`、CEP `Save`/`Delete`），無跨 member 的原子性需求。這是 Redis Cluster 最容易適應的情境。

**兩個需要處理的例外**：
- `rule_engine:progress:{uuid}`：沒綁 member_id，`Save` 會同時寫它 + `rule_engine:member:{memberID}`。在 Cluster 上這兩個 key 很可能落在不同 shard → pipeline 會 `CROSSSLOT` 報錯。
- `rule_engine:active_rules`：單一全域 key，所有 API 都讀。Cluster 上會全部打到同一個 shard，變成熱點。

---

## 設計決策

### 1. Cluster 拓樸

最小配置：**3 masters + 3 replicas**（每個 master 一個 replica）
- slot 分佈：master-1 持有 0-5460、master-2 持有 5461-10922、master-3 持有 10923-16383
- 失效時對應 replica 升為 master
- 可後續 scale 到 6 masters、9 masters，逐步搬 slots

### 2. Hash Tags 強制 co-location

Redis Cluster 用 CRC16(key) mod 16384 決定 slot，**但 key 中 `{...}` 裡的字串會優先作為 hash**。靠這個把同 member 所有 keys 鎖在同一 slot：

```
rule_engine:events:{user123}:CryptoWithdraw   ← hash tag = "user123"
rule_engine:events:{user123}:FiatDeposit      ← 同一 slot
rule_engine:progress:{user123}:uuid-abc       ← 同一 slot
rule_engine:member:{user123}                   ← 同一 slot
```

上述所有 key 的 hash 都是 `CRC16("user123") mod 16384`，必然落在同 shard。pipeline / 跨 key Lua script 都合法。

### 3. CEP progress key 改名

把 `rule_engine:progress:{uuid}` 改為 `rule_engine:progress:{member_id}:{uuid}`（含 hash tag）。這樣 `Save` 的 pipeline（SET progress + SADD member index）的兩個 key 一定同 shard。

相容性：`Get` 要從 uuid 反推 member_id 才能組出完整 key。對應的做法是讓呼叫端**永遠帶 member_id** 下來，把 `Get(id)` 改成 `Get(memberID, id)`。檢查 `ProgressStore` 介面的使用者：`cep.go` 的 `advanceProgress` 拿到的是 `*PatternProgress`（有 MemberID 欄位），OK；`ListByMember` 拿到的也有 memberID。這個改動可控。

### 4. Global `active_rules` 快取：移除

Phase 7 已經加了 `atomic.Pointer` 的 in-memory cache（`strategy.go`）。Redis 上這個全域 key 其實只是 fallback，而且：
- 在 Cluster 下變熱點
- In-memory cache 命中率 ~100%

**方案**：直接移除 Redis 上的 `rule_engine:active_rules` 讀寫，規則變更改用 **Redis Pub/Sub**（Cluster 原生支援，不綁 slot）通知所有 API instance invalidate 自己的 atomic cache。

Pub/Sub channel：`rule_engine:rule_reload`

### 5. go-redis client 換法

從 `redis.NewFailoverClient` 換成 `redis.NewClusterClient`。

讓現有 repo 程式碼不被綁死在 `*redis.Client`，改用 **`redis.UniversalClient`** 介面（go-redis 內建，`*redis.Client` 和 `*redis.ClusterClient` 都 implement）：

```go
// pkg/redis/redis.go
type RedisConfig struct {
    Mode          string   `mapstructure:"mode"`           // "cluster" | "sentinel" | "single"
    ClusterAddrs  []string `mapstructure:"cluster_addrs"`  // cluster 模式用
    MasterName    string   `mapstructure:"master_name"`    // sentinel 模式用
    SentinelAddrs string   `mapstructure:"sentinel_addrs"` // sentinel 模式用
    Password      string
    DB            int
    PoolSize      int
    MinIdleConns  int
}

var client redis.UniversalClient

func Init(cfg RedisConfig) {
    once.Do(func() {
        switch cfg.Mode {
        case "cluster":
            client = redis.NewClusterClient(&redis.ClusterOptions{
                Addrs:        cfg.ClusterAddrs,
                Password:     cfg.Password,
                PoolSize:     cfg.PoolSize,
                MinIdleConns: cfg.MinIdleConns,
            })
        case "sentinel":
            // 保留現有 sentinel 邏輯，做為 fallback / dev 環境
        }
    })
}

func GetClient() redis.UniversalClient { return client }
```

所有使用方（`service/base/behavior/repository/redis/behavior.go`, `service/base/cep/repository/redis/cep.go`, `service/base/rule/usecase/strategy.go`）把 struct 的 `*redis.Client` 欄位換成 `redis.UniversalClient`。API 層面的 `Pipeline()`, `ZRangeByScore()`, `Get()`, `MGet()` 等方法簽章兩邊都一樣。

---

## 要改的檔案

| 檔案 | 改動 |
|---|---|
| `pkg/redis/redis.go` | 新增 cluster 模式、`UniversalClient` |
| `service/base/behavior/repository/redis/behavior.go` | `eventKey()` 加 hash tag：`rule_engine:events:{%s}:%s`；client type 換 |
| `service/base/cep/repository/redis/cep.go` | progress key 改為 `rule_engine:progress:{%s}:%s`；`Get(id)` → `Get(memberID, id)`；`redisMemberIndex` 加 braces；client type 換 |
| `service/base/cep/model/interface.go` | `ProgressStore.Get` 介面簽章改 |
| `service/base/cep/usecase/cep.go` | 呼叫 `Get` 時帶 memberID |
| `service/base/rule/usecase/strategy.go` | 移除 `rule_engine:active_rules` 的 Redis 讀寫；`invalidateCache` 改為 Pub/Sub publish；新增 subscriber 處理訊息 |
| `service/bff/apis/wire/wire_gen.go` | 啟動時註冊 Pub/Sub subscriber |
| `scripts/cluster-start.sh` | **新檔案**：啟動 6 個 native redis-server process + bootstrap cluster |
| `scripts/cluster-stop.sh` | **新檔案**：停止 cluster |
| `config.yaml` / `config.example.yaml` | `redis.mode: "cluster"`、`redis.cluster_addrs` |
| `docker-compose.yml` | 移除 `redis-master`, `redis-replica-*`, `redis-sentinel-*` services（本機開發不再用 Docker 跑 Redis） |

---

## Native macOS Redis Cluster（避開 Docker for Mac 網路開銷）

之前 benchmark 已經證實 Docker for Mac 會吃掉 31-58% 吞吐量，所以**本機 benchmark 要用 native Redis**。Homebrew 的 `redis` 套件已經包含 `redis-server` 和 `redis-cli --cluster`，完全可以在 localhost 跑 cluster。

### 架構

6 個 redis-server process 跑在 localhost 的不同 port：

| Role | Port | Data dir |
|---|---|---|
| master-1 | 7001 | `/tmp/redis-cluster/7001` |
| master-2 | 7002 | `/tmp/redis-cluster/7002` |
| master-3 | 7003 | `/tmp/redis-cluster/7003` |
| replica-1 | 7004 | `/tmp/redis-cluster/7004` |
| replica-2 | 7005 | `/tmp/redis-cluster/7005` |
| replica-3 | 7006 | `/tmp/redis-cluster/7006` |

每個 process 用同一套 `redis.conf` template + override port：

```conf
port 700X
cluster-enabled yes
cluster-config-file nodes-700X.conf
cluster-node-timeout 5000
appendonly yes
dir /tmp/redis-cluster/700X
```

### Bootstrap 腳本

`scripts/cluster-start.sh` 範例流程：

```bash
#!/usr/bin/env bash
set -euo pipefail

BASE=/tmp/redis-cluster
REDIS=/opt/homebrew/opt/redis/bin/redis-server
CLI=/opt/homebrew/opt/redis/bin/redis-cli

# 1. 清理舊資料 + 建 6 個 data dir
rm -rf "$BASE"
for port in 7001 7002 7003 7004 7005 7006; do
    mkdir -p "$BASE/$port"
    cat > "$BASE/$port/redis.conf" <<EOF
port $port
cluster-enabled yes
cluster-config-file nodes-$port.conf
cluster-node-timeout 5000
appendonly yes
dir $BASE/$port
daemonize yes
pidfile $BASE/$port/redis.pid
logfile $BASE/$port/redis.log
EOF
    $REDIS "$BASE/$port/redis.conf"
done

# 2. 等 process 起來
sleep 1

# 3. Bootstrap cluster（一次性，auto-confirm）
$CLI --cluster create \
    127.0.0.1:7001 127.0.0.1:7002 127.0.0.1:7003 \
    127.0.0.1:7004 127.0.0.1:7005 127.0.0.1:7006 \
    --cluster-replicas 1 --cluster-yes
```

`scripts/cluster-stop.sh`：對每個 pidfile kill 掉 process。

### Config

```yaml
# config.yaml
redis:
  mode: "cluster"
  cluster_addrs:
    - "127.0.0.1:7001"
    - "127.0.0.1:7002"
    - "127.0.0.1:7003"
  pool_size: 0  # 預設（go-redis ClusterOptions 針對 cluster 模式有自己的預設）
```

ClusterClient 只需要任一個 node 的地址就能 discover 全部，但提供多個可以在啟動時有 fallback。

### 如果要回到 Docker Sentinel？

保留 config switch：`redis.mode: "sentinel"` 走舊路徑。production 部署可能想用 docker / k8s。

---

## Resharding 策略：從 3 masters 擴到 4 masters

Redis Cluster 的分片**不是 `hash % N`**，而是兩層抽象：

### 層一：key → slot（固定對應）

```
slot = CRC16(hash_tag) mod 16384
```

Hash tag `{user123}` 的 slot **永遠是同一個值**（例如 8234），不管叢集有幾台 master。

### 層二：slot → master（可變對應）

**3 masters 時**：
```
master-1: slots 0–5460       (5461 slots)
master-2: slots 5461–10922   (5462 slots)
master-3: slots 10923–16383  (5461 slots)
```
`{user123}` slot=8234 → master-2

**加入 master-4 後**（執行 `redis-cli --cluster reshard`）：
```
master-1: slots 0–4095        (4096 slots)
master-2: slots 4096–8191     (4096 slots)
master-3: slots 8192–12287    (4096 slots)
master-4: slots 12288–16383   (4096 slots)
```
`{user123}` slot=8234 → master-3（從 master-2 搬過去）

### 跟 naive `hash % N` 比

| Member | 3 nodes (%3) | 4 nodes (%4) | 搬家？ |
|--------|--------------|--------------|--------|
| user123 (hash=7) | node-1 | node-3 | ✗ 要搬 |
| user456 (hash=10) | node-1 | node-2 | ✗ 要搬 |
| user789 (hash=12) | node-0 | node-0 | ✓ 不動 |

Naive modulo：**約 75% 的 key 要搬**。
Redis Cluster：**只有被分配到 master-4 的 ~1/4 slot 要搬**（即 ~25%），其他 slot 的 key 完全不動。

這本質上是 **consistent hashing**，用「16384 slots 作為中介」讓每次 rebalance 只動最小必要的 key。

### 實際擴充流程

```bash
# 1. 啟動新的 master-4（port 7007）
./scripts/start-node.sh 7007

# 2. 加入 cluster
redis-cli --cluster add-node 127.0.0.1:7007 127.0.0.1:7001

# 3. 重新平衡 slots（自動分配 ~4096 個 slots 到 master-4）
redis-cli --cluster rebalance 127.0.0.1:7001

# 或手動指定：從 master-2 搬 1365 個 slot 到 master-4
redis-cli --cluster reshard 127.0.0.1:7001 \
    --cluster-from <master-2-node-id> \
    --cluster-to <master-4-node-id> \
    --cluster-slots 1365 \
    --cluster-yes
```

### 資料搬遷過程（不丟資料、不中斷服務）

當 slot X 正在從 master-2 搬到 master-4：

```
1. master-2 把 slot X 標記為 MIGRATING
2. master-4 把 slot X 標記為 IMPORTING
3. 逐個 key 做 MIGRATE（單一 atomic 操作）
4. 期間對 slot X 的 key 操作：
   - key 還在 master-2  → master-2 回應
   - key 已搬到 master-4 → master-2 回 "ASK target-addr" → client 重試 master-4
   - go-redis v9 自動處理 ASK redirect，app 無感
5. 全部搬完後，所有 cluster 節點更新 slot ownership map
```

零 downtime，短暫的額外 latency（每個 migrating key 最多一次 ASK redirect）。

### Hash tag 的設計意義

我們的 key schema 用 `{member_id}` 作為 hash tag，讓：
- 同一個 member 的**所有 keys**（events、progress、member index）都落在同一個 slot
- Resharding 時一起搬，保證 pipeline / Lua script 不會跨 shard（不會遇到 CROSSSLOT 錯誤）
- 整個 member 永遠在同一個 shard，data locality 好

如果沒用 hash tag，不同 behavior 的 key 會落在不同 slot → 不同 master → pipeline 跨 shard 直接 `CROSSSLOT` 錯誤。這就是為什麼**計畫早期要把 key schema 全改掉**，而不是先部署 cluster 再慢慢改。

### 何時該 reshard？

監控指標：
- 每個 master 的 `used_memory`、`ops_per_sec`
- 若某個 master 明顯負載高於其他（例如 2x 以上），加一個 master 並 rebalance
- 若整體已經吃滿，加 master 同時 rebalance 擴容

---

## 固定 Member 到特定 Master（進階需求）

Redis Cluster **沒有原生的「pin member」機制**。要固定某個 member 在某個 master 上，分兩個層次處理：

### 層次一：同一個 cluster 拓樸下 — 自然固定

用 `{member_id}` 當 hash tag 時：
```
{user123} → CRC16 mod 16384 = 8234 → master-2
```
只要 **slot-to-master 的對應表不變**，member 就固定在 master-2。Redis Cluster 不會自動搬 slot，除非你跑 `rebalance` 或 `reshard`。

實務做法：
- **不跑 `--cluster rebalance`**（會自動重分配 slot）
- 只在 **明確需要擴容** 時手動 `--cluster reshard` 搬特定的 slot
- 監控每個 master 的 `used_memory` / `ops_per_sec`，不平衡才介入

這是最簡單的做法，**99% 的場景夠用**。

### 層次二：完全可控 — 兩層映射（Application-Level Shard）

如果業務有嚴格需求（例如：VIP 大戶永遠在獨占 master、普通用戶分散到其他 masters），在 key 設計上加一層：

#### Key schema
```
{shard-vip}:user123:events:CryptoWithdraw    ← hash tag = "shard-vip"
{shard-0}:user456:events:CryptoWithdraw      ← hash tag = "shard-0"
{shard-1}:user789:events:CryptoWithdraw      ← hash tag = "shard-1"
```

注意：Redis 只取**第一個** `{...}` 當 hash tag，`{user123}` 變成純文字。所以同一個 `shard-X` 值的所有 key 會在同一個 slot。

#### Application-level 對應

`member_id → shard_id` 由應用層決定，存在單獨的地方：

```go
type MemberShardRouter struct {
    // source of truth: DB / config service / dedicated Redis
    // small table: {member_id → shard_name}
}

func (r *MemberShardRouter) Shard(memberID string) string {
    // 大戶查 DB 決定
    if r.isVIP(memberID) {
        return "shard-vip"
    }
    // 一般用戶用 consistent hash 分到 shard-0/1/2
    return fmt.Sprintf("shard-%d", hashring.Get(memberID))
}

// Key builder
func eventKey(shardTag, memberID, behavior string) string {
    return fmt.Sprintf("rule_engine:{%s}:events:%s:%s", shardTag, memberID, behavior)
}
```

#### 把 shard-X 映射到特定 master

Redis Cluster 端用 `CLUSTER KEYSLOT` 查 hash tag 對應 slot，再用 reshard 手動指派：

```bash
# 查 shard-vip 這個 hash tag 落在哪個 slot
redis-cli -p 7001 cluster keyslot "{shard-vip}:x"
# → 例如輸出 12345

# 若這個 slot 目前不在 master-4 上，用 reshard 搬過去
redis-cli --cluster reshard 127.0.0.1:7001 \
    --cluster-from <current-owner-id> \
    --cluster-to <master-4-id> \
    --cluster-slots 1 \
    --cluster-yes
# 這個 reshard 只搬這一個 slot，對應這個 hash tag 的所有 key
```

#### 換 shard 怎麼辦

VIP 用戶要從 `shard-vip` 搬到 `shard-0`？這是**應用層的遷移**：
1. 讀舊 key（`{shard-vip}:user123:*`）
2. 寫到新 key（`{shard-0}:user123:*`）
3. 刪舊 key

不能用 Redis 內建的 slot migration，因為兩個 key 的內容不同（hash tag 不同）。這個成本在應用層要能接受。

### 兩層方案對比

| 需求 | 推薦做法 |
|------|---------|
| 一般情境，只是不想被自動 rebalance 打散 | 層次一：hash tag = member_id + 避免自動 rebalance |
| VIP/大戶要獨佔某個 master | 層次二：用 `{shard-vip}` 這類 tag + 手動把該 slot 固定到特定 master |
| 地理合規（特定用戶必須在某地區） | 直接部署**獨立的 cluster 到該地區**，應用路由 |
| 完全自訂 member → master | 層次二 + 維護應用層 member-to-shard 映射表 |

### 建議

**先不要做層次二**，除非有明確業務需求。理由：
- 層次二增加應用層複雜度（映射表、遷移邏輯、一致性）— 如果映射表有 race condition，就會寫到錯的 shard
- Redis Cluster 預設的 slot 分佈在沒有 auto-rebalance 下已經穩定
- 加一層抽象容易失控

---

## 遷移注意

這是 breaking change，不能無痛 rolling upgrade。建議步驟：

1. **Dev/staging 先跑**：改完後在 docker-compose 本機驗證。
2. **清空舊資料**：舊 sentinel Redis 和新 cluster 資料格式不相容（key 名稱不同）。生產切換時事件短暫 miss，但 rule cache 60s 內會自動重建。若要無縫，需寫一次性 migration script 讀舊 keys 改名寫進 cluster。
3. **相容時期**：`pkg/redis/redis.go` 保留 sentinel mode，用 config 切換，回退容易。

---

## 驗證

### 功能驗證

```bash
# 先把 brew 上的單一 redis 停掉
brew services stop redis

# 起 native cluster
./scripts/cluster-start.sh

# 確認 slot 分佈
redis-cli -p 7001 cluster slots
redis-cli -p 7001 cluster nodes

# 跑所有 unit tests（使用 miniredis 不受影響）
go test ./service/base/behavior/repository/redis/ -count=1
go test ./service/base/cep/repository/redis/ -count=1

# 清資料 + 跑 API integration benchmark
redis-cli -p 7001 -c flushall
go test -bench=BenchmarkCheckEvent_Throughput_Integration -benchtime=10s ./service/bff/apis/usecase/

# 測完停 cluster
./scripts/cluster-stop.sh
```

### 效能驗證

真正的效益要在**多台 client 同時打不同 member** 的情境下才看得出來。單一 benchmark process 的 workers-32 可能看不到線性提升，因為：
- 該 process 仍受單一 TCP stack / goroutine scheduler 限制
- 真正測試要開 N 個 benchmark process，每個打不同 member 範圍

建議 benchmark 方法：
1. 預設跑一次 single-Redis baseline
2. 切到 cluster，跑同樣 benchmark
3. 跑一個新的 multi-process benchmark：開 4 個 process，每個只打 1/4 members

預期效果：
- Single-process single-member load：throughput 大致持平（可能因為 ClusterClient 有 MOVED redirect 所以略差 5-10%）
- Multi-process cross-member load：throughput 應 ~線性（3 masters → 吞吐量上限 × 3）

### Key 分佈驗證

```bash
# 確認 hash tags 有效，同 member 的 keys 在同 slot
redis-cli -p 7001 cluster keyslot "rule_engine:events:{user123}:CryptoWithdraw"
redis-cli -p 7001 cluster keyslot "rule_engine:progress:{user123}:uuid-abc"
# 兩個輸出應該一樣（同一個 slot number）
```

---

## 風險與取捨

1. **ClusterClient 會有 MOVED redirect overhead**：第一次打錯 node 會被 redirect。go-redis 會自動 cache slot map，之後直接打對的 node。benchmark 啟動時會看到一個短暫的 degradation window。

2. **Pub/Sub 跨 node**：Cluster 的 Pub/Sub 會 broadcast 給所有 master，subscriber 自動收到。但每個 publish 也會在 cluster bus 上傳播，高頻 publish 會略增 internal bandwidth。`rule_engine:rule_reload` 是低頻事件（規則才改時才 publish），影響可忽略。

3. **增加 shard（resharding）是線上操作**：`redis-cli --cluster reshard` 可線上搬 slots，但正在被搬的 slot 上的 key 會被 ASK redirected，client 會稍微慢。實作時需確認 go-redis 對 ASK 的處理是透明的（v9 是）。

4. **失敗容忍度下降**：Sentinel 架構下任一 replica 下線不影響服務；Cluster 下如果某個 master + 它的 replica 同時下線，該 shard 就不可用（hash slot 沒人服務）。生產環境應該 master/replica 部署到不同 availability zone。
