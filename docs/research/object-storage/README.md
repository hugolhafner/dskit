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

The documents on closed-source systems label each claim as stated by the vendor or inferred.
