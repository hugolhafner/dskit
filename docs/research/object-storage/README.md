# Object-Storage-Backed Data Systems: Research Index and Comparison

This directory holds six research notes. Each one covers how a production system stores, reads and changes data when object storage (S3, GCS, Azure Blob) is its data backend. This README links the notes and compares the systems side by side.

| System | Document | Evidence basis |
|---|---|---|
| Datadog Husky | [datadog-husky.md](datadog-husky.md) | Closed source. Datadog engineering blog series (2022–2025) and podcast. No code. |
| Quickwit | [quickwit.md](quickwit.md) | Code at `quickwit-oss/quickwit@5fd12206`, in-repo docs. Blog posts read only through search summaries. |
| Grafana Loki | [grafana-loki.md](grafana-loki.md) | Code and docs at `grafana/loki@f3de4f5a`. Blog posts read only through search summaries. |
| Grafana Tempo | [grafana-tempo.md](grafana-tempo.md) | Code and docs at `grafana/tempo@9f689b08` (3.2.0-dev). |
| WarpStream | [warpstream.md](warpstream.md) | Closed source. Docs and blog read only through search excerpts. The only code is the Helm chart (`warpstreamlabs/charts@0ac68e6b`). |
| ClickHouse | [clickhouse.md](clickhouse.md) | Code at `ClickHouse/ClickHouse@977ed4e7`, docs at `clickhouse-docs@89036285`. SharedMergeTree (Cloud) is closed source: docs, blogs and `shared_merge_tree_*` settings only. |

**Conventions in this README.**
- Every fact here comes from the six documents above. Nothing was added from outside them.
- **†** marks a claim that the source document labels as inferred (`[I]`, `[inference]`, `[Inferred]`) rather than taken from code, docs or a primary blog post.
- Two systems are closed source: Husky and WarpStream. Their notes contain more inference than the others, and none of their byte-level formats or object key layouts are published.

---

## 1. System summaries

**Datadog Husky.** Husky is Datadog's third-generation event store for logs, APM spans, RUM, network data and profiles. Datadog reports more than 100 trillion events and billions of queries a day. Writers, Compactors and Readers are separate stateless or near-stateless services. The only stateful pieces are FoundationDB (FDB), which holds the metadata, and the blob store, which holds the data. Writers buffer events per tenant table, sort them by a sort-schema row key, and PUT immutable columnar *fragments*, one tenant table and one time window per object. They then commit the fragments and the dedup event IDs to FDB in one transaction, which gives exactly-once ingest. Compactors stream-merge fragments, first size-tiered and then as a best-effort locality LSM, reading each input with one GET. They swap outputs in atomically in FDB. Queries prune through FDB metadata (time, zone maps, per-fragment pruning regexes), then through a result cache, a predicate cache and a RocksDB blob-range cache. Only about 0.4% of fragment queries reach blob storage.

**Quickwit.** Quickwit is a Rust distributed search engine for logs and traces, built on Tantivy. Each *split* is an immutable Tantivy index (up to 10M docs) bundled into one `{split_id}.split` object. The bundle's footer holds a file table plus a *hotcache*: the byte ranges Tantivy reads when it opens an index, recorded at build time. The catalog is PostgreSQL, which also stores the footer offsets for each split. A split is staged in PostgreSQL, uploaded, then published in one transaction together with the source checkpoint. Searchers open a split with one ranged GET and no HEAD. They prefetch every byte range a query needs concurrently, then run Tantivy synchronously from RAM. Rendezvous hashing keeps each split's cache on the same searcher. Merges, deletes and garbage collection are all driven by the metastore. GC never LISTs the bucket.

**Grafana Loki.** Loki is a multi-tenant log store that indexes only labels. The note covers two storage generations. The *classic* store is GA and the default. Ingesters (RF=3 with a local WAL) PUT a content-addressed chunk of about 1.5 MiB per stream, and ship TSDB index files to the bucket every 15 minutes. Those index files are the catalog: they are discovered by a cached LIST, downloaded and mmapped by index gateways, and chunks are fetched with whole-object GETs behind memcached. The *data object* path is experimental and off by default. It consumes from Kafka, writes multi-tenant columnar DOBJ objects of about 512 MB with metadata at the head and content-addressed SHA-224 keys, and builds index objects alongside them. It commits by conditionally rewriting a per-tenant, per-12h-window Table of Contents (ToC) object in the bucket. Queries read with coalesced, parallel range GETs and need no LIST.

**Grafana Tempo.** Tempo is a distributed-tracing backend that stores data only in object storage. It has no metadata database. In Tempo 3.x, Kafka is the WAL. Block-builders consume partitions, build a Parquet block on local disk, and copy it to `<tenant>/<blockID>/`: `data.parquet`, sharded ~100 KiB bloom filters, a row-group index and `nocompact.flg`. `meta.json` goes last and is the commit marker. Block IDs are deterministic UUIDv5 values derived from Kafka offsets, so retries are idempotent. The catalog is a per-tenant *tenant index* object in the bucket. Two builders per tenant rebuild it every 5 minutes by LIST, and every other component reads it with one GET. Queriers issue hedged range GETs into Parquet, and the Parquet footer, column and offset indexes, pages and blooms are cached in Memcached or Redis. A singleton scheduler assigns compaction to workers. Workers retire input blocks by COPYing `meta.json` to `meta.compacted.json` and then deleting `meta.json`, and the objects are deleted after a 1-hour grace period.

**WarpStream.** WarpStream is a Kafka-protocol-compatible streaming system (BYOC, acquired by Confluent). Agents are stateless and diskless. Each Agent buffers produce requests from all partitions and writes **one object per Agent per flush** (default 250 ms), sorted by topic then partition. After the PUT succeeds, the Agent commits the file to a hosted control-plane metadata store (backed by DynamoDB, Spanner or Cosmos DB). The commit assigns offsets. The Agent acks the producer only after both the PUT and the commit succeed. Reads ask the control plane for file pointers, then go through a per-AZ, RAM-only cache. The cache uses a consistent-hash ring keyed by file ID and fetches aligned 4 MiB blocks, deduplicated across the zone. Leveled compaction (L0→L1→L2, fan-in of 32) restores per-partition locality. GC uses a delayed deletion queue in the metadata store, with a rare LIST-based reconciliation sweep as a backstop.

**ClickHouse.** ClickHouse is a columnar OLAP database whose MergeTree engine writes immutable sorted *parts* and merges them in the background. On the open-source S3 disk, every logical file of a part becomes one or more randomly keyed blobs. The file-to-blob mapping lives in a separate metadata layer: local files by default, or `plain`, `plain_rewritable` (in the bucket), `web`, or Keeper in ClickHouse Cloud. A part is committed by renaming its temporary directory, which is a metadata operation, and no data is copied. Marks turn granules into exact byte ranges, so reads are column- and granule-exact range GETs behind a local NVMe filesystem cache with 4 MiB-aligned segments, and in Cloud a per-AZ distributed cache. ClickHouse Cloud's SharedMergeTree moves all part metadata into Keeper, which makes compute stateless. Recent releases focus on reducing object counts: compact parts, packed skip indexes, packed parts, and inlining small files.

---

## 2. Comparison tables

### 2.1 Data format and granularity of objects

| System | Unit of storage | Format | Typical object size | Tenancy per object |
|---|---|---|---|---|
| Husky | Fragment: one table, one time window. Text-search *segments* are "additional files attached to fragments" (whether these are separate objects is †). | Custom columnar, "similar to Parquet". Whole columns are concatenated, each with an inline column header. Row groups, per-column min/max, a skip list of column offsets. | Writer output about 1k rows ("a few thousand events" at most). Compacted output about 1M or more rows. No byte targets published. Hundreds of MB to GB is †. | Single tenant ("never mix tables in one file"). |
| Quickwit | Split: one immutable Tantivy index in one `.split` object. | Tantivy files concatenated, then a footer (file-table JSON + hotcache) and an optional 16-byte `QWFT` trailer. | Up to 10M docs per split. Mature splits are typically 1–10 GB (vendor cost doc). | Per index. Tenants can be separated inside an index with `partition_key`. |
| Loki (classic) | Chunk: one stream, one time span. TSDB index file: one per ingester per period, compacted to one per tenant per day. | Row-oriented chunk of independently compressed 256 KiB blocks plus a block-meta table. Prometheus-derived TSDB index. | Chunks ≤ ~1.5 MiB compressed. TSDB files KB–GB. | Chunks per tenant. Ingester index files are multi-tenant until compaction. |
| Loki (data objects) | DOBJ data object; index object; ToC object. | Columnar container. **Metadata in a header**, with all section metadata packed together. Pages carry min/max stats. | Data objects target 512 MB. ToC objects target 32 MB. | Multi-tenant at ingest. Compaction outputs are per tenant. |
| Tempo | Block = a key prefix holding `data.parquet`, `bloom-N`, `index`, `meta.json`, `nocompact.flg`. | Apache Parquet (vParquet5): one row per trace, sorted by trace ID, with dedicated attribute columns. | Level-0 blocks are a few MB to tens of MB (`max-block-bytes` 20 MiB). Compacted blocks up to 100 GB. Row groups about 250 MB (800 MiB hard cap). Bloom shards ≤ 100 KiB. | Single tenant (key prefix). |
| WarpStream | File: one Agent, one flush window, all partitions that Agent received in that window. | Kafka record batches sorted by topic then partition. lz4 or zstd (zstd is the default since v671). The internal index or footer is not published (†). | L0 bounded by 250 ms, `batchMaxSizeBytes` (64 MiB) and `batchMaxCompressedSizeBytes` (1 MiB). In practice hundreds of KiB to a few MiB (†). Compaction caps inputs at 128 GiB uncompressed. | Multi-partition, multi-topic within one Virtual Cluster. |
| ClickHouse | One blob per logical file of a part (wide part: one per column stream plus marks). Compact or packed parts use fewer files. | MergeTree: compressed column blocks (64 KiB–1 MiB), marks, sparse primary index, skip indexes. | Bytes to many GB. Fresh or metadata files are tiny. The long tail of tiny objects is called the main inefficiency (†). | Per table. |

### 2.2 Write path

| System | Durable buffer before object store | Batching / object-size control | Who writes objects | Upload shape |
|---|---|---|---|---|
| Husky | Kafka. Writers are reset to the last committed fragment on conflict, so Kafka is the durable buffer (†). | Per-tenant in-memory buffers flushed on a time cap (a size cap is †). Shard routing gives each Writer few tenants, which means fewer, larger objects. | Stateless Writers (L0), Compactors (merged). | Single PUT for small Writer fragments, streaming multipart for compactor outputs (both †). |
| Quickwit | Ingest V2: local `mrecordlog` WAL, **not fsynced**, replication "not working yet". Pull sources (Kafka etc.) use the source's offsets as the WAL. | Split cut on 10M docs, 60 s `commit_timeout_secs`, or a memory limit. | Indexers. Merges by the merge pipeline, moving to a separate compactor service. | `MultiPartPolicy`: single PUT below 128 MiB, otherwise **5 GiB parts** to minimise PUT count. 12 concurrent split uploads. Streamed from local files without a temp bundle. |
| Loki (classic) | Ingester local WAL plus RF=3 replication. | Chunk cut at 1.5 MiB, 30m idle, 2h max age, or 1h sync period. TSDB head rotated every 15m, uploaded every 1m. | Ingesters (chunks, index), Compactor (index). | One single-part PUT per chunk. Synchronised cuts across replicas give identical content-addressed keys, so the chunk cache lets replicas skip duplicate PUTs. |
| Loki (data objects) | Kafka. The offset is committed only after upload, index and ToC write. | 128 MB section buffers sorted into stripes. Flush at 512 MB, 1h idle, or 1h builder age. | dataobj-builder (one per partition), compaction workers. | One `bucket.Upload`. minio-go switches to multipart for large bodies. Up to 20 retries, safe because keys are content-derived. |
| Tempo | Kafka ("Kafka provides immediate durability"). The local scratch disk is rebuildable. | 5-min consume cycle or 5 GB per cycle. One level-0 block per tenant per partition per cycle. | Block-builders only (microservices mode). Live-stores in monolithic mode. Backend-workers for compaction. | Streaming append: S3 multipart with **one part per row group** by default. GCS 10 MiB resumable chunks, Azure 3 MiB blocks. When copying a locally built block, minio-go handles multipart (†). Small files as single PUTs, `meta.json` last. |
| WarpStream | None. The Agent buffers in memory and acks only after PUT + commit. | One file per Agent per 250 ms (configurable down to 50 or 25 ms) or size bound. | Agents (`proxy` role for ingest, `jobs` role for compaction). | Single PUT for ingestion files (†). Multipart for large compaction outputs is likely but undocumented (†). GCS Rapid uses pre-opened appendable-object writers. S3 Express uses a quorum of three single-zone buckets (2-of-3 ack is †). |
| ClickHouse | None in the object store. Async inserts (on by default since 26.2) buffer in server memory and flush at 10 MiB, 450 queries or a timeout. | Batching via async inserts. `parts_to_delay_insert` 1000 / `parts_to_throw_insert` 3000 back-pressure. | Every server (OSS). Any replica in SharedMergeTree. | Single PUT ≤ 32 MiB. Otherwise multipart with 16 MiB parts, doubling every 500 parts, up to 20 in flight per file, with an idempotency id on Complete. |

### 2.3 Metadata / catalog location

| System | Where the catalog lives | What it holds | In bucket or external? |
|---|---|---|---|
| Husky | FoundationDB via the Metadata Service. Multiple FDB clusters; the sharding key is †. | Fragment path, version, row count, time bounds, zone maps, pruning regexes, ID-table versions. | **External DB.** |
| Quickwit | PostgreSQL (production). A file-backed `metastore.json` on object storage is single-writer only and recommended for tests. | Split state (Staged/Published/MarkedForDeletion), time range, tags, footer offsets, checkpoints, shards, delete tasks. | **External DB.** `split_recovery_metadata` and the `QWFT` trailer look like groundwork for rebuilding the catalog from the bucket (†). |
| Loki (classic) | TSDB index files in the bucket. Index gateways download them to disk. | Series → chunk refs with time, KB and entry counts. | **In bucket**, discovered by LIST. |
| Loki (data objects) | Per-tenant, per-12h-window ToC objects → index objects → data-object sections. | Index pointers with time bounds; postings, blooms, stats. | **In bucket**, keys computed from tenant and window. |
| Tempo | Tenant index (`index.pb.zst` / `index.json.gz`) per tenant, plus `meta.json` per block. Scheduler state in `work.json`. | Full snapshot of all block metas and compacted metas. | **In bucket**, rebuilt by polling. |
| WarpStream | Hosted control plane, one replicated state machine per Virtual Cluster, journaled to a strongly consistent log. | File → offset-range index, offsets, consumer groups, job state, deletion queue. Per-file byte ranges are †. | **External** (WarpStream's cloud). Tableflow Iceberg metadata is the only bucket-resident exception. |
| ClickHouse | OSS: local metadata files on the server disk (default), or `plain_rewritable` under `__meta/` in the bucket. Cloud: Keeper. | File → blob key list with sizes, ref counts, part sets. | **Local disk** (OSS default), **in bucket** (`plain_rewritable`), or **external Raft store** (Keeper, Cloud). |

### 2.4 Read path

| System | Pruning before object I/O | Object open / index access | Range reads | Caching tiers |
|---|---|---|---|---|
| Husky | Time partitioning, FDB zone maps and pruning regexes (about 30% of fragments pruned on average, up to 60%). | Column metadata, then segment posting lists. Embedded skip list of column offsets. | Column- and row-group-selective. The exact range shape is †. | Result cache (~80% hit), predicate cache, RocksDB blob range cache on local disk (~70% hit). About 0.4% of fragment queries touch blob storage. |
| Quickwit | PostgreSQL time-range and tag pruning (zero object requests). Term-absence short-circuit. | **One ranged GET** of footer + hotcache, using offsets from the metastore. | Many concurrent small range GETs planned in an async warmup phase. No generic range coalescing found (†). | Footer cache 500 MB, fast-field cache 1 GB, partial-result cache 64 MB, predicate cache 256 MB, optional whole-split disk cache. |
| Loki (classic) | Label postings in TSDB, time, fingerprint-range shards sized to 300–600 MB. | TSDB downloaded whole and mmapped on index gateways. | **None.** Chunks are whole-object GETs (up to 150 parallel per querier). | Results cache, L1/L2 chunk cache (memcached), index-stats caches, local TSDB disk cache. |
| Loki (data objects) | ToC time windows, then index-object postings and blooms, then stream and shard-bucket filters, then page stats. | Head prefetch of 256 KiB (data) or 64 KiB (index) usually covers all metadata. HEAD only lazily. | Ranges are coalesced when gaps ≤ 1 MiB, split above 8 MiB (pieces ≥ 1 MiB), 10 in parallel. Two-phase reads: predicate columns first. | Metadata cache (≤ 8 MiB items, keyed by immutable path). |
| Tempo | Block time range from the tenant index, then trace-ID bloom (one ~100 KiB GET per block), then footer row-group stats, column index, dictionary, page. | Footer read with no HEAD; `meta.size` and `footerSize` travel with each job. Offset indexes loaded in one GET for trace-by-ID. | Range GETs only, ~2 MiB (search) or 512 KiB (trace-by-ID), async prefetch. **Hedged GETs** (off by default). | Memcached/Redis roles: bloom, footer, column index, offset index, page, frontend search. Bloom write-through for long-lived blocks. |
| WarpStream | Control plane returns the files covering the fetch offset. | Not documented. Coarse range reads followed by in-memory parsing are †. | **Aligned 4 MiB range GETs**, one per block per AZ, deduplicated across Agents. One block often serves several partitions. | Per-AZ, RAM-only distributed cache on a consistent-hash ring by file ID. No disk cache. |
| ClickHouse | Partition min/max, sparse primary index, skip indexes, query condition cache, all in memory. | Marks cache (5 GiB) turns granules into byte ranges. | GETs bounded by marks. Stream reuse for gaps < 1 MiB. Async prefetch (up to 200 / 1 GiB). Minimum task 2 MiB. No hedged GETs. | Mark, PK and skip-index caches; local NVMe filesystem cache (4 MiB-aligned, 32 MiB segments, SLRU, write-through); optional page cache; per-AZ distributed cache in Cloud. |

### 2.5 Compaction / merge strategy

| System | Strategy | Notable mechanics | Commit of merge |
|---|---|---|---|
| Husky | Size-tiered (~1k → ~1M rows), then best-effort locality LSM with levels whose row limits grow exponentially. **Lazy**: waits for "a few dozen" fragments. | **One GET per input**, streamed; two-pass k-way merge (sort columns, then column by column); fixed memory; adaptive row-group size with restart. Locality compaction cut the Reader pool by 30%. | One FDB transaction removes inputs and inserts outputs. |
| Quickwit | `stable_log` (merge factor 10, max 12, ~3× per level, 48h maturation). Merges only within a `MergePartition` (partition ID + doc-mapping UID). | Immature splits are kept in a local cache, so early merges skip GETs. A cache miss means one full streaming GET. Single-threaded Tantivy merge. | Same `publish_splits` transaction: new split → Published, inputs → MarkedForDeletion. |
| Loki (classic) | Index-only compaction every 10m: per table (day), per tenant. Chunks are never compacted. | LIST, download, merge, dedupe RF=3 chunk refs, upload, delete sources. Singleton "main" compactor. | Upload new, then delete old. Not atomic; safe because queries dedupe chunk refs (†). |
| Loki (data objects) | Size-leveled K-way merge: L0 < 16 GiB, ratio 8, K = 8. Opt-in. | Outputs are content-addressed and per tenant. Keeps object boundaries intact where possible. | `ReplaceIndexPointers`: one conditional ToC write. Fails if any old path is gone. GC of replaced objects was not found in the code (†). |
| Tempo | Time-window block selector (1h windows), smallest first in the active 24h window. 2–4 inputs per job; groups must share a dedicated-column hash. | Singleton scheduler plus stateless workers. K-way merge on trace ID with span dedup. Streams output row group by row group. | Outputs written fully, then each input retired by COPY `meta.json` → `meta.compacted.json` + DELETE. Readers may briefly see both, never neither (†). |
| WarpStream | Leveled: 32 L0 → L1, 32 L1 → L2. Inputs capped at 128 GiB uncompressed. Key dedup for compacted topics at L1+. | Runs as jobs on `jobs`-role Agents. Can read from an S3 Express ingestion bucket and write to an S3 Standard compaction bucket. | Atomic metadata swap of input references for outputs. The atomic swap itself is partly † (implied by the GC post). |
| ClickHouse | Background merge selector (SIMPLE by default) within a partition; cap 150 GiB. | Full read + full rewrite. Merges do not populate the cache by default. Zero-copy: Keeper lock so one replica merges. Cloud: merge coordinator. | Metadata: new part added, sources become outdated and are removed after 480 s. |

### 2.6 LIST / HEAD / GET / PUT patterns and request-cost mitigations

| System | LIST | HEAD | GET | PUT / DELETE | Request-cost mitigations |
|---|---|---|---|---|---|
| Husky | Hot-path avoidance and LIST only for GC/audit are both †. | Unnecessary because metadata holds sizes (†). | One whole GET per compaction input; partial range GETs for queries; singleflight per node; cache bypass under disk saturation. | Object per flush and per merge output. Retention by bucket deletion is †. | Minimise object count (per-object cost and bucket rate limits); lazy compaction ("an order of magnitude" fewer API calls); 0.4% of queries reach blob storage. High-entropy key prefixes are †. |
| Quickwit | Not on any hot path. `Storage::list` exists only for the connectivity probe and tests. | Only in the file-backed metastore. | One GET per split open; per-query GETs ≈ splits × (fields × terms × 3 + …) + docs returned. `DebouncedStorage` coalesces identical in-flight GETs. | 1 PUT (< 128 MiB) or a few multipart requests. `DeleteObjects` 1,000 keys per request, GC batches of 10,000, optional rate limit. | 5 GiB part size; footer offsets in catalog; `QW_RANDOM_SPLIT_PREFIX`; per-process request semaphore (10,000); pruning in PostgreSQL. |
| Loki (classic) | **Central**: tables and index files are LISTed. Cached for 1 min with singleflight. | Not on the hot path. | Whole-chunk GETs, 150 parallel, batches of 50, behind L1/L2 cache. | One PUT per chunk (very high PUT counts at scale); a DELETE per chunk on retention (150 workers). | Chunk-cache write dedup; fingerprint key prefixes (per-prefix throttling motivation is †); caches. |
| Loki (data objects) | Not on the query path (keys computed). Used by the compactor to discover tenants and by the explorer UI. | Lazy, only when the size is needed. | Head prefetch, then coalesced ranges (1 MiB gap, 8 MiB max, 10 parallel). | Few large multipart PUTs (512 MB); about 300× fewer PUTs than 1.5 MB chunks (†). ToC via conditional PUT. | Big objects; computed keys; metadata cache; 2-hex-char SHA prefix (256 prefixes). |
| Tempo | Confined to ~2 tenant-index builders per tenant and `ClearBlock`. Parallelised by splitting the UUID space with `StartAfter`. No delimiter, so it enumerates every object (cost reasoning partly †). | Essentially none: sizes come from `meta.size`; existence checks are GETs. | Range GETs into Parquet; whole GETs of bloom shards and index; **hedged** GETs (off by default); every reader does one tenant-index GET per tenant per 5 min. | Per block: data parts + bloom shards + index + flag + meta. Retire uses server-side COPY + DELETE. | Tenant index replaces per-pod LIST; caches by semantic role; compaction reduces LIST cost; RF1 in 3.0 removed 2–2.5× duplication. |
| WarpStream | Only in the slow GC reconciliation sweep, deliberately kept rare. | Not mentioned. Unnecessary because metadata knows sizes (†). | 4 MiB aligned blocks, once per AZ: GET cost goes from O(consumers × partitions) to O(bytes / 4 MiB / AZ). | ~4 PUTs/s per Agent at 250 ms (†), whatever the partition count. Deletes are delayed, batched and throttled. | Multi-partition files; per-AZ cache dedup; S3 Express PUTs at ~1/5 the cost of regular S3 PUTs; claimed > 10× cheaper than inter-zone replication. |
| ClickHouse | Not used for queries. Used to load `plain_rewritable` at startup (sharded parallel LIST, or `snapshot.bin`), for `plain` disks, `s3()` globs. | Optional post-upload check, `plain` metadata, some exists checks. Not on the read hot path. | Mark-bounded range GETs; cache misses fetch 4 MiB-aligned segments; read retries resume at the current offset. | ~files-per-part PUTs per part; `DeleteObjects` ≤ 1,000 keys (single deletes on GCS); CopyObject for hard copies. Hard links and renames cost no S3 calls. | Compact parts (1 GiB wide threshold in Cloud); `skp_idx.packed`; packed parts; inline small files; random 3-letter prefix (motivation †); client-side `max_get_rps`/`max_put_rps`. |

### 2.7 Consistency and atomic commit mechanism

| System | What the object store must guarantee | Commit point | Atomic multi-object changes | Orphan handling |
|---|---|---|---|---|
| Husky | Only "store and don't lose these bytes". | One FDB transaction (fragments + dedup IDs). Strictly serializable. | FDB transaction swaps compaction inputs and outputs. | Uncommitted uploads are orphans; GC with a grace period is † (mechanism unpublished). |
| Quickwit | Atomic PUT of a new key and read-after-write for it. Never overwrites, never relies on LIST (partly †). | PostgreSQL `publish_splits` transaction, guarded by a publish token, carrying the checkpoint (exactly-once). | Same transaction publishes new splits and marks replaced ones. | Staged before upload, so orphans are found without LIST. Stale Staged rows (> 24h) are GC'd. 32-min deletion grace period. |
| Loki (classic) | Strong read-after-write and list-after-write. | Upload of a TSDB index file referencing the chunk ("chunk first, then index" is † from code). | None. Compactor swaps are upload-then-delete and rely on query-time dedup. | Lost-WAL chunks become unreferenced orphans (†). Retention deletes after a 2h delay. |
| Loki (data objects) | Conditional writes (`If-Match` / `If-None-Match`): S3 (GA late 2024), GCS generations or Azure ETags. | Conditional rewrite of the ToC object (GET + ETag, then CAS PUT, up to 10 retries). | Whole-ToC CAS swaps all pointers at once. minio-go drops `If-Match` on unknown-size multipart; Loki buffers the body to fix this. | Earlier uploads become unreferenced after a crash and replay (†). GC not found in code (†). |
| Tempo | Read-after-write for new objects; eventual LIST consistency is tolerated. | `meta.json` written last (single-object atomicity). | None. Retire is COPY + DELETE and **not atomic**; a crash leaves both metas and the poller treats the block as compacted (†). | Deterministic block IDs mean a retried build overwrites its own orphans. Compacted blocks deleted after 1h. The documented `replaces` field was not found in code. |
| WarpStream | Read-after-write for **new** keys only. No conditional PUTs. | Control-plane metadata commit, which assigns offsets; atomic across topics and partitions. | Metadata swap for compaction; deletion-queue enqueue in the same atomic operation as logical delete. | Uncommitted files are orphans cleaned by the reconciliation sweep (†). Idempotent-producer duplicates become "retroactive tombstones" (details partly †). |
| ClickHouse | Read-after-write for new random keys; no overwrites, no LIST dependency. | Metadata op: local rename, a single `prefix.path` PUT (`plain_rewritable`), or a Keeper multi-op ("commit point is the metadata operation" is †). | Keeper multi-op (Replicated/Shared). `plain_rewritable` uses tombstone markers for crash-safe multi-object removals. | Orphan blobs from crashed inserts need `tmp_*` cleanup or GC (†). Zero-copy reference locks in Keeper are EXPERIMENTAL and known to be fragile. |

### 2.8 Scaling model

| System | Ingest compute | Query compute | Maintenance compute | Stateful dependencies | Compute/storage separation |
|---|---|---|---|---|---|
| Husky | Stateless Writers, autoscaled on CPU; shard routing and autosharding by bytes. | "(Almost) stateless" Readers with large local caches; shuffle-sharded pools per product, tenant or origin. | Stateless Compactors. | FDB (several clusters), Kafka, blob store. | Full. Reader cost dominates. |
| Quickwit | Indexers "mostly stateless" (WAL shards, local split cache). | Stateless searchers; rendezvous hashing; optional Lambda offload. | Merge pipeline moving to a separately scaled compactor tier (†); janitor. | PostgreSQL (the main limit at millions of splits), object store. | Full for search; indexers hold a soft durability point. |
| Loki | Classic: stateful ingesters (RF=3 or one Kafka partition each). Dataobj: one builder per Kafka partition. | Stateless queriers; index gateways stateless apart from a disk cache. | Singleton compactor "main" (+ workers for deletes); dataobj compaction workers. | Memcached, ring KV, Kafka (dataobj), object store. No catalog DB. | Partial (classic ingesters are stateful and serve the last ~3h). |
| Tempo | Block-builders: one per Kafka partition, effectively stateless. Live-stores own partitions per zone. | Stateless queriers and frontends. | **Singleton** backend-scheduler + horizontal workers; tenant-index building sharded by ring. | Kafka, Memcached/Redis, object store. | Full for history. Limits: tenant-index size and block counts. |
| WarpStream | Stateless, diskless Agents; HPA on CPU; no partition ownership, so scaling moves no data. | Same Agents (role-split possible); per-AZ cache ownership changes on scale events. | `jobs`-role Agents. | Hosted control plane (hard availability dependency unless Ripcord is used). | Full. Metadata commits scale with Agents × flush rate (†). |
| ClickHouse | OSS: stateful servers owning local metadata; shards + replicas; adding a replica copies data. Cloud: stateless replicas. | Same servers; parallel replicas; Cloud warehouses share data. | Merges on servers; Cloud merge coordinator. | OSS: local metadata disk (losing it loses the table), Keeper for replication. Cloud: Keeper. | None in OSS default (S3 is often only a cold tier); full in Cloud, where Keeper becomes the hot spot. |

---

## 3. Recurring design patterns

1. **Immutable, write-once data objects.** All six systems write data objects once and never modify them in place. That lets them get by with weak object-store semantics, and it makes caching easy: Husky says unchanged metadata means unchanged data, and Loki keys its metadata cache by the immutable path. The exceptions are small mutable control objects: Loki's ToC, Tempo's tenant index and `work.json`, ClickHouse `plain_rewritable` `prefix.path`, and Quickwit's file-backed `metastore.json`. Tempo also overwrites a block's own objects when a block-builder retries with the same deterministic ID.

2. **Upload first, then a separate metadata commit makes data visible.** Every system writes the data object before it publishes anything, and visibility comes from a metadata step:
   - an FDB transaction (Husky);
   - a PostgreSQL transaction (Quickwit);
   - a control-plane commit (WarpStream);
   - a rename or Keeper op (ClickHouse);
   - a commit marker written last (Tempo `meta.json`);
   - a CAS'd manifest (Loki data objects) or index file (Loki classic).

   An upload that never reaches the commit becomes an orphan, and each system needs a way to find and remove orphans.

3. **Keep LIST off the hot path.** Every read path except Loki classic computes or looks up exact keys:
   - Husky (†), Quickwit, WarpStream and ClickHouse use a catalog;
   - Loki data objects compute ToC keys from (tenant, window);
   - Tempo confines LIST to two builders per tenant.

   Loki classic is the outlier: LIST is central to its index, and it caches the results for 1 minute with singleflight.

4. **Avoid HEAD by storing sizes and footer locations in metadata.** Quickwit keeps `footer_offsets`, Tempo keeps `meta.size` and `footerSize` (and sends them with each job), ClickHouse keeps blob sizes in its metadata, and Loki puts DOBJ metadata in a speculative head read. Husky and WarpStream are documented only as not needing HEAD (†).

5. **Treat object count as a first-class cost.** The systems control the number of objects in several ways:
   - batching across streams: WarpStream puts all partitions in one file; Loki data objects are multi-tenant;
   - per-tenant buffering and locality routing: Husky;
   - async inserts, compact or packed parts, and inlining small files: ClickHouse;
   - multipart sizing chosen to minimise PUTs: Quickwit uses 5 GiB parts.

   Tempo and Loki classic both name many small level-0 objects or chunks as a known weakness.

6. **LSM-style compaction that deliberately pays write amplification** to cut GETs, metadata rows and catalog size: Husky (tiered, then leveled), WarpStream (L0/L1/L2), Loki data objects (size-leveled), Quickwit (stable-log), Tempo (time-window), ClickHouse (MergeTree).

7. **Two-phase delete with a grace period** longer than the staleness of any reader's view:
   - Quickwit: 32 min;
   - Tempo: 1h;
   - Loki classic retention: 2h;
   - ClickHouse outdated parts: 480 s;
   - WarpStream: a delayed deletion queue, which also keeps metadata backups restorable;
   - Husky: a grace period is likely, but it is †.

8. **Cache affinity through hashing instead of data ownership.**
   - Husky: consistent hashing on fragment ID within a tenant's shuffle shard.
   - Quickwit: rendezvous hashing on split ID.
   - WarpStream: a per-AZ ring keyed by file ID.
   - ClickHouse: rendezvous hashing for `s3Cluster`, and a per-AZ distributed cache in Cloud.

   Tempo and Loki use shared Memcached or Redis tiers instead.

9. **Request deduplication and coalescing.**
   - Single-flight of identical in-flight reads: Husky, Quickwit (`DebouncedStorage`), WarpStream (per-AZ block dedup), Loki (LIST cache).
   - Coalescing of adjacent ranges: Loki data objects (explicit gap, max and min parameters), ClickHouse (stream reuse below `min_bytes_for_seek`).
   - Aligned fetch units: WarpStream 4 MiB, ClickHouse 4 MiB cache alignment.

10. **A log in front of the object store.** Kafka is the durable buffer for Husky (†), Tempo 3.x and Loki data objects. Quickwit pull sources use the source's offsets the same way. WarpStream *is* the log. In each case the offset or checkpoint is committed only after the object is published, or together with it (Quickwit, Husky).

11. **Spread the key space.** Quickwit has `QW_RANDOM_SPLIT_PREFIX`, ClickHouse uses a random 3-letter prefix, Loki uses SHA and fingerprint prefixes, and Tempo uses UUIDs. The motivation is stated in code for Quickwit only. For ClickHouse and Loki it is †, and Husky's and WarpStream's key layouts are unpublished.

---

## 4. Notable divergences

- **Where the catalog lives.** This is the biggest split.
  - An external transactional store: FDB (Husky), PostgreSQL (Quickwit), a hosted DynamoDB/Spanner/Cosmos-backed control plane (WarpStream), Keeper (ClickHouse Cloud).
  - The bucket itself: Tempo (polled tenant index), Loki (TSDB files, or CAS'd ToCs), ClickHouse `plain_rewritable`.
  - A node's local disk: ClickHouse OSS default, which ties data to that node.

  The external-store systems get multi-object atomicity and fast lookups, but take on a second distributed system. The in-bucket systems have no database, but accept polling lag (Tempo ~5–10 min, partly †), non-atomic swaps (Tempo, Loki classic), or a dependency on conditional writes (Loki data objects).
- **Conditional writes.** Only Loki's data-object path relies on S3/GCS/Azure conditional PUTs. Tempo uses real preconditions only on GCS, and only for overrides; its S3 path does read-compare-write with a known race. Quickwit's file-backed metastore and ClickHouse `plain_rewritable` avoid concurrency by being single-writer.
- **File format and where its metadata sits.**
  - Husky uses inline column headers so a compactor can stream a fragment with one GET.
  - Loki DOBJ puts metadata in a **header**, so one speculative head read opens the object.
  - Quickwit uses a **footer** whose offsets live in the catalog, plus an optional trailer.
  - Tempo uses stock Parquet with footer elision from metadata, and keeps bloom filters outside Parquet.
  - ClickHouse keeps marks and the primary index as separate small files.
- **Whole-object vs ranged GETs.** Loki classic chunks are always fetched whole. Every other read path uses range GETs, and WarpStream reads in fixed 4 MiB aligned blocks.
- **Tenancy of objects.**
  - Per tenant: Husky, Tempo and Loki classic chunks, for isolation and pruning.
  - Shared: WarpStream files and Loki ingest-time data objects, to amortise request cost.
  - Converted at compaction: Loki's data-object compactor turns multi-tenant inputs into per-tenant outputs; Loki's TSDB compactor does the same for index files.
- **Multipart policy.** Quickwit uses 5 GiB parts to minimise PUT count. ClickHouse uses 16 MiB parts, growing, 20 in flight, for throughput. Tempo uses one part per Parquet row group. Loki leaves part size to minio-go.
- **Deduplication.**
  - On write, before commit: Husky (FDB ID tables).
  - At commit, after upload: WarpStream (retroactive tombstones).
  - At query or compaction time: Tempo (span dedup), Loki classic (chunk-ref dedup).
  - Through content addressing: Loki (CRC, SHA-224).
- **Bucket lifecycle rules.** WarpStream rejects them for correctness. ClickHouse docs warn never to use them (opaque keys). Loki allows them only on `<tenant>/` chunk prefixes, with expiry longer than retention plus the delete delay.
- **Hedging.** Tempo and Loki offer true hedged GETs (off by default; Loki adds a per-second cap). Quickwit has cancel-and-reissue on slow requests (off by default). ClickHouse has no hedged object-store GETs. Husky's is unknown, and WarpStream's is undocumented (†).
- **Durability before upload.** Quickwit's WAL is not fsynced. Loki classic relies on WAL + RF=3. WarpStream and ClickHouse (with `wait_for_async_insert`) ack only after the object is durable. The Kafka-fronted systems ack on the Kafka write.

---

## 5. Key takeaways for designing a new object-storage-backed data system

1. **Choose the commit point first.** Write data to unique, never-reused keys, then make it visible with one metadata operation:
   - a transaction in a strongly consistent store (FDB, PostgreSQL, Spanner-class, Raft);
   - a CAS on a small manifest object;
   - a commit marker written last.

   This decision determines your consistency story, how orphans arise, and whether you need LIST. If you use object-store CAS, test it end to end (Loki's minio-go `If-Match` multipart pitfall).
2. **Commit offsets or checkpoints after the object is visible, or in the same transaction.** Quickwit and Husky do it in one transaction. Tempo and Loki commit Kafka offsets afterward. Derive object IDs from input positions (Tempo UUIDv5) or content (Loki SHA-224) so retries are idempotent.
3. **Budget object count and request count from day one.** Batch across logical streams or tenants per writer per tick, pack small files, and inline tiny ones in metadata. Size multipart deliberately: few parts for cost (Quickwit) or many in flight for throughput (ClickHouse). Then compact, accepting the write amplification.
4. **Keep LIST and HEAD off the read path.** Store keys, object sizes and footer or header offsets in the catalog, or compute keys from (tenant, time window). If you must LIST, confine it to a few owners (Tempo), shard it by key-space ranges, cache it with singleflight, or keep a snapshot (ClickHouse `snapshot.bin`).
5. **Design the format for two access patterns.** Compaction wants sequential, single-GET streaming with bounded memory (Husky). Queries want small, precise range reads from a compact index that opens with one GET: a hotcache footer (Quickwit), head metadata (Loki), marks (ClickHouse), or Parquet footer plus offset indexes (Tempo). Keep each logical read contiguous.
6. **Prune before any GET.** Put cheap, no-false-negative summaries (time range, tags, zone maps, regexes, blooms) in the catalog or in tiny side objects, so most objects are never opened. Husky reaches blob storage for about 0.4% of fragment queries.
7. **Treat caching as part of the storage engine.** Layer caches from coarse to fine (results, predicates, metadata/footers, byte ranges). Use aligned blocks, dedupe in-flight fetches, and route with consistent or rendezvous hashing for affinity. Prefer AZ-local tiers, write through on ingest, and keep compaction from polluting the cache.
8. **Make deletion a separate, delayed, metadata-driven subsystem.** Mark first, wait for a grace period longer than any reader's staleness, then delete in batches (≤ 1,000 keys per `DeleteObjects`) with throttling. Keep LIST-based reconciliation only as a backstop, and do not rely on bucket lifecycle rules for correctness.
9. **Expect the metadata layer to become the next bottleneck.** Several systems name it explicitly:
   - FDB transaction limits (Husky);
   - PostgreSQL `list_splits` at millions of splits (Quickwit);
   - tenant-index size and polling (Tempo);
   - ToC contention per tenant and window (Loki);
   - Keeper fan-out (ClickHouse Cloud);
   - commits per Virtual Cluster (WarpStream, †).

   Plan for compaction of metadata, batching and sharding.
10. **Know the latency floor and how to buy it down.** Object-store PUT and GET latency sets a floor. Husky's median rose by a few hundred ms over local disks while its tail improved. WarpStream's default p99 produce latency is about 400 ms. Options include zonal or express tiers (S3 Express, GCS Rapid), quorum writes across single-zone buckets, delayed sequencing (WarpStream Lightning Topics), and a separate hot tier for recent data (Tempo live-stores, Loki ingesters). Hedge only idempotent GETs.
