# Object-Storage-Backed Data Stores: Cross-System Synthesis

> This synthesis is built from the six per-system research documents in this directory:
> [Datadog Husky](datadog-husky.md), [Quickwit](quickwit.md), [Grafana Loki](grafana-loki.md),
> [Grafana Tempo](grafana-tempo.md), [WarpStream](warpstream.md) and [ClickHouse](clickhouse.md).
> Each claim here traces back to one of those documents, where the primary sources (blogs, docs, code
> paths at pinned commits) are cited. Husky and WarpStream are closed source, so statements about them rest on
> vendor material, and the per-system docs mark what is inferred. Where this synthesis adds its own
> interpretation, it says so.
>
> Snapshot date: 2026-10-31.

---

## Contents

1. [The systems in one paragraph each](#1-the-systems-in-one-paragraph-each)
2. [Comparison at a glance](#2-comparison-at-a-glance)
3. [The shared reference architecture](#3-the-shared-reference-architecture)
4. [Common patterns](#4-common-patterns)
5. [Modality-specific patterns](#5-modality-specific-patterns)
6. [Where the systems differ](#6-where-the-systems-differ)
7. [Interesting approaches, system by system](#7-interesting-approaches-system-by-system)
8. [Convergent trends](#8-convergent-trends)
9. [Design guide: choices for a new object-storage-backed store](#9-design-guide-choices-for-a-new-object-storage-backed-store)
10. [Caveats and open questions](#10-caveats-and-open-questions)

---

## 1. The systems in one paragraph each

**Datadog Husky** is a multi-tenant, schemaless, columnar event store for logs, spans, RUM and other events,
running at more than 100 trillion events a day. Stateless writers turn Kafka shards into small immutable
"fragments" on S3, GCS or Azure, and commit them to **FoundationDB**, which is the only source of truth for
what is visible. A two-stage LSM compactor (size-tiered, then "locality" compaction on a sort key) grows
fragments from about 1k to about 1M rows. Readers sit behind deep caches, so only about 0.4% of fragment
queries ever reach object storage.

**Quickwit** is a Rust search engine built on tantivy. Each **split** is a self-contained inverted index
written as one immutable object, with a footer ("hotcache") that makes opening it cheap. **PostgreSQL** holds
the split catalog and the ingest checkpoints. Searchers open a split with one ranged GET, using the footer
offsets stored in Postgres, then issue many small concurrent range GETs for exactly the postings and columns a
query needs. Datadog acquired Quickwit in January 2025.

**Grafana Loki** is a label-indexed log store, and it currently runs **two engines side by side**. The
classic engine writes about 1.5 MB per-stream compressed chunks plus TSDB index files to the bucket, with no
external database. The newer **data-object** engine (`pkg/dataobj`, the Loki 4 direction) reads from Kafka and
writes columnar objects of up to 512 MB, content-addressed by SHA-224. Discovery goes through small
per-tenant **Table of Contents** objects updated with conditional writes (`If-Match`).

**Grafana Tempo** is a trace store. Blocks are Parquet files (vParquet5 by default) plus sharded bloom
filters and a `meta.json`, all in the bucket. Writing `meta.json` is the single-object commit. In Tempo 3.x,
**Kafka is the WAL** (RF1): block-builders cut one block per tenant per partition every 5 minutes, using
deterministic block IDs so replays are idempotent. A **tenant index** object replaces LIST for most readers.

**WarpStream** is a diskless, Kafka-compatible streaming system. Stateless, leaderless Agents batch records
from *many partitions* into one roughly 1 MiB object per flush (250 ms by default). They commit file and batch
metadata to a proprietary, strongly consistent **Virtual Cluster** metadata service, which assigns offsets at
commit time. Reads go through a per-AZ consistent-hash "distributed mmap" cache, so each chunk is fetched about
once per AZ. An LSM-style compactor turns small multi-partition files into large partition-local ones.

**ClickHouse** is a general-purpose columnar OLAP database. Its MergeTree engine **does not know it is on
S3**. `DiskObjectStorage` emulates a filesystem: every file becomes write-once blobs with random keys, plus
a metadata record (blob list, size, hard-link refcount). That metadata lives on local disk (OSS default), in
the bucket (`plain_rewritable`), or in **Keeper** (SharedMergeTree in ClickHouse Cloud). Marks and a sparse
primary index turn queries into precise ranged GETs. Cloud adds a per-AZ distributed cache.

---

## 2. Comparison at a glance

### 2.1 Identity and workload

| | Husky | Quickwit | Loki | Tempo | WarpStream | ClickHouse |
|---|---|---|---|---|---|---|
| **Modality** | Events (logs, spans, RUM…) | Search over logs and traces | Logs | Traces | Streaming log (Kafka API) | General OLAP |
| **Source** | Closed (blogs) | Open (Rust) | Open (Go) | Open (Go) | Closed (blogs, docs) | Open (C++); SMT is closed |
| **Dominant read** | Needle search + aggregations over recent data | Full-text search, top-K, aggregations | Label select + brute-force scan | Trace-by-ID + TraceQL scans and metrics | Sequential tail reads and replays by offset | Analytical scans, point lookups by sort key |
| **Write latency target** | Seconds (buffer time cap) | `commit_timeout` 60 s + upload | Classic: chunk flush; dataobj: up to 1 h flush, 3 h "storage lag" | 5-minute cycles (live-stores serve recent data) | 250 ms default; ~33 ms median with S3EOZ + Lightning | Synchronous with INSERT |
| **Schema** | Schemaless, up to ~10^6 columns per fragment | Doc mapping (tantivy fields) | Labels + line (+ structured metadata) | Nested OTLP + dedicated columns | Opaque records | Strict schema |

### 2.2 Storage unit and format

| | Husky | Quickwit | Loki classic | Loki dataobj | Tempo | WarpStream | ClickHouse |
|---|---|---|---|---|---|---|---|
| **Unit** | Fragment (one tenant table, one time window) | Split | Chunk (one stream) | Data object (many streams, one tenant, one 12 h window) | Block (one tenant; per partition per cycle from the builder) | File (many partitions, one Agent flush) | Part (directory of files) |
| **Fresh size** | ≤ a few thousand events | Tens to hundreds of MB | ~1.5 MB compressed target | Up to 512 MB | KBs to a few GB | ~1 MiB compressed | Whatever the INSERT carries |
| **Compacted size** | ~1M rows | ~10M docs (a few GB, 15 GB in one example) | n/a (chunks not merged) | Levels from 16 GiB base, ratio 8 | Up to 100 GB (`max_block_bytes`) | Up to 128 GiB uncompressed (then frozen) | Up to 150 GiB per merge |
| **Format** | Custom columnar, column-major with headers and an offset skip list | tantivy index bundle + footer | Row blocks + block metas | Custom columnar "DOBJ", **header-first** | Apache Parquet | Custom (re-encodes Kafka batches) | MergeTree (`.bin` + marks + sparse PK) |
| **Side structures** | Text-search "segments" (bitset postings, n-grams) | Inverted index inside the split | TSDB index files, bloom blocks | Index objects (postings + blooms + stats) | `bloom-N` shards, `index` | Metadata section inside the file | Skip indexes, minmax, `primary.idx` |
| **Key naming** | Opaque, paths stored in FDB | ULID (optionally random-prefixed) | `<tenant>/<fp>/<from>:<through>:<crc>` | `objects/<sha224[:2]>/<rest>` | `<tenant>/<uuid>/<file>` | Derived from file ID (range allocator spreads prefixes) | `<prefix>/[a-z]{3}/[a-z]{29}` random |

### 2.3 Metadata, commit and durability

| | Husky | Quickwit | Loki classic | Loki dataobj | Tempo | WarpStream | ClickHouse |
|---|---|---|---|---|---|---|---|
| **Catalog lives in** | FoundationDB | PostgreSQL (or single-writer JSON in bucket) | Bucket (TSDB files, found by LIST) | Bucket (ToC objects) | Bucket (`meta.json` + tenant index) | Vendor-hosted Virtual Cluster | Local FS / bucket / Keeper |
| **Commit primitive** | FDB transaction | Postgres transaction + checkpoint CAS | Index file upload; LIST-after-PUT | `If-Match` conditional PUT on ToC | PUT `meta.json` last | VC `CommitFile` assigns offsets | Local rename / Keeper multi-op / rewrite `prefix.path` |
| **Pre-object-store durability** | Kafka | Kafka checkpoint or replicated `mrecordlog` WAL | Ingester WAL, RF=3 | Kafka | Kafka (RF1) | None; ack after PUT + commit (or a journal in object storage for Lightning) | None; part written directly |
| **Exactly-once mechanism** | Dedupe ID tables committed in the same FDB txn | Checkpoint delta CAS in publish txn | Cache-based write dedupe + query-time dedupe | Content addressing; commit offset after ToC | Deterministic v5 UUIDs from (tenant, partition, offset) | Producer seq check at commit ("retroactive tombstones") | Insert dedup by block hash / token |
| **Needs object-store CAS?** | No | No | No | **Yes** | No | No (for the core path) | No (MergeTree); yes for Iceberg writes |
| **Needs LIST on hot path?** | No | No | Yes (index sync, cached 1 min) | No (LIST only for tenant discovery in compactor) | Builders only; readers GET the tenant index | No (GC and journal recovery only) | No (`local`/Keeper); startup only for `plain_rewritable` |

### 2.4 Background work

| | Husky | Quickwit | Loki classic | Loki dataobj | Tempo | WarpStream | ClickHouse |
|---|---|---|---|---|---|---|---|
| **Compaction** | Lazy size-tiered, then leveled "locality" on sort key | Log-scale merge (factor 10), maturity 48 h | Index only (per tenant per day) | Size-leveled K-way, K = ratio = 8 | Time window + level, 2–4 inputs, k-way by trace ID | LSM L0→L4, ~32-file triggers | Merge adjacent parts, no levels |
| **Compaction I/O** | One streamed GET per input | Full download + re-upload (or local split cache) | GET all files, PUT one | Range-GET sections, PUT merged objects | 64 × 2 MiB buffered reads → multipart, one part per row group | One GET per input, streamed | Range GETs → PUT/multipart |
| **Deletes / updates** | Not described (copy-on-write inferred) | Delete tasks rewrite mature splits | Rewrite chunks; mark + sweep after 2 h | No GC path found in code | Redaction = single-block compaction | Tombstones in metadata only; compaction drops | Mutations (hard-link unchanged columns), patch parts, lightweight delete |
| **Retention** | Table TTLs, whole time buckets | Metadata mark → GC after 32 min grace | Marker files → sweeper | Not visible | Soft delete → hard delete after 1 h | Delete queue + LIST/HEAD "deadscanner" (no bucket TTLs) | TTL in merges, or drop whole parts |

---

## 3. The shared reference architecture

Strip away the modality-specific details, and all six systems implement the same skeleton:

```
                ┌───────────────────── control / metadata plane ─────────────────────┐
                │  catalog: which immutable objects are live, their ranges/stats,    │
                │  ingest positions, dedupe state, compaction leases                 │
                │  (FDB | Postgres | Keeper | VC | objects-in-the-bucket)            │
                └───────▲──────────────────────▲──────────────────────▲──────────────┘
                   2. commit                4. swap               1. resolve
                        │                      │                      │
  producers ─► [durable buffer] ─► writers ────┤      compactors ─────┤       readers ◄─ queries
               (Kafka / WAL /     (batch, sort,│      (merge, sort,   │      (prune via catalog,
                none)              encode)     │       drop deleted)  │       range-GET, cache)
                        │                      │          │  ▲        │          │   ▲
                   1. PUT new              3. PUT new  GET old         2. range GET  │
                        ▼                      ▼          ▼  │                   ▼   │
                ┌──────────────────────── object storage (immutable bytes) ──────────┴─┐
                │  write-once objects, unique keys, never overwritten in place         │
                └──────────────────────────────────────────────────────────────────────┘
                             ▲ 5. delayed DELETE / GC (after a grace period)
```

Every system follows the same lifecycle:

1. **Write bytes first.** PUT an immutable object under a never-reused key.
2. **Then publish.** Make it visible with a small atomic metadata change.
3. **Read through the catalog.** Never discover data by listing the bucket on the hot path.
4. **Compact by writing new objects** and atomically swapping catalog pointers.
5. **Delete late.** Physically remove superseded objects only after a grace period longer than any reader's
   staleness. Reconcile orphans from crashed writers.

The interesting engineering is in *where* each system puts the catalog, *how* it makes step 2 atomic, and
*how* it keeps steps 3 and 4 cheap in requests.

---

## 4. Common patterns

### 4.1 Immutable, write-once objects with never-reused keys

Every system writes objects once and never modifies them. Keys are always unique:

- **Husky**: opaque paths stored in FDB.
- **Quickwit**: ULIDs.
- **Loki classic**: a checksum embedded in the key. **Loki dataobj**: SHA-224 of the content.
- **Tempo**: v4 UUIDs, or deterministic v5 UUIDs from the block-builder.
- **WarpStream**: allocated file IDs.
- **ClickHouse**: 32 random characters.

This choice pays off several times over:

- **Weak consistency is enough.** Only read-after-write on *new* keys is needed, which every major store
  provides. No system depends on read-after-overwrite.
- **Caches never need invalidation.** Husky says so explicitly ("if the metadata is unchanged, the data
  behind it is, too"), and so do Quickwit (keyed by split ID), Tempo (`tenant:block:name:offset:length`),
  WarpStream (`file_id, chunk`) and the ClickHouse filesystem and distributed caches.
- **Retries are idempotent** when the key is derived from content or input (Loki dataobj, Tempo
  block-builder, Loki classic chunks).
- **Updates and deletes become copy-on-write**, which reuses the compaction machinery (§4.7).

The single exception proves the rule. Tempo "renames" `meta.json` to `meta.compacted.json` as copy plus
delete, and readers are designed to tolerate both files existing at once.

### 4.2 The catalog is separate from the bytes, and visibility is a metadata commit

All six systems use the same two-step protocol: **upload, then publish**. An object is invisible until the
catalog references it. That gives crash safety for free: a writer that dies mid-flight leaves only garbage,
never a partial or corrupt view.

| System | "Publish" step |
|---|---|
| Husky | FDB transaction adds fragments (and dedupe IDs) |
| Quickwit | Postgres transaction flips Staged → Published and advances the checkpoint |
| Loki dataobj | Conditional PUT of the ToC |
| Tempo | PUT of `meta.json`, always last |
| WarpStream | `CommitFile` to the Virtual Cluster |
| ClickHouse | Rename `tmp_insert_x` → `all_1_1_0` in the metadata layer |

Quickwit adds a **stage-before-upload** step: it records the split as `Staged` *before* the PUT. Orphans
are then discoverable from the catalog, which cleans up any Staged split older than 24 hours, with no bucket
LIST needed. Husky and WarpStream, in contrast, need a reconciler or GC to find orphans (stated for
WarpStream, inferred for Husky).

### 4.3 LIST is kept off the hot path

LIST is slow, paginated, billed per 1,000 keys, and historically only eventually consistent. Every system
avoids it for reads:

- **External catalog** (Husky, Quickwit, WarpStream, ClickHouse with `local` or Keeper metadata): there is no
  need to LIST at all, except for orphan reconciliation.
- **Bucket index** (Tempo): a few ring-elected builders LIST the bucket and write one
  `index.pb.zst` per tenant. Every other component does one GET per tenant per poll.
- **Manifest object** (Loki dataobj): the ToC replaces "what exists?" with one GET per tenant per 12-hour
  window.
- **Snapshot object** (ClickHouse `plain_rewritable`): `snapshot.bin` lets read-only replicas load the whole
  layout "in a single request" instead of "tens of thousands of requests".

The one remaining LIST-dependent hot path is **Loki classic index sync**, which mitigates it with a 1-minute
LIST cache and 5-minute resync. Its successor removes the dependency.

### 4.4 Request count, not bytes, is the cost model

Every per-system document independently concludes that **object count and request count** dominate cost and
rate limits. Every system therefore has explicit mechanisms to cut PUTs and GETs.

**Fewer PUTs on write:**

- **Husky**: tenant-local shard routing, because "the number of files (objects) created is an important cost
  dimension".
- **WarpStream**: one file per Agent per flush *across all partitions*. A file per partition every 250 ms would
  cost about $50/month in PUTs per partition.
- **Loki**: moved from per-stream ~1.5 MB chunks to 512 MB multi-stream objects, cutting PUTs by 2–3 orders of
  magnitude.
- **ClickHouse**:
  - compact parts below 10 MiB (1 GiB in Cloud);
  - packed single-archive parts;
  - inline storage of tiny files inside metadata records;
  - a single PUT below 32 MiB.
- **Quickwit**: a single PUT for any object under 5 GiB, choosing fewer requests over upload parallelism.

**Fewer GETs on read:**

- Footer offsets stored in the catalog (Quickwit, Tempo) remove the Parquet/tantivy "read tail, read length,
  read footer" chain.
- Range coalescing: Loki merges gaps of 1 MiB or less and splits ranges over 8 MiB. ClickHouse keeps reading
  rather than seeking below about 4 MiB.
- Request de-duplication: Husky and Quickwit use `singleflight`-style debouncing, and WarpStream de-duplicates
  concurrent misses per chunk.
- Deep caching (§4.6).

**Fewer requests in background work:**

- Husky: lazy compaction, waiting for a few dozen inputs, is "an order of magnitude" cheaper in API calls.
- Husky and WarpStream: compaction designed around one streamed GET per input.
- `DeleteObjects` batches of 1,000 (Quickwit, ClickHouse).

### 4.5 Small writes, big reads, and compaction in between

There is a fundamental tension in every system. Freshness wants small, frequent objects, while read efficiency
and request economics want large, well-organized ones. Every system resolves it with some form of
**LSM-style compaction over immutable objects**:

- **Husky**: ~1k-row fragments grow to ~1M rows (size-tiered), then a leveled pass on the sort key narrows the
  key range per fragment.
- **Quickwit**: a log-scale merge policy targeting 10M docs, after which splits "mature" and are never
  touched again.
- **Loki dataobj**: size-leveled runs with fan-in equal to the level ratio (K = 8), so a merge of 8 runs lands
  in the next level.
- **Tempo**: groups by time window, then by level, merging 2–4 blocks at a time.
- **WarpStream**: L0→L4, with ~32-file triggers. It also changes the *layout*, from multi-partition to
  partition-local.
- **ClickHouse**: merges adjacent parts without levels, capped at 150 GiB.

Compaction is also where secondary work gets done:

- applying deletes (all six);
- dropping tombstoned or duplicate data (WarpStream, Tempo);
- moving data between storage tiers (WarpStream: S3 Express → S3 Standard; ClickHouse: `TTL … TO VOLUME`);
- building pruning metadata (Husky's pruning regexes are generated during locality compaction).

### 4.6 Layered caching, with affinity routing to keep caches warm

The shape of the cache hierarchy is nearly universal:

```
result / partial-result cache  →  metadata / footer cache  →  column / page / range cache  →  object store
   (Husky 80% hit,                 (Quickwit 500 MB footer,      (Husky RocksDB range cache 70%,
    Quickwit 64 MB,                 Tempo parquet-footer,          Quickwit split cache on disk,
    Tempo frontend-search,          Loki dataobj metadata          Tempo parquet-page,
    ClickHouse query cache)         cache, ClickHouse marks)       ClickHouse FileCache 4 MiB-aligned)
```

Caches only work if the same object goes to the same node, so every system that scales readers horizontally
uses **hash-based affinity**:

| System | Affinity mechanism |
|---|---|
| Husky | Shuffle sharding + consistent hashing on fragment ID; virtual-node ramp-up for cold nodes; load-aware secondary |
| Quickwit | Load-aware rendezvous hashing (5% slack); "new split" notifications pre-warm the future owner's disk cache |
| WarpStream | Per-AZ consistent-hash ring on `(file_id, 16 MiB extent)`; replicated |
| ClickHouse | Rendezvous hashing of files to nodes in `s3Cluster`; consistent hashing in the Cloud distributed cache |
| Tempo | Shared external Memcached keyed by range; queriers are interchangeable |
| Loki | Shared external Memcached for chunks and metadata |

The split is clear (this is our interpretation): **in-process or local-disk caches need affinity routing**,
as in Husky, Quickwit, WarpStream and ClickHouse; **external shared caches let compute stay fully
interchangeable**, as in the Grafana systems. ClickHouse Cloud's distributed cache is a hybrid: a shared
service that is itself consistently hashed.

### 4.7 Copy-on-write deletes and updates

Because objects are immutable, logical changes are implemented by **rewriting and swapping**:

- Quickwit delete tasks are a single-split merge without the deleted docs.
- Tempo redaction is a single-block compaction with a drop predicate.
- Loki classic rewrites partially affected chunks.
- ClickHouse mutations rewrite only the changed columns and hard-link the rest.

The systems that care most about update cost avoid rewriting at write time:

- **WarpStream** drops duplicates *in metadata only* ("retroactive tombstones"). Dead bytes stay in the file
  until compaction.
- **ClickHouse** **patch parts** store only the changed rows and columns, apply them at read time, and
  materialize them during merges. This is an O(changed rows) PUT instead of an O(part) rewrite.
- **ClickHouse Cloud** "on-the-fly mutations" store update expressions in Keeper.

### 4.8 Deferred physical deletion with a grace period

Readers often hold a snapshot of the catalog: an in-flight query, a stale blocklist, or a cached index. So
every system separates **logical removal** (catalog) from **physical removal** (DELETE) by a delay:

| System | Grace period / mechanism |
|---|---|
| Quickwit | 32 min default (5 min–48 h), "in-flight queries … scroll queries"; plus a 24 h window for Staged orphans |
| Loki classic | 2 h `retention-delete-delay` so gateways load the new index first |
| Tempo | `compacted_block_retention` 1 h, which must be ≥ 2 × `blocklist_poll` |
| WarpStream | `CreatedAt + DELETION_DELAY`; must also exceed the oldest restorable metadata backup |
| ClickHouse | `old_parts_lifetime` 8 min; refcount must reach 0 |
| Husky | Not described; required by snapshot isolation (inferred) |

Orphans from crashed writers get a **second mechanism**:

- Quickwit sweeps the Staged state in its catalog.
- WarpStream runs a LIST/HEAD "deadscanner" that treats the Virtual Cluster as truth.
- ClickHouse deletes "metadata first, then blobs", accepting rare orphan leaks.

Loki's data-object path and Tempo's compactor orphans have **no explicit sweeper** in the code reviewed.

### 4.9 Stateless compute with durability delegated

Durable state lives only in the object store, plus the catalog and, where used, Kafka. All compute is
stateless or holds only soft state:

- Husky writers, compactors and readers ("(almost) stateless").
- Quickwit searchers ("adding or removing nodes takes seconds and requires no data movement").
- Tempo queriers, block-builders and backend-workers.
- WarpStream Agents ("losing every Agent loses no acknowledged data").
- ClickHouse Cloud nodes ("just CPU and memory").

Loki classic ingesters (WAL) and the ClickHouse OSS `local` metadata disk are the notable stateful leftovers,
and both are being designed away.

### 4.10 Spreading key prefixes for request-rate limits

S3 rate-limits per key prefix (roughly 3,500 PUT and 5,500 GET per second). Several systems spread keys on
purpose:

- **Quickwit**: `QW_RANDOM_SPLIT_PREFIX` moves the last 4 ULID characters to the front.
- **Loki dataobj**: a 2-hex-character SHA prefix (256 prefixes).
- **ClickHouse**: a random 3-character directory (17,576 prefixes).
- **WarpStream**: a range file-ID allocator, the default since v853, plus striping across 2–32 buckets.
- **Tempo**: UUIDs naturally spread blocks under the tenant prefix.

### 4.11 Hedged requests against S3 tail latency

Several systems retry slow GETs (and some PUTs) before they time out:

- Quickwit's `TimeoutAndRetryStorage` (timeout plus bytes ÷ minimum throughput).
- Tempo's `hedgedhttp` on S3 and GCS GET clients.
- WarpStream's speculative uploads and reads, capped at 2% of requests.
- Husky's fan-out redirect to a consistently chosen secondary reader.

The tail is S3's most visible weakness. ClickHouse's cache blog cites ~500 ms at p99.99, and Husky's
per-fragment p99 is 257 ms against a 2 ms p50.

---

## 5. Modality-specific patterns

The modalities differ in *what a query asks for*. That changes the file layout, the auxiliary structures, and
which object-store operations dominate.

### 5.1 Log and event search: Husky, Quickwit, Loki

The workload is append-only, time-ordered, high-cardinality, semi-structured text. Queries are a mix of
needle-in-haystack ("find request ID X") and aggregations over recent data.

| Concern | Husky | Quickwit | Loki |
|---|---|---|---|
| **What is indexed** | Columns + optional per-term bitset postings and hashed n-grams ("segments") | Everything: full inverted index (FST term dict, postings, positions) + fast fields | Only labels (classic). Dataobj adds postings + bloom filters per column |
| **How a needle is found** | Metadata regex/zone-map prune → result cache → column metadata → segment bitsets → row-group stats | Metastore time + tag prune → term-absence cache → term dict → postings range GETs | TSDB label lookup → fetch all chunks for matching streams → brute-force scan |
| **Who pays** | Writers and compactors (sorting, building segments) | Indexers (~80% more CPU than Loki in Quickwit's benchmark) | Queriers, at query time |
| **Time partitioning** | Fragments are per time bucket | `time_range` per split in Postgres | Per-day index tables; per-12 h ToC |

Patterns specific to this modality:

- **Time is the primary pruning axis.** Every log system tags objects with a time range in the catalog and
  prunes on it before anything else.
- **Tags and labels as metadata-level filters.** Quickwit stores up to 1,000 tag values per split in
  Postgres. Husky stores trimmed-FSA *regexes* of sort-column values in FDB, which rejects values that fall
  between min and max but are absent. Loki's labels are the whole index.
- **The "index everything" vs "index nothing" spectrum.** Quickwit indexes fully and wins on request count
  (88–255 GETs per query against up to ~205k for Loki in Quickwit's benchmark). Loki classic indexes minimally
  and pays at query time. Husky sits in the middle: columnar with optional text segments. Loki's dataobj moves
  it toward the middle as well (columnar + postings + blooms).
- **Caching negative results.** Quickwit's term-absence cache ("required term proven absent per split") and
  Husky's predicate cache (cached bitsets for the most expensive predicates) exploit the fact that log queries
  repeat: dashboards, monitors, auto-refresh.
- **Very wide, sparse schemas.** Husky handles up to ~10^6 columns per fragment with a column-offset skip list
  inside the file. Loki dataobj creates one column per structured-metadata key, and deliberately keeps
  logs-section metadata out of its metadata cache because "it can grow arbitrarily large with schema width".
- **Multi-tenancy at file granularity.** Husky and Loki never mix tenants in a file. Quickwit separates by
  index and `partition_key`.

### 5.2 Traces: Tempo

The workload is spans grouped into traces. Queries are either a **point lookup by trace ID** or **TraceQL
scans** over span attributes and structure.

Patterns specific to this modality:

- **Sort by the lookup key, so statistics become the index.** Rows (one per trace) are sorted by trace ID,
  so the footer's per-row-group min/max on TraceID locates the one row group that can match. Tempo's own
  row-group index is now off by default.
- **Sharded bloom filters as tiny side objects.** One bloom filter per block is split into ≤100 KiB shards
  (1% false-positive rate), and a trace ID maps to exactly one shard. A negative lookup costs **one small GET
  per block** (usually cached), whatever the block size. This is the main object-storage trick for point
  lookups over large immutable files.
- **Nested columnar schema.** Parquet's nested model keeps resource → scope → span → event structure, and
  TraceQL iterates column-by-column, joined by row number. Only referenced columns are read.
- **Precomputed structure.** Nested-set `NestedSetLeft/Right` columns turn parent/child/descendant operators
  into interval comparisons with no tree walk.
- **Dedicated columns** for hot attributes: up to 75% lower tail latency and 70% lower querier CPU in load
  tests. These are assigned per block and recorded in `meta.json`. The cost is that blocks with different
  assignments never compact together.
- **Query-time reassembly.** A trace can be split across blocks and cycles ("hard cuts"), so combiners
  de-duplicate spans across blocks. This also makes overlapping old and new blocks during compaction safe.

### 5.3 Streaming log: WarpStream

The workload is ordered, offset-addressed append and tail-read with consumer fan-out. Latency matters in
the hundreds of milliseconds or less, and historical replay must be efficient.

Patterns specific to this modality:

- **Separate durability from ordering.** Bytes are durable when the PUT succeeds, but *order* is assigned by
  the metadata service at commit. This allows **leaderless** writes: any Agent can write any partition, and
  two files flushed in one order may be sequenced in the other.
- **Multi-partition files on write, partition-local files after compaction.** Ingestion files are optimized
  for PUT economics and latency. Compaction re-sorts for replay locality. No other system in this set mixes
  *logical streams* in a fresh file this aggressively. Loki dataobj mixes streams, but within one tenant and
  sorted.
- **Fan-out read coalescing.** The live edge is read by many consumers at once. The per-AZ ring de-duplicates
  so that each chunk is fetched about once per AZ. Going from 1× to 3× consumer fan-out raised GETs only
  ~22%, and quadrupling partitions changed PUT and GET counts not at all.
- **Prefetching sequential consumers.** Up to 64 MiB of speculative prefetch per request gives ~99% hit
  ratio for single-partition consumers.
- **The latency budget is explicit.** Produce latency ≈ batch timeout + PUT (+ quorum) + metadata commit.
  WarpStream attacks each term separately:
  - a shorter batch timeout (down to 25 ms);
  - S3 Express One Zone, with quorum across buckets;
  - asynchronous commit ("Lightning Topics": journal to the object store, ack with offset 0, sequence later).
- **Exactly-once without a leader.** The VC checks `(producer_id, partition, seq)` against the last five
  committed values at commit time, and drops duplicates from metadata only.
- **Tiering inside the object store.** The premium bucket (S3EOZ, ~7× storage price) holds only seconds of
  data before compaction moves it to S3 Standard.

### 5.4 General OLAP: ClickHouse

The workload is analytical SQL over sorted columnar data, with mutations, varied engines (Replacing,
Summing…), joins, and both point and range predicates.

Patterns specific to this modality:

- **A generic filesystem abstraction over blobs.** Unlike the purpose-built systems, ClickHouse has to
  support local disks, tiering and S3 with one engine. `DiskObjectStorage` provides POSIX-ish semantics
  (directories, rename, hard link, append) on top of immutable blobs and a metadata layer. Hard links
  (refcounts) make mutations, `FREEZE` and backups cheap.
- **Sparse primary index + marks = precise range GETs.** One index entry per 8,192-row granule (about 1,000
  entries per 8.1M rows) stays in memory. Marks map each granule to `(offset_in_compressed_file,
  offset_in_decompressed_block)`, so a query issues a ranged GET for exactly granules *i..j* of each column.
- **PREWHERE and two-phase reads.** Filter columns are read first, and the rest only for surviving granules.
  Loki dataobj (primary vs secondary columns) and Tempo (TraceQL second pass) independently reinvented this
  for object storage.
- **Many files per unit is the native problem.** A wide part with 100 columns is about 200 PUTs per INSERT on
  a `local`-metadata S3 disk. That explains the stack of mitigations: compact parts, packed parts, inline
  data, async inserts, and a 1 GiB wide-part threshold in Cloud.
- **Replication must be rethought.** ReplicatedMergeTree on S3 stores N copies. Zero-copy replication tried to
  share them with Keeper locks and stayed experimental. SharedMergeTree moved *metadata* to Keeper and keeps
  one copy of the data.
- **Lake interoperability.** The `s3`/`s3Cluster` table functions and Iceberg/Delta engines query open
  formats in place. Iceberg writes use **S3 conditional writes** (`If-None-Match` / ETag CAS) for the
  metadata pointer. This is the only part of ClickHouse that depends on object-store CAS.

### 5.5 How modality shapes object-store usage (summary)

| Modality | Dominant object-store op | Main read trick | Main write trick |
|---|---|---|---|
| Log search (index-heavy: Quickwit) | Many small concurrent range GETs | Footer/hotcache + async warmup of exact byte ranges | Large splits, single PUT |
| Log search (index-light: Loki classic) | Whole-object GET per chunk | Memcached chunk cache, massive parallel scan | Per-stream chunks (too small, hence dataobj) |
| Events (columnar: Husky, Loki dataobj) | Range GETs of column pages | Metadata pruning + result/range caches | Sort by key, columnar, tenant-local batching |
| Traces (Tempo) | Bloom GET + footer + page GETs | Sort by trace ID, sharded blooms, column pruning | Per-partition blocks, multipart per row group |
| Streaming (WarpStream) | PUT per flush + chunk-aligned GET per AZ | Distributed per-AZ cache, prefetch | Cross-partition batching |
| OLAP (ClickHouse) | Range GETs per column per granule range | Sparse index + marks + prefetch pool | Batching, compact/packed parts, inline tiny files |

---

## 6. Where the systems differ

### 6.1 Where the source of truth lives

This is the single most consequential difference. There are four families:

| Family | Systems | Strengths | Costs |
|---|---|---|---|
| **External transactional DB (self-operated)** | Husky (FoundationDB), Quickwit (PostgreSQL), ClickHouse SMT (Keeper) | Multi-object atomic commits; dedupe and checkpoints in the same transaction; no object-store CAS needed; cheap catalog queries (`list_splits` by time and tags) | A second stateful system with its own limits: FDB's 5 s transactions and 100 KB values, Postgres row counts, Keeper as the vertical bottleneck |
| **Vendor-hosted metadata service** | WarpStream (Virtual Cluster) | Very high op rate (claimed millions/s), offsets assigned at commit, tombstones as metadata, journaled and backed up to the object store | Hard runtime dependency: reads always need it, and produce does too unless Ripcord/Lightning is used. Throughput is the scaling ceiling (~400k batches/s guidance) |
| **Objects in the bucket** | Loki (TSDB files; dataobj ToC), Tempo (`meta.json` + tenant index), ClickHouse `plain_rewritable` | One dependency (the bucket); trivially portable; no database to run | Either needs LIST (Loki classic, Tempo builders) or object-store CAS (Loki dataobj). Visibility is eventually consistent with explicit staleness bounds (Tempo: 2 × poll) |
| **Local per-node metadata** | ClickHouse OSS `local` metadata | Fastest, simplest | The node is stateful; losing the disk orphans the bucket; no sharing between nodes |

An observation from comparing them: the **external-DB family puts compaction swaps, deduplication and ingest
checkpoints behind one commit point**. The bucket family has to build those guarantees from weaker parts:
deterministic IDs, CAS on manifests, and overlapping visibility windows.

### 6.2 How atomic commit is achieved

| Primitive | Used by | Notes |
|---|---|---|
| Multi-key DB transaction | Husky, Quickwit, ClickHouse SMT | Atomic swap of N inputs for M outputs, plus side state (checkpoints, dedupe IDs) |
| Metadata-service RPC | WarpStream | `CommitFile` and `ApplyCompaction` |
| **Conditional PUT (CAS) on a manifest** | Loki dataobj ToC (`If-Match`), ClickHouse Iceberg writes (`If-None-Match`/ETag) | Depends on S3's 2024 conditional-write support. Loki found minio-go silently dropped `If-Match` on its multipart path and had to force sized PUTs |
| **Single-object "commit marker" written last** | Tempo (`meta.json`) | No CAS; concurrency prevented by ownership (one builder per partition, singleton scheduler) and deterministic IDs |
| **Indirection object for atomic rename** | ClickHouse `plain_rewritable` (`__meta/<dir>/prefix.path`) | Renaming a directory rewrites one small object instead of copying every file |
| Local `rename()` | ClickHouse `local` metadata | Atomic only on that node |

### 6.3 Durability before the object store

| Approach | Systems | Implication |
|---|---|---|
| **Kafka as WAL** | Husky (input), Quickwit (source), Loki dataobj, Tempo 3.x | Object writes become replayable functions of an offset range. Ingest-time replication drops (Tempo RF3 → RF1 removed "3 to 3.5 times" storage). Kafka becomes a required dependency |
| **Replicated local WAL** | Loki classic (RF=3 ingesters), Quickwit ingest v2 (`mrecordlog` + 1 follower) | Lower freshness latency; stateful ingesters; duplicate objects to dedupe |
| **No WAL; the object store is the log** | WarpStream | Ack only after PUT (+ quorum) and commit. WarpStream *is* the Kafka replacement |
| **No WAL; write the final part directly** | ClickHouse | "Writes inserts directly to disk while other LSM-tree based stores typically use write-ahead logging." Clients must batch |

### 6.4 What goes into one object

| Mixing policy | Systems |
|---|---|
| One tenant, one stream per object | Loki classic chunks |
| One tenant table, one time window | Husky ("we never mix data from different tables in the same file") |
| One index (optionally one partition), many docs | Quickwit |
| One tenant, many streams, one window, sorted | Loki dataobj |
| One tenant, one Kafka partition, one cycle | Tempo block-builder output |
| **Many topics and partitions from one Agent flush** | WarpStream (L0) |
| One table partition, one insert | ClickHouse |

The more a system mixes, the fewer PUTs it issues. The price is a more expensive read path, which must
range-read a slice, and more complex retention (WarpStream cannot use bucket lifecycle TTLs, because one file
mixes 1-hour and 90-day topics).

### 6.5 File format and metadata placement inside objects

| | Format | Where in-file metadata sits | How the reader avoids the probe |
|---|---|---|---|
| Husky | Custom columnar, column-major, self-describing column headers | Header (sort schema, min/max row keys) + embedded column-offset skip list | Single streaming GET for compaction; range cache for reads |
| Quickwit | tantivy bundle | **Footer** (bundle metadata + hotcache), optional 16-byte trailer | Footer byte range stored in Postgres → 1 GET |
| Loki dataobj | Custom "DOBJ" | **Header**: file metadata first, then all section metadata, then all data | One read at offset 0 (16–64 KiB) resolves the layout; metadata is a cacheable prefix |
| Tempo | Parquet | Footer | `footerSize` + `size` in `meta.json` and in job requests → 1 footer GET, no tail probe |
| WarpStream | Custom | A separately readable "metadata section" | Normal reads never need it; the VC returns per-batch pointers. Used only for journal replay |
| ClickHouse | MergeTree files | Per-part side files (marks, `primary.idx`, `checksums.txt`) | Marks and PK cached in memory; marks map granule → byte offset |

Two opposite answers to "Parquet-style footer or not?":

- **Loki moved to header-first**, so the first read needs no size lookup.
- **Quickwit and Tempo kept footers** but store the footer location in the catalog.

Both reach a **one-GET open**.

### 6.6 Compaction philosophy

| Dimension | Range across systems |
|---|---|
| **Trigger** | Eager-ish (ClickHouse, Quickwit within a level) ↔ **lazy** (Husky waits for "a few dozen" inputs; ~10× cheaper) |
| **Goal** | Object count only (Quickwit, Tempo) ↔ object count **and** key locality for pruning (Husky locality compaction, Loki dataobj sort-schema runs, WarpStream partition locality) |
| **Termination** | Never stops (ClickHouse up to 150 GiB) ↔ **maturity** (Quickwit: 10M docs or 48 h; WarpStream: 128 GiB) |
| **Data path** | Full download to local disk (Quickwit) ↔ **streamed, bounded memory** (Husky one GET per input, column-at-a-time; Tempo 64 × 2 MiB buffers, multipart per row group; WarpStream streaming) |
| **Location** | Colocated with ingest (old Quickwit, Loki classic singleton) ↔ **dedicated, scheduled worker pools** (Quickwit `quickwit-compaction`, Tempo backend-scheduler/worker, Loki `pkg/engine/compactor`, WarpStream "jobs" Agents) |
| **What is compacted** | Data and index (most) ↔ **index only** (Loki classic never merges chunks) |
| **Write amplification** | ~log₁₀ (Quickwit, 2–4×) ↔ WarpStream ~2× (small) or ~4× (Storage V2, for 256× less metadata) |

### 6.7 Update and delete models

| Model | Systems | Cost |
|---|---|---|
| **No updates; delete = rewrite** | Quickwit, Tempo, Loki | O(object) per affected object |
| **Metadata-only tombstones + drop at compaction** | WarpStream | O(1) at write time; dead bytes until compaction |
| **Column-level copy-on-write with hard links** | ClickHouse classic mutations | O(changed columns × part) |
| **Delta (patch) parts merged on read** | ClickHouse lightweight UPDATE/DELETE | O(changed rows) |
| **Expression stored in the catalog, applied on read** | ClickHouse Cloud on-the-fly mutations | O(1) until materialized |
| **Unknown** | Husky ("a topic for another time") | — |

### 6.8 Read routing and caching topology

| | Cache placement | Routing | New-node warmth |
|---|---|---|---|
| Husky | Reader-local RAM + RocksDB on disk | Shuffle shard + consistent hash per tenant | Virtual-node ramp-up |
| Quickwit | Searcher RAM + optional local split cache | Load-aware rendezvous | Proactive split download via `ReportSplits` |
| Loki | External Memcached (chunks, results, dataobj metadata) | Queue (queriers pull work) | N/A (shared cache) |
| Tempo | External Memcached by role | Frontend job queue | N/A (shared cache) |
| WarpStream | Agent RAM (on-heap), replicated, per AZ | Consistent hash ring on file extent | Ring rebalancing |
| ClickHouse | Local NVMe FileCache; Cloud distributed cache per AZ | Rendezvous for `s3Cluster`; consistent hash for distributed cache | Cache warmers; `ignore_cold_parts_seconds` hides unwarmed parts |

### 6.9 Freshness: where recent data is served from

Several systems do **not** serve the newest data from object storage at all:

- **Tempo**: live-stores serve the last ~15 minutes (`query_backend_after`).
- **Loki classic**: ingesters serve `query_ingesters_within`. **Loki dataobj**: a 3-hour storage lag.
- **WarpStream**: the newest data *is* in the object store (that is the product), so it lowers latency through
  premium tiers instead.
- **Husky, Quickwit, ClickHouse**: data is visible as soon as it is committed to the catalog. Freshness is
  bounded by buffer and commit timeouts.

### 6.10 What each system demands of the object store

| Requirement | Husky | Quickwit | Loki classic | Loki dataobj | Tempo | WarpStream | ClickHouse |
|---|---|---|---|---|---|---|---|
| Read-after-write (new keys) | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| LIST consistency | — | — | Tolerated stale | — | Tolerated stale (2× poll) | Only for GC | Only `plain_rewritable`/globs |
| Conditional writes (CAS) | — | — | — | **Required** | — | — | Iceberg only |
| Multipart | Likely for compaction | >5 GiB only | Rare | Large objects | Compaction output | Inferred for compaction | >32 MiB |
| Batch delete | Not stated | ✓ (1,000/req) | Per-object | No GC found | Per-object (no `DeleteObjects`) | Per-file queue + sweeps | ✓ (1,000/req) |
| Range GET | ✓ | ✓ (core) | Whole-object for chunks | ✓ (core) | ✓ (core) | ✓ (inferred) | ✓ (core) |
| Versioning/soft-delete | — | — | — | — | — | **Must be off** | — |

---

## 7. Interesting approaches, system by system

These are the ideas most worth borrowing. Each is distinctive to its system, or done notably well there.

### 7.1 Datadog Husky

1. **Shape the ingest stream to control object count.** Deterministic, time-bounded "shard placements"
   route each tenant to a few shards, so each writer sees few tenants and produces fewer, larger files. Most
   systems fix object count afterwards with compaction. Husky also attacks it *upstream*.
2. **Dedupe state stored as data, in the same transaction.** Event-ID tables are ordinary Husky tables,
   committed in the same FDB transaction as the events. Exactly-once ingestion needs no external KV store.
3. **A file format designed for single-GET streaming merges.** Column-major layout, self-describing column
   headers and uniform row-group sizes let a compactor stream each input with **one GET**, using memory bounded
   to about one row group per input and less than one CPU's worth of memory. Row-group size adapts to the
   largest value (75 KiB log messages), and an output restarts with smaller groups if it overflows.
4. **Lazy compaction.** Waiting for a few dozen inputs instead of merging each new fragment immediately cut CPU
   and API costs by an order of magnitude.
5. **Trimmed-FSA "pruning regexes".** A per-sort-column automaton, trimmed to bound its size without false
   negatives and stored as a regex in metadata. It rejects absent values *between* min and max, which zone
   maps cannot do.
6. **The pruning funnel, measured.** Of 1,000 fragment queries, 300 were pruned by metadata, 560 answered by
   the result cache, 78 pruned by column metadata, 28 by segments, 30 served from local cache, and **4 reached
   S3**.
7. **Explicitly trading median for tail latency.** The median rose by a few hundred ms versus local SSD,
   while p95/p99/max became "dramatically lower". Partial results stream back progressively.

### 7.2 Quickwit

1. **Footer offsets in the catalog.** Opening a remote index costs exactly one ranged GET. A new 16-byte
   trailer keeps files self-describing for catalog-less tools.
2. **Record-and-replay hotcache.** At packaging time, the index is opened through a proxy directory that
   *records every read*, and those bytes are persisted in the footer. This works for any library with a VFS
   abstraction, with no hand-picking of structures to cache.
3. **Async warmup, then synchronous execution.** tantivy is synchronous and mmap-oriented. Quickwit
   precomputes the exact read set, fetches it concurrently, and then runs tantivy against memory. Synchronous
   reads *panic* by design, so a missed prefetch is caught immediately.
4. **Stage → upload → publish, with the checkpoint in the same transaction.** This gives exactly-once from
   Kafka and catalog-discoverable orphans, with no LIST.
5. **Proactive cache warming via placement prediction.** When a split is staged, its likely future searcher
   (by rendezvous hash) is told to download it, with an artificial last-access time so it does not evict
   hotter splits.
6. **Lambda offload.** When pending leaf searches pass a threshold, batches of splits go to AWS Lambda. Leaf
   compute becomes elastic without growing the cache-holding fleet.
7. **Maturity.** Splits stop merging after 10M docs or 48 hours, which bounds write amplification and makes
   cost predictable.

### 7.3 Grafana Loki

1. **A manifest in the bucket, guarded by CAS.** Per-tenant, per-12-hour ToC objects are updated with
   `If-Match`. Partitioning the manifest by (tenant, window) bounds both contention and object size.
   Compaction publishes with `ReplaceIndexPointers`, which removes the old paths and adds the new ones in one
   conditional write, and only if every old path is still present.
2. **A header-first container format.** File metadata, then all section metadata, then all data. One read at
   offset 0 resolves the layout, and the metadata region is a cacheable prefix.
3. **Explicit range-read engineering.** Coalesce gaps of 1 MiB or less, split ranges over 8 MiB, use 10-way
   parallelism, and download in ≤16 MB batches by priority (same phase, secondary, read-ahead).
4. **Two-phase column reads.** Predicate columns first; the large `message` pages only for surviving rows.
5. **Content-addressed objects** (SHA-224) with a 2-hex-character prefix fan-out. Uploads are idempotent and
   keys spread across 256 prefixes.
6. **A cache as dedupe oracle (classic).** RF=3 ingesters synchronize chunk cut points to produce
   byte-identical chunks with identical keys. Before a PUT, the chunk cache is checked as a stand-in for
   "already in S3". Cheap, but leaky.
7. **A cautionary tale.** The minio-go client dropped `If-Match` on its unknown-length multipart path. CAS on
   object stores is only as good as the client actually sending the header.

### 7.4 Grafana Tempo

1. **Deterministic block IDs from the input offset range.** v5 UUIDs over `tenant ‖ partition ‖ startOffset ‖
   seq` make every block-builder upload a replayable function of its Kafka input. A replay retires any
   previously visible copy before writing, so there are never two live copies, and no CAS is needed.
2. **The `nocompact.flg` handshake.** A zero-byte flag stops compaction from touching a block until the
   builder has committed its Kafka offset, closing the race between replay and compaction.
3. **A materialized bucket listing with explicit staleness.** A few ring-elected builders LIST and write a
   tenant index. Everyone else does one GET. The bound "stale by at most 2 × `blocklist_poll`" then *drives*
   the other settings: `query_backend_after`, `compacted_block_retention`, and searching recently compacted
   blocks.
4. **Sharded bloom filters as separate objects.** Negative trace-ID lookups cost one ≤100 KiB GET per block,
   whatever the block size.
5. **Role-tagged range caching.** Every range read is labelled (footer, column index, offset index, page) so
   operators can size caches per access pattern.
6. **A streaming compactor writing directly to multipart.** One `UploadPart` per row group, no local staging,
   bounded memory.
7. **Planning without opening files.** `footerSize`, `size`, `totalRecords` (row groups) and the
   dedicated-column map travel inside query jobs. Queriers never GET `meta.json` per job, and the frontend
   plans row-group-granular jobs without opening Parquet.

### 7.5 WarpStream

1. **Leaderless writes; order assigned at commit.** Any Agent writes any partition. The metadata service
   sequences batches when they commit, so Agent failure needs no leader election.
2. **Cross-partition batching makes PUT cost independent of partition count.** At 1 GiB/s, going from 1,152
   to 4,608 partitions did not change PUT or GET counts.
3. **"Distributed mmap".** A per-AZ consistent-hash ring over `(file_id, 16 MiB extent)` with aligned chunk
   paging and single-flight de-duplication turns the cluster's RAM into one shared page cache per AZ. It cuts
   GETs and keeps reads AZ-local, so there is no inter-AZ transfer.
4. **Retroactive tombstones.** Idempotency is checked at commit; duplicates are removed from *metadata only*.
   The bytes stay in the immutable file and are dropped by compaction.
5. **Lazy sequencing (Lightning / Ripcord).** Journal the file to the object store, ack immediately, and
   commit metadata in the background. Recovery LISTs per-Agent "sequences" and re-commits from each file's
   self-describing metadata section. This gives a ~33 ms median produce and tolerance of control-plane
   outages, in exchange for ordering guarantees and idempotency.
6. **Bucket composition as a primitive.** `warpstream_multi://` replicates with quorum (S3EOZ AZ resilience,
   multi-region RPO=0). `warpstream_stripe://` hashes across 2–32 buckets to get past per-bucket rate
   ceilings. DynamoDB and Spanner can stand in as the "bucket".
7. **Belt-and-braces GC.** An optimistic in-memory delete queue after compaction, plus a LIST/HEAD
   reconciliation scanner spread across prefixes. WarpStream also checks bucket lifecycle policies that "can
   corrupt your WarpStream clusters".
8. **Tiered storage inside the object store.** Premium buckets hold only seconds of data before compaction
   moves it to S3 Standard.

### 7.6 ClickHouse

1. **A filesystem emulated over blobs, with the directory tree in a metadata layer.** Random write-once keys
   plus `(blobs, size, refcount, inline_data)` records give hard links, atomic renames and appends on a store
   that has none of these. The same engine runs on local disk, S3, or a tiered mix.
2. **Atomic rename via an indirection object** (`plain_rewritable`). Every directory gets a random remote
   name, and its logical path lives in `__meta/<dir>/prefix.path`, so a part commit is one small PUT. A ZSTD
   `snapshot.bin` lets replicas load the full layout in one GET.
3. **Inline tiny files.** Files below a threshold live *inside* the metadata record, which removes a PUT
   for each of `count.txt`, `columns.txt` and the like.
4. **Packed parts.** Small parts become one `data.packed` archive with an internal index: one PUT, then ranged
   GETs inside it.
5. **Patch parts.** Lightweight UPDATE writes only the changed rows and columns, keyed by
   `(_part, _part_offset)` or `(_block_number, _block_offset)`. They are applied at read time by linear merge
   or hash join, and materialized during merges.
6. **SharedMergeTree's lesson.** Don't coordinate per-replica metadata over shared blobs (zero-copy
   replication). Move metadata to a consensus store and keep one data copy. Inserts no longer replay a
   replication log (~45 min queue drain at 20 nodes before), and merges scale linearly to 80 nodes.
7. **A split filesystem cache.** A separate "system" segment for small hot files (`.idx`, `.cidx`,
   `.txt`…) so large scans don't evict indexes. SLRU eviction, 4 MiB-aligned segments, write-through on insert
   and merge.
8. **Budgeted prefetch pool.** `MergeTreePrefetchedReadPool` refines ranges *before* prefetching, so pruned
   ranges are never fetched. Prefetch runs within memory and count budgets, with task stealing between threads.

---

## 8. Convergent trends

Reading the six systems side by side shows a clear direction of travel:

1. **Kafka (or Kafka-compatible) as the WAL, RF1 storage behind it.** Tempo 3.x and Loki dataobj both moved
   from replicated stateful ingesters to Kafka consumers that write object storage idempotently. Tempo notes
   that the old design stored "3 to 3.5 times" the data. (WarpStream is itself a Kafka replacement that Tempo
   lists as a compatible WAL.)
2. **Columnar everything.** Loki is moving from row chunks to columnar data objects. Husky is reported to be
   moving its custom format toward Parquet, Arrow and DataFusion. Tempo moved to Parquet in 2.x. Quickwit's
   new metrics path uses Parquet with bloom filters. ClickHouse can read and write Iceberg.
3. **Fewer, larger objects.** Loki chunks grew from ~1.5 MB to 512 MB objects, ClickHouse Cloud's wide-part
   threshold is 1 GiB, Quickwit targets ~10M-doc splits, and Husky compacts to ~1M rows.
4. **Dedicated, scheduled compaction services.** Quickwit's new `quickwit-compaction` service, Tempo's
   backend-scheduler and workers, Loki's `pkg/engine/compactor`, and WarpStream's "jobs" Agent role all
   separate compaction from ingest so it can be scaled and isolated on its own.
5. **Object-store conditional writes are entering designs.** S3 added `If-None-Match` and `If-Match` in 2024.
   Loki's ToC and ClickHouse's Iceberg writer rely on them. The older designs (Husky, Quickwit, Tempo,
   WarpStream) avoided needing them with external catalogs or ownership.
6. **Stateless compute, shared caches.** ClickHouse Cloud nodes need "just CPU and memory" with a shared
   per-AZ distributed cache. Datadog says it is looking at splitting cache storage from query compute in Husky.
7. **Per-AZ awareness as a cost control.** WarpStream's AZ-local leaders and per-AZ cache ring, and
   ClickHouse's per-AZ distributed cache, both exist to avoid inter-AZ transfer fees, which can exceed
   object-storage API costs.
8. **Spreading prefixes as standard practice.** Random prefixes, hash prefixes and striped buckets now appear
   in most systems, often added later after they hit `503 SlowDown` (WarpStream's range allocator, Quickwit's
   random split prefix).

---

## 9. Design guide: choices for a new object-storage-backed store

This section is our synthesis: the decisions the six systems show are unavoidable, with the options they
demonstrate.

| Decision | Options (and who chose them) | Choose based on |
|---|---|---|
| **Where is the catalog?** | Transactional DB (Husky, Quickwit, ClickHouse SMT) · metadata service (WarpStream) · manifest objects with CAS (Loki dataobj) · bucket index with polling (Tempo) | Need for multi-object atomicity and side state (dedupe, checkpoints) → DB. Single-dependency simplicity → bucket. Target store lacks CAS → avoid the manifest-with-CAS pattern |
| **What makes writes durable before the object store?** | Kafka (Tempo, Loki dataobj, Husky) · replicated local WAL (Loki classic, Quickwit v2) · nothing (ClickHouse, WarpStream) | Freshness target versus operational dependencies. With Kafka, derive object keys from offsets for idempotency (Tempo) |
| **How big are fresh objects, and what do they mix?** | Per-stream (Loki classic: too small) · per tenant and window (Husky, Loki dataobj) · cross-partition (WarpStream) | PUT cost per second ≈ writers × flush rate × objects per flush. Mix more to cut PUTs; then compaction must restore locality |
| **Footer or header metadata?** | Footer + offset in catalog (Quickwit, Tempo) · header-first (Loki dataobj) | Either way, aim for **one GET to open**. Never do tail-probe chains |
| **How are reads pruned before touching bytes?** | Time ranges (all) · tags and regexes in catalog (Quickwit, Husky) · blooms as side objects (Tempo) · postings in index objects (Loki) · sparse PK + marks (ClickHouse) | Put the coarsest, cheapest filters in the catalog. Push point-lookup filters into small separate objects |
| **How is compaction run?** | Lazy, streaming, one GET per input (Husky, WarpStream) · full download (Quickwit) | Make it streaming with bounded memory. Add maturity to cap write amplification. Run it as a separate scheduled pool |
| **How do deletes work?** | Metadata tombstones (WarpStream) · patch/delta parts (ClickHouse) · rewrite (Quickwit, Tempo, Loki) | Frequency of deletes and updates. Regulatory deletes need guaranteed rewrite eventually |
| **How is GC made safe?** | Grace period ≥ max reader staleness (all) + orphan reconciliation (Quickwit Staged sweep, WarpStream deadscanner) | Always do both. Don't rely on bucket TTLs if objects mix retention classes. Turn off versioning and soft-delete if you rewrite often |
| **Cache topology?** | In-process + affinity routing (Husky, Quickwit, WarpStream) · external shared cache (Loki, Tempo) · shared service with consistent hashing (ClickHouse Cloud) | Affinity gives the best hit rates and no extra hop; shared caches give interchangeable compute. Keep caches per AZ |
| **Prefix and rate-limit strategy?** | Random or hash prefixes (Quickwit, Loki, ClickHouse) · striping across buckets (WarpStream) | Plan from day one; retrofitting a key layout is painful |

**Checklist of request-count mitigations seen across the systems:**

- [ ] One GET to open any object (footer offset in catalog, or header-first)
- [ ] No LIST on any read path (catalog, bucket index or manifest)
- [ ] Batch across logical streams when writing fresh objects
- [ ] Single PUT below a generous threshold; multipart only for large objects
- [ ] Coalesce nearby ranges; split huge ones; parallelize
- [ ] Single-flight / de-duplicate concurrent identical reads
- [ ] Cache keyed by immutable identity (no invalidation)
- [ ] Route by consistent or rendezvous hash for cache affinity
- [ ] Lazy, streaming compaction with one GET per input
- [ ] Batched deletes (`DeleteObjects`, 1,000 keys) after a grace period
- [ ] Spread key prefixes
- [ ] Hedge slow requests (with a cap on amplification)

---

## 10. Caveats and open questions

- **Closed-source systems.** Husky's object key scheme, multipart use, GC protocol, codecs and FDB key layout
  are not public. WarpStream's file format, key schema and Virtual Cluster internals are not public. Claims
  about them are vendor-stated or inferred, as marked in the per-system docs.
- **Moving targets.**
  - Loki's data-object engine is the Loki 4 direction, but 3.8.0 is the latest tagged release. The dataobj
    path had **no GC of superseded objects** visible in the code reviewed.
  - Tempo's 3.x docs disagree with `main` in at least two places (the `replaces` field, the flush order).
  - ClickHouse's `plain_rewritable` docs lag the code on mutations and hard links.
  - Quickwit's architecture docs still say "UUID" where the code uses ULIDs.
- **Vendor benchmarks.** Figures are vendor-reported and use different workloads and hardware: Quickwit vs
  Loki request counts, WarpStream TCO, ClickHouse distributed cache, Husky's pruning funnel, and Loki's
  GrafanaCON 20×/10× claims. They show the *shape* of each design's trade-offs, not comparable absolute
  performance.
- **Not covered here.** Each per-system document has the full code paths, configuration defaults and source
  lists. This synthesis deliberately leaves them out.
