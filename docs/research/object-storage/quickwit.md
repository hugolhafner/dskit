# Quickwit: storage architecture on object storage

> Research notes on how Quickwit stores, retrieves and changes data when an object store (S3, GCS or Azure Blob) is its data backend.
>
> **Code baseline:** `quickwit-oss/quickwit` at commit `5fd12206ee3113b54a656e4cd768785a804722f9` (HEAD of `main`, committed 2026-10-31). Paths below are relative to the repo root. Permalink prefix: `https://github.com/quickwit-oss/quickwit/blob/5fd12206ee3113b54a656e4cd768785a804722f9/`.
> Tantivy is pinned to the `quickwit-oss/tantivy` fork at rev `e1acb3e0112f2b4819946aa4ea5742bf429853d8` (`quickwit/Cargo.toml`).
>
> **Evidence labels:** **[code]** means checked in source at the commit above. **[docs]** means taken from the repo's `docs/` tree or official doc pages. **[blog]** means a vendor blog post; quickwit.io could not be fetched from this session, so these claims come from search-result summaries and are weaker. **[inference]** means my own reading or extrapolation.

---

## 1. Overview

Quickwit is a distributed search engine written in Rust. It targets append-mostly, time-series-shaped data such as logs, traces (it has a native OTLP endpoint and a Jaeger gRPC API) and, more generally, JSON documents. All indexed data lives on object storage. Compute nodes are stateless or nearly stateless, so the bill is mostly cheap object storage plus compute that scales independently. It provides:

- full-text search through an inverted index, built on **Tantivy**, a Lucene-like library also maintained by the Quickwit team;
- columnar "fast fields" for aggregations, sorting and range filters;
- a row-oriented doc store for fetching hits;
- an Elasticsearch-compatible query and ingest API subset.

**Who uses it.** Binance moved its log platform from about 20 Elasticsearch clusters to Quickwit. The vendor blog reports the platform grew to about 100 PB of logs, with per-index throughput ranging from several GB/s down to a few MB/s [blog: "How Binance built a 100PB log service with Quickwit"]. Datadog acquired Quickwit in January 2025 and announced a relicense to Apache 2.0 [blog: datadoghq.com "Datadog acquires Quickwit"]. At the baseline commit, file headers read `Copyright 2021-Present Datadog, Inc.` under Apache-2.0 **[code]**.

**Workload assumptions:**

- writes are append-only;
- deletes are rare (the docs describe them as "mainly intended to comply with GDPR … a few queries per hour or day");
- queries are usually time-bounded;
- the system tolerates search latency in the hundreds of milliseconds to seconds, in exchange for storing everything in S3 [docs: `docs/overview/concepts/deletes.md`].

---

## 2. Architecture

Every node runs the same `quickwit` binary. Services are enabled per node:

- `indexer`
- `searcher`
- `metastore`
- `control_plane`
- `janitor`
- newer roles: `compactor` (crate `quickwit-compaction`) and `metastore_read_replica`

Cluster membership uses **chitchat**, a scuttlebutt-style gossip protocol with phi-accrual failure detection [docs: `docs/overview/architecture.md`].

| Component | State | Role |
|---|---|---|
| **Object storage** (S3 / GCS / Azure / local FS) | durable data | One immutable `.split` object per split. This is the only copy of indexed data. |
| **Metastore** (PostgreSQL, or a JSON file on object storage) | durable metadata | Indexes, sources, split catalog (state, time range, tags, footer offsets), source checkpoints, shards, delete tasks. |
| **Ingester** (part of the indexer node) | local WAL (`mrecordlog`) | Buffers ingested docs in *shards* until they are indexed (ingest V2). |
| **Indexer** | ephemeral, plus a local split cache | Runs pipelines that turn docs into splits, upload them, and publish them. |
| **Merge pipeline / Compactor** | ephemeral, plus a local split cache | Merges immature splits into mature ones and applies deletes. |
| **Searcher** | stateless, with RAM and optional disk caches | Root/leaf map-reduce over splits; split assignment uses rendezvous hashing. |
| **Control plane** | soft state | Schedules indexing pipelines and ingest shards onto nodes. |
| **Janitor** | none | Garbage collection, retention, delete-task planning. |
| **AWS Lambda leaf** (optional) | stateless | Offloaded leaf searches during load spikes. |

```mermaid
flowchart LR
  subgraph Ingest
    C[Client / OTLP / ES bulk] --> R[Router]
    R --> I1[Ingester shard WAL<br/>mrecordlog]
    K[Kafka/Kinesis/Pulsar/SQS] -.->|direct source| P
  end
  I1 --> P[Indexing pipeline<br/>DocProcessor→Indexer→Serializer→Packager→Uploader→Sequencer→Publisher]
  P -- "1. stage_splits" --> M[(Metastore<br/>PostgreSQL)]
  P -- "2. PUT {split_id}.split" --> S3[(Object storage<br/>s3://bucket/indexes/{index_id}/)]
  P -- "3. publish_splits + checkpoint (txn)" --> M
  P -->|immature split dir| LC[Local split cache]
  LC --> MG[Merge pipeline / Compactor]
  S3 -->|GET whole split on cache miss| MG
  MG -->|PUT merged split + publish replace| S3
  MG --> M
  subgraph Search
    Q[Query] --> Root[Root searcher]
    Root -->|list_splits time/tag pruning| M
    Root -->|rendezvous-hash jobs| L1[Leaf searcher]
    Root --> L2[Leaf searcher]
    Root -.->|overflow| LAM[Lambda leaf]
  end
  L1 -->|range GETs: footer, term dict blocks, postings, fast fields, docstore| S3
  L2 --> S3
  J[Janitor GC/retention] --> M
  J -->|DeleteObjects| S3
```

---

## 3. Data model and on-object format

### 3.1 Index → splits

An index is a set of **splits**. Each split is an independent, immutable Tantivy index of up to `split_num_docs_target` docs (default **10,000,000**; `IndexingSettings::default_split_num_docs_target`, `quickwit/quickwit-config/src/index_config/mod.rs`) **[code]**. Each split is stored as a single object named `{split_id}.split` (`quickwit_common::split_file`, `quickwit/quickwit-common/src/lib.rs`) **[code]**.

**Split IDs** are ULIDs (26 Crockford base32 chars). With `QW_RANDOM_SPLIT_PREFIX=true`, the last 4 (purely random) ULID chars are moved to the front as `XXXX_<22 chars>`. This "spreads S3 key prefixes across partitions and avoids hot-spots when writes concentrate on the same time window" (`quickwit/quickwit-proto/src/types/split_id.rs`) **[code]**. The code requires split IDs to be treated as opaque, so nothing may parse a timestamp out of them.

**Object key layout [code]:**

```
<index_uri>/<split_id>.split
  index_uri default = <default_index_root_uri>/<index_id>
  default_index_root_uri default = <data_dir>/indexes      (quickwit-config/src/node_config/serialize.rs)
e.g. s3://my-bucket/indexes/otel-logs-v0_7/01HAV29D4XY3D462FS3D8K5Q2H.split
     s3://my-bucket/indexes/otel-logs-v0_7/Q2H5_01HAV29D4XY3D462FS3D8K.split   (random prefix on)
```

The layout is flat, with one prefix per index. There are no per-day directories, no manifests and no small metadata objects next to the splits. The only exception is the file-backed metastore (§7.6).

### 3.2 The `.split` bundle

A split is built locally as a Tantivy directory and then concatenated into one bundle. The format is documented in `docs/internals/split-format.md` and implemented by `SplitPayloadBuilder::finalize_with_footer_trailer` in `quickwit/quickwit-storage/src/split.rs` **[code]**:

```
+-------------------------------------------------------------+
| tantivy files, concatenated (no padding):                   |
|   <seg>.idx (postings)  <seg>.pos (positions)               |
|   <seg>.term (term dictionary)  <seg>.store (doc store)     |
|   <seg>.fast (columnar fast fields)  <seg>.fieldnorm        |
|   meta.json, ...                                            |
|   split_fields             (Quickwit: field list/types/caps)|
|   split_recovery_metadata  (versioned protobuf, optional)   |
+-------------------------------------------------------------+  <- footer_start
| BundleFileRanges JSON  { files: {name: [start,end)} }       |
| u32 LE  len(BundleFileRanges JSON)                          |
| hotcache bytes                                              |
| u32 LE  len(hotcache)                                       |
| [optional trailer: u64 LE footer_start | u32 LE version=1 | |
|  b"QWFT"]  (16 bytes)                                       |
+-------------------------------------------------------------+  <- footer_end = object size
```

Notes on the format:

- **Footer = file table + hotcache.** They are placed next to each other on purpose ("the hotcache needs to be next to the bundle metadata in order to read both in one continuous read"), so opening a split costs **one ranged GET** **[code]**.
- **Footer offsets are stored in the metastore.** `SplitMetadata.footer_offsets: Range<u64>` (`quickwit/quickwit-metastore/src/split_metadata.rs`) lets searchers fetch the footer directly, with no HEAD request and no "read the last N bytes, then read again" step **[code]**.
- **Footer trailer (`QWFT`, version 1) is the newest change.** It lets a reader with no metastore entry find the footer from `object size − 16 bytes`. Without the trailer, a legacy reader must walk backward: read the last 4 bytes (hotcache length), then the 4 bytes before the hotcache (bundle-metadata length). `BundleStorage::fetch_split_tail` reads an initial tail window of `DEFAULT_SPLIT_TAIL_WINDOW_NUM_BYTES = 1 MiB`. It needs one extra read on a miss for new splits and up to two for legacy splits (`quickwit/quickwit-storage/src/bundle_storage.rs`) **[code]**.
- **The trailer is off by default for writers.** It is gated by `QW_ENABLE_SPLIT_FOOTER_TRAILER` because older readers would misread the last 4 bytes. The docs prescribe a two-phase rollout: upgrade all readers first, then enable writers [docs: `split-format.md`].
- **`split_recovery_metadata` holds enough to rebuild the split's metastore row.** Together with the trailer and `Storage::list` (which returns object sizes), this points toward rebuilding the metastore from the bucket alone (`BundleStorage::fetch_file_from_split` takes "the split length … from object listing metadata to avoid a separate metadata request") **[code]**. **[inference]** This looks like recent disaster-recovery work: the catalog becomes reconstructible from object storage, so PostgreSQL is no longer a single point of data loss.

### 3.3 Hotcache

`quickwit_directories::write_hotcache` (`quickwit/quickwit-directories/src/hot_directory.rs`) builds the hotcache when the split is packaged **[code]**:

1. It opens the freshly built index through a `DebugProxyDirectory` that records every read.
2. It opens the inverted index of every indexed field in every segment, which is exactly the set of reads Tantivy needs to open a split.
3. It stores those byte ranges in a `StaticDirectoryCache`. Slices from files ending in `store` or `term` (the doc store index and the term-dictionary index) are always kept; other slices are kept only if `< 10,000,000` bytes. File lengths for every file are also recorded.

At read time, `HotDirectory::open(dir, hotcache_bytes)` serves those reads from RAM and sends everything else to storage. The docs claim that "thanks to the hotcache, opening a split on Amazon S3 only takes 60ms" [docs: `architecture.md`]. The Quickwit-101 blog puts the hotcache at under 0.1% of split size [blog].

### 3.4 Inside the Tantivy files

These are standard Tantivy structures, compiled with the fork's `quickwit` feature (plus `zstd-compression`, `lz4-compression`, `columnar-zstd-compression`, `jitexpr`) **[code: Cargo.toml]**:

- **Term dictionary (`.term`).** **[inference / prior knowledge]** With the `quickwit` feature this is an SSTable-based dictionary with a small block index. The index goes into the hotcache, and one term lookup costs a single ranged GET for one dictionary block. This fits the `ends_with("term")` special case above.
- **Postings (`.idx`) and positions (`.pos`).** Block-compressed (bitpacked blocks of 128 docs). One term's postings form a contiguous range, so a whole posting list is one ranged GET.
- **Fast fields (`.fast`).** Tantivy's columnar format: one column per field, with optional zstd for columns. Column ranges are contiguous and fetched whole or by range.
- **Doc store (`.store`).** Row-oriented, compressed in blocks. The defaults are `docstore_blocksize = 1,000,000` bytes and **zstd** level `8` (`IndexingSettings`, plus `Compressor::Zstd` in `quickwit-indexing/src/actors/indexer.rs`) **[code]**. The skip index into the store goes into the hotcache, so fetching a doc costs one GET for its block.
- **Segments per split.** **[inference]** The indexing pipeline typically emits one segment per split, and merges produce one segment. This keeps per-split fan-out fixed.

### 3.5 Split metadata (the catalog row)

Fields of `SplitMetadata` **[code: `quickwit-metastore/src/split_metadata.rs`]**:

- identity: `split_id`, `index_uid`, `partition_id`, `source_id`, `node_id`
- size: `num_docs`, `uncompressed_docs_size_in_bytes`
- pruning: `time_range: Option<RangeInclusive<i64>>`, `tags: BTreeSet<String>` (values `field:value`)
- lifecycle: `create_timestamp`, `maturity` (`Mature` or `Immature{maturation_period}`), `delete_opstamp`, `num_merge_ops`
- reading: `footer_offsets`
- schema: `doc_mapping_uid`

**PostgreSQL schema.** The `splits` table has primary key `(index_uid, split_id)`. Columns include `split_state` (`Staged` | `Published` | `MarkedForDeletion`), `time_range_start/end` (B-tree indexed), `tags TEXT[]`, `split_metadata_json`, `delete_opstamp`, `maturity_timestamp`, `publish_timestamp`, and a generated `split_size_bytes` column (`footer_offsets.end`) (`quickwit-metastore/migrations/postgresql/*.up.sql`) **[code]**.

**Tags.** Tag values are recorded only if a field has at most `MAX_VALUES_PER_TAG_FIELD = 1000` distinct values in the split (`quickwit-indexing/src/actors/packager.rs`) **[code]**.

**Metrics/Parquet signal.** Migrations 26–28 add `metrics_splits` and `sketch_splits` tables. The `metrics_splits` migration comments describe "two-tier pruning": PostgreSQL GIN indexes on low-cardinality tags (`tag_service`, `tag_env`, `tag_host` …), and "Parquet bloom filters for high-cardinality tags". They also add time-window compaction columns (`window_start`, `sort_fields`, `zonemap_regexes`) **[code]**. **[inference]** This is a Parquet-based metrics engine, presumably Datadog-driven. Its Rust code is not in this repo at this commit, so this document covers only the Tantivy split path.

---

## 4. Write path

### 4.1 Ingest and WAL (before object storage)

The two ingest versions differ as follows [docs: `docs/internals/ingest-v2.md`]:

- **Ingest V2** is the default since 0.9. Docs arrive at a router and are written into **shards**: append-only queues kept in `mrecordlog` on an ingester's local disk under `wal/`. The control plane places shards on indexers to balance load. Per-shard progress is tracked in the metastore `shards` table.
- **WAL durability.** Writes are flushed to the OS before the ack but **not fsynced**: "a host failure or power loss can still lose acknowledged documents". Shard replication (`ingest_api.replication_factor`) is "not working yet". Durability of *indexed* data is delegated entirely to the object store.
- **Pull sources.** Kafka, Kinesis, Pulsar and similar sources skip the WAL. The pipeline pulls from them directly, and the source's offsets act as the WAL. Exactly-once comes from atomically publishing checkpoints with splits [docs: `concepts/indexing.md`].

Default ingest limits are `max_queue_memory_usage = 2 GiB`, `max_queue_disk_usage = 4 GiB` and `content_length_limit = 10 MiB` (`quickwit-config/src/node_config/mod.rs`) **[code]**.

### 4.2 Indexing pipeline (actor chain)

The actors live in `quickwit-indexing/src/actors/` **[code]**:

1. **Source → DocProcessor.** Parses JSON, applies optional VRL transforms, and maps docs to the schema.
2. **Indexer.** Builds the in-memory Tantivy segment and routes docs by `partition_key` into separate splits, up to `max_num_partitions`. It commits (cuts a split) on `CommitTrigger::{NumDocsLimit, Timeout, MemoryLimit, ForceCommit, Drained, NoMoreDocs}`. The defaults are `split_num_docs_target = 10M`, `commit_timeout_secs = 60` and `resources.heap_size = 2 GB`.
3. **IndexSerializer.** Finalizes the segment on local disk (CPU heavy).
4. **Packager.** Extracts tags (≤1000 values per field), writes the hotcache, and builds the `SplitPayload` lazily. The bundle is **never materialized as one file**: `SplitPayload` is a list of payload pieces (local files plus the in-memory footer), and `range_byte_stream` streams any byte range across them. That is how multipart parts are produced.
5. **Uploader** (`uploader.rs`). It is limited by a semaphore of `max_concurrent_split_uploads` (default **12** for indexer and compactor). For each batch it runs, in this order:
   1. `metastore.stage_splits(...)`, which inserts rows in state **Staged**;
   2. `IndexingSplitStore::store_split` → `remote_storage.put("{split_id}.split", payload)`;
   3. if the split is immature, *moves* the local split directory into the indexer's `IndexingSplitCache`, so later merges need not download it.
6. **Sequencer → Publisher.** Keeps publish order per pipeline. The Publisher calls `metastore.publish_splits(staged_ids, replaced_ids, checkpoint_delta, publish_token)`.

### 4.3 Commit protocol and guarantees

The PostgreSQL `publish_splits` (`quickwit-metastore/src/metastore/postgres/metastore.rs`) runs **one transaction** **[code]**:

- it loads index metadata and applies the source checkpoint delta (for ingest V2 sources, `try_apply_delta_v2` against the `shards` table, guarded by a **publish token** that fences stale indexers);
- it then runs a single CTE that `SELECT … FOR UPDATE`s the input splits;
- if and only if every split to publish is `Staged` and every split to replace is `Published`, it flips `Staged→Published` and `Published→MarkedForDeletion` and writes the new index metadata (checkpoint).

The resulting guarantees:

- **Atomic visibility.** A split becomes searchable only when its row turns `Published`. The object is fully written before that happens, so readers never see partial objects. The design needs **no object-store atomicity** beyond "a PUT is atomic and read-after-write consistent". **[inference]** S3 has been strongly consistent since December 2020, but Quickwit never needed that: it never overwrites keys (split IDs are unique) and never relies on LIST.
- **Exactly-once.** The checkpoint and the split publish are in the same transaction. On failure, a pipeline restarts from the last published checkpoint.
- **Orphans are bounded.** A crash after `stage_splits` but before publish leaves a `Staged` row and maybe an uploaded object. The janitor GC handles these (§6.4). Staging *before* upload is what makes orphans discoverable without LIST.
- **Time-to-search** is roughly `commit_timeout_secs` (60 s default) plus serialize and upload time.

### 4.4 Uploading (PUT shapes)

See §7.2.

---

## 5. Read / query path

### 5.1 Root

The node receiving the query acts as **root** for it [docs: `concepts/querying.md`, code: `quickwit-search/src/root.rs`]:

1. **Prune in the metastore.** It calls `list_splits` for `Published` splits, filtering on `time_range` (from the query's `start_timestamp`/`end_timestamp` or range filters on the timestamp field) and on **tags** (query terms on tagged fields). This pruning costs **zero object-store requests**.
2. **Build jobs.** Each split becomes a job with an estimated `cost`. `SearchJobPlacer` (`search_job_placer.rs`) assigns jobs with **rendezvous hashing** on `split_id`, so each split has a stable preferred node, which keeps caches warm. It is also **load-aware**: it queries each node's current load with a 200 ms timeout (`GET_LOAD_TIMEOUT`) and computes a target load of `(existing + total) × 1.05`. A node above target is skipped in favour of the next node in the split's rendezvous order. Jobs are sorted by descending cost (LPT-style).
3. **Fan out and merge.** It fans out leaf requests, then merges partial results (top-K, aggregations). A second **fetch-docs** phase retrieves only the final hits' documents from the doc store.

### 5.2 Leaf: opening a split

`open_index_with_caches` (`quickwit-search/src/leaf.rs`) stacks the storage layers, from outermost to innermost **[code]**:

```
tantivy Index
 └─ HotDirectory(hotcache bytes)                         ← RAM, per split, from footer
     └─ CachingDirectory(ByteRangeCache)                 ← per-request ephemeral cache
         └─ StorageDirectory
             └─ StorageWithCache(fast_fields_cache)      ← global LRU, files ending ".fast"
                 └─ BundleStorage(file table from footer) ← maps file+range → split byte range
                     └─ SearchSplitCache (optional)       ← whole .split on local disk
                         └─ TimeoutAndRetryStorage (opt.) ← hedged-ish retries on get_slice
                             └─ DebouncedStorage          ← coalesce identical in-flight GETs
                                 └─ S3CompatibleObjectStorage / Azure / GCS(opendal)
```

1. **Footer.** `get_split_footer_from_cache_or_fetch` looks up `split_footer_cache` (a `MemorySizedCache<String>` keyed by split ID, default **500 MB**). On a miss it issues **one** `get_slice("{split_id}.split", footer_start..footer_end)`.
2. **Bundle.** `BundleStorage::open_from_split_bytes` parses `BundleFileRanges` and returns the hotcache.
3. **Index.** `Index::open(HotDirectory)` makes no further I/O, because all open-time reads hit the hotcache.

### 5.3 Leaf: warmup, then synchronous search

Tantivy's search code is synchronous, so Quickwit **prefetches everything a query needs asynchronously before running it** (`warmup` in `leaf.rs`) **[code]**. Depending on the query AST and collectors, warmup:

- **term dictionary:** fetches dictionary blocks for each term (`warm_up_terms`), term ranges (`warm_up_term_ranges`) and automatons for regex or prefix queries (`warm_up_automatons`); for terms aggregations it fetches the full dictionary (`warm_up_full_term_dictionaries`);
- **postings:** fetches posting lists, and positions when needed for phrase queries; some features need all postings (`warm_up_all_postings`);
- **fast fields and norms:** fetches fast-field columns used by sort, aggregations or range filters (`warm_up_fastfields`), and fieldnorms if BM25 scoring is requested (scoring is off by default).

All of these become concurrent `get_slice` calls. Warmup is short-circuited when a required term is **absent** in a single-segment split, and that absence is cached (`term_absence_cache_key`). After warmup, Tantivy runs from RAM.

**Request count per query.** The vendor's cost model, with the hotcache cached [docs: `docs/operating/aws-costs.md`]:

```
#GET = #splits × (#search_fields × #terms × 3 + #fields_with_fieldnorms + 1 [timestamp fast field])
       + #docs_returned
```

The factor 3 per term is term-dictionary block, postings and positions. It drops to 2 when positions are not indexed. Each returned doc costs one GET for its doc-store block.

### 5.4 Caching layers (summary)

| Cache | Where | Key | Default size | Code |
|---|---|---|---|---|
| Split footer (hotcache) | searcher RAM | split_id | 500 MB | `SearcherConfig.split_footer_cache` |
| Fast fields | searcher RAM | (path, byte range), routed by suffix `.fast` | 1 GB | `QuickwitCache::new` in `quickwit-storage/src/cache/quickwit_cache.rs` |
| Partial request (leaf result) | searcher RAM | 128-bit digest of (split, request minus time range, effective time range) | 64 MB | `quickwit-search/src/leaf_cache.rs` |
| Predicate cache | searcher RAM | query predicate per split | 256 MB | `SearcherConfig.predicate_cache` |
| Ephemeral byte-range cache | per request | (path, range) | unbounded, lifetime = request | `ByteRangeCache` |
| Searcher split cache | searcher **disk** (`searcher-split-cache/`) | whole `.split` | off by default; `max_num_splits` 10,000, `num_concurrent_downloads` 1, `max_file_descriptors` 100 | `quickwit-storage/src/split_cache/` |
| Indexing split cache | indexer/compactor disk | immature split dirs | 100 GiB / 1,000 splits | `IndexingSplitCache` |

Two details of the searcher split cache [docs: `docs/internals/searcher-split-cache.md`]:

- **Tips from indexers.** Searchers learn about new splits through `ReportSplitsRequest` events, which name the searcher with the best rendezvous affinity, and also when splits are queried.
- **Admission.** Splits are downloaded whole with `copy_to_file` (one streaming GET). Admission and eviction use an LRU-like rule.

The cache trades disk for fewer GETs and lower latency on hot data. Because each split is assigned to the node with the strongest rendezvous affinity, each node caches a stable, disjoint subset of splits.

### 5.5 Memory and concurrency control

`SearchPermitProvider` (`search_permit_provider.rs`) serves split searches "by priority, then by lowest estimated cost" **[code]**. Each permit reserves:

- a **warmup slot**, which limits concurrent downloads (`max_num_concurrent_split_searches`, default 100);
- a pessimistic memory budget: `warmup_single_split_initial_allocation` (300 MB) per split against `warmup_memory_budget` (100 GB). Once warmup finishes, the reservation is adjusted to the actual usage.

---

## 6. Data manipulation

### 6.1 Merging / compaction

Merges serve three purposes: fewer splits per query, fewer per-split requests, and larger and better-compressed objects. Because splits are immutable, every merge is a rewrite: download N splits, Tantivy-merge them, upload one new split, then atomically publish the new split and mark the N old ones `MarkedForDeletion`, all in the same `publish_splits` transaction.

**Merge policy** (`quickwit-config/src/merge_policy_config.rs`, `quickwit-indexing/src/merge_policy/`) **[code]**:

- The default is `stable_log`, with `merge_factor = 10`, `max_merge_factor = 12`, `min_level_num_docs = 100,000` and `maturation_period = 48h`. Splits are sorted by recency and bucketed into log-scale levels (each about 3× larger) up to `split_num_docs_target`. A split is mature once it reaches the target or once the maturation period has passed.
- The alternatives are `limit_merge` (`ConstWriteAmplification`, with `max_merge_ops` default 4) and `no_merge`.
- Merges happen only within a `MergePartition { partition_id, doc_mapping_uid }`. Splits from different tenant partitions, or written under different schemas, are never mixed.

**Merge input I/O.** `IndexingSplitStore::fetch_and_open_split` first tries the local `IndexingSplitCache`, where immature splits were *moved* after upload. On a hit, it mmaps the directory with `Advice::Sequential`. On a miss it calls `remote_storage.copy_to(path, file)`: **one full-object streaming GET**, rate-limited through `IoControls`.

**Merge execution.** `MergeExecutor::merge_split_directories` builds a `UnionDirectory` over:

- the output `MmapDirectory`;
- a RAM directory holding a synthetic combined `meta.json`;
- all input split directories.

It then runs a single-threaded Tantivy `IndexWriter` merge (15 MB arena) into one segment. The merged split's time range is the union of the inputs' ranges, `num_merge_ops = max + 1`, and the partition ID is a hash-combine of the inputs' partition IDs.

**Evolution: a separate compactor service.** The new `quickwit-compaction` crate has two parts:

- a central `CompactionPlanner`, which re-scans immature published splits each tick, sorted by `maturity_timestamp ASC` and capped per tick;
- `CompactorService` nodes, which run `CompactionPipeline`s. Defaults are `max_concurrent_merge_executions = num_cpus − 1`, 2 pipeline slots per execution, and a 300 s decommission/drain timeout.

A code comment says the indexer-local split store is "slated for removal once the compactor service lands" (`quickwit-indexing/src/split_store/indexing_split_cache.rs`) **[code]**. **[inference]** Merges are moving off indexers onto a separate, independently scaled tier. Indexers would then stop needing local disk for merge inputs, and compactors would read inputs from object storage (or their own cache).

### 6.2 Deletes (GDPR-style)

The delete API creates a `DeleteTask` with an increasing **opstamp** [docs: `concepts/deletes.md`; code: `quickwit-janitor/src/actors/delete_task_*`]:

- The janitor's `DeleteTaskPlanner` finds mature splits whose `delete_opstamp` is below the latest task.
- It runs the pending delete queries in batch. If any doc matches, it rewrites the split through the merge executor with a single input (`process_delete_and_merge`). The rewrite is a full GET, a Tantivy delete plus merge, a PUT of a new split, and a publish that replaces the old one.
- Immature splits wait until they mature, because a merge would rewrite them anyway.

Deletes are therefore *expensive* (whole-split rewrite), asynchronous, and possibly hours-long.

### 6.3 Retention

`retention_policy_execution.rs` lists published splits with `max_time_range_end < now − retention_period` and calls `mark_splits_for_deletion` **[code]**. It is pure metadata at first; the objects are removed later by GC. Retention works at split granularity, which is another reason to keep splits time-coherent (the stable-log policy merges by recency).

### 6.4 Garbage collection

`run_garbage_collect` (`quickwit-index-management/src/garbage_collection.rs`) runs every **10 min** (`RUN_INTERVAL`, `quickwit-janitor/src/actors/garbage_collector.rs`) **[code]**:

1. **Stale staged splits.** `Staged` splits older than `STAGED_GRACE_PERIOD = 24h` are marked for deletion. These are failed uploads or publishes.
2. **Expired marked splits.** `MarkedForDeletion` splits not updated for `split_deletion_grace_period()` (default **32 min**, `QW_SPLIT_DELETION_GRACE_PERIOD_SECS`) are deleted. The grace period lets in-flight queries that already resolved the split list finish reading the old objects.
3. **Batched deletion.** Splits are deleted in batches of `DELETE_SPLITS_BATCH_SIZE = 10,000`, optionally rate-limited per second with an environment variable. For each batch it calls `storage.bulk_delete(paths)` (S3 `DeleteObjects`, 1,000 keys per request), then deletes the metastore rows *only for the objects that were deleted*.

The design point: **GC is metastore-driven and never LISTs the bucket.** The metastore is the source of truth for which objects exist. The flip side is that an object PUT whose `stage_splits` row was lost could leak forever. Staging-before-upload makes that window essentially empty.

### 6.5 Schema evolution

Doc mappings can be updated. Each split records the `doc_mapping_uid` it was written with. Searches handle mixed mappings, and merges never combine splits with different `doc_mapping_uid`s (`MergePartition`) **[code]**. Old splits are never rewritten for schema changes alone **[inference]**.

---

## 7. Object storage interaction patterns

### 7.1 The `Storage` abstraction

`quickwit-storage/src/storage.rs` defines a deliberately small trait, "meant to allow for multipart download/upload" **[code]**:

```rust
put(path, Box<dyn PutPayload>)            // whole object, may be multipart
copy_to(path, &mut AsyncWrite)            // full streaming GET
copy_to_file(path, local_path)            // full GET to temp file + atomic rename
get_slice(path, Range<usize>) -> OwnedBytes        // ranged GET, buffered
get_slice_stream(path, Range) -> AsyncRead         // ranged GET, streamed
get_all(path)                             // small whole-object GET
delete(path) / bulk_delete(&[path])       // DeleteObject / DeleteObjects
list(prefix) -> stream of {path,size,mtime}   // ListObjectsV2 (newer addition)
exists(path) / file_num_bytes(path)       // HEAD
```

**Backends:**

- `S3CompatibleObjectStorage`, using the AWS SDK and covering AWS, MinIO, Garage, DigitalOcean and GCS in S3 mode;
- `AzureBlobStorage`;
- native GCS through **OpenDAL** (`opendal_storage/`);
- `LocalFileStorage` and `RamStorage`.

**Decorators:** `PrefixStorage`, `DebouncedStorage`, `StorageWithCache`, `TimeoutAndRetryStorage`, `BundleStorage`, `CountingStorage`.

### 7.2 Writes: PUT vs multipart

Writes go through `S3CompatibleObjectStorage::put` (`object_storage/s3_compatible_storage.rs`) and `MultiPartPolicy` (`object_storage/policy.rs`) **[code]**:

- **The default policy minimizes request count.** Its fields are `multipart_threshold_num_bytes = 128 MiB`, `target_part_num_bytes = 5 GiB`, `max_num_parts = 10,000`, `max_concurrent_uploads = 100` and `max_object_num_bytes = 5 TB`. The comment explains: "we want to end up with as few parts as possible since each part is charged as a put request."
  - Objects under 128 MiB use **one PutObject**.
  - Larger objects use multipart with 5 GiB parts, so a typical 1–10 GB mature split takes 1–2 parts plus Create and Complete. The vendor doc's estimate is "usually … 2 PUT requests (1 PUT request / 5GB)" [docs: aws-costs].
  - **[inference]** Large parts limit upload parallelism per object. Quickwit gets parallelism across splits instead (12 concurrent split uploads by default).
- **Streaming parts.** Each part streams its byte range from `SplitPayload::range_byte_stream`, which reads directly from the local Tantivy files and the in-memory footer. No full-bundle temp file is ever written.
- **Failure handling.** Each part is retried with `aws_retry`. On failure, `AbortMultipartUpload` is issued.
- **Checksums.** `checksum_algorithm` defaults to `crc32c` through SDK flexible checksums. `md5` sends `Content-MD5`, computed in 1 MB chunks on a CPU pool (`MD5_CHUNK_SIZE`); `disabled` turns checksums off. Response checksum validation is set to `WhenRequired` because "we mostly do range request anyway".
- **Provider quirks.** `flavor: gcs` disables multipart upload, multi-object delete and checksums. `disable_multipart_upload` and `disable_multi_object_delete` exist for other non-AWS stores [docs: `storage-config.md`].
- **Other backends.** Azure uses 100 MB blocks with a 100 MB threshold, up to 50,000 blocks. GCS through OpenDAL uses a chunked writer sized by the same policy.

**Write request budget per split [inference from code]:**

| Step | Requests |
|---|---|
| Upload | 1 PUT (<128 MiB) or 3+ requests (create, part(s), complete) |
| Merges | Up to `max_merge_ops` more PUT generations for the same docs, plus full GETs on indexing-cache misses |
| GC | One `DeleteObjects` call per 1,000 dead splits |

Quickwit never rewrites an object in place and never writes manifests or index files besides the split itself (with PostgreSQL as the metastore).

### 7.3 Reads: ranged GETs

- **Opening a split costs one ranged GET.** The range is exactly `footer_offsets` from the metastore, and the response is cached by split ID. **No HEAD request is made**, because the size and footer location are already known.
- **Query data are many small ranged GETs, one per needed byte range.** `get_slice` sends `Range: bytes={start}-{end-1}` (`get_object`, `s3_compatible_storage.rs`). Typical ranges are:

  | Read | Typical size |
  |---|---|
  | Term-dictionary block | a few KB **[inference]** |
  | Posting list | proportional to document frequency |
  | Positions | similar |
  | Fast-field column | up to the whole column for that field in that split |
  | Doc-store block | ≤ about 1 MB uncompressed, zstd-compressed |

- **Full GETs** happen only for:
  - merges (`copy_to`) and the delete pipeline;
  - searcher split-cache fills (`copy_to_file`, which writes to a `.temp` file then renames atomically);
  - file-backed metastore JSON (`get_all`).
- **Range coalescing.** **[inference]** I found no generic coalescing of adjacent ranges into one GET in the storage layer. The format makes each logical read contiguous instead (one term's postings, one column, one doc-store block). Concurrency comes from issuing all warmup reads at once.

### 7.4 Concurrency, fan-out, retries and hedging

- **Global request semaphore.** `REQUEST_SEMAPHORE` limits concurrent S3 requests per process to `QW_S3_MAX_CONCURRENCY` (default 10,000), "because some object stores (R2, SeaweedFS...) return errors when too many concurrent requests are emitted". GCS reuses the same variable through OpenDAL's `ConcurrentLimitLayer` **[code]**.
- **Fan-out per query.** The bound is roughly `#leaf nodes × max_num_concurrent_split_searches (100)` splits in warmup at once, each issuing several parallel `get_slice`s. Optional Lambda offload adds burst capacity: up to `max_splits_per_invocation = 10` splits per invocation once local pending searches exceed `offload_threshold = 100`. Lambdas read S3 directly and need only GetObject permission [docs: `lambda-config.md`; code].
- **Retries.** `RetryParams::aggressive()`: base delay 250 ms, max delay 20 s, 5 attempts, applied to transient SDK errors. GCS (OpenDAL) uses jittered backoff from 250 ms to 20 s with a ×2 factor and 4 retries **[code]**.
- **Hedging-style timeouts.** `TimeoutAndRetryStorage` is enabled through `searcher.storage_timeout_policy {timeout_millis, min_throughtput_bytes_per_secs, max_num_retries}`. Each `get_slice` attempt gets a deadline of `timeout_millis + num_bytes / min_throughput`. When the deadline expires, the request is dropped and **re-issued**, up to `max_num_retries + 1` attempts. This is "retry on slow", not true hedging, because the slow request is cancelled rather than raced; the code has `TODO test avoid aborting timed out requests. #5468`. Metrics count successes after 0, 1 and 2+ timeouts. The policy is off by default **[code: `timeout_and_retry_storage.rs`, `node_config/mod.rs`]**.
- **Debouncing.** `DebouncedStorage` wraps every resolved S3, Azure and GCS storage. Concurrent `get_slice` calls for the *same (path, range)* share one in-flight future. This prevents a thundering herd when many concurrent queries warm up the same split **[code: `debouncer.rs`]**.

### 7.5 LIST and HEAD

- **LIST is avoided on every hot path.** The split catalog lives in the metastore: root pruning, GC, retention and merge planning all query PostgreSQL. In the code at this commit, `Storage::list` (ListObjectsV2 with URL encoding and continuation tokens, returning `{path, size, mtime}`) is used only by the `check_connectivity` probe and by the decorators and tests. **[inference]** Its new presence, together with the `QWFT` trailer and `split_recovery_metadata`, is groundwork for rebuilding the catalog from the bucket.
- **HEAD (`file_num_bytes`, `exists`) is used only by the file-backed metastore**, to check whether `metastore.json` or `manifest.json` exists.

### 7.6 Metadata and catalog placement

**PostgreSQL** is the production metastore. It holds the catalog, checkpoints, shards and delete tasks, and is the commit point. Searchers can read from a `metastore_read_replica` service (`use_metastore_read_replica`).

**The file-backed metastore** puts the metadata *on object storage*:

- one `<index_id>/metastore.json` per index, holding the full index metadata and *all* its split rows;
- a `manifest.json` mapping index IDs to their states;
- readers poll it (the default metastore URI carries `#polling_interval=30s`).

It is "strongly advised" only for testing, single-node use, or a single writer with multiple readers (`quickwit-metastore/src/metastore/file_backed/mod.rs`). I found no conditional-PUT (`If-Match`) usage, so concurrent writers would race; the single-writer rule is what keeps it consistent **[code: grep found no `if_match`/`if_none_match`]**.

**Consistency assumptions.** Quickwit needs only two things from the object store: atomic PUT of a new key, and read-after-write for that key. Atomic commits happen in the metastore transaction, not in the object store.

### 7.7 Cost considerations (as designed)

- **Few PUTs per split.** The 5 GiB part target exists explicitly to save PUT requests.
- **One GET per split open**, and zero once the hotcache (footer) is cached.
- **No LIST or HEAD on hot paths.**
- **Bulk delete** at 1,000 keys per request.
- **The main cost risk is GET volume at high QPS × many splits**, which the vendor flags ("could add up quickly if you have a high number of splits or QPS > 10"). Mitigations are pruning (time, tags, partitions), larger and more mature splits, the fast-field and partial-result caches, and the on-disk split cache [docs: aws-costs].
- **Keep compute in-region.** The docs recommend instances in the same region with high network bandwidth (`c5n.2xlarge` was their sweet spot) [docs: aws-costs].

---

## 8. Performance

1. **Hotcache and footer in one GET** remove the open-time chain of small dependent reads. Without it, opening a Lucene-style index on S3 would cost tens of sequential round trips. With it, opening is about 60 ms on S3 [docs].
2. **Async warmup, then sync execution.** All I/O for a split's query is known up front from the query AST, so it is issued **concurrently**. Total latency ≈ one or two round trips (term dictionary block → postings/positions) plus transfer time, not N sequential reads.
3. **Contiguous layouts.** Postings, columns and doc-store blocks are each contiguous, so every logical read is one ranged GET.
4. **Pruning before I/O.** Time-range and tag pruning in PostgreSQL. `partition_key` routing gives tenants their own splits, which makes tags effective. Term-absence short-circuits warmup.
5. **Cache locality from rendezvous hashing.** Each split keeps hitting the same searcher, so its RAM and disk caches stay hot, and adding or removing a node only reshuffles about 1/N of splits.
6. **Layered caches.** Footer (500 MB), fast fields (1 GB), partial results (64 MB; reused when dashboard queries shift only their time bounds), predicates (256 MB), optional whole-split disk cache.
7. **Request deduplication** (debouncer) and **tail-latency control** (timeout-and-retry storage).
8. **Admission control.** Priority- and cost-ordered permits with memory budgets keep a burst of heavy queries from thrashing memory or bandwidth.
9. **Elastic burst.** Lambda leaf offload for overflow splits.
10. **Write side.** Streaming multipart from local files, with no bundle temp file. Immature splits are kept locally, so the first merges cost no GETs. Uploads run concurrently (12 by default). Tantivy indexing runs on dedicated CPU, with `cpu_capacity` used for pipeline scheduling.

---

## 9. Scaling

- **Searchers are stateless.** Any node can be root, and leaves need no data ownership. Adding nodes "takes seconds and does not require moving data around" [docs]. Lost cache locality is the only cost of a membership change, bounded by rendezvous hashing.
- **Indexers are mostly stateless.** Their state is the WAL shards (ingest V2) and the local split cache. The control plane spreads pipelines and shards. `enable_cooperative_indexing` limits memory when many indexes are written on one node.
- **Compactors** are becoming a separately scaled tier, with planner-driven assignment and graceful drain.
- **Storage** scales with the object store. Hot key-prefix throttling is mitigated by `QW_RANDOM_SPLIT_PREFIX`.
- **Multi-tenancy.** Tenants can be split across indexes, which share the same searchers and bucket (the Binance post highlights "pooling searcher resources" across indexes). Within one index, tenants are separated with `partition_key` (for example `hash_mod(tenant_id, N)`) plus tags. `max_num_partitions` guards against split explosion [docs: querying.md].
- **Bottlenecks and limits:**
  - **Metastore.** Every publish, merge, GC batch and search does split listing in PostgreSQL. With millions of splits, the `splits` table and `list_splits` latency dominate. Mitigations: read replicas, GC batching and rate-limiting (`DELETE_SPLITS_BATCH_SIZE`, per-second rate env var), hash indexes on `index_uid`, `idx_splits_stats`.
  - **Split count.** Too many small splits multiply GETs and metastore rows; merges fix this at the cost of write amplification of roughly log₃(target / commit size) generations.
  - **WAL durability.** No fsync and no working replication, so indexers are a soft durability point before upload.
  - **Deletes** rewrite whole splits.

---

## 10. Trade-offs and lessons

**Notable design choices:**

1. **One immutable object per split, with the catalog in a transactional database.** This avoids nearly every object-store consistency problem: no LIST, no overwrites, no manifest races. The atomic commit is a PostgreSQL transaction that also carries the source checkpoint, which gives exactly-once. The cost is that PostgreSQL becomes a critical, scale-limiting dependency. The recent `QWFT` trailer and embedded `split_recovery_metadata` are steps toward rebuilding the catalog from the bucket.
2. **Stage, upload, publish, then GC with grace periods.** Deletion is two-phase (MarkedForDeletion, then a 32-minute grace period) so in-flight readers never hit a deleted object. Staging before upload makes orphans discoverable without LIST. This pattern carries over directly to other systems.
3. **Keep the split footer offsets in the catalog**, so a split opens with one GET and no HEAD.
4. **Precompute the open-time working set (hotcache)** by recording real reads at build time. It is a generic, format-agnostic trick: any reader with a deterministic open path can do it.
5. **Prefetch before computing.** Turning a synchronous engine into one with an "async I/O planning" phase is what makes S3 latency tolerable. This needs a format whose I/O plan can be derived from the query and a small index.
6. **Make objects large and PUTs few.** The 128 MiB / 5 GiB multipart policy optimizes request cost rather than upload parallelism.
7. **Cache affinity through rendezvous hashing** instead of data ownership. The result has the cache efficiency of sharding with the elasticity of statelessness.

**Weaknesses:**

- **GET cost and latency at high QPS** over many splits. Each query touches every unpruned split. There are no global or cross-split term indexes, so pruning relies on time, tags and partitions only.
- **Write amplification from merges.** Each document is rewritten about `max_merge_ops` times, and each merge input needs a full GET on cache misses.
- **Expensive deletes** that rewrite whole splits.
- **Retention at split granularity.**
- **Weak ingest WAL durability** (no fsync, replication unimplemented).
- **A file-backed metastore that is single-writer only** (no conditional writes).
- **Timeout-and-retry is cancel-and-reissue**, not true hedging.

**Transferable to a new object-store-backed system:**

- Immutable, uniquely named objects.
- A transactional catalog as the commit point, with two-phase delete and grace periods.
- A footer that includes both a file table and a small precomputed "open set", with its offsets kept in the catalog.
- Contiguous per-key byte layouts so each logical read is one ranged GET.
- An explicit prefetch phase.
- Single-flight (debounced) GETs.
- Per-process request semaphores for fragile S3-compatible stores.
- Request-count-aware multipart sizing.
- Randomized key prefixes for partition spreading.
- Rendezvous-hash cache affinity.
- Pruning metadata (time range, low-cardinality tags) kept in the catalog, with high-cardinality filters inside the objects. The Parquet-metrics migration hints at that split for Quickwit's next engine.

---

## 11. References

### Source code (quickwit-oss/quickwit @ `5fd12206ee3113b54a656e4cd768785a804722f9`)

Permalink base: `https://github.com/quickwit-oss/quickwit/blob/5fd12206ee3113b54a656e4cd768785a804722f9/`

- `quickwit/quickwit-storage/src/storage.rs`: `Storage` trait
- `quickwit/quickwit-storage/src/object_storage/s3_compatible_storage.rs`: PUT and multipart, ranged GET, DeleteObjects, ListObjectsV2, HEAD, `REQUEST_SEMAPHORE`, checksums
- `quickwit/quickwit-storage/src/object_storage/policy.rs`: `MultiPartPolicy` defaults
- `quickwit/quickwit-storage/src/object_storage/azure_blob_storage.rs`: Azure multipart policy
- `quickwit/quickwit-storage/src/opendal_storage/base.rs`: GCS through OpenDAL (retry, timeout and concurrency layers)
- `quickwit/quickwit-storage/src/split.rs`: `SplitPayloadBuilder`, footer layout
- `quickwit/quickwit-storage/src/bundle_storage.rs`: `BundleStorage`, `QWFT` trailer, tail reads
- `quickwit/quickwit-storage/src/debouncer.rs`: `DebouncedStorage`
- `quickwit/quickwit-storage/src/timeout_and_retry_storage.rs`
- `quickwit/quickwit-storage/src/cache/{mod.rs,quickwit_cache.rs,byte_range_cache.rs,storage_with_cache.rs}`
- `quickwit/quickwit-storage/src/split_cache/{mod.rs,download_task.rs}`
- `quickwit/quickwit-directories/src/hot_directory.rs`: hotcache build and read
- `quickwit/quickwit-directories/src/{storage_directory.rs,caching_directory.rs,union_directory.rs,bundle_directory.rs}`
- `quickwit/quickwit-indexing/src/actors/{indexer.rs,index_serializer.rs,packager.rs,uploader.rs,publisher.rs,merge_executor.rs,merge_planner.rs,merge_split_downloader.rs}`
- `quickwit/quickwit-indexing/src/split_store/{indexing_split_store.rs,indexing_split_cache.rs}`
- `quickwit/quickwit-indexing/src/merge_policy/stable_log_merge_policy.rs`
- `quickwit/quickwit-compaction/src/{compactor_service.rs,planner/compaction_planner.rs}`
- `quickwit/quickwit-search/src/{leaf.rs,root.rs,search_job_placer.rs,search_permit_provider.rs,leaf_cache.rs,fetch_docs.rs}`
- `quickwit/quickwit-metastore/src/split_metadata.rs`
- `quickwit/quickwit-metastore/src/metastore/postgres/metastore.rs`: `publish_splits` transaction
- `quickwit/quickwit-metastore/src/metastore/file_backed/{mod.rs,store_operations.rs,file_backed_metastore_factory.rs}`
- `quickwit/quickwit-metastore/migrations/postgresql/` (2_create-splits, 22_change-splits-pkey, 25_add-split-size, 26_create-metrics-splits, 27_add-compaction-metadata, 28_create-sketch-splits)
- `quickwit/quickwit-index-management/src/garbage_collection.rs`
- `quickwit/quickwit-janitor/src/actors/garbage_collector.rs`, `quickwit/quickwit-janitor/src/retention_policy_execution.rs`
- `quickwit/quickwit-common/src/{lib.rs,shared_consts.rs,retry.rs}`
- `quickwit/quickwit-proto/src/types/split_id.rs`: `QW_RANDOM_SPLIT_PREFIX`
- `quickwit/quickwit-config/src/{node_config/mod.rs,node_config/serialize.rs,index_config/mod.rs,merge_policy_config.rs}`: defaults
- `quickwit/Cargo.toml`: Tantivy fork rev and features, mrecordlog, chitchat, opendal

### Docs (in repo at the same commit, also published at quickwit.io/docs)

- `docs/internals/split-format.md`
- `docs/internals/searcher-split-cache.md`
- `docs/internals/ingest-v2.md`
- `docs/overview/architecture.md` (https://quickwit.io/docs/overview/architecture)
- `docs/overview/concepts/{indexing.md,querying.md,deletes.md}`
- `docs/operating/aws-costs.md` (https://quickwit.io/docs/operating/aws-costs)
- `docs/configuration/{storage-config.md,node-config.md,lambda-config.md}`

### Blog posts and articles

quickwit.io was not reachable from this research session, so these were used through search summaries only:

- Quickwit 101: Architecture of a distributed search engine on object storage. https://quickwit.io/blog/quickwit-101
- Searching the web for < $1000 / month (Common Crawl on S3). https://quickwit.io/blog/commoncrawl
- Scaling search to terabytes on a budget / benchmarking on an adversarial dataset (23 TB GitHub Archive). https://quickwit.io/blog/benchmarking-quickwit-engine-on-an-adversarial-dataset
- How Binance built a 100PB log service with Quickwit. https://quickwit.io/blog/quickwit-binance-story
- Quickwit: A highly cost-efficient search engine in Rust. https://quickwit.io/blog/quickwit-first-release
- Datadog acquires Quickwit (Jan 2025). https://www.datadoghq.com/blog/datadog-acquires-quickwit/
- GitHub issues: #1112 (split-to-leaf assignment), #1761 (SSD cache), #5468 (timeout retry without aborting). https://github.com/quickwit-oss/quickwit/issues
- Related projects: Tantivy fork https://github.com/quickwit-oss/tantivy; mrecordlog https://github.com/quickwit-oss/mrecordlog; chitchat https://github.com/quickwit-oss/chitchat
