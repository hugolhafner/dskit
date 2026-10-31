# Datadog Husky: an event store built on object storage

> **Source status.** Husky is closed source. There is no public repository, design doc or published file-format specification. Everything here comes from Datadog's four-part engineering blog series (2022–2025), the Datadog On podcast episode, and the public FoundationDB documentation that the posts cite. Claims are tagged as follows:
>
> - **[D]** Documented: stated explicitly in a Datadog primary source (cited).
> - **[I]** Inferred: my reasoning from documented facts, from general practice in object-store-backed systems, or from the constraints of the named dependencies (S3, FoundationDB). Treat these as hypotheses.
>
> No object key formats, byte-level layouts, struct names or config defaults have been published. Wherever this document would normally give them, it says so instead of guessing.

---

## 1. Overview

**What it is.** Husky is Datadog's third-generation **event store** [D]. The authors describe it as an "unbundled, distributed, schemaless, vectorized column store with hybrid analytics/search capabilities, designed from the ground up around commodity object storage" (Artoul & Watt, 2022). It is "a distributed storage system that is layered over object storage (e.g., Amazon S3, Google Cloud Storage, Azure Blob Storage, etc.), with the query system acting as a cache over this storage" (Profeta & Talbot, 2025) [D].

**Who uses it.** Husky backs Datadog's "Event Platform" [D]:
- Log Management (including the Flex Logs tier)
- APM traces and spans
- Network Performance Monitoring
- Real User Monitoring (RUM)
- Continuous Profiler
- Other products whose events "looked almost exactly like structured logs"

A 2025 post reports **more than 100 trillion events and billions of queries per day**, with queries that span "millions of files in object store (called fragments) and petabytes of data" [D]. The 2022 post says some single queries must "search PiBs of events" [D].

**Workload it targets** [D]:
- **Writes.** Very high, bursty, append-mostly ingest. Tenant volume often bursts by "one to two orders of magnitude", with "little to no updates of existing records". Events are often several kilobytes each, and a log `message` can be up to 75 KiB.
- **Reads.** Queries lean heavily toward the newest data. There are two dominant shapes:
  - "Needle-in-a-haystack" searches: a request ID, an IP, an error string.
  - "Analytics-style" aggregations: a time series of latency over a week, or breakdowns by tag.
- **Schema.** There is no fixed schema or column types. Tenants differ wildly in event shape, and a compacted fragment may hold "hundreds of thousands (if not millions) of columns".
- **Correctness.** Ingestion must be exactly-once, because a single duplicate can flip a monitor evaluation or skew billing.

**History** [D]:
- **Gen 1.** A clustered, multi-tenant storage engine. One bad node could take down the cluster, and rebalancing made overloads worse.
- **Gen 2.** A custom clustering layer over the same single-node engine. A *Shard Router* rewrote Kafka into tenant "shards" (groups of partitions), two replicas consumed each shard, and a custom query engine fanned out across shards. Tenants were split by their data volume over the last five minutes.
- **Gen 3 (Husky).** Separates compute from storage, inspired by Snowflake and Procella. The local storage engine "has been blown apart into three different roles: Writers, Readers, and Compactors". The only stateful pieces left are FoundationDB (metadata) and S3-class blob storage (data). It took more than 18 months from first line of code to the first fully migrated product.

---

## 2. Architecture

### Components [D unless marked]

| Component | Role | State |
|---|---|---|
| **Shard Router** (carried over from gen 2) | Reads raw intake Kafka and republishes each event to a *shard* (a group of Kafka partitions, possibly spread across topics and clusters), chosen from a time-bounded placement. | Stateless; caches immutable placements in memory |
| **Sharding Allocator** | Returns, or atomically creates, the placement (list of shards) for a `(tenant, timestamp)`. Uses FoundationDB transactions. Split into isolated deployments, each with its own FDB cluster. | Stateful through FDB |
| **Autosharder / Sharding Balancer** | Adjusts each tenant's shard count from byte-throughput metrics. Rebalances hot shards by minimizing the population variance of per-shard throughput. | Control plane |
| **Writers** | Consume assigned shards from Kafka, buffer per tenant, sort, write *fragments* to blob storage, then commit them, together with event-ID dedup data, to the Metadata Service in one FDB transaction. Never contacted at query time. | Stateless, autoscaled (Watermark Pod Autoscaler on CPU) |
| **Compactors** | Scan metadata for compactable buckets, stream-merge fragments (size-tiered, then locality/LSM), upload outputs, and atomically swap them in metadata. | Stateless, autoscaled |
| **Metadata Service** | "Thin abstraction" / "lightweight frontend" over FoundationDB. It is the "strongly consistent source of truth for the set of files currently visible to each customer". It stores per-fragment metadata, including path, version, row count, time bounds, zone maps, and pruning regexes. | Stateful (FDB) |
| **Query Planner** | Entry point for queries. Resolves context (facets, index config, Flex settings), validates, throttles, optimizes, splits each query into *time-based steps*, and merges the results. | Stateless |
| **Query Orchestrator** | For each step: fetches fragment metadata, prunes with zone maps and regexes, dispatches per-fragment queries to Readers through shuffle-sharded consistent hashing, and aggregates the partial results. | Stateless; holds load-tracking soft state |
| **Reader service** ("leaf" nodes) | Executes a query against a list of fragments. Serves from the result cache, predicate cache or blob range cache (RocksDB on local disk), or else from blob storage. | "(Almost) stateless": large local caches |
| **Blob storage** | Stores fragments and segment files. S3 is named; GCS and Azure Blob are listed as supported backends. | Durable data |

### Diagram

```
                 intake
                   │
             ┌─────▼──────┐   placement lookup   ┌───────────────────┐
             │Shard Router├─────────────────────►│Sharding Allocator │──► FDB (alloc)
             └─────┬──────┘                      └───────────────────┘
                   │ events → shards[hash(event_id) % n]  (time-bounded placement)
          ┌────────▼─────────┐
          │ Kafka (shards =  │
          │ partition groups)│
          └────────┬─────────┘
                   │ consume assigned shards
             ┌─────▼─────┐  PUT fragment(s)  ┌──────────────────────┐
             │  Writers  ├──────────────────►│  Blob storage (S3 /  │
             │ stateless │                   │  GCS / Azure Blob)   │
             └─────┬─────┘                   └──▲───────────▲───────┘
   commit fragments│+ ID-table fragments         │GET(1/input)│GET / range GET
   (1 FDB txn)     │                  PUT output │            │ (on cache miss)
             ┌─────▼──────────────────────┐ ┌────┴─────┐ ┌────┴────────────┐
             │ Metadata Service           │◄┤Compactors│ │ Reader pools    │
             │ (thin layer on FoundationDB│ │stateless │ │ result cache    │
             │  - fragment list per table │ └──────────┘ │ predicate cache │
             │    & time bucket           │  atomic swap │ blob range cache│
             │  - zone maps, regexes      │              │ (RocksDB, disk) │
             │  - ID-table versions)      │              └────▲────────────┘
             └─────▲──────────────────────┘                   │ per-fragment queries
                   │ fetch fragment metadata                  │ (shuffle shard +
             ┌─────┴──────────────┐                           │  consistent hash)
             │ Query Orchestrator ├───────────────────────────┘
             └─────▲──────────────┘
                   │ time-based steps
             ┌─────┴──────┐
             │Query Planner│◄── API / UI / monitors (streamed partial results)
             └────────────┘
```

The 2022 post shows that Reader pools are decoupled from storage: "both Reader pools 1 and 2 have access to both metadata stores and blob stores" [D]. Queries can therefore be isolated by product, by tenant, or by origin. For example, automated monitor queries run separately from human queries, independently of how ingestion is sharded.

---

## 3. Data model and on-object format

### Logical model [D]
- **Event.** A row with a timestamp and an arbitrary set of attributes (schemaless). Values can include DDSketches, which are re-aggregated at query time.
- **Table.** Each tenant gets dedicated tables; Husky "never mix[es] tables in one file". Separate **ID tables** hold event IDs for deduplication, one set per shard ID, partitioned into files by time.
- **Bucket.** The pair *(table, time window)*. Each fragment holds events for exactly one time window of one table. Compaction only merges within a bucket.

### Fragment (the data file) [D]
- A **fragment** is a single object in blob storage containing a subset of one table's events for one time window.
- The format is columnar and "works similarly to Parquet". Its layout:
  - **Concatenated whole columns.** A fragment is a concatenation of entire columns, and each column is preceded by a **column header**. The header lets a compactor stream a fragment from **one GET**, discovering columns as bytes arrive. This differs from Parquet, whose footer must be read before the column chunks.
  - **Row groups.** Inside each column, data is split into **row groups**. The posts call these "similar to pages in Parquet terminology" and also describe the layout as "one row group and many pages". Every column in a fragment uses the **same fixed row count per row group**, so rows align across columns by row-group index.
  - **Row-group metadata.** Per-column min/max values, which let the scan skip a whole row group.
  - **Column offset skip list.** Fragments "embed a skip list of column position offsets … to quickly find specific columns when serving queries". With hundreds of thousands to millions of columns, a flat index would be costly, and the skip list allows a logarithmic seek to a column's byte offset.
  - **Header / fragment-level metadata.** The **sort schema** used, the **min and max row keys**, and column existence and value-level metadata such as min/max or "explicit value lists, if they're small enough".
- **Sizes** [D]:
  - Writer output is small: "a maximum of a few thousand events", or "thousand-row-size".
  - Size-tiered compaction raises this to "roughly one million rows".
  - Compaction "more often target[s] millions of events in a fragment".
  - Query-time row groups hold "just a few hundred rows", small enough to fit in CPU caches.
  - Row group size per output file is chosen dynamically (see §6).
  - No byte-size targets for fragments or row groups have been published.
- **Encoding and compression.** Not documented in detail. The posts say that keeping similar events close together "improves compression", and that the reader uses vectorized operators over value lists [D]. Dictionary, RLE and general-purpose block compression are likely [I], but nothing confirms which codecs are used.

### Segments (text-search sidecar files) [D]
- Text search is served by **segments**: "additional files … attached to fragments". The term "files" suggests separate objects [I on whether they are separate objects or embedded regions].
- A segment holds two kinds of index:
  - **Term posting lists.** Each term maps to two posting lists stored as **bitsets**: one for "standard text fields" (`message`, title, `error.stacktrace`) and one for "all other fields", which supports full-text search across every attribute.
  - **Hashed n-gram posting lists** for wildcard queries. The examples use 4-grams. N-grams are stored **by hash** to bound segment size, accepting false positives, and a configurable knob caps the number of n-grams per segment.
- At query time, boolean text predicates are rewritten into row bitsets. For example, `(login AND error) OR unavailable` becomes `row in [1,3] ∧ row in [0,1] ∨ row in [5]`, which reduces to `row in [1,5]`.

### Sort schema and row key [D]
- Each table has a **sort schema**: zero or more columns plus `timestamp`. The logs default is `service,status,env,timestamp`, chosen by analyzing product query patterns.
- Writers concatenate the sort-column values into a **row key**, sort the rows by it, and record the schema and the min/max row key in the fragment header. All fragments maintain this lexicographic ordering invariant.

### Pruning regex (stored in metadata, not in the object) [D]
- For each sort-schema column, the writer or compactor builds a **finite-state automaton** that accepts every value in that column of the fragment.
- The automaton is **trimmed** to limit its number of state transitions while preserving "no false negatives". It is then mechanically converted to a **regular expression**, which is stored in the **fragment metadata in FoundationDB**. Example: `^(compactor|reader|writer)$`.
- This behaves like a compact, superset-matching bloom filter. Unlike a min/max range, it can exclude `service:metadata` even though that value falls lexically inside `[compactor, writer]`.

### Fragment metadata record (in FDB) [D for fields, I for structure]
Documented fields: **file path**, **fragment version**, **row count**, **timestamp boundaries**, **zone maps**, **pruning regexes**, and min/max row keys. The metadata is partitioned by time so that a scan returns only the fragments intersecting a query's time range. The FDB key schema is unpublished. A likely shape is `(tenant/table, time bucket, fragment id) → metadata blob`, which turns time-range lookups into FDB range reads [I].

---

## 4. Write path

### Routing and sharding upstream of storage [D]
1. **Placements.** A placement maps a tenant to a list of shards for a time interval ("usually just a few minutes"). Placements are **immutable**, never overlap, and always map the same `(tenant_id, timestamp)` to the same shards. The Sharding Allocator creates them with an FDB check-and-create transaction, and routers cache them.
2. **Event routing.** Within a placement, an event goes to `shards[hash(event_id) % num_shards]`. A given event ID and timestamp therefore always lands on the same shard, so **deduplication is shard-local**.
3. **Shard selection.** Consistent hashing `H(tenant_id [+ salt], totalShards)` picks a first shard, then the next n−1 consecutive shards, skipping excluded ones. Changes to the shard count or to exclusions move as few shards as possible. The per-tenant salt lets the balancer move a tenant.
4. **Autosharding.** Tenant cost is measured mainly in bytes, which predict ingest cost better than event counts. The system scales up fast and down slowly. Overloaded shards can be excluded from future placements so they drain.

Why this matters for object storage [D]: because tenants never share a file, "the number of files uploaded … scales almost linearly with the number of tenants a Writer handles", and each file costs money. Routing gives locality, so one Writer sees a small number of tenants per shard and produces fewer, larger objects.

### Writers [D]
- **Inputs.** A Writer consumes its assigned shards from Kafka and buffers events **per tenant (table)** in memory. Buffering serves two goals: avoiding tiny objects and keeping the "time any event can wait" bounded. Each buffer flushes as **one fragment per buffer** when the time cap is reached. A size cap is likely as well [I].
- **Before upload.** Events are sorted by the sort-schema row key. The Writer computes header metadata (min/max row key, column info) and pruning automata, and builds segments for text search [D for sort and header; I that writers also build segments; the posts don't say who builds them].
- **Upload.** The Writer uploads the fragment to blob storage. It also writes **ID-table fragments** containing the event IDs it just persisted.
- **Commit.** In a **single FoundationDB transaction**, the Writer commits the new data fragments and the ID-table updates to the Metadata Service. "An event exists if it has been committed there, and does not if it hasn't."
- **Design goals.**
  - Stateless and autoscalable.
  - **Minimize the number of files uploaded**, for three reasons: per-object cost, avoiding bucket **rate limiting**, and reducing compactor and query load.
  - Minimize disruption on restart or deploy.

### Exactly-once and deduplication [D]
- Event IDs are kept in memory and paged in lazily, by time interval, from the Husky ID tables, with LRU eviction. Writers can page in IDs about 100× faster than they ingest events. Because data arrives roughly in time order, real page-in volume is "several orders of magnitude lower" than ingest.
- **Conflict detection.** Updates to ID tables are versioned using **optimistic concurrency in FDB**, and out-of-order updates are rejected. On conflict, the Writer discards its ID cache and **resets consumer progress to the latest committed fragment**. Kafka offsets are therefore effectively tied to committed fragments [I on exact mechanism: offsets or the last-committed position are likely stored in the same FDB transaction].
- **Rejected alternatives.** Point lookups in Husky itself, RocksDB on Writers (which would break statelessness), Redis or Cassandra (which would create two sources of truth), and write-then-dedup-at-query/compaction. The last was rejected because data is rarely rewritten and writes greatly outnumber queries.

### Durability and consistency [D/I]
- **Durability.** Data is durable once the object PUT succeeds and the FDB commit lands. Until then, Kafka is the durable buffer [I, implied by the consumer-reset behaviour].
- **Visibility.** Visibility is decided only by FDB metadata, never by listing the bucket.
  - An object uploaded but never committed is invisible, and is an orphan to be garbage-collected [I].
  - Commits are strictly serializable, so a reader sees a table either before or after a commit or compaction swap [D].
- **Freshness.** Freshness is bounded by the Writer buffer time cap plus upload and commit latency [D qualitatively; no number published].

---

## 5. Read / query path

1. **Planner.** Resolves context and throttles. It uses statistics to split the query into **time-based steps**, such as slices of a seven-day query, and schedules each step on an orchestrator [D].
2. **Metadata fetch and pruning (orchestrator ↔ Metadata Service)** [D]:
   - **Time pruning.** Fragments are partitioned by time in metadata, so only fragments in buckets overlapping the step are returned.
   - **Regex / zone-map pruning.** Pruning regexes and zone maps are evaluated against the query predicates. This removes up to 60% of downstream work for structured events, about 30% on average. In Datadog's 1,000-fragment sample, 300 fragments were pruned here.
   - **Transaction length.** FDB transactions are limited to 5 seconds, and the Metadata Service preserves atomicity regardless. Large scans therefore probably use multiple transactions at a pinned read version, or paginate metadata with a version check [I]. The post states the goal but not the mechanism.
3. **Dispatch** [D]:
   - Per-fragment queries go to Readers through **shuffle sharding**: each tenant gets a subset of Reader nodes whose size is inferred from usage, and **consistent hashing on fragment ID** within that subset gives cache affinity.
   - Overloaded nodes are skipped unless they can answer from cache. The fragment then goes to a "consistently chosen secondary node".
   - Load is learned from reader responses and a background discovery loop, with no central coordinator.
   - Fan-out per query is bounded. One example shows 5 parallel fragment queries out of 8.
4. **Reader execution, in cascade order** [D]:
   - **Result cache** (fragment-level, in memory and on disk): about **80% hit rate**. Hits come from UI auto-refresh, repeated monitors, and dashboard widgets that share filters.
   - **Column metadata.** If a column is absent, or the value falls outside min/max or the explicit value list, the reader returns empty immediately.
   - **Segment posting lists and predicate cache.** These can reduce a query to `NO_MATCH`, or to `MATCH_ALL` for `COUNT(*)`.
   - **Data read.** Columns are read through the **blob range cache**, which holds RocksDB chunks on local disk with about a **70% hit rate**. Only misses issue blob GETs.
   - **Volcano-style execution** (`open/next/close`). The scan yields **lazy row-group references**. Row-group min/max skips groups, a cost-based optimizer evaluates the cheapest predicates first, and columns that are never needed are never decoded or fetched. Execution is vectorized over small row groups.
5. **Results** [D]. Partial aggregates stream back to the orchestrator, then the planner, then the client. Clients poll a job-like interface for progressively complete partial results. Retries rely on checkpointing to avoid double-counting.

**Pruning funnel (Datadog sample of 1,000 fragment queries)** [D]: 300 pruned in metadata → 700 sent to readers → 560 answered from the result cache → 78 pruned by column metadata → 28 pruned by other caches (segments, predicate cache) → **34 read data**, of which **30 came from local disk cache and 4 from blob storage**. Overall, 3.4% scan real data, **0.4% touch blob storage**, and on average only about **0.6% of underlying data** is scanned.

**Latency** [D]: per-fragment reader latency is p50/p75 ≈ 2.01 ms, p90 ≈ 6.04 ms, p95 ≈ 19.93 ms, p99 ≈ 257 ms, max ≈ 12.8 s, measured over 6.44 trillion points. Compared with gen 2, Husky's median is "a few hundred milliseconds" higher because of the "latency floor" of remote storage, while p95/p99/max improved by seconds to hundreds of seconds.

---

## 6. Data manipulation

### Compaction goals [D]
Compaction balances four concerns:
- **Fewer fragments.** Fewer GETs and fewer FDB metadata rows.
- **Less compaction work.** Less CPU and fewer PUT/GET requests.
- **Fragment size for queries.** Large enough to amortize object-store latency and vectorized scanning, small enough to keep parallelism high.
- **Layout.** Clustering similar data, which helps pruning and compression but costs more merge work.

### Lazy compaction of fresh data [D]
Compaction of recent data waits until "a few dozen fragments are available to merge". Merging eagerly would mostly join one big accumulating file with one tiny new one. Waiting is "cheaper in CPU and object store API costs—by an order of magnitude". When "the probability of receiving more data becomes low", a presumably final compaction produces the long-term layout. Compactors track "which buckets received new data recently". Late or old data is handled the same way: "just another bucket with modified data to be compacted".

### Streaming k-way merge [D]
The scaling criteria for a single compaction:
1. **One GET per input fragment**, with all inputs streamed in parallel.
2. Saturate the CPU.
3. Use **fixed memory**, less than one CPU's share of node memory.

The merge has two passes:
1. Read the **sort columns** from all inputs and build the global output order (a permutation).
2. Stream **each column**, one column at a time and one row group at a time, from all inputs simultaneously, writing rows in the global order.

Memory is bounded to about one row group per input. Because the column headers sit inline, a single sequential GET per input suffices, as long as output columns are written in the same order the inputs present them [I: the order must be consistent across fragments, such as sorted column names, for single-pass streaming to work; otherwise the merge would need to buffer or re-request]. Output can be one fragment, or several fragments that are **disjoint in sort-key space**.

### Row-group sizing [D]
The row-group row count is chosen per output file from the maximum value size of the heaviest column among the inputs. Example: 75 KiB log messages × 15,000 rows is more than 1 GiB. If, during writing, a row group exceeds the maximum, the compactor **restarts the whole output with a smaller row-group size**. This trades occasional rework for strict memory bounds.

### Size-tiered then locality (LSM) compaction [D]
- **Size-tiered.** Fragments are merged in exponentially increasing size classes, from about 1k rows to about 1M rows. Each step is: PUT the new fragment, then **one atomic FDB transaction** removes the inputs and inserts the output.
- **Locality compaction.** A best-effort LSM:
  - Size-tiered outputs enter **level 0**, and each level has a row limit that grows exponentially.
  - Overlapping fragments, judged by min/max row key, are k-way merged into non-overlapping fragments. When a level overflows, fragments are promoted to the next level.
  - Fragment size stays constant, so higher levels are lexically narrower and therefore much easier to prune.
  - It was configured to use no more CPU than size-tiering alone, and allowed a **30% reduction in the Reader pool**, the most expensive component.
- Throughput is "thousands of fragments and … dozens of GB of data every second" [D].

### Retention, deletion, GC [mostly I]
- **Retention.** Tables, including ID tables, support **TTL** [D]. Data is bucketed by time window and "we know … we will have to delete it eventually" [D]. Retention is therefore most likely enforced by deleting whole buckets: dropping their fragment metadata in FDB, then deleting objects asynchronously [I]. Bucket lifecycle rules keyed on time-based prefixes are a possible alternative, but are undocumented.
- **Deletes and updates.** The 2025 query post mentions that "updates, deletions, and background compactions happen (a topic for another time)" [D]. The mechanism has not been published. A likely design is a metadata-level fragment-version bump with rewrite-on-compaction, consistent with "fragment versions" being part of the metadata [I].
- **Object GC.** After a compaction commit, input objects are no longer referenced. They must outlive any in-flight query that already holds the old fragment list, because metadata immutability guarantees "if the metadata is unchanged, the data behind it is, too". Physical deletion is therefore presumably delayed by a grace period, with orphan sweeping for uploads that were never committed [I].
- **Schema evolution.** Husky is schemaless [D]. New attributes simply become new columns in new fragments. Compaction unions column sets, and absent columns are a pruning signal at query time [D for the pruning signal]. Type conflicts across fragments are handled, but the details are unpublished.

---

## 7. Object storage interaction patterns (the key section)

The following is documented [D]:

| Aspect | What Husky does |
|---|---|
| **Backends** | Generic blob store: S3 (named), GCS and Azure Blob (listed as examples). |
| **Unit of storage** | One immutable object per fragment, holding one tenant table and one time window. Segments are "additional files attached to fragments". |
| **Object size** | Writer objects hold about 1k rows, a few thousand events at most. Compacted objects hold about 1M or more rows. With multi-KB events, compacted objects are likely hundreds of MB to GBs [I], but no byte targets have been published. |
| **Writes** | Object-per-flush PUTs from Writers; object-per-merge-output PUTs from Compactors. Objects are never modified in place. Writers explicitly minimize object count, for cost and for **request-rate limits on the bucket**. |
| **Reads (compaction)** | **Exactly one GET per input fragment**, streamed end to end, possible because column headers are inline. Inputs are read in parallel. |
| **Reads (query)** | Column- and row-group-selective reads through the **blob range cache**, which caches "chunks of data retrieved from the storage service". Readers locate columns with the embedded skip list of column offsets, so reads are partial. Blob storage is "slow (at the scale of interactive queries) and expensive", and "issuing multiple GETs on each [fragment] for every query isn't viable". Only about 0.4% of fragment queries reach blob storage. |
| **Request dedup** | Singleflight-style coalescing of concurrent reads for the same range on a node. |
| **Cache bypass** | Oversized reads, or reads arriving while local disk I/O is saturated, skip the cache and go straight to the blob store. |
| **Catalog** | **External**: FoundationDB, through the Metadata Service. File path, version, row count, time bounds, zone maps and pruning regexes all live there. The query path discovers fragments without touching the object store. |
| **Atomic commit** | PUT the objects, then one FDB transaction that makes new fragments visible and old ones invisible. Writers commit data and dedup-ID fragments in the same transaction. |
| **Consistency assumptions** | Only "store and don't lose these bytes" is delegated to S3 and FDB. Visibility and ordering come from FDB's strict serializability, not from the object store. |
| **Cost levers named** | Object count (PUT cost and per-object cost), GET count per query, and compaction API calls. Lazy compaction cuts these "by an order of magnitude". |

The following is inferred [I], with reasoning:

- **LIST avoidance.** The catalog is in FDB, so the hot path almost certainly never LISTs. LIST is likely confined to orphan GC or audit sweeps. This follows the standard pattern; it is not stated.
- **HEAD avoidance.** Fragment metadata already stores what a HEAD would return, plausibly including size, so query-path HEADs are unnecessary [I].
- **Range GET shape.**
  - A query needs a few columns out of possibly millions. The reader probably first reads a fragment-level header or skip-list region, then issues range GETs for the needed column extents or row-group extents.
  - The blob range cache stores those "chunks". Chunks are likely aligned to fixed-size blocks so that partial overlaps hit the cache, though no chunk size has been published.
  - This differs from compaction's single whole-object GET.
  - Columns are stored whole and contiguous, so a projection of k columns costs about k range requests, or fewer when adjacent ranges are coalesced. Row-group min/max metadata is presumably inside each column, so skipping row groups saves decode work more than it saves I/O.
- **Multipart upload.** Compacted outputs of hundreds of MB to GB are written as streams, with output size unknown in advance and restartable. That strongly suggests streaming multipart uploads for compactor outputs and single PUTs for small Writer fragments [I].
- **Key layout.** Unpublished. Bucket rate limits are a stated concern, so keys likely start with high-entropy prefixes such as a hash or fragment UUID rather than a pure `tenant/time/` prefix, to spread S3 partitions [I]. Keys are opaque to queries, since paths come from FDB.
- **Hedged requests.** Not documented for blob GETs. The documented mechanisms for tail tolerance are streaming partial results, secondary-node rerouting, and caching [D]. Whether blob GETs are hedged is unknown.
- **Write amplification.** Each byte is written once by a Writer, then rewritten by roughly log(1000) size-tiered merges plus locality levels. This is classic LSM write amplification, paid deliberately to cut GETs and Reader CPU.
- **FDB limits.** FDB caps transactions at about 10 MB and 5 s. That pushes the design toward small per-fragment metadata rows: compact regexes trimmed to bounded transitions, zone maps rather than full indexes, and large indexes kept in object-side segments. The automaton trimming is explicitly documented as limiting transitions [D], which fits this explanation.

---

## 8. Performance

How Husky achieves low latency and high throughput despite its object-store floor [D]:

1. **Avoid work first.** The ladder runs: time partitioning → FDB regex and zone-map pruning → result cache → column-existence and value metadata → segments and predicate cache → local disk cache → blob. Only about 0.4% of fragment queries touch the blob store.
2. **Result cache.** Per fragment, persisted to disk, about 80% hit rate. It is cheap to keep correct because fragments are immutable: unchanged metadata means unchanged data, so no invalidation is needed.
3. **Blob range cache.** On RocksDB on local disk, about 70% hit rate. It uses singleflight dedup and bypasses itself under disk saturation.
4. **Predicate cache.** Only about 3% hit rate, but efficiency is about 11–15×: each cached bitset saves about 10× the CPU spent building it. It caches the top N% most expensive predicates per fragment, refreshed in the background. Typical hits are RBAC filters, dashboard template variables, and progressively refined queries.
5. **Cache affinity routing.** Consistent hashing on fragment ID within a tenant's shuffle shard. New nodes get fewer virtual nodes so they warm up gradually, and node weights account for differences in CPU.
6. **Lazy, cost-ordered, vectorized execution.**
   - Row groups of a few hundred rows fit in CPU caches.
   - Lazy row-group references mean columns ruled out by cheaper predicates are never decoded or fetched.
   - The compiler can auto-vectorize simple operators.
7. **Sorted, clustered layout.** The sort schema plus locality compaction makes fragments lexically narrow and therefore prunable, and also improves compression.
8. **Text indexes.** Bitset posting lists plus hashed n-grams turn boolean text search into bitset algebra.
9. **Parallelism and streaming.** Queries are split by time and fanned out per fragment. Partial results stream back, so the long tail (p99 about 257 ms per fragment) doesn't block the bulk of results; about 90% of the data is visible after 1 s in the documented example.
10. **Compaction throughput.** Single-GET streaming, CPU saturation, and fixed memory per merge.

Future direction [D]: adopting Apache Arrow, Parquet, Substrait and DataFusion, and "decoupling query compute from cache storage", which would move caches out of Reader nodes.

---

## 9. Scaling

- **Stateless compute everywhere** [D]:
  - Writers, Compactors, Planners and Orchestrators are stateless and autoscaled. Writers use the Watermark Pod Autoscaler on CPU.
  - Readers are "(almost) stateless": their caches are soft state, warmed through consistent hashing and vnode ramp-up.
  - The only stateful systems are FDB and the blob store.
- **Independent scaling of ingest, storage and query** [D]:
  - Query compute is decoupled from tenant data volume. A small tenant's one-year query can "burst" across many Readers.
  - A large tenant on Flex Logs can be given little compute.
- **Ingest partitioning** [D]:
  - Time-bounded immutable placements, consistent-hash shard selection, and an Autosharder driven by bytes.
  - A variance-minimizing balancer with capped moves per run.
  - Shard exclusion to drain hot spots.
  - Shards abstract over Kafka partitions, so Kafka can scale or fail over independently.
- **Metadata scaling** [D]:
  - FDB was chosen for horizontal scalability and strict serializability.
  - The Sharding Allocator runs in multiple isolated deployments, each with a dedicated FDB cluster.
  - The 2022 diagram shows multiple metadata stores and multiple blob stores accessible to multiple reader pools. Metadata is therefore sharded across several FDB clusters, probably by tenant or product [I on the exact sharding key].
- **Multi-tenancy and isolation** [D]:
  - Per-tenant tables, with no mixed files.
  - Shuffle-sharded reader pools with per-tenant shard size.
  - Separate reader pools per product, tenant or origin, so monitors are isolated from humans.
  - Throttling in the planner.
  - Load-aware rerouting.
- **Bottlenecks and limits** [D/I]:
  - **Object count.** This is linear in tenants per Writer [D]; locality routing and buffering are the mitigations.
  - **Bucket request rates** [D].
  - **FDB.** The 5 s transaction limit [D], plus metadata volume: millions of new fragments per day, each with regexes and zone maps. That makes compaction necessary for metadata size, not just for queries [D: "number of fragments tracked by the metadata service is reasonable"].
  - **Reader cache capacity.** Pools are sized "vertically, with ample storage and disk to make caching effective" [D]. Reader cost is the dominant cost of the system [D].
  - **Compaction CPU and write amplification** [I].

---

## 10. Trade-offs and lessons

**Notable design choices**
1. **External transactional catalog (FDB) plus immutable objects.** This one choice gives atomic compaction swaps, exactly-once ingest (data and dedup IDs in one transaction), no LIST on the hot path, and trivially correct caching. The cost is a second distributed system to operate, plus FDB's 5 s and 10 MB transaction limits, which shape metadata size.
2. **Custom streaming columnar format instead of Parquet.**
   - Inline column headers allow single-GET streaming merges with bounded memory.
   - A skip list of column offsets handles millions of sparse columns.
   - Parquet's footer-first layout and per-row-group column chunks suit these needs less well.
   - The cost is a proprietary format and ecosystem lock-in, which Datadog is now unwinding toward Arrow, Parquet and DataFusion.
3. **Per-tenant files and routing locality.** This is the right choice for isolation and pruning, but it multiplies small objects. The whole Shard Router, placement and autosharding apparatus exists partly to keep object counts reasonable.
4. **Dedup on write, not on read.** This is justified by "write volume ≫ query volume" and by data that is rarely rewritten. The ID tables reuse Husky's own storage, compaction and TTL machinery.
5. **Lazy compaction.** It trades a bit of query cost on fresh data for about 10× fewer compaction API calls and less CPU.
6. **Hybrid size-tiered plus best-effort locality LSM.** This paid for itself through a 30% reduction in Readers.
7. **Pruning regex in metadata.** A compact, no-false-negative membership summary stored per fragment in the catalog, which beats min/max ranges for low-cardinality tags.
8. **Cache-centric reads.** Datadog says it outright: "the query system act[s] as a cache over this storage". Median latency rose by a few hundred ms versus local SSDs, but tail latency improved dramatically.

**Weaknesses and risks**
- Performance depends heavily on cache hit ratio and routing affinity. Cold caches after scale-out, or query patterns with poor locality, fall back to the object-store floor.
- Write amplification from multi-level compaction.
- Metadata volume and FDB operations at the scale of "millions of fragments per day".
- Streaming results complicate retries and require checkpointing.
- Limited public detail on deletes and updates, retention GC, and the exact range-read strategy.

**Transferable lessons for a new object-store-backed system**
- Put the catalog in a strongly consistent transactional store. Make object visibility a metadata commit, never rely on LIST, and keep objects immutable so every cache needs no invalidation.
- Design the file format for **both** access patterns:
  - Sequential single-GET streaming for compaction, with headers inline and bounded memory per row group.
  - Random partial reads for queries, with an offset index and column-major layout.
- Treat **object count** as a first-class cost and rate-limit dimension: buffer per tenant, route for locality, and compact lazily.
- Store small, no-false-negative pruning summaries (regex, automata, zone maps) **in the catalog**, so most fragments are eliminated before any GET.
- Layer caches from coarse to fine (result, then predicate bitset, then byte range), route with consistent hashing for affinity, ramp new nodes slowly, and singleflight duplicate fetches.
- Choose row-group sizes adaptively from the heaviest column, so memory stays bounded.
- Separate read pools by workload class (monitors versus humans, tenant shuffle shards), independently of ingest sharding.

---

## 11. References

Primary sources (Datadog engineering blog; Markdown versions fetched 2026-10-31):
1. Richard Artoul, Cecilia Watt. "Introducing Husky, Datadog's third-generation event store." 2022-05-17. https://www.datadoghq.com/blog/engineering/introducing-husky/
2. Daniel Intskirveli, Cecilia Watt. "Husky: Exactly-once ingestion and multi-tenancy at scale." 2023-02-22. https://www.datadoghq.com/blog/engineering/husky-deep-dive/
3. Damien Profeta, George Talbot. "Husky: Efficient compaction at Datadog scale." 2025-01-29. https://www.datadoghq.com/blog/engineering/husky-storage-compaction/
4. Sami Tabet. "Inside Husky's query engine: Real-time access to 100 trillion events." 2025-10-01. https://www.datadoghq.com/blog/engineering/husky-query-architecture/
5. Datadog On podcast: "Datadog on Building an Event Storage System" (Guillaume Duranceau, Ryan Worl; host Ara Pulido). 2022-12-13. https://datadogon.datadoghq.com/episodes/datadog-on-building-an-event-storage-system (episode page only; no technical transcript was available).

Secondary and supporting sources:
6. InfoQ. "Datadog Husky data ingestion" (2023). https://www.infoq.com/news/2023/06/datadog-husky-data-ingestion/
7. FoundationDB known limitations (5 s transaction limit and transaction size limits), cited by [4]. https://apple.github.io/foundationdb/known-limitations.html
8. FoundationDB consistency and simulation testing, cited by [1]. https://apple.github.io/foundationdb/consistency.html and https://apple.github.io/foundationdb/testing.html
9. AWS Builders' Library. "Workload isolation using shuffle-sharding," cited by [4]. https://d1.awsstatic.com/builderslibrary/pdfs/workload-isolation-using-shuffle-sharding.pdf
10. Alibaba Cloud. "An in-depth discussion on the LSM compaction mechanism," cited by [3] for its hybrid tiered and leveled terminology. https://www.alibabacloud.com/blog/an-in-depth-discussion-on-the-lsm-compaction-mechanism_596780
11. Datadog Flex Logs announcement, cited by [1]. https://www.datadoghq.com/blog/flex-logs/
12. Snowflake (SIGMOD 2016) and Procella (VLDB 2019) papers, cited by [1] as inspiration.

Code: **none available.** Husky is proprietary; no repository was cloned and no commit SHA applies. Every implementation-level statement above marked [I] is inference and should not be read as a description of Datadog's code.
