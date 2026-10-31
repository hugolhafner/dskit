# Grafana Loki: Object-Storage-Backed Log Storage

> Research note on how Grafana Loki stores, retrieves and changes log data in object storage (S3, GCS, Azure Blob and others).
>
> **Code basis:** `github.com/grafana/loki` at commit `f3de4f5acef4225c9ca671060206440569eae45c` (commit date 2026-10-31, `main` branch, shallow clone). Paths below are relative to the repo root. Permalink base: `https://github.com/grafana/loki/blob/f3de4f5acef4225c9ca671060206440569eae45c/`.
>
> **Confidence markers:** **[code]** means checked in source at that SHA. **[docs]** means checked in the `docs/sources` tree at that SHA, which is the source of grafana.com/docs/loki. **[web]** means taken from web search results only (grafana.com, lokidex.com and techstrong.it could not be fetched from this sandbox). **[inference]** means my own reading or reasoning, not stated by a source.

---

## 1. Overview

Grafana Loki is a horizontally scalable, multi-tenant log aggregation system. Its main design choice is to **index only a small set of labels per log stream** (as Prometheus does for metrics), not the log content. Log lines are compressed into large immutable objects in cheap object storage. Queries (LogQL) pick streams by label matchers, then brute-force scan the selected data in parallel, applying line filters, parsers and aggregations.

**Workload:** append-heavy, write-once log data at very high volume. Grafana Cloud reports multi-PB/day. The docs say blooms target tenants above about 75 TB/month **[docs]**. Most data is never read. Queries come in two kinds:
- "needle in a haystack" searches (trace ID, customer ID);
- metric-style aggregations (`rate`, `count_over_time`) over hours or days.

**Users:** Grafana Cloud Logs, Grafana Enterprise Logs (GEL) and many self-hosted deployments.

**Two storage generations coexist in the code base today:**

| Generation | Status at this SHA | Data format | Index | Metadata/catalog |
|---|---|---|---|---|
| **Chunk store** ("classic", Loki 1.x to 3.x) | GA, default | Row-oriented, gzip/snappy/zstd-compressed **chunks** (~1.5 MB), one object per stream per time span | **TSDB** index files (schema v13). Earlier: BoltDB-shipper, Cassandra/Bigtable/Dynamo | Index files themselves in the bucket. No external DB. |
| **Data objects** (`pkg/dataobj`, "next-gen" Loki: Kafka + columnar + new query engine) | Experimental, behind `-dataobj.enabled=false` by default **[code]** | Columnar, multi-tenant-capable container files ("DOBJ"), target ~512 MB | Per-object **index objects** (postings, streams, stats sections) | Per-tenant **Table of Contents** (ToC) objects updated with **conditional writes** (If-Match / If-None-Match) |

The brief for this research calls the columnar format "Thor". The code and docs at this SHA only say "data objects"/"dataobj", so I did not verify that name **[inference]**. Web coverage of GrafanaCON says the new architecture adds Kafka as a durable stream layer, the columnar "data objects" format, a new query engine and a new scheduler. Grafana claims "up to 20x less data scanned" and "10x faster aggregated queries" **[web; claims not independently verified]**.

---

## 2. Architecture

### 2.1 Classic (chunk + TSDB) architecture

```
              push (HTTP/gRPC, OTLP)
                    │
              ┌─────▼──────┐   hash ring (RF=3 default)
              │ Distributor│──────────────────────────────┐
              └─────┬──────┘                              │ (optional) Kafka topic
                    │ gRPC                                │ partition ring
     ┌──────────────▼───────────────┐                     ▼
     │ Ingesters (stateful)          │◄──── consume one partition each
     │  - per-stream MemChunk        │
     │  - WAL on local disk          │
     │  - TSDB "head" + WAL          │
     └───┬───────────────────┬──────┘
   PUT chunk           PUT index file (*.tsdb.gz, every 15m rotation / 1m upload tick)
         ▼                   ▼
   ┌─────────────────────────────────────────────────────────────┐
   │ Object store bucket                                          │
   │  <tenant>/<fp>/<from>:<through>:<crc>        (chunks)        │
   │  index/index_<day>/<ts>-<ingester>.tsdb.gz   (uncompacted)   │
   │  index/index_<day>/<tenant>/<..>-compactor-<..>.tsdb.gz      │
   │  index/delete_requests/...  bloom/...  (blooms, deprecated)  │
   └─────▲──────────────▲───────────────────────▲────────────────┘
         │ GET chunk    │ LIST+GET index files  │ GET/PUT/DELETE
   ┌─────┴─────┐  ┌─────┴────────┐          ┌───┴────────┐
   │ Queriers  │◄─┤ Index Gateway│          │ Compactor   │ (singleton "main", optional workers)
   │ (stateless│  │ (downloads   │          │ - index compaction per table/tenant
   │  workers) │  │  TSDB to disk│          │ - retention + delete requests
   └─────▲─────┘  │  mmap)       │          └────────────┘
         │        └──────────────┘
   ┌─────┴──────────┐   ┌───────────────┐
   │ Query frontend │──►│Query scheduler│ (split by time, shard by bytes, results cache)
   └────────────────┘   └───────────────┘
   Caches: results cache, chunk cache (L1, optional L2), index-stats/volume caches (memcached)
```

Components **[code: `pkg/loki/modules.go`, `pkg/ingester`, `pkg/indexgateway`, `pkg/compactor`; docs]**:
- **Distributor** (stateless): validates and rate-limits pushes, then hashes `(tenant, labels)` onto the ingester ring and writes to RF ingesters (default `replication_factor = 3`). Optionally it writes to Kafka instead or as well (`distributor.kafka_writes_enabled`).
- **Ingester** (stateful): builds compressed chunks per stream in memory, protected by a local WAL. It flushes chunks to object storage, keeps a TSDB "head" index, and ships TSDB files. It also serves the most recent data (default `query_ingesters_within = 3h`).
- **Query frontend / scheduler**: split queries by time interval, use TSDB statistics to shard each query by bytes, queue the shards per tenant, and cache results.
- **Querier** (stateless): resolves chunk refs through the index gateway, fetches chunks from cache or object storage, and runs LogQL.
- **Index gateway**: downloads TSDB files from the bucket to local disk and serves index queries over gRPC. The mode is `simple` or `ring`.
- **Compactor**: one per cluster for the "main" role, with optional horizontally scaled workers. It merges index files, applies retention and processes delete requests.
- **Bloom planner, builder and gateway**: deprecated **[docs]**.

### 2.2 Data-object ("next-gen") architecture

```
Distributor ──produce──► Kafka topic (partitioned; WarpStream etc. also supported)
                              │ one consumer per partition
                              ▼
                 dataobj-builder (target `dataobj-builder`)
                  1. accumulate records → logsobj.Builder (sections: streams, logs)
                  2. FlushAndSort → scratch store (memory/disk)
                  3. Upload  → dataobj/objects/<sha224[0:2]>/<sha224[2:]>
                  4. Build index object → dataobj/index/v0/indexes/<h[0:2]>/<h[2:]>
                  5. ToC conditional write → dataobj/index/v0/tocs/<window>/<tenant>/toc.toc
                  6. Commit Kafka offset
                              │
       ┌──────────────────────┴───────────────────────┐
       ▼                                              ▼
Querier / new engine (pkg/engine)               Compaction planner + workers
 metastore: GET ToC(s) → index objects           (pkg/engine/compactor,
  → postings → section descriptors                 pkg/dataobj/compaction/v2)
 planner: open object (256 KiB head prefetch,      size-leveled K-way merges,
  metadata cache) → read streams section           ToC ReplaceIndexPointers (atomic swap)
 reader: columnar range reads (coalesced, parallel)
```

**[code: `pkg/dataobj/README.md`, `pkg/dataobj/builder/*`, `pkg/dataobj/metastore/*`, `pkg/querier/dataobjread/*`, `pkg/engine/compactor/*`]**

The querier serves data older than `dataobj.storage_lag` (default **3h**) from data objects. It serves newer data from ingesters. Validation requires `storage_lag >= query_ingesters_within`, otherwise both sources would return the same data **[code: `pkg/querier/querier.go:124`, `pkg/dataobj/config/config.go`]**.

---

## 3. Data model & on-object format

### 3.1 Logical model
- **Tenant** (`X-Scope-OrgID`): the top-level isolation unit, part of every chunk key and index path.
- **Stream** = a unique label set. Its identity is a 64-bit **fingerprint** (a hash of the labels).
- **Entry** = `(timestamp ns, line, structured metadata k/v)`. Structured metadata is non-indexed per-line key-value data. It was added in v3 and is used heavily by OTLP ingestion.

### 3.2 Classic chunk format

**Inner chunk (`pkg/chunkenc/memchunk.go`) [code]:**

```
| magic 0x012EE56A (4B BE) | format (1B: V1..V4) | encoding (1B, V2+) |
| [V4] structured-metadata symbol table (compressed) + CRC32 |
| block_0 bytes + CRC32 | block_1 + CRC32 | ... |
| #blocks (uvarint) | per block: #entries, mint, maxt, offset, [V3+] uncompressedSize, len |  + CRC32
| [V4] structured-metadata length (8B) + offset (8B) |
| block-meta offset (8B) |
```

- **Blocks:** the head block is cut when it exceeds `ingester.chunks-block-size` = **256 KiB uncompressed**. Each block is compressed on its own with the chunk codec (`ingester.chunk-encoding`, default **gzip** at this SHA, with snappy, lz4, zstd and others supported). The block-meta table at the end lets a reader seek to blocks by time without decompressing the others.
- **Chunk size:** `ingester.chunk-target-size` = **1,572,864 B (1.5 MiB) compressed** **[code: `pkg/ingester/ingester.go:157`]**. A value of 0 means a fixed `blocksPerChunk = 10`.
- **Chunk cut / flush triggers:** target size reached, `chunks-idle-period` **30m** idle, `max-chunk-age` **2h**, or the `sync-period` **1h** roll-over. The roll-over is skipped if utilization is below `sync-min-utilization` 0.1. **[code]**
- **V4** adds a per-chunk symbol table for structured metadata. Head-block formats include an "unordered" head that accepts out-of-order writes within a validity window of `max_chunk_age/2` (`pkg/ingester/stream.go:423`).

**Outer envelope (`pkg/storage/chunk/chunk.go: EncodeTo`) [code]:**

```
| metadataLen uint32 BE | snappy( JSON(chunk metadata: tenant, fingerprint, from, through, labels, encoding...) ) |
| dataLen uint32 BE | MemChunk bytes |
```

`Checksum = crc32.Castagnoli(encoded bytes)`. The checksum is part of the object key, so **chunks are content-addressed**.

**Object key (`pkg/storage/config/schema_config.go: ExternalKey`) [code]:**
- Schema ≥ v12: `fmt.Sprintf("%s/%x/%x:%x:%x", UserID, Fingerprint, From, Through, Checksum)` → `<tenant>/<fp-hex>/<from-ms-hex>:<through-ms-hex>:<crc-hex>`
- Schema < v12: `<tenant>/<fp>:<from>:<through>:<crc>`, a flat layout per tenant.

The v12 change gives each stream its own "directory". This spreads keys across many prefixes, which matters for S3 per-prefix request-rate partitioning. My recollection is that the v12 release notes give this S3-performance reason, but I did not re-verify it here **[inference/recollection]**.

### 3.3 TSDB index format (schema v13 → TSDB format v3)

This format is derived from the Prometheus TSDB index **[docs: `docs/sources/reference/tsdb-index-format/*.md`; code: `pkg/storage/stores/shipper/indexshipper/tsdb/index/`]**.

```
magic 0xBAAAD700 (4B) | version (1B: 2,3,4)
symbol table | series | label indices | postings | label offset table
| postings offset table | fingerprint offsets table | TOC (last 76 bytes)
```

- **Series entries** are 16-byte aligned. A series ref is the offset divided by 16, so 4-byte refs can address 64 GB. Series are sorted by **fingerprint**.
- **Chunk metas are inline in each series entry:** `(min_time delta, max_time-min_time, KB, #entries, checksum)`. `KB` and `#entries` are Loki additions. The query frontend uses them for byte-based sharding. The `ChunkMeta` struct is in `tsdb/index/chunk.go`.
- **v3 (schema v13)** adds chunk **page markers**:
  - one marker per **16 chunks** (`ChunkPageSize = 16`), holding the summed KB and entries, min/max time and offset;
  - series with fewer than **64 chunks** (`DefaultMaxChunksToBypassMarkerLookup`) skip the markers;
  - stats for fully covered pages come straight from the markers, without decoding each chunk meta.
- **v4 (schema v14, experimental)** adds `IngestedAt` (a day-granular delta) so retention can be based on ingestion time for back-filled data.
- **Fingerprint offsets table:** a sample taken every 1024th series, mapping fingerprint to series ref. A shard query `fp ∈ [a,b)` can seek directly into the series section. This is what makes fingerprint-range sharding cheap.
- **Multi-tenant files:** ingesters write one file per period holding all tenants. A synthetic `__loki_tenant__` label separates tenants, and the fingerprint matches the single-tenant fingerprint.
- **File naming [code: `tsdb/identifier.go`]:**
  - ingester (multi-tenant): `<unix-ts>-<nodeName>.tsdb`
  - compacted (per tenant): `<ts>-compactor-<from>-<through>-<checksum-hex>.tsdb`
  - Both are gzip-compressed on upload with a `.gz` suffix (`uploads/index_set.go:148,246`).
- **Tables** are periodic: `index.period` must be **24h** for TSDB (`ObjectStorageIndexRequiredPeriod`). Table name = `prefix + (unix_time / period)`, e.g. `index_20392`, under `path_prefix` default `index/`.

### 3.4 Data object ("DOBJ") container format

**[code: `pkg/dataobj/README.md`, `encoder.go`, `decoder.go`, `internal/metadata/*`]**

```
offset 0 : "DOBJ" magic (4B)
offset 4 : file-metadata size (uint32 LE)
offset 8 : format version (varint) + File Metadata (protobuf: dictionary, section types,
           per-section layout {data region offset/len, metadata region offset/len}, tenant)
         : [all sections' metadata regions, contiguous]
         : [all sections' data regions, contiguous]
end-4    : "DOBJ" magic
```

Key design points:
- **The metadata is in a header, not a footer.** The file metadata sits at the start of the file and is followed by every section's metadata, packed together. One ranged GET from offset 0 can therefore return the file metadata and often every section's metadata (see §7). Parquet and ORC, by contrast, need a footer read and then a second read.
- **Sections** are typed by `(namespace, kind, version)` and can be tenant-specific. One object can hold several tenants (`Object.Tenants()`). Section kinds at this SHA:
  - `logs`: columns `stream_id` (int64), `timestamp` (int64 ns), one column per structured-metadata key (binary), `message` (binary).
  - `streams`: `stream_id`, `min_timestamp`, `max_timestamp`, one column per label key, `rows`, `uncompressed_size`, plus shard bucket.
  - `postings` (index objects): `kind` (bloom | label), `object_path`, `section_index`, `column_name`, `label_value`, `bloom_filter`, `stream_id_bitmap`, `uncompressed_size`, min/max timestamp, min/max shard bucket.
  - `stats` (index objects).
  - `indexpointers` (ToC objects): `path`, `min_time`, `max_time`.
  - `pointers` (legacy, no longer written or read).
- **Columnar dataset (`internal/dataset`):** each column is split into **pages**. Each page has a descriptor with offset, size, row count, compression and **statistics** (min/max value, cardinality).
  - Encodings: `PLAIN`, `DELTA`, `BITMAP` (repetition/null levels).
  - Compression: `NONE`, `SNAPPY`, `ZSTD`.
  - zstd `SpeedFastest` is used for intermediate stripes and zstd `SpeedDefault` for final sections.
- **Sort order:** the logs section uses `SortSchemaASC` (`logsobj/builder.go:268`). Rows are sorted by a per-tenant "sort schema" of labels (e.g. service), then by stream, then by time. The other sort orders are `SortStreamASC` (stream ASC, timestamp DESC) and `SortTimestampDESC`.
- **Shard buckets:** each stream gets a bucket = top bits of `labels.StableHash`. The default is `DefaultShardBits = 5`, i.e. **32 buckets**. Bucket values are persisted so readers can push shard predicates down. The comment says "Do not change it" (`sections/streams/shard.go`).
- **Size defaults** (`pkg/dataobj/builder/config.go`) **[code]**:

| | logs objects | index objects | ToC objects |
|---|---|---|---|
| target page size (uncompressed) | **1 MB** | **128 KB** ("smaller pages gives more opportunities to prune") | 4 MB |
| target object size (compressed / builder memory) | **512 MB** | 512 MB | 32 MB |
| buffer size | 128 MB | 128 MB | 32 MB |
| target section size (uncompressed) | 512 MB | 512 MB | 4 MB |

---

## 4. Write path

### 4.1 Classic write path
1. The **distributor** validates the push and hashes the stream onto the ring. It sends to `replication_factor` (3) ingesters and needs a quorum.
2. The **ingester** appends to the stream's head block and writes a **WAL** entry on local disk (`pkg/ingester/wal`). Durability before flush comes from WAL plus replication, not from object storage.
3. **Chunk flush:** a flush loop runs every `flush-check-period` (30s, with jitter). It flushes closed chunks with up to `concurrent-flushes` = **32** per ingester. Each flush has a `flush-op-timeout` of 10m and retries with backoff from 10s to 1m, up to 10 times **[code]**.
   - The chunk is encoded (§3.2) and the key is computed from its checksum.
   - **Replica deduplication:** `Writer.PutOne` (`pkg/storage/stores/series_store_write.go`) first checks the **chunk cache**. "If this chunk is in cache it must already be in the database so we don't need to write it again". Because replicas cut chunks at synchronized boundaries (`sync_period`), the three replicas often produce byte-identical chunks, hence the same key. In that case only one PUT happens and the others skip. Even without a cache hit, a duplicate PUT of the same key is idempotent.
   - The upload is a single `PutObject` per chunk (`pkg/storage/chunk/client/object_client.go:PutChunks`). For S3 this is one PUT of about 1.5 MB, no multipart.
   - The chunk ref is then added to the ingester's TSDB head.
4. **Index shipping** (`indexshipper/tsdb/head_manager.go`, `uploads/`):
   - The TSDB head (with its own WAL) is **rotated every 15 minutes** (`defaultRotationPeriod`) into an immutable multi-tenant TSDB file per period.
   - The upload loop runs every **1 minute** (`UploadInterval`). It gzips each file and PUTs it to `index/<table>/<ts>-<ingester>.tsdb.gz`.
   - Ingesters keep serving recent data, so the index can lag a little behind chunk uploads.
5. **Consistency model [inference from code]:** the order is "chunk first, then index". A chunk object only becomes visible to queries once a TSDB file referencing it is uploaded. Index files are immutable and add-only until compaction. If a chunk upload succeeds but the ingester dies before shipping the index, the WAL replays it. If the WAL is lost, the chunk becomes an orphan that nothing references. No catalog DB is involved.

### 4.2 Kafka-based ingestion (experimental)
The distributor produces records with franz-go to a partitioned topic. Each ingester owns **exactly one partition** through the partition ring and replays from its last committed offset. A tenant is shuffle-sharded over `ingestion_partitions_tenant_shard_size` partitions **[docs: `operations/kafka.md`]**. Replication then comes from Kafka (or WarpStream) rather than RF=3 ingesters. Reads need `querier.query_partition_ingesters=true` and have **no replica**: if a partition's owner is down, recent reads for that partition fail **[docs]**.

### 4.3 Data-object write path

**[code: `pkg/dataobj/builder/*`, `logsobj/builder.go`, `uploader/uploader.go`, `metastore/toc_writer.go`]**

1. **dataobj-builder** consumes one Kafka partition. The partition comes from the hostname, or `-dataobj.builder.partition-id`.
2. Records go into `logsobj.Builder`. Each section builder buffers up to **128 MB**. When the buffer fills, it sorts the records into a zstd-fastest-compressed **stripe**. Stripes are merged when the section is flushed (`SectionStripeMergeLimit`). The README says objects are accumulated "one per tenant and 12-hour window".
3. **Flush triggers:** the estimated size exceeds `TargetObjectSize` (512 MB compressed), `idle-flush-timeout` (**1h**) passes without writes, or `max-builder-age` (**1h**) is reached.
4. `FlushAndSort` re-sorts into the final layout (`SortSchemaASC`, streams remapped) in a **scratch store** (memory or local filesystem, `pkg/scratch`), producing a `dataobj.Object` snapshot.
5. **Upload:**
   - The key is computed by reading the whole object through **SHA-224**: `objects/<hex[0:2]>/<hex[2:]>` (`uploader.getKey`, `sha_prefix_size` default **2**). This is content-addressed, and the 2-hex-char prefix spreads keys over 256 prefixes.
   - The upload is one `bucket.Upload` (thanos `objstore`). On S3, minio-go switches to multipart automatically for large or unknown-size bodies. `s3.part-size` defaults to 0 = "optimally computed".
   - Retries use backoff from 100 ms to 10 s, up to **20 retries**. Retries are safe because the key is content-derived.
6. **Index object:** the builder builds an index object for the uploaded data object. It holds streams, postings (label postings with stream-ID bitmaps, plus bloom filters over structured-metadata column values) and stats sections. It is uploaded to `indexes/<h[0:2]>/<h[2:]>` (`index/indexer_simple.go:154`), under the bucket prefix `dataobj/` plus the metastore prefix `index/v0`. Retries are safe because "the index is not referenced from the metastore until it is recorded in the ToC" (`flush_committer.go:239`).
7. **ToC commit:** `TableOfContentsWriter.WriteEntry` adds an `indexpointers` row `(path, min_time, max_time)` to `tocs/<window RFC3339 with ':'→'_'>/<tenant>/toc.toc`.
   - Windows are **12h** (`MetastoreWindowSize`), and an entry may not span two windows.
   - The ToC is rewritten as a whole with `GetAndReplace`: GET the current object with its ETag, rebuild it with the new pointer, then PUT conditionally with `If-Match: <etag>`, or `If-None-Match: *` if it does not exist yet.
   - A lost race retries with backoff, up to **10 attempts**.
   - The write is idempotent, because "the path of an index object is a hash of its content, so the same path means the same pointer".
8. **Kafka offset commit**, only after **every** object in the flush has been uploaded, indexed and recorded in the ToC (`flush-concurrency` default 1).

**Durability and atomicity:** Kafka holds the data until the commit. The **ToC conditional write is the commit point** that makes data visible to queries. If the process crashes between the upload and the ToC write, the Kafka records are replayed and produce the same objects (same SHA, if the build is deterministic) or new ones. Either way the earlier upload becomes an unreferenced object **[inference]**.

---

## 5. Read / query path

### 5.1 Classic read path
1. **Frontend:** splits the query by time (`split_queries_by_interval`) and asks the index for stats. It then shards by fingerprint range: the shard count doubles until each shard is at most `tsdb_max_bytes_per_shard` (**600 MB**, so typically 300–600 MB per shard). `tsdb_max_query_parallelism` defaults to **128** **[docs: `operations/storage/tsdb.md`]**. The results cache returns cached sub-ranges.
2. **Scheduler → queriers:** per-tenant fair queues. Each querier runs `max_concurrent` workers (16 is recommended with TSDB).
3. **Index lookup:** the querier calls the **index gateway** (or reads the index locally).
   - The gateway lazily **downloads all TSDB files for a (table, tenant)** plus the table's multi-tenant files to `cache_location`. The download is bounded by `download-timeout` of 1m.
   - It **memory-maps** the files (`index-reader-mode=mmap`; a `stream` mode is experimental).
   - It **re-syncs every 5 min** (`resync-interval`) and drops files after `cache-ttl` **24h**.
   - `query-ready-num-days` pre-downloads recent days.
   - Postings intersection → series → chunk metas filtered by time (page markers) and fingerprint shard.
   - The docs say TSDB needs **no index cache** **[docs]**.
4. **Chunk fetch** (`pkg/storage/chunk/fetcher/fetcher.go`):
   - First a **L1 chunk cache** lookup (typically memcached).
   - Optionally a **L2 cache** for chunks older than `l2CacheHandoff` (plus 10%).
   - Misses go to object storage through `GetParallelChunks`, with at most `store.max-parallel-get-chunk` = **150** concurrent GETs. Chunks are fetched in batches of `max-chunk-batch-size` **50**.
   - Each chunk is a **whole-object GET**. There are no range reads: a chunk is about 1.5 MB, and its block index is at the end.
   - Fetched chunks are written back to the cache (`skipQueryWritebackOlderThan` can suppress this).
5. **Execution:** decompress blocks, filter lines, parse, aggregate. Partial results are merged in the frontend.
6. **Blooms (deprecated):** the bloom gateway was consulted with `(series, chunk, filter)` to drop chunks whose structured-metadata blooms rule out a match.

### 5.2 Data-object read path

**[code: `pkg/dataobj/metastore/object.go`, `pkg/querier/dataobjread/*`, `pkg/dataobj/decoder.go`, `internal/dataset/row_reader_downloader.go`, `pkg/util/rangeio/rangeio.go`]**

1. **Resolve ToCs without LIST.** `IterTableOfContentsPaths(tenant, start, end)` computes `tocs/<window>/<tenant>/toc.toc` for each 12h window in the query range. Each ToC is a GET.
2. Scan the `indexpointers` section and keep the index objects whose `[min_time, max_time]` overlaps the query.
3. **Open each index object** with a **64 KiB head prefetch** (`indexPrefetchBytes`).
   - Evaluate stream matchers against the **postings** section: label postings → stream-ID bitmaps, and bloom postings for structured-metadata equality predicates.
   - The result is `DataobjSectionDescriptor{ObjectPath, SectionIdx, StreamIDs, RowCount, Size, Start, End}`.
   - Section descriptors are **streamed**: "the reader can start on one object's logs while the metastore still reads later index objects" (`dataobjread/planner.go`). Data objects are planned concurrently.
4. **Open each data object** with a **256 KiB head prefetch** (`DefaultHeadPrefetchBytes`), with a minimum of 16 KiB. The **metadata cache** stores the "extended metadata region": the file metadata plus the metadata of every non-logs section, up to `MaxItemBytes` (default **8 MiB**). Logs-section metadata is never cached, because it grows with schema width. The cache key is the object path, which is safe because objects are immutable and content-addressed. The object size is learned lazily through `Attributes` (a HEAD), only when needed.
5. Read the **streams** section of the object once (`planObject`) to get labels, hashes and shard buckets for the listed stream IDs. Apply per-stream filters and query-shard predicates. One task is created per surviving logs section.
6. **Columnar logs read** (two-phase `RowReader`):
   - **Phase 1:** read the *primary* (predicate) columns, e.g. `stream_id`, `timestamp` and filtered metadata. Page statistics prune pages, and the reader evaluates the predicate.
   - **Phase 2:** read only the matching rows' *secondary* columns, e.g. `message`.
   - `rowReaderDownloader` batches page downloads by priority: P1 for current-phase pages in range, P2 for secondary pages in range, P3 for pages ahead. The target is **16 MB** of compressed pages in flight per reader. Each `Read` needs **at most two** round-trip batches.
7. **Range I/O** (`rangeio.ReadRanges`, defaults):
   - ranges whose gap is ≤ **1 MiB** are coalesced;
   - ranges larger than **8 MiB** are split into pieces of at least **1 MiB**;
   - ranges are also split to fill parallelism;
   - up to **10** ranges are read in parallel ("10 appears to … saturate the network").

---

## 6. Data manipulation

### 6.1 Index compaction (classic)

**[code: `pkg/compactor/*`, `tsdb/compactor.go`]**

- The compactor runs every `compaction-interval` **10m**. For each table (day) it:
  1. LISTs the files;
  2. downloads them to `working-directory`;
  3. splits the multi-tenant ingester files by tenant and merges them with existing per-tenant compacted files;
  4. writes **one compacted TSDB file per tenant per day** (deduplicating chunk refs from RF=3 ingesters);
  5. uploads it (`PutFile(table, userID, "<name>.gz")`);
  6. deletes the source files (`DeleteFile`), with `upload-parallelism` 10.
- During the swap, readers can briefly see both old and new files. That is harmless because queries deduplicate chunk refs **[inference]**.
- Only one compactor may own the "main" role. The compactor ring has a fixed 1 token and RF 1.

### 6.2 Retention (classic)
- With `retention_enabled`, the compactor uses the index to find chunks that have expired, based on per-tenant or per-stream `retention_period`.
- It writes **marker files** to local disk, or to object storage if `deletion_marker_object_store_prefix` is set. It rewrites the index without those chunks.
- After `retention-delete-delay` (**2h**, so index gateways see the new index first), a **sweeper** with `retention-delete-worker-count` **150** workers issues a **DELETE per chunk object**.
- Chunks are never moved or rewritten for retention. Only the index changes, followed by plain deletes.
- Docs warn that bucket lifecycle rules must only target `<tenant>/` chunk prefixes, and must use an expiration longer than retention plus the delete delay **[docs: `operations/storage/retention.md`]**.

### 6.3 Delete requests (log entry deletion)
- Requests are stored in a BoltDB or SQLite DB that is itself persisted in the bucket under `index/delete_requests/`.
- Requests can be cancelled for `delete-request-cancel-period` (**24h**). Up to `delete-batch-size` **70** requests are processed per cycle. A request with line filters is sharded into pieces no longer than `delete-max-interval` 24h.
- A deletion **with line filters rewrites chunks**: download, filter, re-encode, PUT a new chunk under a new key, update the index, and mark the old chunk for sweeping.
- A newer **horizontal scaling mode** (main + workers) stores **deletion manifests** in the bucket under `__deletion_manifest__/`. Jobs time out after 15m and are retried up to 3 times.

### 6.4 Schema evolution
- `schema_config` is a list of **periods**, each with `from` date, `store`, `object_store`, `schema` (v11/v12/v13/v14) and `index.period`.
- Data is never migrated in place. New periods take effect at a future date, and readers dispatch by time (`SchemaForTime`).
- One binary reads every TSDB format version, and `tools/tsdb/migrate-versions` can rewrite index files **[docs]**.

### 6.5 Data-object compaction

**[code: `pkg/engine/compactor`, `pkg/dataobj/compaction/v2`]**

- The planner and workers are opt-in (`compaction.enabled=false` by default). The planner polls every **5m** and runs up to 16 tasks.
- **Size-leveled K-way merge:**
  - L0 holds runs < **16 GiB** uncompressed (`DefaultSizeLevelBase`). The source comment says a fresh run is about 6 GiB.
  - The ratio between levels is **8**, with **K = 8** runs per task (`max-runs-per-task`).
  - Runs are sorted sequences of objects with inclusive ordering-key bounds. Merges keep physical objects intact where possible ("IsConverged treats touching-only object boundaries as a terminal fixed point").
- Outputs are content-addressed per tenant: `objects/tenants/<tenant>/<h2>/<rest>` and `indexes/tenants/<tenant>/<h2>/<rest>` (`compaction/v2/output_naming.go`). Inputs are multi-tenant, outputs are single-tenant.
- **Publish:** `ReplaceIndexPointers(tenant, window, oldPaths, newEntries)` swaps all pointers in **one conditional ToC write**. If any old path has disappeared (a concurrent compactor), it writes nothing and returns an error, so the data is never double-counted.
- **Garbage collection of replaced objects:** I found no `bucket.Delete` call in `pkg/dataobj` or `pkg/engine` at this SHA. Deleting superseded or orphaned data objects is presumably done elsewhere (a separate tool, a later feature, or lifecycle rules), and is **not verified** **[inference]**. Retention for data objects is also not visible in this code.

### 6.6 Blooms (deprecated)
- The bloom planner (single instance) computes fingerprint-range gaps per tenant and day. Stateless builders read chunks and write **blocks** (tar, optionally compressed) and **metas** (JSON).
- Keys are `bloom/<table>/<tenant>/metas/<minFp>-<maxFp>-<checksum>.json` and `bloom/<table>/<tenant>/blocks/<bounds>/<start>-<end>-<checksum>.tar[.<codec>]` (`bloomshipper/resolver.go`).
- The planner also enforces retention for blooms.

---

## 7. Object storage interaction patterns (key section)

### 7.1 Key layout summary

| Object | Key (relative to bucket / configured prefix) | Typical size | Mutability |
|---|---|---|---|
| Chunk (v12+) | `<tenant>/<fp-hex>/<from-hex>:<through-hex>:<crc32c-hex>` | ≤ ~1.5 MiB compressed (often smaller) | immutable, content-addressed (CRC in key) |
| Uncompacted TSDB | `index/<prefix><day#>/<unix-ts>-<ingester>.tsdb.gz` | KB–MB, one per ingester per 15 min rotation | immutable; deleted by compactor |
| Compacted TSDB | `index/<prefix><day#>/<tenant>/<ts>-compactor-<from>-<through>-<crc>.tsdb.gz` | MB–GB per tenant-day | immutable; replaced by compactor |
| Delete-request DB | `index/delete_requests/…` | small | mutable (whole-object rewrite) |
| Bloom meta/block | `bloom/<table>/<tenant>/{metas,blocks}/…` | MB–hundreds MB | immutable |
| Data object | `dataobj/objects/<sha224[0:2]>/<sha224[2:]>` | target **512 MB** compressed | immutable, content-addressed |
| Compacted data object | `dataobj/objects/tenants/<tenant>/<h2>/<rest>` | multi-GB runs, split into objects | immutable |
| Index object | `dataobj/index/v0/indexes/<h2>/<rest>` (`…/indexes/tenants/<tenant>/…` after compaction) | MB-scale | immutable, content-addressed |
| ToC | `dataobj/index/v0/tocs/<YYYY-MM-DDTHH_MM_SSZ>/<tenant>/toc.toc` | ≤ 32 MB target | **mutable**, CAS via ETag |

### 7.2 PUT patterns
- **Classic chunks:** one small single-part PUT per chunk. At scale this means a very large number of PUTs. As a back-of-envelope figure, 1 PB of 1.5 MB chunks is about 700M objects **[inference]**. Replica deduplication goes through the chunk cache.
- **TSDB files:** each ingester PUTs a gzip file per index period every 15 minutes, plus compactor rewrites. The request count is small.
- **Data objects:** few large PUTs (512 MB), multipart through minio-go and objstore. This cuts PUT count by roughly 300× compared with 1.5 MB chunks **[inference]**, and it makes request cost per GB negligible.
- **Content addressing everywhere** (CRC32C for chunks, SHA-224 for dataobjs and index objects) gives **idempotent retries** with no coordination.

### 7.3 GET patterns
- **Chunks:** whole-object GET (`GetObject`). `GetObjectRange` exists on the S3 client but chunk fetching does not use it. Up to 150 GETs run in parallel per querier, behind memcached L1 and L2.
- **TSDB:** whole-file download to local disk, then mmap. Object storage is touched only on cache miss or resync.
- **Data objects:** **ranged GETs only**.
  - A head read of `[0, 256 KiB)` (data objects) or `[0, 64 KiB)` (index objects) almost always covers the 8-byte header, the protobuf file metadata and the packed section-metadata region. If the metadata is larger, one extra GET reads `[8, 8+metadataSize)` (`fetchAndDecodeMetadata`).
  - The prefetched buffer is reused as a "prefetched window" (`prefetchedRangeReader`), so later section-metadata reads in that window cost nothing.
  - Page reads are coalesced: gaps ≤ 1 MiB are merged, ranges > 8 MiB are split, and resulting ranges are ≥ 1 MiB. Up to 10 run in parallel per `ReadRanges` call.
  - Column-pruned and page-pruned reads mean a query touches a small fraction of the 512 MB object.
- **Metadata cache:** only immutable objects are cached (keyed by path). The docs warn that installations must not share a cache backend, because keys are not bucket-qualified **[code: `dataobj/config/config.go`]**.

### 7.4 LIST usage
- **Classic:** LIST is central to the index.
  - Index gateways, queriers and the compactor LIST table names and per-table or per-tenant files.
  - `cachedObjectClient` caches LIST results, refreshing at most every **1 minute** (`cacheTimeout`), and deduplicates concurrent refreshes with `singleflight`.
  - It handles the S3 quirk of "directory" objects showing up in listings.
- **Data objects:** the **query path does not use LIST**, because ToC keys are computed from (tenant, 12h window). LIST (`bucket.Iter`) is used only by the compactor to discover tenants per window (`ListTableOfContentsTenants`) and by the explorer UI.

### 7.5 HEAD usage
- Classic: `ObjectExists` and `GetAttributes` exist but are not on the hot path.
- Dataobj: `Attributes` (HEAD) is called lazily only when the object size is needed (`decoder.objectSize`). Opening an object normally needs no HEAD, because the head read gives everything needed.

### 7.6 Consistency, atomic commits and catalog
- **Classic:** no catalog. The **index files in the bucket are the catalog**, and visibility comes from immutable, add-only index files.
  - The design assumes strong read-after-write and list-after-write consistency, which S3 has provided since Dec 2020; GCS and Azure also provide it.
  - Index files written before 2020 handled eventual consistency with resync intervals and retention delays **[inference]**.
  - Compactor swaps are "upload new, then delete old" and are not atomic. Deduplication at query time makes them safe.
- **Data objects:** an explicit **optimistic-concurrency catalog in the bucket**.
  - Per-tenant, per-window ToC files are updated through `GetAndReplace`: GET plus ETag, then PUT with `If-Match`, or `If-None-Match` for creation. This needs S3 conditional writes (GA in late 2024), GCS generation preconditions or Azure ETags.
  - Loki wraps the bucket in `sizedGetAndReplaceBucket` because of a subtle bug. With an unknown-size reader, minio-go falls into `putObjectMultipartStreamNoLength`, which **drops the `If-Match` header** on `CompleteMultipartUpload`. Stale writes would then be accepted silently. The fix buffers the body so its size is known (`pkg/storage/bucket/client.go:305-330`). This is a useful lesson for any CAS-on-object-store design.
  - Multi-pointer swaps (compaction) are atomic because the whole ToC is one object.
- **External dependencies:**
  - memcached for caches (optional but standard);
  - a KV store or memberlist for rings;
  - Kafka for the next-gen write path;
  - the delete-request DB as an SQLite/BoltDB file stored in the bucket.

### 7.7 Request fan-out, hedging and cost
- **Hedged requests** are available for all object clients through `storage_config.hedging`:
  - `hedge_requests_at` default **0** (disabled);
  - `hedge_requests_up_to` 2;
  - `hedge_max_per_second` 5, a rate-limited round-tripper that caps the cost (`pkg/storage/chunk/client/hedging/hedging.go`).
- **Fan-out:** classic queries fan out to thousands of chunk GETs, which is why chunk caches are important. Data objects fan out to a handful of 1–8 MiB range GETs per section.
- **Cost levers:**
  - bigger objects (fewer PUTs);
  - coalesced ranged reads;
  - computed keys (no LIST);
  - caches for metadata;
  - no LIST in the hot path;
  - `sha_prefix_size` / fingerprint prefixes to avoid S3 per-prefix throttling (3,500 PUT / 5,500 GET per second per prefix) **[inference re: motivation]**.

---

## 8. Performance

- **Small index by design:** only labels are indexed. TSDB is compact enough to need no index cache. The fingerprint offsets table and page markers make shard planning (`GetShards`/stats) cheap.
- **Byte-based dynamic sharding:** the frontend sizes shards to 300–600 MB of compressed chunk data using `KB` from the chunk metas. This gives uniform work units across stateless queriers, with `tsdb_max_query_parallelism` 128.
- **Caching layers:**
  - results cache (frontend);
  - index-stats and volume caches;
  - chunk cache (L1 plus optional L2 with time-based handoff);
  - local TSDB disk cache on index gateways (mmap);
  - dataobj metadata cache (≤ 8 MiB items);
  - in-memory ingester data for the last ~3h.
- **Parallel I/O:** 150 parallel chunk GETs per querier; 10-way parallel coalesced range reads for data objects; concurrent planning of objects while the metastore is still streaming.
- **Columnar pruning (data objects):**
  - projection (read only needed columns);
  - page-level min/max statistics;
  - two-phase predicate-then-materialize reads;
  - shard-bucket pushdown;
  - stream-ID bitmaps and per-section bloom filters in index objects to skip whole sections or objects.
  - Sorting by sort-schema labels then stream makes streams contiguous, which gives good compression and narrow ranges.
- **Write-side CPU:** the 128 MB buffer is sorted into stripes with fast zstd, and the final merge uses default zstd. When appends arrive in order, stripe creation is skipped.
- **Prefetch:** a head prefetch (16 KiB minimum, 256 KiB in the querier) turns object open into one GET. The downloader prefetches P3 pages ahead of the read cursor.
- **Classic chunk scan:** blocks are independently compressed with a block-meta index, so the scan can skip blocks outside the time range. It still has to download the whole chunk.

---

## 9. Scaling

- **Stateless components:**
  - distributor, query frontend (mostly), querier, index gateway (stateless apart from its disk cache);
  - bloom builder, dataobj compaction workers, and the new engine workers;
  - all scale horizontally.
- **Stateful components:**
  - ingesters (memory plus WAL, ring membership; RF=3 or one Kafka partition each);
  - compactor (single "main" for each cluster; horizontally scalable workers for deletes in the newer mode);
  - bloom planner (singleton);
  - dataobj-builder (one per Kafka partition; replica count reported through `loki_dataobj_builder_required_replicas`).
- **Multi-tenancy:**
  - classic: tenant prefix on chunk keys, per-tenant compacted index files, per-tenant limits and shuffle sharding;
  - data objects: multi-tenant objects at ingest (fewer, larger objects for small tenants) and per-tenant ToCs. Compaction splits outputs per tenant.
- **Partitioning:**
  - write: hash ring over stream hashes, or Kafka partitions (shuffle-sharded per tenant);
  - index: by day (24h tables) and tenant;
  - data objects: 12h ToC windows, and shard buckets (32 by default) inside the streams.
- **Bottlenecks and limits:**
  - Classic: very high object counts and PUT/GET rates, which need memcached at scale.
  - Index gateways must download large per-tenant daily indexes, bounded by the 1m download timeout. Very large tenants need more disk and time.
  - Compactor singleton throughput and disk.
  - High-cardinality labels cause stream explosion and tiny chunks, which is Loki's best-known failure mode.
  - Queries that brute-force large volumes (needle searches) are I/O bound. Blooms were an attempt to fix this and are now deprecated in favour of data-object postings and blooms.
  - Data objects: ToC contention per tenant and window. CAS retries are capped at 10, and ToC size is bounded (32 MB target). Kafka becomes a required dependency.

---

## 10. Trade-offs & lessons

**Classic design choices:**
1. *Index labels, not content.* This makes the index tiny and keeps ingest and storage cheap. The cost is brute-force scans, which Loki mitigates with massive parallelism and sharding. Fine-grained search needs extra structures (blooms, columnar data objects).
2. *Many small immutable objects (~1.5 MB chunks), one per stream.* This is simple, there is no read-modify-write, and it is content-addressed. The cost is very high request counts, prefix-hotspot concerns (hence the v12 key change) and a heavy reliance on caches. Fragmentation is bad for low-volume streams.
3. *Bucket as catalog through immutable index files plus LIST.* There is no external DB. LIST is cached and the files are downloaded to disk. This works because index files are compact, but it makes index gateways stateful-ish.
4. *Retention by index rewrite then deferred delete (2h).* This is safe against readers holding the old index. The general lesson: in object-store systems, **delete only after every reader can no longer reference the object**.
5. *Replication at the ingester (RF=3) with deterministic chunk cutting* makes identical chunks deduplicate for free through content addressing. Kafka moves durability out of the ingesters.

**Next-gen (data objects) choices:**
1. *Few large, sorted, columnar objects (512 MB)* with **metadata at the head**. Object open is one GET, and coalesced ranged reads plus projection and page pruning follow. This reuses Parquet ideas but is tuned for object stores (head metadata, packed section-metadata region, cacheable "extended metadata region").
2. *Two-level index in the bucket:* small ToC (by tenant and window) → index objects (postings, blooms, stats) → data-object sections. **Computed keys remove LIST from the query path.**
3. *Optimistic concurrency with conditional PUT* on a small mutable manifest per tenant and window, and immutable content-addressed data everywhere else. This is the same pattern as Delta, Iceberg and WarpStream-style metadata, without a database. Note the minio-go multipart pitfall: test CAS semantics end to end.
4. *Kafka as the WAL and the coordination point.* Objects are committed before the offset, which gives at-least-once delivery with idempotent, content-addressed outputs.
5. *Size-leveled compaction* (L0 16 GiB, ratio 8, K=8) keeps write amplification low and converges to non-overlapping key ranges per window.
6. **Open weaknesses visible at this SHA:**
   - GC of orphaned or superseded data objects is not in the code I read;
   - the metadata cache is keyed by path only;
   - ToC hot-spotting for very large tenants in a 12h window;
   - experimental status overall.

**Transferable lessons for a new object-store-backed system:**
- Use **content-addressed immutable data objects** plus **one small CAS-updated manifest per partition**. This gives idempotent retries and atomic multi-object swaps without a DB.
- Put **metadata at the head (or a fixed tail) and pack all section metadata together**, so one speculative ranged GET (16–256 KiB) opens a file. Cache that region by immutable key.
- Do **range coalescing with explicit parameters**: a gap threshold of about 1 MiB, a maximum range of about 8 MiB, a minimum split of about 1 MiB, and about 10 parallel requests. Use **two-phase reads** (predicate columns first).
- **Avoid LIST** on hot paths by computing keys from (tenant, time window). Where LIST is unavoidable, cache it with singleflight.
- **Size-based dynamic sharding** driven by index statistics gives predictable work units for stateless workers.
- **Hash prefixes** (2 hex chars) on keys spread load across object-store partitions.
- Make delete safe with **mark → delay → sweep**, and keep lifecycle rules scoped to data prefixes only.

---

## 11. References

### Source code (grafana/loki @ `f3de4f5acef4225c9ca671060206440569eae45c`)
Permalink base: `https://github.com/grafana/loki/blob/f3de4f5acef4225c9ca671060206440569eae45c/`

- `pkg/dataobj/README.md`: DOBJ format, sections, encoding and decoding, metastore overview
- `pkg/dataobj/decoder.go`: `minimumPrefetchBytes = 16 KiB`, `fetchAndDecodeMetadata`, `fetchExtendedMetadataRegion`, `extendedMetadataRegionEnd`, lazy `objectSize`
- `pkg/dataobj/dataobj.go`, `range_reader.go`, `prefetch_range_reader.go`: `FromBucket`, `bucketRangeReader` (GetRange/Attributes), prefetched window
- `pkg/dataobj/uploader/uploader.go`: SHA-224 key `objects/<prefix>/<rest>`, retry backoff 100ms–10s × 20
- `pkg/dataobj/config/config.go`: `dataobj/` bucket prefix, `storage_lag` 3h, metadata cache
- `pkg/dataobj/builder/config.go`, `flush.go`, `flush_committer.go`: size defaults, flush triggers, upload → index → ToC → offset commit ordering
- `pkg/dataobj/logsobj/builder.go`: `BuilderBaseConfig`, `FlushAndSort`, `SortSchemaASC`
- `pkg/dataobj/sections/logs/builder.go`: sort orders, stripes
- `pkg/dataobj/sections/streams/shard.go`: `DefaultShardBits = 5`
- `pkg/dataobj/sections/postings/postings.go`: postings columns, bloom/label kinds
- `pkg/dataobj/index/indexer_simple.go`: `indexes/<h2>/<rest>` key
- `pkg/dataobj/metastore/object.go`: `MetastoreWindowSize = 12h`, `tocs/` layout, `indexPrefetchBytes = 64 KiB`, `ListTableOfContentsTenants`, `IterTableOfContentsPaths`
- `pkg/dataobj/metastore/toc_writer.go`: `DefaultTocBuilderConfig`, `WriteEntry`, `ReplaceIndexPointers`, `GetAndReplace` CAS
- `pkg/dataobj/metastore/config.go`: `index/v0` prefix
- `pkg/dataobj/metadatacache/cache.go`: `DefaultMaxItemBytes = 8 MiB`
- `pkg/dataobj/internal/dataset/row_reader_downloader.go`: page batching priorities, `defaultTargetDownloadedBytes = 16 MB`
- `pkg/dataobj/internal/metadata/datasetmd/datasetmd.proto`: encodings, compression, statistics
- `pkg/dataobj/compaction/v2/{doc.go,size_leveled.go,output_naming.go}`: K-way, size-leveled compaction, output keys
- `pkg/engine/compactor/{config.go,publish.go}`: compaction planner defaults, ToC swap publication
- `pkg/querier/dataobjread/{planner.go,objects.go}`, `pkg/querier/dataobj_store.go`, `pkg/querier/querier.go`: dataobj read planning, 256 KiB head prefetch, storage-lag validation
- `pkg/util/rangeio/rangeio.go`: `DefaultConfig` (parallelism 10, coalesce 1 MiB, max 8 MiB, min 1 MiB)
- `pkg/storage/bucket/client.go`: `sizedGetAndReplaceBucket` (minio-go If-Match drop on multipart)
- `pkg/storage/bucket/s3/config.go`: `s3.part-size` default 0
- `vendor/github.com/thanos-io/objstore/providers/s3/s3.go`: `GetAndReplace` (ETag-conditional upload)
- `pkg/chunkenc/memchunk.go`: chunk magic `0x12EE56A`, formats V1–V4, `writeTo` layout
- `pkg/storage/chunk/chunk.go`: outer envelope, CRC32C checksum
- `pkg/storage/config/schema_config.go`: `ExternalKey`, `newerExternalKey`, `IndexTables.PathPrefix = "index/"`, period config
- `pkg/storage/chunk/client/object_client.go`, `client/util/parallel_chunk_fetch.go`, `client/aws/s3_storage_client.go`: chunk PUT/GET
- `pkg/storage/chunk/fetcher/fetcher.go`: L1/L2 chunk cache, handoff
- `pkg/storage/chunk/client/hedging/hedging.go`: hedging flags
- `pkg/storage/factory.go`: `max-parallel-get-chunk` 150, `max-chunk-batch-size` 50
- `pkg/storage/stores/series_store_write.go`: cache-based chunk write deduplication
- `pkg/ingester/ingester.go`: flush and chunk defaults
- `pkg/storage/stores/shipper/indexshipper/shipper.go`: `UploadInterval = 1m`, resync 5m, cache TTL 24h, reader mode
- `pkg/storage/stores/shipper/indexshipper/tsdb/{head_manager.go,identifier.go,compactor.go}`: 15m rotation, file naming
- `pkg/storage/stores/shipper/indexshipper/tsdb/index/{index.go,chunk.go}`: magic `0xBAAAD700`, `ChunkMeta`, `ChunkPageSize = 16`
- `pkg/storage/stores/shipper/indexshipper/uploads/index_set.go`: gzip on upload
- `pkg/storage/stores/shipper/indexshipper/storage/cached_client.go`: LIST cache (1m)
- `pkg/compactor/config.go`, `pkg/compactor/index_set.go`, `pkg/compactor/retention/*`, `pkg/compactor/deletion/*`: compaction, retention and delete defaults
- `pkg/storage/stores/shipper/bloomshipper/resolver.go`: bloom key layout

### Documentation (in repo at the same SHA; published at grafana.com/docs/loki)
- `docs/sources/operations/storage/tsdb.md`: TSDB, dynamic sharding (600 MB per shard), no index cache. https://grafana.com/docs/loki/latest/operations/storage/tsdb/
- `docs/sources/reference/tsdb-index-format/{_index,v2,v3,v4}.md`: TSDB binary format
- `docs/sources/operations/storage/retention.md`: retention, lifecycle policies, bucket object layout. https://grafana.com/docs/loki/latest/operations/storage/retention/
- `docs/sources/operations/kafka.md`: Kafka-based ingestion. https://grafana.com/docs/loki/next/operations/kafka/
- `docs/sources/operations/bloom-filters.md`: blooms (deprecated)
- `docs/sources/shared/configuration.md`: config defaults (`chunk_encoding` gzip, `sha_prefix_size` 2, limits)
- `docs/sources/release-notes/v3-7.md`: dataobj partitioning and index-object changes
- Architecture (next): https://grafana.com/docs/loki/next/fundamentals/architecture (not fetchable from sandbox)

### Blog posts, talks, articles (via web search; pages could not be fetched from this sandbox)
- Grafana Labs, "Grafana Loki 3.0 release: all the new features" (2024-04-09): https://grafana.com/blog/2024/04/09/grafana-loki-3.0-release-all-the-new-features
- Owen Diehl, TSDB design post (referenced by the Loki docs): https://lokidex.com/posts/tsdb (not fetched)
- GrafanaCON session "Loki: petabyte-scale logging architecture": https://grafana.com/events/grafanacon/2026/agenda/loki-petabyte-scale-logging-architecture
- Techstrong, "Loki pairs with Kafka and smarter storage to help calm cost and scaling challenges": https://techstrong.it/features/loki-pairs-with-kafka-and-smarter-storage-to-help-calm-cost-and-scaling-challenges/
- InfoQ (2026-04) on Loki re-architecture: https://infoq.com/news/2026/04/grafana-loki-ai-agents
