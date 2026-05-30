# NATS JetStream vs Kafka — 完整比較與選型理由

**用途**：[in-memory-rule-engine-plan.md](./in-memory-rule-engine-plan.md) 的選型附錄,面試備忘。

統整過往討論的所有面向,**包含我們踩過的錯誤直覺與更正**——這些「為什麼這個常見講法是錯的」往往才是面試的關鍵。

---

## TL;DR

| 視角 | 結論 |
|------|------|
| 對**這個專案**的最終決策 | **雙 backend(MQ-pluggable)**:Kafka 為 production default(金融同業背書),NATS 為低延遲變體 + 內嵌測試 |
| Kafka 為 production default 的核心理由 | 金融業實戰背書(Coinbase / PayPal / ING)+ KIP-405 tiered storage + Kafka transactions EOS |
| NATS 為 alternative 的核心理由 | 可內嵌測試(Kafka 不行)、log+KV 一套、Go 原生、無 JVM GC 尾延遲穩 |
| 「Kafka 是 JVM 所以慢」 | ❌ **錯**,Kafka 吞吐通常更強;真正差別在尾延遲(GC)跟足跡 |
| 「NATS push 比較有效率」 | ❌ **錯**,push 省 round-trip 但失去背壓;我們連在 NATS 內都選 pull |
| 「Flink 配 Kafka,所以我們也該配 Kafka」 | ❌ **不成立**,那些理由是 Flink 帶來的,不是 streaming 本質需要的 |
| 「我們既有系統有 Kafka,所以用 Kafka」 | ⚠️ 這是**運維方便**理由,不是**技術選型**理由,面試別主打 |

---

## 一、決策框架(看你的需求落在哪一格)

| 需求 / 場景 | 選 NATS | 選 Kafka |
|------------|:---:|:---:|
| Go 技術棧,不想引入 JVM | ✓ | |
| 單 binary 部署、可內嵌(測試/邊緣) | ✓ | |
| 低且可預測的尾延遲(p99) | ✓ | |
| 需要 log + KV 在同一套(checkpoint metadata) | ✓ | |
| 流量 < 100K msg/s per stream | ✓ | |
| Subject 階層路由(`x.{shard}.{member}` 通配符) | ✓ | |
| **極大吞吐 + 金融業實戰背書**(aggregate > 5M msg/s;**金融**:PayPal 詐欺偵測 / Coinbase 加密貨幣風控 / ING 核心銀行 / Goldman Sachs 交易;**通用**:LinkedIn / Uber / Netflix 10M+/s) | | ✓ |
| **多年保留 + 透明 broker replay**(法遵 / 全歷史回填) | | ✓ |
| **生態需求**(Kafka Streams / Connect / Schema Registry / Flink 一級整合) | | ✓ |
| **跨分區 exactly-once 事務** | | ✓ |

---

## 二、側對側功能矩陣

| 維度 | NATS JetStream | Kafka |
|------|---------------|-------|
| 語言 / 部署 | Go,單 binary | JVM,叢集(JobManager/Broker + KRaft 或舊 ZK) |
| 訊息模型 | Subject(hierarchical + wildcard) | Topic + Partition |
| Consumer 模型 | **Push 跟 Pull 都支援** | **僅 Pull** |
| 持久化 | File store / Memory store(本機) | Log-structured(本機)+ **Tiered Storage 可放 S3/GCS** (KIP-405) |
| 內建 KV | ✓ JetStream KV(同叢集) | ✗ 要另外搭(etcd / Redis / DB) |
| 內建 Object Store | ✓ JetStream Object Store | ✗(可用 tiered storage,但語意不同) |
| Subject 路由 | Wildcards(`x.*.y`, `x.>`)server-side filter | Topic + partition key,過濾較粗 |
| Exactly-once | Publish dedup window(`Nats-Msg-Id`)+ double-ack;**較弱** | Transactional producer/consumer,跨 topic/partition;**成熟** |
| 多租戶 | Accounts + Leaf Nodes(edge) | ACLs |
| 吞吐量 | 單 stream 約 ~100K-300K msg/s(視硬體) | 單分區 ~100K+,叢集可達數百萬 msg/s |
| 延遲(p50,小訊息) | sub-ms ~ 低 ms | 低 ms(調過後) |
| 延遲(p99) | 較穩(無 JVM GC) | 受 JVM GC 影響可能尖刺,可調但複雜 |
| 啟動時間 | 秒級 | 較慢(JVM 暖機) |
| 記憶體足跡 | 小 | 大(JVM heap + page cache 預期較大) |
| 生態 | 較小;NACK CLI、官方 SDK 多語言 | **極龐大**:Connect、Streams、ksqlDB、Schema Registry、Flink/Spark 原生 |
| 大規模實戰 | 較新(2021 GA);中小規模穩 | 久經考驗,百萬 msg/s 部署常見 |

---

## 三、維度深挖

### 3.1 Consumer 模型(Push vs Pull)

**Kafka 只有 pull**(`poll()`)。NATS 兩種都支援,我們**選 pull**。

#### Pull 的優勢(我們選它的真實理由)

```
PUSH:  broker ──盡力推──▶ consumer
       consumer 慢 → in-flight 累積 → OOM(有 flow control 但你被推著跑)

PULL:  consumer ──「給我 N 筆」──▶ broker
       處理完才拉下一批 → 速度由 consumer 決定 → 天然背壓
```

對「有狀態 + 吞吐受限 + 要 single-writer 保序」的處理器,pull 必勝:
1. **天然背壓**:處理不完就不拉,記憶體有界
2. **跟 single-goroutine 架構契合**:`for { msg := fetch(); process(msg); ack() }` 一條同步迴圈,天然保序;push 走 callback/channel 偏並發,要多做一層序列化
3. **offset 完全在 consumer 手上**:跟 checkpoint 機制接得乾淨

#### 「Push 比較有效率」是錯的直覺(自我糾正)

push 唯一優勢:**省 fetch round-trip → 單筆延遲略低**。但這是 per-msg 延遲,不是吞吐;代價是失去背壓。對有狀態處理器,這是錯的交易。

#### 跟 Flink 的對照(關鍵深度)

| 層次 | Flink 怎麼做 |
|------|-------------|
| Flink ↔ Kafka(外部來源) | **Pull**(Kafka 只能 pull) |
| Flink runtime ↔ SourceReader(新 Source API, FLIP-27) | **Pull**(runtime 呼叫 `pollNext`) |
| Operator 之間 | **Credit-based 流控**——看起來像 push,但沒 credit 推不動,等同 consumer-paced |

**淨效果**:整條 Flink pipeline 是「消費者掌控節奏」,背壓從 sink 一路傳回 source。**我們的 `pull + MaxAckPending` 是它的單階段簡化版**——同樣的哲學,不用跨多 operator 傳播。

---

### 3.2 吞吐量

#### Kafka 通常贏

Kafka 為極大吞吐而生,靠這幾招:
- **Zero-copy**(`sendfile`):broker 直接從磁碟 page cache → socket,不走 JVM heap
- **OS page cache**:不在 JVM heap 內 cache log 資料,避開 GC
- **Sequential disk I/O + log-structured**:寫入摩擦最低
- **Batching + compression**:per-msg 開銷攤平
- **Partition 並行**:多 partition 線性擴展

實測:Kafka 單分區可達 100K+ msg/s,叢集數百萬。

#### NATS 在中等規模穩

NATS 單 stream 約 100K-300K msg/s(視訊息大小、replication、硬體)。對我們**per-shard 100K 目標**綽綽有餘。要更高就**多 stream / 分 shard**。

#### ⭐ 實戰背書的差距(真實重要的選型依據)

兩者「能不能撐」跟「**有沒有人撐到過**」是兩回事,而且**「有沒有同業撐到過」更重要**:

| | NATS JetStream | Kafka |
|--|---------------|-------|
| 單 stream/partition | ~100K-300K msg/s | ~100K-300K msg/s(同量級) |
| **公開可查通用業 aggregate 上限** | 低 millions msg/s | **10M+ msg/s**(LinkedIn、Uber、Netflix) |
| **公開可查金融業案例** | ⚠️ **大規模金融案例很少公開** | **多家頂級金融機構**(見下表) |
| 數十 M / 百 M msg/s 級別 | 沒有公開案例 | 有 |

##### Kafka 在金融業的公開實戰案例(對風控專案最相關)

| 機構 | 用途 | 為什麼跟我們相關 |
|------|------|---------------|
| **Coinbase** | Kafka + **Flink** 即時風控/詐欺偵測 | **最直接對標**——加密貨幣交易所 + 風控,plan §為什麼不用 Flink 就引用它(`references.md` A.6) |
| **PayPal** | 詐欺偵測 pipeline(公開過 billions/day 量級) | 即時詐欺風控,直接對標 |
| **ING Bank** | 核心銀行交易 on Kafka(Kafka Summit 公開分享) | 銀行核心系統可靠性背書 |
| **Goldman Sachs** | 交易系統 + 市場資料 pipeline | 高頻金融系統 |
| **Robinhood** | 交易數據 pipeline | 零售交易平台 |
| **Mastercard / Visa** | 交易處理(部分公開) | 支付網路 |

##### NATS 在金融業的情況(誠實版)

- 一些小型 fintech / 加密貨幣團隊用 NATS,但**大規模公開金融案例少**
- 不是「不能用」,是「**沒人在這個級別公開驗證過**」
- 對 production 風控/金融場景,**這個 gap 是真實風險**——不只是技術 spec,而是「監管、稽核、可靠性背書」這個層面同業沒人替你背書

##### 實務上的吞吐分區(務實版)

```
aggregate 吞吐         該選
< 1M msg/s            NATS 完全 OK,其他 profile 優勢還在
1M – 5M msg/s         兩者都行,NATS 開始接近公開案例邊界
5M – 10M msg/s        Kafka 有金融業實戰背書,NATS 沒有
> 10M msg/s           Kafka 或專用方案(Pulsar、自研)
```

**「金融同業實戰背書」本身就是 production 選型的重要理由**——金融場景選用「沒同業在這個量級用過」的東西,要面對監管/稽核/上線審批時很難交代。我們專案目標 500K aggregate(plan §M5)離邊界遠,NATS 還算安全;但**真要奔 production 金融風控的大規模,Coinbase 用 Kafka+Flink 是最直接的可仿效路徑**。

#### ⚠️ 面試陷阱:不要說「Kafka 慢」

說「Kafka 慢」會被立刻打臉。正確說法:**「Kafka 不慢——吞吐它通常更強,而且金融業如 PayPal 詐欺偵測、Coinbase 加密貨幣風控、ING 核心銀行都有公開實戰背書;我選 NATS 是因為其他原因(延遲輪廓、log+KV 一套、Go 原生),不是吞吐。」**

---

### 3.3 延遲輪廓(JVM 在這裡才出現)

#### p50(中位數)

兩者都可低 ms。Kafka 預設為吞吐優化(batching),要低延遲要調 `fetch.min.bytes`、`linger.ms`。NATS 預設就低延遲。

#### p99(尾延遲)— NATS 的真實優勢

JVM 的 **GC pause** 會打到 Kafka broker 的尾延遲:Major GC 可能造成 10ms+ 尖刺。NATS 用 Go,GC pause 通常 < 1ms 且可預測。

對風控目標 sub-ms p50 + 穩定 p99 的場景,這是 NATS 的**真實技術優勢**。

#### ⚠️ 但別吹過頭

- Go 也有 GC
- Kafka p99 可調(G1GC tuning、heap 大小、避免 full GC)
- 差別是「modest edge」,不是天壤之別

#### 正確的講法

> 「不是 Kafka 慢,是 JVM GC 會讓尾延遲不穩、足跡大。NATS 的 Go runtime 讓 p99 更可預測——對 sub-ms 延遲目標是真實優勢,但別講成 Kafka 不能低延遲。」

---

### 3.4 部署足跡(技術事實參考,非選型決定性)

> 這節是事實參考——團隊規模 / SRE 編制不該影響純技術選型,所以這些差異**不是**選 NATS 的理由,僅供瞭解兩者部署型態。

| | NATS | Kafka |
|--|------|-------|
| 部署單元 | 單一 Go binary | JVM + broker + (KRaft controller 或 ZK 叢集) |
| 啟動時間 | 秒級 | 較慢(JVM 暖機) |
| 記憶體足跡 | 小(每節點數百 MB 起跳) | 大(JVM heap GB 起跳 + page cache) |
| 設定複雜度 | 較簡單 | 較高(heap、GC、broker config 一大堆) |
| 可內嵌(in-process) | ✓(我們測試就嵌進來) | ✗ |
| 邊緣部署 / leaf nodes | ✓ 原生支援 | ✗(較困難) |

其中「可內嵌」對 **測試** 是真實技術價值(我們測試就嵌入 NATS server 跑端到端 — 見 `consumer_test.go`),這是 §5 verdict 已列的理由。

---

### 3.5 ⭐ 儲存模型與分層儲存(Kafka 的真實大優勢)

**這是我們之前差點繞過去的點——必須誠實面對。**

#### Kafka:KIP-405 Tiered Storage(3.6 GA, 2023)

- 熱層:本機 disk(快)
- **冷層:S3 / GCS / Azure Blob**(便宜、近乎無限)
- **對 consumer 完全透明**:按 offset 讀,broker 自動從冷層拉回
- consumer 感受不到熱/冷差別

#### NATS:沒有原生分層

- 只有本機 file store(或 memory)
- 超過 `MaxAge/MaxBytes` 就刪掉
- 想長期保留 = DIY 出去(寫自己的 archive consumer 灌 S3)

#### 儲存規模算給你看(at 100K events/s,200 bytes/event)

```
1 天    1.7 TB    本機 NVMe 一顆夠
7 天    12 TB     幾顆 NVMe
30 天   50 TB     一台機器塞滿
90 天   150 TB    開始痛
1 年    600 TB    本機 SSD 不可能
7 年    4+ PB     法遵保留 → 一定要物件儲存
```

**plan §Schema Evolution 寫「retention ≥ 30 天」= 50 TB,本機勉強撐**;超過 90 天本機現實上崩。

#### 對我們專案的處理方式:分兩層

```
NATS:     操作熱窗(7~30 天)
          ─ 災難復原 replay(~snapshot_interval,秒級)
          ─ Schema migration replay(週~月級)
          ─ Shadow traffic 比對(週級)
            ↓ (consumer-to-S3 archiver)
S3/物件儲存:  審計 / 法遵歸檔(任意長期)
          ─ 不走 NATS,獨立批次/DW 管線
          ─ 不需要透明 replay 回引擎
          ─ 給 audit query / 報表 / 回填用
```

#### 為什麼這個分層也站得住

即便用 Kafka 分層儲存,**多數 production 也會另開 DW 管線**(BigQuery/Snowflake)做 audit query——Kafka 分層解的是「broker 內透明 replay」,**不是 ad-hoc query**。所以「NATS 熱窗 + S3 歸檔 + DW」跟「Kafka 分層 + DW」在實務上差別比想像小。

#### Kafka 真的贏到該換的情境

| 情境 | 該選 |
|------|------|
| 法遵需要原始 stream 多年可 broker 透明 replay | **Kafka 分層** |
| Operator 邏輯每年大改、要從零 replay 數月 | **Kafka 分層** |
| 多年保留只為 audit query(常見) | NATS + S3 歸檔 |
| 30 天內 schema migration / shadow traffic | NATS 本機儲存夠 |
| 災難復原 / 日常 replay | NATS 綽綽有餘 |

**多數風控落在中間幾欄**;真要原生多年 replay 才該換 Kafka。

---

### 3.6 Exactly-Once 語意

#### Kafka:成熟的 Transactional Producer/Consumer

- 跨多 topic/partition 的 atomic write
- `read-process-write` 迴圈用 transaction 包起來,exactly-once 端到端
- Kafka Streams 原生用這套

#### NATS JetStream:較弱

- Publish dedup window(`Nats-Msg-Id` + `Duplicates` window,預設 2 分鐘)
- Double-ack(consumer ack 後等 server 確認)
- **沒有跨 stream 的事務**
- 真要 exactly-once 要靠 **應用層 + 冪等**(我們的做法:`event_id` 比對 snapshot 還原的 state)

#### 我們的處理(plan §End-to-End Exactly-Once)

不依賴 NATS 原生 dedup,因為:
- NATS dedup window 是 **publish 端、分鐘級、狀態在 server 不進 snapshot**
- 真正的重複來自 **consumer redelivery + 故意 replay**
- 我們的 `event_id` 冪等 + snapshot 含去重狀態,跨重啟存活

所以連在 NATS 內,我們也選**應用層冪等而非原生 dedup**——這個決策跟 NATS/Kafka 選型其實**正交**。

---

### 3.7 路由模型

#### NATS:Subject 階層 + Wildcards

```
rule.events.0.user_abc
rule.events.0.>           ← server-side filter,wildcards
rule.events.*.high_risk   ← 任意 shard、特定後綴
```

跟我們 sharding(`rule.events.{shard}.{member}`)**完美對應**。consumer 端用 `FilterSubjects` 撈自己那份,粒度細。

#### Kafka:Topic + Partition

```
Topic "rule-events" with 4 partitions
Partition key = member_id(consumer 看不到「shard」這個概念,看 partition)
```

- 過濾較粗:consumer 拿到整 partition 後自己過濾,或用 Kafka Streams 操作
- Partition 數一旦上線很難改(rescaling 痛)
- Per-member routing 靠 partition key 慣例

#### 對我們設計的影響

我們的 plan key-group 抽象解決 Kafka rescaling 痛點的同一個問題,但 NATS 的 subject 天生支援我們要的路由形狀,**更原生**。

---

### 3.8 內建 KV / 多系統架構

#### NATS:一套搞定

JetStream 同叢集提供:
- **Stream**:event log
- **KV bucket**:給 checkpoint metadata 用(`shard_N → {barrier_id, nats_seq}`)
- **Object Store**:大物件
- **Request-Reply**:RPC

→ **一個叢集**就解決 log + KV + RPC,維運只一組。

#### Kafka:要組合

- Kafka:event log
- 另開 **etcd / Consul / Redis** 給 checkpoint KV
- 另開 RPC(gRPC/REST)
- 通常還要 **Schema Registry**(維運另一個)

→ production 多組系統 = 多組故障模式、多組維運。

#### 對 plan 的具體價值

plan §Checkpoint 用 `kv := js.KeyValue("checkpoint_meta")` 一行接到——**Kafka 寫不出這行**,要去寫 etcd/Redis 客戶端、處理另一套設定。

---

### 3.9 生態

**Kafka 完勝**,毫無爭議:

| 項目 | Kafka | NATS |
|------|-------|------|
| Stream processing | Kafka Streams, ksqlDB | 較少 |
| Connectors | Kafka Connect(數百個) | 較少 |
| Schema management | Schema Registry(Avro/Proto/JSON) | 無原生 |
| Flink / Spark 整合 | 一級公民 | 少 |
| 觀測性 | Confluent Control Center 等成熟 | 較陽春 |
| 人才池 | 龐大 | 小 |
| 文件 / 書籍 / 培訓 | 海量 | 較少 |

**如果你的選型要看「能不能找到會用這個的人」、「能不能直接接 Snowflake/Flink/任何工具」、「能不能買 enterprise support」——Kafka 贏。**

---

## 四、Flink 為什麼配 Kafka?那些理由對我們成立嗎?

### Flink 配 Kafka 的三個根本原因

1. **Flink 靠來源 partition 餵 operator 平行度** — Flink 把 job 拆 operator 散到多 TaskManager,需要來源本身分區。Kafka partition 直接餵
2. **Flink 的 checkpoint 跟 Kafka offset 深度整合** — exactly-once 靠 offset 跟 state 一起 checkpoint
3. **同生態、同量級** — 都是 JVM、為極大吞吐而生

**關鍵洞察:這三個理由,是「因為它是 Flink」才需要,不是「因為要做 streaming」才需要。**

### 對我們是否成立?

| Flink 需要 Kafka 的理由 | 對我們成立嗎 |
|----------------------|--------------|
| ① Partition 餵 operator 平行度 | ❌ 我們**自己做 sharding**(key group + subject) |
| ② Offset 跟 checkpoint 整合 | ❌ 我們**自己管 offset**(`nats_seq` 進 snapshot);NATS 還順便給 KV |
| ③ 極大吞吐 + JVM 生態 | ❌ Per-shard 100K 在 NATS 範圍內;我們**刻意不要 JVM** |

**全部不成立。**「Kafka 對 Flink 是必須」的點,我們要嘛自己處理掉了、要嘛在我們的量級用不到。

### 邏輯鏈

```
1. 我們一開始就拒絕 Flink(plan §為什麼不直接用 Flink)
        ↓
2. 拒絕 Flink → 同時擺脫「Flink 需要 Kafka」這個約束
        ↓
3. 來源選擇變成「由我們的需求決定」
        ↓
4. 我們的需求:Go 原生、低延遲、單 binary、log+KV 一套、可內嵌
   → NATS 比 Kafka 更貼合
```

**Kafka 是「Flink 的最佳拍檔」,不是「我們的最佳拍檔」。拒絕了 Flink,就不該繼承它的拍檔。**

---

## 五、本專案的最終決策:雙 backend(MQ-pluggable)

> **決策**:Core 設計成 MQ-agnostic(透過 `EventConsumer` interface),**實作 Kafka 跟 NATS 兩個 backend**。**Production default = Kafka**(對齊金融業實戰背書),**NATS 為低延遲變體 / 內嵌測試**。

### 為什麼是雙 backend(不是擇一)

| 角度 | 收益 |
|------|------|
| **面試 / 履歷** | 同時拿到「會 Kafka」+「會 NATS」+「會做正確抽象」+「同業正解(Kafka)+ 探索深度(NATS 手刻內核)」 |
| **技術上正當** | A–G 七個檔本來就 MQ-agnostic;抽 interface 是自然落點,不是硬湊 |
| **驗證手段** | Shadow traffic 比對:同一串事件兩 backend 跑出同樣 state → 可量化證明 MQ-agnostic 設計成立 |
| **未來彈性** | 真實切換場景(用戶想換 broker)成本低 |

### Production Default = Kafka(三個理由)

| 理由 | 強度 | 說明 |
|------|:---:|------|
| **金融業實戰背書** | ⭐⭐⭐ | Coinbase(加密貨幣風控)、PayPal(詐欺偵測)、ING(核心銀行)、Goldman Sachs(交易)——同業在大規模 production 用過,稽核 / 上線審批好交代 |
| **吞吐 + 分層儲存** | ⭐⭐⭐ | 10M+ msg/s aggregate 有公開案例;KIP-405 tiered storage 解長期保留 |
| **生態與 Schema Registry** | ⭐⭐ | Connect / Streams / Schema Registry / Flink 一級整合 |

### NATS 作為 Alternative Backend(四個理由)

| 理由 | 強度 | 說明 |
|------|:---:|------|
| **可內嵌測試** | ⭐⭐⭐ | NATS 能 in-process(我們 `consumer_test.go` 真的這樣做);Kafka 沒這選項,測試只能跑 testcontainers |
| **一套系統 = log + KV** | ⭐⭐ | 對 checkpoint metadata 場景優雅;不用配 etcd/Redis |
| **尾延遲可預測** | ⭐⭐ | 無 JVM GC,適合「低延遲變體」這個角色 |
| **Go 原生、單 binary** | ⭐⭐ | 給「想用 NATS 的 client」一個選項;教學 / 學習版的乾淨樣板 |

### 雙 backend 共用的設計(MQ-agnostic 的根)

```go
// 兩個 backend 共用:
type EventConsumer interface {
    Run(ctx context.Context, core *Core) error
}

// 各自實作:
type natsConsumer  struct { ... }  // 已完成(Task H)
type kafkaConsumer struct { ... }  // 待做(Task J)
```

**A–G 八個檔不知道 backend 是誰**——這是抽象設計成立的證據,不是包裝話術。

### 必須承認 + 在雙 backend 下被消解的弱點

| 原本弱點 | 雙 backend 下的解法 |
|---------|-------------------|
| NATS 無分層儲存 → 長保留 | Production default 走 Kafka,直接拿 KIP-405 |
| NATS 大規模金融案例少 | Production default 走 Kafka,直接拿 Coinbase / PayPal / ING 背書 |
| NATS exactly-once 跨 stream 弱 | Production default 走 Kafka transactions;或一致用應用層 `event_id` 冪等(兩 backend 通用) |
| Kafka 不能內嵌測試 | NATS backend 提供 in-process 測試;單元 / 整合測試走 NATS |
| Kafka 維運重 | NATS backend 給 dev / 教學 / 學習場景一個輕量選項 |

**雙 backend 把兩邊的弱點互相補掉。**

---

## 六、面試常見陷阱(別講錯)

| 陷阱講法 | 為什麼錯 | 正確版 |
|---------|---------|--------|
| 「Kafka batch pull 所以不即時」 | Kafka 調 `fetch.min.bytes` / `linger.ms` 也能低延遲 | 不要主打延遲打 Kafka,改用「NATS 尾延遲更穩」 |
| 「Kafka 是 JVM 所以慢」 | 吞吐 Kafka 通常更強(zero-copy + page cache + batching);JVM 不是吞吐瓶頸 | 「JVM GC 會打到尾延遲、足跡大,不是吞吐慢」 |
| 「NATS push 比較有效率所以選 NATS」 | 我們其實選 pull;push 省 round-trip 但失背壓 | 不主打 push;主打 pull + MaxAckPending 的背壓 |
| 「我們用 NATS 因為公司沒 Kafka」 | 運維方便理由,不是技術選型理由 | 別講;用真實技術理由(log+KV、延遲、Go) |
| 「NATS 全面比較好」 | 過度宣稱會被打臉 | 主動講弱點:無分層儲存、生態小、exactly-once 較弱 |
| 「Flink 配 Kafka 所以我也應該」 | 那些理由是 Flink 帶來的,不是 streaming 本質需要 | 「Flink 需要 partition 給 operator 平行 + offset 整合,我們自己 shard + 自己管 offset,那些約束不存在」 |

---

## 七、面試 talking points(一句話版)

### ⭐ 主答:為什麼做雙 backend(MQ-pluggable)

> 「我把 Core 設計成 MQ-agnostic,實作了 Kafka 跟 NATS 兩個 backend、用 shadow traffic 比對結果一致。**Production default 是 Kafka**——金融業 Coinbase、PayPal、ING 都有大規模實戰背書,而且 KIP-405 分層儲存解長期保留、生態跟 Flink 一級整合。**NATS 留作低延遲變體 + 內嵌測試**——Go 原生、可 in-process(我們整合測試真的這樣跑)、log+KV 一套清爽,適合 dev/教學/低延遲場景。雙 backend 證明我能做正確的抽象,而且 A-G 七個檔本來就 MQ-agnostic,這個 interface 是自然落點不是硬湊。」

### 為什麼 Kafka 是 production default(同業正解)

> 「金融場景,production 選『有沒有同業在這個量級驗證過』比技術 spec 重要。Coinbase 用 Kafka + Flink 做加密貨幣風控、PayPal 用 Kafka 做詐欺偵測 billions/day、ING 拿 Kafka 跑核心銀行——這是稽核 / 上線審批時拿得出來的背書。技術上 Kafka 還有 KIP-405 tiered storage(NATS 沒有,長保留只能 DIY 到 S3)、跨分區 exactly-once 事務、Schema Registry。所以 production default 走 Kafka 是負責任的選擇,即使我自己手刻內核時 NATS 更順手。」

### 為什麼用 pull 不用 push

> 「pull 讓消費者掌控節奏——處理完才拉下一批,配 MaxAckPending 就是天然背壓,慢消費者不會被推爆。pull 的同步迴圈也對應我 single-writer 模型,offset 完全由 checkpoint 掌握。push 適合低延遲扇出,但對有狀態、吞吐受限、要復原的處理器,消費者掌控才安全。」

### Flink 配 Kafka 為什麼不影響我選 NATS

> 「Flink 配 Kafka 是因為 Flink 需要 partition 餵叢集平行度、offset 跟 checkpoint 整合、且同屬 JVM 重型生態。我刻意不用 Flink——手刻它的概念在單 binary Go 上跑,自己 shard、自己管 offset、per-shard 量級用不到 Kafka 極限吞吐。所以『Flink 需要 Kafka』那些理由對我都不成立,我按自己 profile(Go 原生、低延遲、log+KV 一套、可內嵌)選 NATS。要擬真 Flink 部署 Kafka 更忠實,但我的目的是手刻內核,NATS 讓整套小到能完全掌握。」

### 關於分層儲存的弱點(誠實版)

> 「NATS 沒有 Kafka 的分層儲存,這對長保留是真實弱點——本機 SSD 在 100K events/s 大概撐 7-30 天。我會分兩層:NATS 當操作熱窗(復原 / migration / shadow),長期 audit 寫 S3 走獨立歸檔管線。這分層其實滿乾淨,因為即使有 Kafka 分層,你通常也會另一條 DW 給 ad-hoc query。**但如果硬需求是多年 in-broker 透明 replay——例如法遵或頻繁全歷史回填——Kafka 分層儲存才是對的答案,那是該重新考慮選型的明確觸發點。**」

### Exactly-once 怎麼做(承上)

> 「Exactly-once delivery 在分散式系統做不到,能做的是 exactly-once effect,而且唯一的路是 at-least-once + 冪等。NATS dedup window 只是 publish 端、window 內防重複 publish,管不到 consumer redelivery 跟故意 replay。我的 exactly-once 三塊湊:source offset 進 checkpoint、state 更新用 event_id 冪等、sink 用 2PC 或冪等下游。這個設計跟 NATS/Kafka 選型其實正交——換 Kafka 也是這套。」

---

## 八、跟 plan 其他章節的對應

| 比較維度 | 對應 plan / gaps 章節 |
|---------|---------------------|
| 為何 pull / 背壓 | plan §Backpressure |
| Offset 自己管 | plan §End-to-End Exactly-Once「Source 側」 |
| 冪等性 | plan §冪等性保證 + gaps #19/#22 |
| Watermark / 遲到 | plan §時間語意與 Watermark + gaps #27 |
| Snapshot 復原 | plan §Checkpoint + §Restart 流程 |
| Schema 變更 / 長期 replay | plan §Schema Evolution(retention 設定,**本文檔補了長期歸檔策略**) |
| KV 用法 | plan §架構總覽 圖 1(KV Bucket: checkpoint_meta) |
| Flink 借鏡哪些 | plan §為什麼不直接用 Flink |

---

## 九、待補的 plan 缺口(這份文件揭露的)

- **#28** Retention 上限與長期歸檔策略(本文 §3.5 揭露,gaps.md 待開)
  - 明確 NATS 是「操作熱窗」非「永久存儲」
  - 設計獨立 archive 管線(NATS consumer → S3)
  - 設定 stream `MaxAge` 限制
  - 標明「該換 Kafka」的觸發條件

---

**最後一句**:這份比較**完整誠實**——不假裝 NATS 全面更好,也不偽裝 Kafka 是錯的選擇。**對這個專案的 profile,NATS 是對的;對通用 production streaming,Kafka 是穩的。** 把這個取捨講清楚比硬挺一邊強得多。
