# pgxbench

Reference benchmark harness comparing the pgx v5 row-collection APIs against the
hand-written row readers used on the VC and query-service main paths. Written to answer
hyperledger/fabric-x-committer#743; see [ANALYSIS.md](ANALYSIS.md) for the findings and
the numbers.

This package exists for reference and re-measurement. It is not imported by any service.

## Layout

| file | contents |
|---|---|
| `variants.go` | every reader variant under comparison, each with the production signature. The benchmarks call these, so the code in ANALYSIS.md is the code measured. |
| `fakerows_test.go` | `pgx.Rows` / `pgx.Row` stand-ins that decode pre-encoded wire bytes through a real `*pgtype.Map`, replicating `baseRows.Scan` and `connRow.Scan`. |
| `rowscan_bench_test.go` | the isolated benchmarks, one table per row shape. |
| `realdb_test.go` | the same variants against a real PostgreSQL, to weigh the deltas against a query round trip. |
| `protomapping_test.go` | what the struct mappers can and cannot do with `committerpb` messages. |
| `fragility_test.go` | how each mapper fails when the schema or the target struct drifts. |

## Running

Isolated benchmarks need no database:

```bash
go test ./pgxbench/ -run '^$' -bench . -benchtime 300ms -count 10
```

Summarise with `benchstat`, pivoting on the variant and row count:

```bash
go test ./pgxbench/ -run '^$' -bench . -benchtime 300ms -count 10 > /tmp/bench.txt
go tool benchstat -col /variant -row /rows /tmp/bench.txt
```

The real-database measurement is a `Test` rather than a `Benchmark`, because the fixtures
need `*testing.T`; it drives `testing.Benchmark` internally and logs the results:

```bash
DB_DEPLOYMENT=local DB_TYPE=postgres go test ./pgxbench/ -run TestRealDBRowScanCost -v
```

## Complexity numbers

ANALYSIS.md quotes body-only LOC plus `gocognit`/`gocyclo` per variant:

```bash
go install github.com/uudashr/gocognit/cmd/gocognit@latest
go install github.com/fzipp/gocyclo/cmd/gocyclo@latest
gocognit -over -1 pgxbench/variants.go
gocyclo  -over -1 pgxbench/variants.go
```
