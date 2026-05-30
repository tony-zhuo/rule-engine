# In-Memory Rule Engine — 借鏡產品與參考資料

本文是 [in-memory-rule-engine-plan.md](./in-memory-rule-engine-plan.md) 的附錄，列出同規模 / 同領域可借鏡的真實產品，以及推薦研究的資料來源。

## 「百萬級用戶」是什麼樣的量級

```
維度                 典型範圍              對應到主計畫
────────────────    ──────────────        ──────────────────
註冊用戶總數         1M ~ 10M              §Risks「100 萬 member」
日活用戶 (DAU)       10萬 ~ 200萬          5~20% of 註冊
同時在線             1萬 ~ 50萬            1~5% of 註冊
峰值 events/s        5K ~ 100K             §Benchmark「100K events/s」
資料總量             GB ~ TB               §Memory「7 天 < 10 GB」
工程團隊規模         5~20 人
基礎設施             幾十台機器            單 cluster 撐得住
```

對照其他量級：

| 量級 | 註冊用戶 | 工程組織 | 代表 |
|------|---------|---------|------|
| **千級** | 1K~10K | 1~3 人 | 早期 SaaS / 工具 |
| **萬級** | 10K~100K | 3~10 人 | 區域型小服務 |
| **十萬級** | 100K~1M | 5~15 人 | 中型 B2C SaaS |
| **百萬級** ★ | 1M~10M | 10~50 人 | **台灣本土頭部、區域型銀行、Stripe 早期** |
| **千萬級** | 10M~100M | 50~200 人 | LINE TW、國泰世華、Wise、Revolut |
| **億級** | 100M~1B | 200+ 人 | Facebook、Netflix、Alipay、Stripe 現在 |
| **十億級** | 1B+ | 1000+ 人 | WeChat、Instagram、Visa |

百萬級的關鍵特徵：

- **單 region 撐得住**（不用一定要 multi-region 部署）
- **工具鏈靠 OSS + 少量自研**（不像億級要自己重寫 OS / DB）
- **架構複雜度的 sweet spot**（已經要分散式，但還沒到「每個 component 都要自研」）

---

## 同領域可借鏡的產品

### A. Fraud / Risk Engine（最對口，本計畫的本業）

| 產品 | 規模 | 為什麼值得借鏡 | 可找到的資料 |
|------|------|--------------|------------|
| **Stripe Radar** | 早期百萬級、現在十億級 | Rule + ML 混合、real-time scoring，架構演進路徑清楚 | Stripe Engineering Blog: "How Radar built ML..."、論文 |
| **Sift** | SaaS，聚合下游億級 | 純規則 + ML 引擎，公開技術 blog 多 | sift.com/engineering |
| **Feedzai** | 賣給銀行的 risk engine | 專做 sub-second fraud scoring，state machine + CEP | 學術論文、conference talk |
| **DataVisor** | 億級 | Unsupervised + rule，graph-based detection | 偏 ML，但 streaming 架構部分有參考 |
| **Coinbase Risk Engine** | 億級用戶 | **直接用 Flink CEP**，跟我們的 reference 一模一樣 | Coinbase Engineering Blog |
| **Uber Anti-Fraud** | 億級 | Stream-based real-time，公開分享多 | Uber Engineering Blog: "How Uber catches fraud..." |

**最推薦研究**：

- **Stripe Radar 早期架構**（2016-2018 那批文章）——剛好就是百萬級規模、rule-based + 開始加 ML，跟我們處境近
- **Coinbase 用 Flink 做 risk engine 的文章**——我們的 reference architecture 等於是手刻版的這個

### B. Real-time Streaming / Stateful Processing（架構面對口）

| 產品 / 技術 | 為什麼值得借鏡 |
|-----------|-------------|
| **Apache Flink CEP** | 本計畫的 reference architecture，看 docs 跟 Alibaba 的論文 |
| **LinkedIn Samza** | Stateful stream processing 的祖師爺，原始論文清楚說明設計動機 |
| **Apache Pinot** | LinkedIn 開源的 real-time analytics，百萬級規模典型用法 |
| **Materialize.io** | Streaming SQL，技術 blog 對 incremental computation 講得最白話 |
| **Netflix Mantis** | 內部 stream processing，operational use case，文章好讀 |

### C. 高吞吐量 Event-Sourced 系統（核心 pattern 對口）

| 產品 | 為什麼值得借鏡 |
|------|-------------|
| **LMAX Disruptor** | 交易撮合，single-thread 100K+ ops/s 的祖師爺；我們的 single-goroutine-per-shard 直接繼承這個思想 |
| **EventStore (Kurrent)** | Event sourcing 代表產品，snapshot + replay 設計值得看 |
| **Axon Framework** | Java event sourcing framework，CQRS 完整 reference |

LMAX Disruptor 的 2010 那篇 paper 必看——**100K+ events/s on single thread** 不是空話，是 2010 年就證明過的。

### D. 同規模的台灣 / 亞洲產品（context 對口）

| 產品 | 規模 | 公開資訊 |
|------|------|---------|
| **LINE Pay TW** | 百萬級用戶 | LINE Engineering Blog 有 risk 系統相關分享 |
| **街口 JKO** | 百萬級 | 較少公開技術 blog，但 conference 有出來講過 |
| **玉山銀行 E.SUN** | 數百萬會員 | 公開過 fraud detection 架構（採 Flink）|
| **蝦皮 Shopee** | 千萬級（台灣）/ 億級（東南亞） | Shopee Tech Blog 有 streaming 相關文章 |
| **Pinkoi** | 百萬級 | 偶有技術分享 |

---

## 如果只挑 3 個來研究

### 1. Apache Flink + Coinbase 用 Flink 做 risk 的文章

**為什麼**：本計畫的 reference architecture。讀 Flink 的核心設計（key group、barrier snapshot、watermark）+ Coinbase 的 production 經驗，等於把我們計畫的「為什麼這樣設計」跟「實際上線會遇到什麼」一次補齊。

**關鍵資料**：

- Flink 官方 docs §State / §Checkpointing / §Event Time
- Paolo Carbone et al., *"Apache Flink: Stream and Batch Processing in a Single Engine"* (2015)
- Coinbase Engineering Blog: "Building real-time risk decisioning..."

### 2. Stripe Radar 的早期架構文章

**為什麼**：fraud detection + rule engine + 從百萬級長到億級的演進歷史。可以看到「在我們現在這個規模該怎麼做、長大後該怎麼演進」的完整路線。

**關鍵資料**：

- Stripe Engineering: "A primer on machine learning for fraud..." (2017)
- Stripe Engineering: "Online migrations at scale"（NATS 升級可類比）
- Patrick McKenzie 寫的 Stripe Radar 商業 + 技術綜述

### 3. LMAX Disruptor Paper（2010）

**為什麼**：我們最關鍵的決定「single-goroutine per shard + 100K events/s in-memory」直接繼承這個思想。讀完會理解「為什麼自己刻可以打贏看似豪華的分散式方案」。

**關鍵資料**：

- Martin Thompson, *"Disruptor: High Performance Alternative to Bounded Queues"* (2011 paper)
- LMAX 那個 6 million TPS 的 talk（YouTube 搜 "LMAX Disruptor"）

---

## 本計畫設計對應到 reference 的出處

```
本計畫的某個設計          來自哪個 reference            可借鏡的具體做法
────────────────────     ────────────────             ──────────────────
single-goroutine/shard    LMAX Disruptor               ring buffer 結構、
                                                       mechanical sympathy

key group 抽象            Flink KeyGroupAssignment    rescale 演算法

async barrier snapshot    Flink + Chandy-Lamport       barrier alignment 細節

watermark + lateness      Flink event time             trigger 設計

NATS sourcing 不停機升級  Stripe online migrations    dual-write + cutover

result sink 2PC           Flink TwoPhaseCommitSink    Kafka transactional API

CEP pattern matching      Flink CEP / Esper           pattern 編譯器設計

fraud detection 規則組合  Stripe Radar / Sift          rule + ML hybrid 演進
```

---

## TL;DR

- **百萬級 = 1M~10M 註冊用戶、5K~100K events/s 峰值、單 region 撐得住**
- 對口產品：**Stripe Radar 早期、LINE Pay TW、Coinbase Risk Engine**（fraud/rule engine 領域）
- 架構對口：**Apache Flink、LMAX Disruptor、EventStore**
- 如果只挑 3 個讀：**Flink docs + Coinbase risk 文章 + LMAX Disruptor paper**
