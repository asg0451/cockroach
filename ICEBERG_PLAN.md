# Goal & scope

* **Goal:** For each table, publish **one Iceberg snapshot per changefeed resolved timestamp** `R` that contains *only* the files produced for changes `≤ R`, yielding exactly-once **visibility** at the snapshot boundary (changefeed delivery remains at-least-once).
* **Scope (MVP):**

  * **File format:** Parquet.
  * **Deletes:** **Equality deletes** (merge-on-read). Position deletes/deletion vectors later. ([Apache Iceberg][1])
  * **Commit types:** `AppendFiles` (insert-only) and `RowDelta` (when any equality deletes exist). Commit via **Iceberg REST Catalog** with precondition **requirements**. ([Apache Iceberg][2])
  * **Barriering:** Use CockroachDB **resolved timestamps** as the epoch boundary; the resolved timestamp guarantees no earlier, unseen updates remain. ([cockroachlabs.com][3])

---

# Background (what the writer must honor)

* An **Iceberg snapshot** is a complete table view at a point in time; it references a **manifest list** which references **manifests** that enumerate data & delete files. Readers see the union of those files (minus rows masked by deletes). ([Apache Iceberg][4])
* Manifests encode partition data and column metrics (valueCounts, nullCounts, lower/upper bounds) per file; writers should provide these to enable pruning. ([Apache Iceberg][5])
* Row-level deletes use **equality delete files** (by configured key columns). ([Apache Iceberg][1])
* REST catalog commits are **optimistic** with **requirements** (preconditions) and **updates** (append/row-delta). ([Apache Iceberg][2])

---

# High-level design

## Data path (one way: worker ➝ coordinator)

* **Workers (change aggregators)**: write Parquet data and (if needed) equality-delete files; for **each closed file**, emit a **`FileClosed`** message (in-band) to the **coordinator** *before* sending the worker’s **`Resolved(R)`**.
* **Coordinator (change coordinator)**: buffers `FileClosed` by `(table, epoch=R)`; when **global Resolved = R** (existing CDC logic) it builds manifests from the buffered metadata and performs **one** Iceberg commit (AppendFiles/RowDelta). No listing/reading from S3/GCS; no job_info rows. ([cockroachlabs.com][6])

## Idempotence / exactly-once snapshot

* Define **`batch_id = uuid5(job_id:table_fqn:R)`** and include it in the snapshot **summary**. On failover/retry, coordinator **probes history by `batch_id`** and no-ops if it’s already published. (This leverages REST catalog’s snapshot/commit model.) ([Apache Iceberg][2])
* Workers may **replay** recent `FileClosed` after reconnect; coordinator **dedupes by path** (and optional file hash).
* Only **one commit** per `(table,R)`; optimistic requirements prevent silent divergence; retries rebuild the same manifests from buffered metadata.

---

# Storage layout (deterministic; for workers only)

Workers write with deterministic keys to avoid collisions on retry:

```
s3://<warehouse>/<ns>/<table>/
  crdb_job=<job_id>/
    epoch=<R>/
      shard=<worker_id>/
        data-<seq or hash>.parquet
        deletes/eqdel-<seq or hash>.parquet
```

(Readers don’t rely on the path; it’s just to keep worker outputs stable. Iceberg pruning uses manifest entries + metrics.) ([Apache Iceberg][5])

---

# Wire protocol (in-band, per file)

**One new message type** from worker ➝ coordinator:

### `FileClosed`

Per file that contains any records with commit time `≤ R`:

* `table_fqn` (db.schema.table)
* `epoch_hlc` (R) and `batch_id`
* `worker_id`, `schema_id`, `spec_id`
* `path` (full object key/URI), `kind` = `DATA` | `EQUALITY_DELETE`
* `record_count`, `file_size_bytes`
* `partition` (map: field name/ID ➝ encoded value)
* **metrics** (optional but recommended for pruning): `value_counts`, `null_counts`, `lower_bounds`, `upper_bounds` (per column)
* `equality_ids[]` (for delete files; names or IDs)
* `file_hash` (sha256 over `(path,size,record_count,metrics,partition)`)

**Worker invariant:** do **not** send `Resolved(R)` until *all* `FileClosed(≤R)` are sent on that stream. (We are using `Resolved(R)` as the “done@R” fence.) ([cockroachlabs.com][3])

---

# Worker responsibilities

1. **Encode files** (Parquet) for incoming change rows:

   * inserts/updates ➝ **data files**; deletes/updates ➝ **equality delete files** keyed by configured columns (default: table PK). ([Apache Iceberg][1])
2. On file close, **emit `FileClosed`** with all metadata needed to build manifest entries (partition data + metrics). ([Apache Iceberg][5])
3. Only after all files for `≤ R` are closed **and reported**, send **`Resolved(R)`** (existing CDC behavior). ([cockroachlabs.com][3])
4. **Resend buffer:** persist a small rolling buffer (e.g., last few epochs or last 24h) of `FileClosed` for durability across coordinator failover; resend on reconnect. Coordinator dedupes by `(path)` (and optionally `file_hash`).

---

# Coordinator responsibilities

1. **Buffer** incoming `FileClosed` by `(table, R)`, deduping by `path` (verify `file_hash` if present).
2. Use existing CDC logic to compute **global Resolved = R**. ([cockroachlabs.com][6])
3. **Build manifests in memory** from buffered entries: one or more **data manifests** (and delete manifests if any equality deletes). Metrics map 1:1 into Iceberg manifest fields. ([Apache Iceberg][5])
4. **Pre-commit probe:** look up snapshots for the table; if a snapshot `summary` contains `crdb.batch_id = uuid5(job,table,R)`, **skip** (already published) and advance the frontier.
5. **Commit** via REST Catalog:

   * `AppendFiles` if only data; `RowDelta` if any equality deletes.
   * Include **requirements** (assert table UUID; assert ref snapshot id; guard schema/spec IDs) as per REST spec. ([Apache Iceberg][2])
   * Set snapshot **summary**:
     `operation=append|row_delta, crdb.batch_id, crdb.job_id, crdb.resolved_hlc, crdb.data_file_count, crdb.delete_file_count`.
6. On **409** (precondition failed): reload head, re-run the **pre-commit probe**; if not found, retry commit against the new head.
7. **Advance** changefeed frontier to `R` after successful publish (or if probe found a prior publish).

---

# Configuration surface (initial)

* `sink=iceberg://<rest-endpoint>/<namespace>/<table>?warehouse=s3://…` (or GCS/Azure equivalents)
* `equality_delete_by=<cols>` (default: table PK)
* Parquet settings (row group size, page size, dictionary)
* **Auth** for REST catalog (e.g., AWS Glue REST uses SigV4) and object store creds. ([AWS Documentation][7])

---

# Metrics & logging

* `iceberg_files_closed_total{table,kind}` (worker)
* `iceberg_epoch_files_total{table,epoch}` (coord)
* `iceberg_commit_latency_seconds{table}` (coord)
* `iceberg_commit_conflicts_total{table}` (coord)
* `iceberg_duplicate_epochs_skipped_total{table}` (coord; found via pre-commit probe)
* `iceberg_manifest_entries_bytes{table,epoch}` (coord)

---

# Failure & recovery (key cases)

* **Worker crash before `Resolved(R)`:** on restart, resend buffered `FileClosed`; then send `Resolved(R)`.
* **Coordinator failover before commit:** new leader continues buffering; once global Resolved = R, it either **finds** the prior `batch_id` snapshot (skip) or commits.
* **Schema/spec change mid-epoch:** workers include `schema_id/spec_id`. Coordinator may either split the epoch into multiple commits ordered by sequence numbers, or roll those files to the **next** epoch (simpler MVP).
* **Duplicate output on worker retry:** stable file paths + coordinator dedupe by `path` prevent double-reference.

---

# External references (for implementers)

* **Iceberg spec (snapshots, manifests, deletes):** overview & v2 row-level deletes. ([Apache Iceberg][8])
* **Manifest metrics used for pruning:** valueCounts, nullCounts, lower/upper bounds. ([Apache Iceberg][5])
* **REST Catalog:** commit model and requirements; OpenAPI. ([Apache Iceberg][2])
* **CockroachDB changefeed resolved semantics & architecture:** resolved messages and coordinator role. ([cockroachlabs.com][3])

---

# Work breakdown (small, concrete tasks)

## A. Sink scaffolding & wiring

1. **Add sink type `iceberg`** to changefeed options and job planning (URL parsing: rest endpoint, namespace, table, warehouse, equality_delete_by, auth params).
2. **Introduce “Iceberg mode”** into the CDC job: enable **per-file `FileClosed`** emission from aggregators and a **manifest/commit coordinator** path.

## B. Per-file message plumbing

3. **Define `FileClosed` protobuf** (or equivalent) with fields listed above.
4. **Emit `FileClosed` from workers** on Parquet file close: compute partition values and metrics required for manifests (valueCounts, nullCounts, lower/upper bounds) and add `file_hash`. ([Apache Iceberg][5])
5. **Enforce fence:** gate the worker’s **`Resolved(R)`** until all `FileClosed(≤R)` have been sent. ([cockroachlabs.com][3])

## C. Coordinator aggregation & manifest building

7. **Per-epoch buffers** keyed by `(table,R)`; dedupe entries by `path` (verify `file_hash` if present).
8. **Manifest builders**:

   * Convert `FileClosed` ➝ **data manifest entries** and **delete manifest entries** (for equality deletes).
   * Partition data encoding + column metrics mapping per Iceberg spec. ([Apache Iceberg][5])
9. **Batch identity**: `batch_id = uuid5(job,table,R)` helper; **pre-commit probe** that scans table history for snapshots with `summary.crdb.batch_id`.
10. **Commit request builders**: `AppendFiles` vs `RowDelta`; include **requirements** (`assert-table-uuid`, `assert-ref-snapshot-id`, `assert-default-spec-id`, `assert-default-sort-order-id`) per REST spec. ([Apache Iceberg][2])
11. **Snapshot summary** population (`operation`, `crdb.batch_id`, `crdb.job_id`, `crdb.resolved_hlc`, counts).
12. **Conflict handling**: on HTTP 409, reload head, re-probe `batch_id`, retry commit.

## D. Catalog & storage clients

13. **REST Catalog client**: minimal endpoints—load table, commit (`POST /tables/{table}`), list snapshots or load table metadata for snapshot histories. ([Apache Iceberg][2])
14. **Object store writer**: reuse existing cloud-storage sink writers; ensure deterministic path helpers for `epoch=R/shard=<worker_id>/…`.

## E. Schema & partition handling

15. **Type & ID mapping**: map CRDB types to Iceberg; track `schema_id` per epoch.
16. **Schema/spec change detection**: if mixed IDs appear in epoch R, either split into ordered sub-commits or roll to next epoch (feature flag for MVP).

## F. Configuration & auth

17. **Sink URL/opts**: `catalog_uri`, `warehouse`, `namespace`, `table`, `equality_delete_by`, Parquet tuning.
18. **Auth**: pluggable—e.g., SigV4 for AWS Glue REST catalog; object store creds injection. ([AWS Documentation][7])

## G. Telemetry & ops

19. **Metrics** (worker & coordinator) as listed above; structured logs with `table`, `epoch`, `batch_id`, counts.
20. **Admin tooling**: a `SHOW CHANGEFEED <job>` view that surfaces last committed `epoch` and the Iceberg `snapshot_id`.

## H. Validation & tests

21. **Golden path tests**: append-only; append+equality-delete; multi-worker; multi-epoch.
22. **Failure matrix**: worker retry; coordinator failover pre/post commit; 409 conflicts; replays (duplicate `FileClosed`); schema change mid-epoch.
23. **Interoperability**: read with Spark/Trino and validate snapshot contents & equality-delete correctness; verify time-travel across snapshots.
24. **No-storage-reads invariant**: assert that neither workers nor coordinator list/read objects to build manifests.

---

# Deliverables

* `iceberg` sink implementation (workers + coordinator) with per-file in-band metadata and single-commit epochs.
* REST catalog client + manifest builders (AppendFiles/RowDelta). ([Apache Iceberg][2])
* Config flags, metrics, and basic docs pointing to Iceberg and CockroachDB semantics. ([cockroachlabs.com][3])

[1]: https://iceberg.apache.org/spec/?h=equality&utm_source=chatgpt.com "Equality Delete Files - Spec - Apache Iceberg™"
[2]: https://iceberg.apache.org/rest-catalog-spec/?utm_source=chatgpt.com "REST Catalog Spec - Apache Iceberg™"
[3]: https://www.cockroachlabs.com/docs/stable/changefeed-messages?utm_source=chatgpt.com "Changefeed Messages"
[4]: https://iceberg.apache.org/javadoc/0.13.0/org/apache/iceberg/Snapshot.html?utm_source=chatgpt.com "Snapshot"
[5]: https://iceberg.apache.org/docs/latest/performance/?utm_source=chatgpt.com "Performance - Apache Iceberg™"
[6]: https://www.cockroachlabs.com/docs/stable/how-does-a-changefeed-work?utm_source=chatgpt.com "How Does a Changefeed Work?"
[7]: https://docs.aws.amazon.com/glue/latest/dg/iceberg-rest-apis.html?utm_source=chatgpt.com "AWS Glue REST APIs for Apache Iceberg specifications"
[8]: https://iceberg.apache.org/spec/?utm_source=chatgpt.com "Spec - Apache Iceberg™"
