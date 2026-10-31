# WarpStream: Kafka-Compatible Streaming on Object Storage

> Research note on how WarpStream stores, retrieves and manipulates data with object storage (S3 / GCS / Azure Blob) as its only durable data backend.
>
> **Source status, read this first.**
> - WarpStream's server ("Agent") and control plane are **closed source**. Confluent acquired WarpStream Labs in September 2024. The only public code used here is the official Helm chart repository, `github.com/warpstreamlabs/charts`, at commit `0ac68e6be68375457f236189a7e34d0300c518d8` (chart `warpstream-agent` version `1.0.104`, `appVersion: v852`, `appVersionStable: v842`). It holds deployment and configuration facts only, no storage-engine code. `warpstreamlabs/terraform-provider-warpstream` (commit `2b45791a1dace60bcbc718f48fa6c1754a2d4ee1`) was cloned and checked, but it contains only control-plane CRUD and nothing on storage.
> - From this research session, `www.warpstream.com`, `docs.warpstream.com`, `medium.com` and `news.ycombinator.com` **could not be fetched directly** because the hosts were outside the session's allowed network domains. Facts from WarpStream's blog and docs came through web-search result excerpts of those primary pages. Those excerpts quote the pages, but they are not a full reading. Anyone extending this note should read the original posts listed in the References, and the hosts above must be allowed for that.
> - Labels used below: **[Docs]** = WarpStream docs page; **[Blog]** = WarpStream engineering blog / CTO statements; **[Code]** = Helm chart source at the SHA above; **[3P]** = third-party write-up; **[Inferred]** = my reasoning, not stated by a source.

---

## 1. Overview

WarpStream is an Apache Kafka protocol-compatible streaming system. It replaces Kafka brokers, their local disks and their inter-broker replication with:

1. **Stateless "Agents"**, which are Go binaries in the customer's VPC that speak the Kafka wire protocol. They write all data straight to an object storage bucket in the customer's account. **[Blog]** ("Kafka is dead, long live Kafka", Jul 2023)
2. **A hosted control plane / metadata store**, run by WarpStream (now Confluent), that orders data, assigns offsets and stores consumer-group offsets. **[Docs]** (Architecture)

The deployment model is "BYOC" (Bring Your Own Cloud). Raw record data never leaves the customer's bucket. Only metadata (file IDs, offset ranges, batch ordering) goes to the control plane. **[Blog]**

**Target workload.** High-throughput, cost-sensitive Kafka workloads such as logging, telemetry, CDC fan-out and analytics pipelines, where:

- Inter-AZ replication traffic dominates cost. WarpStream claims inter-zone networking is often more than 80% of a self-hosted Kafka bill. **[Blog]** ("Tiered Storage Won't Fix Kafka")
- A produce latency of hundreds of milliseconds is acceptable. Default p99 produce latency is about 400 ms on S3 Standard. **[Blog]**

Later work pushed latency down:

- S3 Express One Zone: p50 105 ms / p99 169 ms. **[Blog]** (S3EOZ benchmark, Apr 2025)
- GCS Rapid zonal buckets. **[Blog]** ("One Weird Trick to Make Rapid Storage 40x Faster")
- "Lightning Topics" with delayed sequencing: p50 33 ms / p99 50 ms on S3EOZ. **[Blog]** ("The Art of Being Lazy(log)", Feb 2026)

Other products built on the same Agent and object-store substrate:

- **Schema Registry** (BYOC)
- **Managed Data Pipelines** (Bento-based, the `pipelines` role)
- **Tableflow**, which materialises topics as Iceberg tables under `<bucket>/warpstream/_tableflow`. **[Docs]**

---

## 2. Architecture

### 2.1 Components

| Component | Where it runs | State | Responsibility |
|---|---|---|---|
| **Agent** (`warpstream_agent` image, `agent` subcommand) | Customer VPC (K8s Deployment/StatefulSet, ECS, VMs) | **Stateless.** No disks. The chart requests only `ephemeral-storage: 100Mi` "we do not need the disk space" **[Code]** | Kafka protocol front-end, write batching, PUT to object store, range-GETs, per-AZ distributed cache, background jobs (compaction, GC, retention) |
| **Control plane / metadata store** | WarpStream cloud (per region; AWS, GCP, Azure) | Durable, delegated to cloud databases | Per-"Virtual Cluster" replicated state machine. Maps object-store files to offset ranges per topic-partition, assigns offsets, stores consumer offsets, schedules jobs, does service discovery **[Docs]** |
| **Object storage bucket** | Customer account | Durable data | All record data (ingestion files and compacted files) |
| **Kafka clients** | Customer | — | Unmodified Kafka clients. The AZ is encoded in `client.id` for zone-aware routing **[Blog]** ("Hacking the Kafka PRoTocOL") |

The control plane is itself described as "completely stateless, like the Agents (it just depends on cloud native data stores like DynamoDB and Spanner)". It is backed by DynamoDB (AWS), Spanner (GCP) and Cosmos DB (Azure). **[Blog]** ("Your WarpStream Questions, Answered")

Every Virtual Cluster metadata operation "is journaled to our strongly-consistent log storage system before being executed by a Virtual Cluster replica". **[Docs]** (Architecture)

### 2.2 Agent roles (from the Helm chart) **[Code]**

`templates/deployment.yaml` passes `-roles=<...>` and `-agentGroup=<...>`. The `values.yaml` examples and docs list these roles:

- `proxy`, which can be split into `proxy-produce` and `proxy-consume`. This is the Kafka protocol front end.
- `jobs`, which runs background jobs: "Compaction jobs periodically rewrite and merge files in object storage" **[Docs]** (Agent Roles). The control plane "automatically routes jobs to agents that have registered for the appropriate job types".
- `pipelines`, which runs Managed Data Pipelines.
- `query`, which appears in `values.yaml` examples (`roles: query`) and is presumably used for the console's data-exploration features. **[Inferred]**

The chart supports `customDeployments` per zone and per role, for example `proxy-zone-a`, `jobs-zone-a` and `query-zone-a`, each with its own HPA. This lets operators scale ingestion, compaction and query capacity separately.

### 2.3 Diagram

```
                       WarpStream Cloud (per region)
             +------------------------------------------------+
             |  Control plane (stateless service)             |
             |   - Virtual Cluster replicated state machine   |
             |   - journal -> strongly consistent log         |
             |   - backed by DynamoDB / Spanner / Cosmos DB   |
             |   - offsets, file->offset-range index,         |
             |     consumer groups, job scheduler, discovery  |
             +----------------------^-------------------------+
                  metadata only     | (commit file, lookup files,
                  (HTTPS)           |  heartbeats, job leases)
 Customer VPC                       |
 +----------------------------------+------------------------------------+
 |  AZ-a                         AZ-b                        AZ-c        |
 | +-----------+  +-----------+  +-----------+                ...        |
 | | Agent     |  | Agent     |  | Agent     |   Kafka clients           |
 | | proxy     |  | proxy     |  | jobs      |   (client.id carries AZ)  |
 | | + cache   |<>| + cache   |  | compaction|                           |
 | +-----+-----+  +-----+-----+  +-----+-----+                           |
 |       | PUT ingestion files   | range GET 4 MiB   | GET/PUT/DELETE     |
 |       v                       v                   v                    |
 |  +-----------------------------------------------------------------+  |
 |  |  Object storage bucket(s) (S3 / GCS / Azure Blob)               |  |
 |  |  - ingestion files (multi-topic-partition, ~250ms batches)      |  |
 |  |  - compacted files (L1, L2 ...)                                  |  |
 |  |  - optional: S3 Express / GCS Rapid ingestion buckets (quorum)  |  |
 |  +-----------------------------------------------------------------+  |
 +-----------------------------------------------------------------------+
```

---

## 3. Data model & on-object format

### 3.1 Logical model

- **Virtual Cluster** = the unit of metadata isolation and atomicity. "Kafka API operations within a Virtual Cluster are atomic, including producing records to multiple topics and partitions." **[Docs]**
- Each Virtual Cluster is "a replicated state machine which stores the mapping between files in object storage and ranges of offsets in each Kafka topic-partition". **[Docs]**
- Offsets and consumer-group commits live in the metadata store. "WarpStream virtual clusters do not publish offset topics." **[Docs]** (Kafka vs WarpStream Configuration Reference)

So the **index** that resolves `(topic, partition, offset) -> (file ID, byte range)` lives entirely in the control plane, not in the bucket. **[Docs]**, with the per-file byte-range detail **[Inferred]** from the read-path description, where Fetch "returns the set of files that contain data starting with the offset".

### 3.2 File format (what is documented)

- **Multi-tenant files.** An Agent writes "a single file every 250 ms (or 8 MiB, whichever comes first) that contains data from all the different topic-partitions it receives batches of data for within that time frame." **[Blog]** ("Kafka as a KV Store") The storage engine "stores data for many topic-partitions in the same files to minimize object storage costs". **[Blog]** ("Taking out the Trash")
- **Sort order inside a file.** "The WarpStream file format sorts all the batches in a given file first by topic, and then by partition." **[Blog]** ("Unlocking Idempotency with Retroactive Tombstones"; also "Distributed mmap") This makes each partition's data a contiguous byte range inside the file. A reader can therefore fetch one partition with one range GET.
- **Record batches.** Uncompacted files can hold many batches per partition. With idempotent producers, "uncompacted files would often have 40+ individual batches for a single partition", which slowed Fetch until compaction or merging. **[Blog]**
- **Compression.** The object-store compression is set by `storageCompression` / `WARPSTREAM_STORAGE_COMPRESSION`, with values `lz4` or `zstd`. The default changed from lz4 to zstd in **v671**. There is no per-topic override. **[Docs]** (Compression) Older material, such as the ShareChat case study, says "LZ4 by default (will update to ZSTD in the future)", which matches the change.
- **File naming / object keys.** The public material I could reach does **not** document the key layout of data files.
  - Documented: Tableflow writes under `<bucket>/warpstream/_tableflow`. With a prefix in the bucket URL, it writes under `<prefix>/warpstream/_tableflow`. **[Docs]**
  - Documented: the low-latency path writes files into a "sequence", described as a folder of about 1,000 consecutive files written by one Agent. **[Blog]**, low-latency post, from a search summary, so treat it as moderate confidence.
  - **[Inferred]** Normal data files are probably keyed under a `warpstream/` prefix by an opaque, unique file ID such as a ULID/UUID that the Agent generates before upload. The hash ring for the cache uses "the file ID as the hash key" **[Blog]**. Keys are write-once and never overwritten. The system needs only read-after-write consistency for **new** keys **[Blog]**, which fits that design.
- **Internal index / footer.** No public description was found. **[Inferred]** For a reader to issue precise range GETs given only a control-plane pointer, the metadata store must hold per-file, per-partition byte offsets, or the file must carry a small header or footer index. The read-path description has the control plane returning the files to read, and the cache works in aligned 4 MiB pages regardless of IO size. Together these suggest coarse range reads followed by in-memory parsing of Kafka record batches.

### 3.3 Compaction levels

These levels are documented in the compacted-topics docs and the metrics docs. **[Docs]**

- **L0** = the files Agents write at ingestion.
- When the scheduler sees "a large number (currently 32) of L0 files", it compacts them into bigger **L1** files. Likewise, 32 L1 files trigger an L1 to **L2** compaction.
- L0 to L1 compaction copies records without deduplicating keys. Compactions beyond L0 to L1 deduplicate records for compacted topics.
- Operators should expect `< 1000` files each at L0 and L1. L2 can be large for high-volume or long-retention workloads.
- WarpStream "never compacts files that contain more than 128 GiB of uncompressed records".
- For compacted topics, if a single compaction sees more than 3 million distinct keys for one topic-partition, the dedup buffer is cleared and restarted. **[Docs]** (Compacted topics)

---

## 4. Write path

### 4.1 Steps

From "Life of a Request", Write Path, the HN CTO comments and "Kafka is dead" **[Docs][Blog]**:

1. **Routing.** The client bootstraps via a DNS name. The `Metadata` response is answered by WarpStream's discovery service. When the client encodes its AZ in `client.id`, discovery returns an Agent **in the client's zone**. Otherwise it returns any live Agent, round-robin per Metadata request. **[Docs]** (Service Discovery) There are no partition leaders: "any Agent can act as the leader for any topic". Discovery reports a same-zone Agent as "leader" for every partition. **[Blog]**
2. **Buffering.** The Agent buffers Produce requests from **all clients and all partitions** in memory.
3. **Flush trigger.** The buffer is flushed when one of these is hit first:
   - `batchTimeout`: default **250 ms**. It can go as low as **50 ms** per the docs, or **25 ms** per the low-latency docs and blog (the sources differ).
   - `batchMaxSizeBytes`: **4 MiB** before v750; after v750 the default is **64 MiB**. v763 raised the maximum to **128 MiB**.
   - `batchMaxCompressedSizeBytes`: added in v750, default **1 MiB**, based on estimated compressed size.

   Older blog posts cite "250 ms or 8 MiB" and "~100 ms". The defaults have changed over time. **[Docs]** (Agent Configuration Reference, Change Log; ShareChat)
4. **PUT.** The Agent builds one file sorted by topic then partition and PUTs it to object storage.
5. **Commit.** After the PUT succeeds, the Agent commits the file to the Virtual Cluster's metadata store. The metadata store **assigns offsets at commit time**: "the agents call out to our cloud metadata store which runs a per-cluster metadata store that assigns offsets to messages at commit time". **[Blog]** (HN, CTO Ryan Worl)
6. **Ack.** Only after **both** the PUT and the commit succeed does the Agent answer each Produce request, with the assigned base offsets. "Agents never acknowledge data before it has been durably persisted." **[Docs]**

### 4.2 Guarantees

- **Durability** equals the object store's durability. S3 Standard and regional GCS replicate across AZs, so a single acked file survives the loss of an AZ. There is no separate replication factor. **[Blog]**
- **Ordering.** Batch order is decided when the batch is committed to the metadata store, not at upload. So many Agents can upload in parallel without coordinating. **[Blog]** ("Unlocking Idempotency") Monotonicity: "If you produce again after receiving that acknowledgement, the next set of offsets you receive are guaranteed to be larger." **[Blog]** (HN)
- **Atomicity.** A commit is atomic across all the topic-partitions in a file, and across topics. This is the basis for transactions. **[Docs]**
- **Idempotent producers.** Duplicate detection happens at commit, which is *after* the file is already in S3. A file can therefore contain a batch that the metadata store rejects as a duplicate. WarpStream marks those regions as **"retroactive tombstones"** in metadata, so readers skip them and compaction drops them. Readers must not see bytes that are in the file but rejected. **[Blog]** ("Unlocking Idempotency with Retroactive Tombstones"; mechanism details partly **[Inferred]** from the title and excerpts)
- **Failure handling [Inferred].**
  - If the PUT succeeds but the commit fails or the Agent crashes, the file becomes an **orphan**: present in the bucket but not referenced by metadata. The GC reconciliation loop cleans it up (§6.3).
  - If the commit succeeds but the ack is lost, the client retries. Idempotent producers dedupe this. Non-idempotent producers may duplicate, which is the same as Kafka's at-least-once behaviour.

### 4.3 Low-latency variants

1. **Separate ingestion and compaction buckets.**
   - `WARPSTREAM_INGESTION_BUCKET_URL` and `WARPSTREAM_COMPACTION_BUCKET_URL` are mutually exclusive with `WARPSTREAM_BUCKET_URL`. The chart's `templates/configmap.yaml` fails the render if only one of the two is set. **[Code]**
   - The ingestion URL may be a quorum multi-bucket URL: `warpstream_multi://s3://bucket-one-us-east-1a?region=us-east-1<>s3://...us-east-1b...<>s3://...` (`values.yaml`). **[Code]**
   - Agents "replicate your ingested data across a quorum of multiple S3 Express single-zone buckets". The docs use three directory buckets. Compaction then moves the data into a regular S3 bucket to keep storage cost low. **[Docs]** (Low Latency Clusters / S3 Express)
   - S3 Express PUTs cost "~1/5th the cost of a regular S3 PUT". **[Docs]**
   - **[Inferred]** Quorum means an ingestion file is acked after a majority (2 of 3) of the single-AZ bucket PUTs succeed. That gives AZ-loss durability without paying for cross-AZ replication in S3 Standard.
2. **GCS Rapid (zonal, appendable objects).**
   - WarpStream pre-opens a pool of gRPC appendable-object writers and sets `FinalizeOnClose=false`, which moves stream setup out of the write path. The result is a p50 of 20 ms and a p99 of about 33 ms to flush a 1 MiB file, 2 to 3 times faster than standard GCS.
   - Constraints: one writer per appendable object; only zonal buckets support appends. **[Blog]**
3. **Lightning Topics (delayed sequencing, LazyLog).**
   - Durability is separated from ordering. Records are *journaled* to S3 Express and the producer is acked immediately. Sequencing (offset assignment in the metadata store) happens afterwards.
   - Records become visible to consumers only once sequenced. So *produce* latency drops to a p50 of 33 ms, but *end-to-end* latency does not.
   - Trade-off: the producer cannot get an exact offset at ack time. **[Blog]** ("The Art of Being Lazy(log)")
   - Related: **Ripcord mode** keeps accepting produces during a control-plane outage, with limits (no idempotent or transactional producers, a restricted set of consumer operations). **[Blog]**
4. **Multi-region clusters (RPO=0).**
   - Agents write to buckets in more than one region. The metadata lives in multi-region Spanner or DynamoDB global tables, so each region is active-active.
   - Cost: p50 is 80 to 100 ms higher than single-region. Comes with a 99.999% SLA. **[Blog]** ("No record left behind")

---

## 5. Read / query path

From the Read Path docs, "Minimizing S3 API Costs with Distributed mmap" and the HN comments **[Docs][Blog]**:

1. **Fetch arrives** at an Agent in the consumer's AZ, using the same `client.id` AZ trick.
2. **Metadata lookup.** The Agent asks the control plane for "the set of files that contain data starting with the offset in your Fetch request". The answer is a list of file pointers covering `[fetchOffset, …)` for each requested partition.
3. **Fan-out to the per-AZ cache.** Agents in one AZ form a **consistent-hash ring** keyed by **file ID**. Each Agent "owns" a subset of files. The receiving Agent sends sub-fetches to the owner Agents in the same AZ, so that "Fetch requests don't cross zonal boundaries". **[Docs]** The control plane decides which Agents take part in the zone-aware cache. **[Docs]** (Architecture)
4. **Owner Agent cache.** The owner pages data in "fixed, aligned chunks of **4 MiB** regardless of how large the initiating IO was". Concurrent requests for the same 4 MiB block **across all Agents in the AZ are deduplicated**. The result is roughly **one GET per 4 MiB per AZ**. **[Blog]** Because files are sorted by topic then partition, one 4 MiB page often serves sub-fetches for **several partitions**: "Agent 2 receives sub-fetch requests for data from 2 other partitions located in that same 4MiB chunk". **[Blog]**
5. **Response assembly.** The receiving Agent combines the record batches, skips retroactive tombstones, and returns a standard Kafka Fetch response.

Notes:

- The distributed mmap cache "is really only required for the live edge of data". For tailing consumers, many consumers and partitions read the same freshly written L0 files. Historical catch-up reads hit compacted L1/L2 files, where data for each partition is contiguous and large. **[Blog]** "Background compaction merges batches for the same topic-partition across files, which improves read I/O for historical replays." **[Docs]**
- Decoupling: "a per-Availability Zone cache that behaves like a distributed mmap ... decouples partition and consumer counts from the number of GET requests". **[Docs]**
- There is **no local disk cache**. The cache is RAM only. The chart enforces at least **4 GiB of RAM per vCPU**: `templates/deployment.yaml` fails the render if `memoryPerCPU < 4000000000` while `enforceProductionResourceRequirements` is on. `GOMAXPROCS` is set to `ceil(cpu request)`, because "the Agents ... size caches and other data structures in accordance with the number of available cores". **[Code]** (`values.yaml` comments, `templates/deployment.yaml`)

---

## 6. Data manipulation

### 6.1 Compaction

- Runs as **jobs** on Agents with the `jobs` role. The control plane schedules them and Agents pull them. **[Docs]**
- The leveled scheme is L0 to L1 to L2, with a fan-in of 32 files per level (§3.3). Each compaction:
  1. Reads its input files with large sequential GETs.
  2. Merges batches of the same topic-partition into contiguous runs.
  3. Drops records that fell out of retention and retroactive-tombstoned duplicates.
  4. For compacted topics (L1 and above), deduplicates keys.
  5. Writes new, larger files.
  6. **Atomically swaps** the metadata: the input file references are replaced by the output file references in a single state-machine operation. **[Docs]** plus **[Inferred]** for the atomic swap, which is implied by "files are removed from the metadata store and added to a queue in the same atomic operation" in the GC post.
- With split buckets, compaction **reads from the ingestion bucket** (S3 Express/Rapid) and **writes to the compaction bucket** (S3 Standard). Hot data is the only thing in expensive storage. **[Docs]**
- Compacted topics support `cleanup.policy`, `retention.ms`, `delete.retention.ms`, `min.compaction.lag.ms` and `max.compaction.lag.ms`. Dedup uses a bounded in-memory key buffer: "deduplicating millions of keys with just 128 MiB of RAM", and the buffer resets at more than 3M keys. **[Blog]** ("Kafka as a KV Store"); **[Docs]**

### 6.2 Retention / deletion

- Retention (`retention.ms`, `retention.bytes`) is enforced **logically** in the metadata store. Offset ranges older than the TTL are dropped from the file index. **[Inferred]** from "a file can be logically deleted in the metadata store, for example when all of its data has expired under the configured topic TTLs". **[Blog]**
- Files are shared across many topic-partitions with different TTLs. A file can only be physically deleted when **every** range in it is expired or has been compacted into another file. Compaction is how mixed-TTL data gets separated. **[Inferred]**

### 6.3 Garbage collection of objects

"Taking out the Trash: Garbage Collection of Object Storage at Massive Scale" (Richard Artoul, Apr 2025) **[Blog]** looks at five approaches and picks a combination:

- **Bucket lifecycle / TTL policies are rejected.** They "only work as a mechanism for cleaning up object storage files for the most simple use-cases". Files hold data with mixed TTLs and compaction makes deletion times unpredictable.
- **Delayed deletion queue in the metadata store.**
  - When a file is logically deleted (compacted away or expired), it is removed from the index and added to a deletion queue **in the same atomic metadata operation**. This means a failed enqueue cannot leak the file.
  - Physical deletes happen **later**. The delay protects readers that already hold pointers to the old file. It also keeps **metadata-store backups restorable**, because a restored backup "will only work if all the files that the backup references still exist in the object store".
- **Optimistic in-Agent deletion queue.** After a compaction, the Agent already knows which input files were logically deleted. It buffers those IDs in memory and deletes them later without a round trip through the control plane. **[Blog]**
- **Asynchronous reconciliation ("mark and sweep").**
  - A background loop LISTs the bucket and compares it with metadata. It deletes **orphans**: files PUT but never committed, or IDs lost from the queues.
  - It catches leaks "within a few hours".
  - Its cost grew with cluster size because it had to run more often. That is why the queues, not LIST, are the main mechanism. The team explicitly wanted to avoid listing the whole bucket regularly.
- **Rate-limit safety.** Deletes are throttled to avoid triggering object storage rate limits, such as S3's per-prefix TPS limits. **[Blog]**

### 6.4 Schema evolution

Records are opaque Kafka record batches. There is no schema on the storage path. Schema handling lives in the separate BYOC Schema Registry and in Tableflow's Iceberg schemas. **[Docs]**

---

## 7. Object storage interaction patterns (key section)

| Aspect | WarpStream behaviour | Confidence |
|---|---|---|
| **Object granularity** | One object per Agent per flush interval, holding all partitions the Agent received in that window. **Not** one object per partition. This is the core cost trick: PUT count scales with *(#Agents × flushes/s)*, not with partition count. | **[Blog][Docs]** |
| **Object sizes** | Ingestion (L0) objects are bounded by `batchMaxSizeBytes` (64 MiB default since v750, max 128 MiB), `batchMaxCompressedSizeBytes` (1 MiB default since v750, by estimated compressed size) and `batchTimeout` (250 ms). In practice they range from hundreds of KiB to a few MiB per Agent per 250 ms, depending on throughput per Agent. Compacted L1/L2 objects are much larger, and inputs are capped at 128 GiB uncompressed per compaction. | **[Docs]**, typical size **[Inferred]** |
| **PUT style** | Ingestion files are small enough for a **single PUT**. **[Inferred]** Multipart is likely used only for large compaction outputs, but this is not documented. On GCS Rapid, pre-opened **appendable-object** gRPC writers are used. | **[Inferred]** / **[Blog]** |
| **Write-once keys** | Files are immutable and never overwritten. The system relies only on **read-after-write consistency for new keys**. No conditional PUTs and no object-store-based commit. | **[Blog]** |
| **Atomic commit** | Done **outside** the object store. A metadata-store transaction (journaled to a strongly consistent log, backed by DynamoDB/Spanner/Cosmos) makes a file visible and assigns offsets. The object store is a "dumb" blob tier. | **[Docs]** |
| **Catalog / metadata** | Fully external, in WarpStream's control plane. It holds the file to offset-range index, consumer offsets, topic config, job state and the deletion queue. **No** manifests or metadata objects in the bucket for core Kafka data. Tableflow is the exception: it periodically syncs Iceberg metadata into `<bucket>/warpstream/_tableflow`. | **[Docs]** |
| **Range GETs** | Reads are **aligned 4 MiB range GETs**, regardless of request size. Each 4 MiB block is fetched **once per AZ**, and concurrent requests are deduplicated across Agents. Sorting by topic then partition means a single 4 MiB GET often serves several partitions. | **[Blog]** |
| **GET fan-out** | Consistent hashing by file ID across same-AZ Agents spreads GET load and cache memory. Large historical reads fan out over many 4 MiB blocks in parallel across Agents. **[Inferred]** for the parallel prefetch details. | **[Docs]** / **[Inferred]** |
| **LIST** | **Avoided on hot paths.** Reads go to known keys from metadata. LIST is used only by the slow GC reconciliation sweep, which the team deliberately keeps rare. | **[Blog]** |
| **HEAD** | Not mentioned anywhere. **[Inferred]**: unnecessary, because object sizes and existence are known from metadata. | **[Inferred]** |
| **DELETE** | Asynchronous, delayed, batched and rate-limited, driven by metadata deletion queues and an in-Agent optimistic queue. No lifecycle policies. | **[Blog]** |
| **Hedging / retries** | Not documented publicly. **[Inferred]**: likely retries and hedges on slow PUTs, because tail PUT latency dominates produce p99. | **[Inferred]** |
| **Replication** | None in the application for S3 Standard/GCS/Azure, which rely on the provider's multi-AZ durability. Quorum writes go to 3 single-AZ buckets for S3 Express (`warpstream_multi://...<>...`). Multi-region clusters write to buckets in more than one region. | **[Docs][Code][Blog]** |
| **Cost model** | Request cost scales roughly **linearly with throughput** and is ">10× cheaper than paying for inter-zone bandwidth". The 2023 example: a 140 MiB/s three-AZ Kafka cluster pays $641/day in inter-zone transfer; WarpStream pays under $40/day in object-store API calls. Lowering `batchTimeout` raises PUT count. Raising the batch size cuts PUTs and adds latency. | **[Blog][Docs]** |
| **Bucket URLs** | `WARPSTREAM_BUCKET_URL` (e.g. `s3://bucket?region=us-east-1`, `mem://mem_bucket` for playground); ingestion/compaction split; `warpstream_multi://` with a `<>` separator for quorum sets. | **[Code]** |

**Back-of-envelope request math [Inferred].** With N Agents and a 250 ms flush, each Agent sends at most 4 PUTs/s. That is 4N PUTs/s whatever the partition count.

- Example: 30 Agents → about 120 PUT/s → about 10.4M PUT/day → about $52/day at $0.005 per 1k PUTs.
- Larger batches or fewer, bigger Agents reduce this. Read GETs ≈ (bytes read once per AZ ÷ 4 MiB) × #AZs.
- Example: 100 MiB/s tailing in 3 AZs → about 75 GET/s.

---

## 8. Performance

- **Throughput through batching across partitions.** One object per Agent per window turns thousands of tiny per-partition writes into a few large PUTs. The S3 per-request cost and the per-request latency floor are paid once per batch, not once per partition.
- **Latency.**
  - Produce latency ≈ `batchTimeout`/2 average wait + PUT latency + metadata commit RTT.
  - S3 Standard PUT is about 100 to 400 ms at the tail, which gives a p99 of about 400 ms by default.
  - Better tails come from:
    - lowering `batchTimeout` (50 ms or 25 ms)
    - S3 Express (single-digit-ms PUTs; p50 105 / p99 169 ms in WarpStream's benchmark)
    - GCS Rapid appendable objects with pre-warmed writer pools
    - delayed sequencing (p50 33 ms)
  - **[Docs][Blog]**
- **The read cache is the main read optimisation.** Per-AZ distributed caching with aligned 4 MiB blocks, request dedup across Agents and consistent-hash ownership keeps tailing-consumer reads in memory. Fan-out to many consumers does not multiply S3 GETs. **[Blog]**
- **Physical layout for locality.** Sorting by topic and partition within files, then leveled compaction into larger per-partition runs, means a historical replay reads long contiguous ranges. **[Blog][Docs]**
- **Compaction keeps metadata small.** Fewer files mean fewer index entries to search in the control plane, and fewer pointers per Fetch. L0/L1 file counts are monitored and should stay under 1000. **[Docs]**
- **CPU and memory sizing.**
  - `GOMAXPROCS` = CPU request. At least 4 vCPU and 16 GiB per Agent, at least 4 GiB per vCPU, and network-optimised instances (m6in) for high volume. **[Code]**
  - Backpressure is driven by metrics per workload type (Produce, Fetch, compaction). **[Blog]** ("Dealing with rejection (in distributed systems)")
- **Compression.** zstd by default since v671 reduces bytes stored and GET bytes, at some CPU cost. **[Docs]**

---

## 9. Scaling

- **Agents are stateless and horizontally scaled on CPU.**
  - Chart default: HPA `minReplicas: 3`, `maxReplicas: 120`, `targetCPU: "60"`.
  - Aggressive scale-up: +50% or +3 pods per 300 s, whichever is larger. Conservative scale-down: 1 pod per 900 s, with a 1800 s stabilisation window. The slow scale-down protects the consistent-hash cache.
  - PDB `maxUnavailable: 1`, because "a 10% disruption could mean a loss of 30% capacity in a single zone".
  - `gracefulShutdownDuration: 300s`.
  - **[Code]** (`values.yaml`)
- **No partition rebalancing.** Partitions are not assigned to Agents, so adding or removing Agents moves **no data**. Only cache ownership on the hash ring changes. Hot partitions spread naturally across all Agents in a zone. **[Blog]**
- **Zone-aware scaling.** `customDeployments` per zone, each with its own HPA, handle skewed client placement across AZs. Clients send `client.id` with AZ info to avoid cross-AZ traffic. **[Code][Blog]**
- **Role splitting.** Separate `proxy-produce`, `proxy-consume`, `jobs` and `pipelines` fleets, plus **Agent Groups** (`-agentGroup`) for isolated networks or VPCs that share one cluster. This isolates compaction CPU and memory from the latency-sensitive paths. **[Code][Docs]**
- **Multi-tenancy.** Customers create many isolated **Virtual Clusters**. Each is its own replicated state machine in the control plane. **[Docs]**
- **Bottlenecks and limits.**
  - **Metadata store throughput per Virtual Cluster.** Every flush from every Agent is a commit, and every Fetch needs a lookup. Commits scale with *(#Agents × flush rate)*, not with record rate. **[Inferred]**
  - Object-store per-prefix request limits, which shape the GC delete throttling. **[Blog]**
  - Control-plane availability: produce stops without it unless Ripcord is used. **[Blog]**
  - Cache memory per AZ for high-fan-out tailing.
  - Latency floor set by object-store PUT latency.

---

## 10. Trade-offs & lessons

**Design choices worth copying**

1. **Separate data from metadata. Make the object store a dumb, immutable blob tier.** Commits, ordering and indexes go in a strongly consistent database (DynamoDB/Spanner/FoundationDB-class). Then the object store only needs read-after-write consistency for new keys. No conditional PUTs, no manifest CAS, no LIST-based discovery.
2. **Batch across logical streams, not per stream.** One object per writer per time window, sorted internally by stream, decouples request cost from cardinality (partitions or tenants). Leveled compaction then restores per-stream locality for cold reads.
3. **Sequence after persisting.** Upload first with no coordination, then assign order in a tiny metadata transaction. Writers never contend on a leader. LazyLog-style delayed sequencing goes further and acks before ordering.
4. **A per-AZ, consistent-hashed, block-aligned RAM cache with request coalescing.** This changes GET cost from O(consumers × partitions) to O(bytes / 4 MiB / AZ). It also avoids cross-AZ transfer charges.
5. **Use metadata-driven GC with a delay, and keep LIST only as a safety net.** Enqueue deletes atomically with logical deletion. Delay physical deletes so in-flight readers and metadata backups stay valid. Throttle deletes.
6. **Make storage tier choice a per-stage config.** Hot ingestion goes on S3 Express or GCS Rapid (quorum across zonal buckets). Compaction output goes on cheap regional storage.
7. **Stateless compute, sized by CPU with a fixed RAM:CPU ratio.** This makes autoscaling trivial, because no data moves on scale events.

**Weaknesses / costs**

- **Latency floor.** Produce latency follows object-store PUT tail latency plus the commit RTT. The default p99 of about 400 ms is unsuitable for low-latency transactional use without S3 Express, Rapid or Lightning Topics, and each of those adds cost or caveats such as exact offsets not being available at ack time.
- **Dependence on a proprietary, hosted metadata store.**
  - It is a hard availability dependency (Ripcord mitigates this only partially).
  - It cannot be independently verified: an HN commenter noted Jepsen-style testing is unlikely.
  - All metadata leaves the customer's account.
- **Idempotency complexity.** Because files are written before dedup, the system needs retroactive tombstones and pays a read penalty on uncompacted data. **[Blog]**
- **Mixed-TTL files** delay physical deletion until compaction separates the data. Storage can briefly exceed retention.
- **Compaction write amplification.** L0 to L1 to L2 means each byte is rewritten at least twice. The trade is made deliberately for read locality and smaller metadata.
- **Config drift across versions.** Batch-size defaults (4 MiB, 8 MiB, 64 MiB / 1 MiB compressed), the minimum batch timeout (50 vs 25 ms) and the compression default (lz4, then zstd at v671) all changed. Pin Agent versions and read the changelog.

**Transferable to a new object-store-backed system (e.g. for dskit-based services)**

- Use a write-ahead "one object per writer per tick" log with an external sequencer or metadata service, then a leveled compactor that reorganises by key or stream.
- Put a block-aligned, consistent-hash read cache (ring keyed by object ID, fixed 4 MiB pages, singleflight dedup) in front of object storage, scoped per AZ.
- Run GC as metadata-transactional enqueue, delayed delete and a rate-limited slow reconciliation sweep. Never rely on bucket lifecycle rules for correctness.
- Keep keys write-once with random or ULID IDs to avoid prefix hot-spotting, and to avoid needing any consistency stronger than read-after-write.

---

## 11. References

**WarpStream blog (primary; read via search-result excerpts, direct fetch blocked)**
- Kafka Is Dead, Long Live Kafka (Jul 2023): https://www.warpstream.com/blog/kafka-is-dead-long-live-kafka
- Minimizing S3 API Costs with Distributed mmap: https://www.warpstream.com/blog/minimizing-s3-api-costs-with-distributed-mmap
- Unlocking Idempotency with Retroactive Tombstones: https://www.warpstream.com/blog/unlocking-idempotency-with-retroactive-tombstones
- Kafka as a KV Store: deduplicating millions of keys with just 128 MiB of RAM: https://www.warpstream.com/blog/kafka-kv-store
- Taking out the Trash: Garbage Collection of Object Storage at Massive Scale (Apr 2025): https://www.warpstream.com/blog/taking-out-the-trash-garbage-collection-of-object-storage-at-massive-scale
- The Art of Being Lazy(log): Lower Latency and Higher Availability With Delayed Sequencing (Feb 2026): https://www.warpstream.com/blog/the-art-of-being-lazy-log-lower-latency-and-higher-availability-with-delayed-sequencing
- One Weird Trick to Make Rapid Storage 40x Faster: https://www.warpstream.com/blog/one-weird-trick-to-make-rapid-storage-40x-faster
- S3 Express is All You Need: https://www.warpstream.com/blog/s3-express-is-all-you-need
- WarpStream S3 Express One Zone Benchmark and Total Cost of Ownership (Apr 2025): https://www.warpstream.com/blog/warpstream-s3-express-one-zone-benchmark-and-total-cost-of-ownership
- No record left behind: How WarpStream can withstand cloud provider regional outages: https://www.warpstream.com/blog/no-record-left-behind-how-warpstream-can-withstand-cloud-provider-regional-outages
- Hacking the Kafka PRoTocOL: https://www.warpstream.com/blog/hacking-the-kafka-protocol
- Dealing with rejection (in distributed systems): https://www.warpstream.com/blog/dealing-with-rejection-in-distributed-systems
- Your WarpStream Questions, Answered: https://www.warpstream.com/blog/your-warpstream-questions-answered
- WarpStream is Dead, Long Live WarpStream (Confluent acquisition): https://www.warpstream.com/blog/warpstream-is-dead-long-live-warpstream
- Cost-Effective Logging at Scale: ShareChat's Journey to WarpStream: https://www.warpstream.com/blog/cost-effective-logging-at-scale-sharechats-journey-to-warpstream
- Tiered Storage Won't Fix Kafka: https://dev.to/warpstream/tiered-storage-wont-fix-kafka-28d7
- Hacker News thread with CTO Ryan Worl's comments: https://news.ycombinator.com/item?id=37036291 , https://news.ycombinator.com/item?id=37036812
- AWS Storage Blog, How WarpStream enables cost-effective low-latency streaming with Amazon S3 Express One Zone: https://aws.amazon.com/blogs/storage/how-warpstream-enables-cost-effective-low-latency-streaming-with-amazon-s3-express-one-zone/

**WarpStream docs**
- Architecture: https://docs.warpstream.com/warpstream/overview/architecture
- Write Path: https://docs.warpstream.com/warpstream/overview/architecture/write-path
- Read Path: https://docs.warpstream.com/warpstream/overview/architecture/read-path
- Life of a Request (Simplified): https://docs.warpstream.com/warpstream/overview/architecture/life-of-a-request
- Service Discovery: https://docs.warpstream.com/warpstream/overview/architecture/service-discovery
- Agent Configuration Reference: https://docs.warpstream.com/warpstream/kafka/advanced-agent-deployment-options/agent-configuration
- Change Log: https://docs.warpstream.com/warpstream/overview/change-log
- Agent Roles: https://docs.warpstream.com/warpstream/kafka/advanced-agent-deployment-options/splitting-agent-roles
- Low Latency Clusters / S3 Express: https://docs.warpstream.com/warpstream/kafka/advanced-agent-deployment-options/low-latency-clusters , https://docs.warpstream.com/warpstream/kafka/advanced-agent-deployment-options/low-latency-clusters/s3-express
- Compacted topics: https://docs.warpstream.com/warpstream/kafka/reference/protocol-and-feature-support/compacted-topics
- Kafka vs WarpStream Configuration Reference: https://docs.warpstream.com/warpstream/kafka/reference/protocol-and-feature-support/kafka-vs-warpstream-configuration-reference
- Compression: https://docs.warpstream.com/warpstream/kafka/reference/compression
- Important Metrics and Logs: https://docs.warpstream.com/warpstream/agent-setup/monitor-the-warpstream-agents/important-metrics-and-logs
- Reducing Infrastructure Costs: https://docs.warpstream.com/warpstream/kafka/advanced-agent-deployment-options/reducing-infrastructure-costs
- Tableflow: https://docs.warpstream.com/warpstream/tableflow/tableflow

**Code (public, deployment only)**
- `github.com/warpstreamlabs/charts` @ `0ac68e6be68375457f236189a7e34d0300c518d8`
  - `charts/warpstream-agent/Chart.yaml`: chart 1.0.104, appVersion v852, appVersionStable v842
  - `charts/warpstream-agent/values.yaml`: roles/customDeployments, `bucketURL`, `ingestionBucketURL` (`warpstream_multi://...<>...`), `compactionBucketURL`, resource guidance (≥4 vCPU, ≥4 GiB/vCPU, `ephemeral-storage: 100Mi`), HPA 3–120 @ 60% CPU, PDB, `gracefulShutdownDuration: 300s`, ports 9092/8080/9094
  - `charts/warpstream-agent/templates/configmap.yaml`: `WARPSTREAM_BUCKET_URL`, `WARPSTREAM_INGESTION_BUCKET_URL`, `WARPSTREAM_COMPACTION_BUCKET_URL` mutual-exclusion validation, `WARPSTREAM_DEFAULT_VIRTUAL_CLUSTER_ID`, `WARPSTREAM_REGION`/`WARPSTREAM_METADATA_URL`
  - `charts/warpstream-agent/templates/deployment.yaml`: `agent` subcommand, `-roles`, `-agentGroup`, `GOMAXPROCS` derivation, 4 GiB/CPU enforcement, `/v1/status` probes
- `github.com/warpstreamlabs/terraform-provider-warpstream` @ `2b45791a1dace60bcbc718f48fa6c1754a2d4ee1` (checked; control-plane resources only, no storage details)

**Third-party (used only for cross-checking; not authoritative)**
- VuTrinh, "I spent 8 hours researching WarpStream": https://vutr.substack.com/p/i-spent-8-hours-researching-warpstream (its about 16 MB cache-chunk figure conflicts with WarpStream's 4 MiB and was not used)
- AutoMQ analyses of WarpStream cost and architecture: https://www.automq.com/blog/warpstream-object-storage-cost-s3-requests-retention-read-patterns
