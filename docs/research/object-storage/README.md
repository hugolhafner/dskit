# Object-storage-backed data stores: architecture research

One document per system, focused on how each stores, retrieves and manipulates data in object storage (S3/GCS/Azure Blob). Every document uses the same section layout: overview, architecture, data format, mechanisms, object storage interaction patterns, performance, scaling, trade-offs and sources. Each one also ends with review notes from a fact-checking pass.

| System | Source availability | Document |
|---|---|---|
| Datadog Husky | Closed (blog posts, talks) | [datadog-husky.md](datadog-husky.md) |
| Quickwit | Open source (Rust, tantivy) | [quickwit.md](quickwit.md) |
| Grafana Loki | Open source (Go) | [grafana-loki.md](grafana-loki.md) |
| Grafana Tempo | Open source (Go) | [grafana-tempo.md](grafana-tempo.md) |
| WarpStream | Closed (blog posts, docs) | [warpstream.md](warpstream.md) |
| ClickHouse | Open source (C++) | [clickhouse.md](clickhouse.md) |

**Cross-system synthesis:** [synthesis.md](synthesis.md) compares the six systems. It covers common patterns, patterns specific to each modality, where the systems differ, the most interesting approach in each, convergent trends, and a design guide.

The documents on closed-source systems label each claim as stated by the vendor or inferred.

**Design sketch:** [design-event-store.md](design-event-store.md) applies these lessons to a proposed architecture for a new schemaless store for logs, traces and wide events. It covers components, the segment file format, the catalog keyspace, write, compaction and read paths, lifecycle, caching, sizing and failure modes.
