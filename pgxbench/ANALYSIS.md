# pgx v5 row-collection APIs: cost analysis

Evaluation of the pgx v5 collect APIs (`CollectRows`, `AppendRows`, `ForEachRow`,
`RowToStructBy*`, `CollectOneRow`) against the five row readers on the VC and
query-service main paths.

Posted to hyperledger/fabric-x-committer#743:
<https://github.com/hyperledger/fabric-x-committer/issues/743#issuecomment-5193392571>

Everything below the rule is that comment verbatim. The benchmarks backing it are in this
directory.

---

I benchmarked the pgx v5 collect APIs against all five of our row readers, in isolation
and against a real PostgreSQL. Recommendation first, evidence after.

The five readers are spread across two services, and between them read four distinct row
shapes. Both the recommendation and the evaluation refer to those shapes by number:

| shape | reader | what it reads | when it runs |
|---|---|---|---|
| 1 | `readQueryRows` | `key, value, version` → `[]*committerpb.Row` | every query batch |
| 2 | `readTxStatusRows` | `tx_id, status, height` → `[]*committerpb.TxStatus` | every status query |
| 2 | `readStatusWithHeight` | the same three columns, VC side | duplicate detection |
| 3 | `readTwoItems` | `key, version` → two parallel slices | validation, config, snapshot |
| 4 | `readArrayResult` | one `BYTEA[]`/`INT[]` cell → `[]T` | every commit |

## Recommendation

Three changes worth making, one to skip. They are numbered to match the shapes above, so
recommendation *N* is backed by the evaluation's shape *N*. Two facts drive every
decision:

1. **End-to-end all the variants are indistinguishable.** Parsing is ~1 ms of a ~21 ms
   query. So shortest working code wins, not fastest.
2. **A pgx helper only helps where it fits.** No mapper can express shapes 2 or 3, and
   shape 4 has a single column with no struct to map. Three of the five readers are also
   shared helpers whose callers want different things.

That is why the answer differs per reader rather than being one blanket rule: shape 1 is
the only place where a short option exists.

### 1. `readQueryRows` — use a mapper via `pgx.AppendRows`

**Scope.** `service/query/query.go:164`. Three callers, all consuming
`[]*committerpb.Row`. No call site changes.

**Readability.** 9-line body → 2 lines. This is the only reader of the five where a
mapper fits, so it is the entire LOC prize in this issue.

**Performance.** Parsing gets 48–95% slower, but parsing is ~1 ms of a ~21 ms query, so
end-to-end this is indistinguishable from today. `pgx.RowToAddrOfStructByPos` adds no
allocations. `pgx.RowToAddrOfStructByName` adds one per row, and in exchange matches
columns by name — more robust than the positional `Scan` we use today.

Use `pgx.AppendRows`, not `pgx.CollectRows`. One extra line keeps the size hint all
three callers already pass, worth 228 KB per 10k-row query.

Two of the three callers are config reads returning ~1 row. There the mappers cost at
most one extra allocation, so the change is safe across all three.

**Why not the others.** Bare `pgx.CollectRows` drops the size hint all three callers pass
(+228 KB per 10k rows). Either collector with a hand-written closure is 5 lines *and* no
faster than today — the closure must build a message per row, which re-escapes the
variadic `[]any`. `pgx.ForEachRow` and the hoisted loop are 8 and 11 lines; they are 17%
faster, but that is the 17% nobody can observe. By-value `pgx.RowToStructByName` copies
`protoimpl.MessageState`.

### 2. `readStatusWithHeight` + `readTxStatusRows` — hoist the scan destinations

**Scope.** `service/vc/database.go:451` and `service/query/query.go:176`. Five callers.
No signature change, so test call sites stay put.

**Readability.** Unchanged. Same LOC, same `gocognit`, same `gocyclo`. Four lines move
above the loop.

**Performance.** 32% faster, 40% fewer allocations. Nothing is traded away. The two
batched callers get the full win; the two single-row callers are unaffected.

**Why not the others.** The mappers cannot express this shape: three columns collapse into
a nested `TxRef`, and `height` needs `servicepb.NewHeightFromBytes`. Measured —
`pgx.RowToAddrOfStructByName[committerpb.TxStatus]` errors with `struct doesn't have
corresponding row field tx_id`. `pgx.AppendRows` with a closure is 17 lines and no faster:
the per-row locals move inside the closure and still escape every row. `pgx.ForEachRow` is
the real near-tie — same numbers, 3 lines shorter, better `gocognit` — and loses only on
the guidelines' callback rule. Worth taking if review prefers it.

### 3. `queryVersionsIfPresent` — give it its own map reader

**Scope.** `service/vc/database.go:158` only. `readTwoItems` has four callers wanting
three different results, so it stays as-is. This adds a reader beside it.

**Readability.** Costs code: **net +7 lines**. The new reader is 11 lines versus the 19
it replaces per call, but those 19 cannot be deleted.

**Performance.** 2.5× faster, 2.5× fewer allocations on the one hot caller. Today it
builds two slices, folds them into a map, and drops them. The new reader scans straight
into the map. The gain scales with batch size; at one row it is a wash.

**Why not the others.** No collector can return a map — `pgx.CollectRows` and
`pgx.AppendRows` build slices. `pgx.RowToMap` looks like the fit and is not: it returns one
`map[string]any` *per row, keyed by column name*, so you get `[]map[string]any` rather than
key→version, and it boxes every value through `rows.Values()`. `pgx.ForEachRow` ties on
numbers and is 3 lines shorter — the same style call as (2).

### 4. `readArrayResult` — leave it

`pgx.CollectOneRow` with `pgx.RowTo` is the only helper that fits — one column, no struct
to map. It saves two lines at break-even speed, because the array arrives as a single cell
and one bulk codec call decodes it either way. But it needs `pgx.Rows`, so those two lines
would move all three callers from `QueryRow` to `Query`, each then owning row cleanup. Not
worth it.

### Do not adopt anywhere

The two options the issue's blog post leads with both fail on every shape, not just one:

- **`pgx.CollectRows` / `pgx.AppendRows` with a hand-written closure.** Rejected above for
  shapes 1, 2 and 3, each time for the same reason: the closure must build the row's
  output object, so the variadic `[]any` stays on the per-row path and the saving cancels.
- **By-value `pgx.RowToStructByName` / `…ByPos` on a protobuf message.** They copy
  `protoimpl.MessageState`, which protobuf-go forbids, and `go vet` does not catch it. Only
  the `RowToAddrOf*` variants are safe on our types.

### For review

**Consistency.** A mapper in `readQueryRows` plus a hand-written loop in
`readTxStatusRows` means two idioms in one file, since shapes 2 and 3 have no mapper
option. The hoisted loop is the only style covering all three, and is also the fastest.
Its cost: on shape 1 at one row it adds 3 allocations, which the mappers do not — so
"hoist everywhere" mildly penalises the two config-read callers.

**Evidence limit.** Allocation counts are exact; their consequence is not measured.
Single-query latency is unaffected, so caring about 10–20k allocations per query rests on
sustained GC pressure — and I ran no sustained-load test. Treat it as a hypothesis. If it
decides (1), measure first; happy to do that.

**The one rejection I hold loosely** is `pgx.ForEachRow` in (2) and (3). It ties the
hoisted loop on every number and is 3 lines shorter. I rejected it only because
`CLAUDE.md` lists *"no callbacks/functions-as-arguments"* among the simplicity rules, and
because the complexity metrics flatter it for reasons a reader doesn't feel. `retry.ExecuteWithResult` is precedent the other way. Reviewers' call.

I'd scope (1)–(3) as one PR, benchmarks included. The evaluation below gives the caller
inventory, method, and per-shape code and numbers.

---

# Evaluation

We use `github.com/yugabyte/pgx/v5 v5.7.6-yb-1`. It already has the `sync.Map`
field-mapping caches that earlier versions lacked, so this measures the good version of
these helpers.

## Callers

Three readers are shared. A change to any of them must suit every caller.

**`readQueryRows`** — `service/query/query.go:164`. All three callers take
`[]*committerpb.Row`, so a substitution lands uniformly.

| caller | rows | hot? |
|---|---|---|
| `query.go:106` `unsafeQueryRows` | `len(keys)`, up to 10k | **yes** |
| `query.go:128` `queryPolicies` | all of `ns__meta`; hint of 1 | no — config |
| `query.go:152` `queryConfig` | ~1 row; hint of 1 | no — config |

**`readTxStatusRows`** — `service/query/query.go:176`. One caller, `query.go:119`
`unsafeQueryTxStatus`. Hot.

**`readStatusWithHeight`** — `service/vc/database.go:451`.

| caller | rows | hot? |
|---|---|---|
| `validator_committer_service.go:222` `GetTransactionsStatus` | batched | **yes** |
| `committer.go:250` duplicate resolution | batched | **yes**, when duplicates exist |
| `snapshot.go:85` | exactly 1 | no |
| `snapshot.go:256` | exactly 1 | no |

**`readTwoItems`** — `service/vc/database.go:550`, via `retryQueryAndReadTwoItems`. Four
callers, three different needs. Only one is hot.

| caller | wants | hot? |
|---|---|---|
| `database.go:158` `queryVersionsIfPresent` | both, folded into a `keyToVersion` map | **yes** |
| `database.go:484` `readNamespacePolicies` | both, as `[]*PolicyItem` | no — config |
| `database.go:503` `readConfigTX` | values only; discards keys | no — config |
| `snapshot.go:162` | values only; discards keys | no — recovery |

**`readArrayResult`** — `service/vc/database.go:563`. All three callers get their row from
`QueryRow`, which is what makes a switch to `pgx.Rows` a three-site change:
`database.go:337` `insertTxStatus`, `database.go:374` `insertStates`, and
`database.go:139` `validateNamespaceReads` via `retryQueryAndReadArrayResult`.

Tests also call three of these directly. No change below alters a signature, so no test
call site moves. The one that would — `readArrayResult` to `pgx.Rows` — is the one I
advise against.

## Method

Every variant is a real function with the production signature, in a local
`variants.go`. The benchmarks call those functions, so the code quoted here is the code
measured. These files are not in the repo yet — I'll include them in the PR if we go
ahead.

Two harnesses:

1. **Isolated.** A `pgx.Rows` stand-in holding pre-encoded wire bytes, decoded through a
   real `*pgtype.Map`. It replicates `baseRows.Scan`, scan-plan cache and arity guards
   included. No network, no server. Rows always arrive as the `pgx.Rows` *interface*, never
   a concrete type: `Scan` is variadic, so behind an interface its argument slice always
   escapes, and giving one variant a concrete type would flatter it by an allocation per
   row. Shape 4's reader takes `pgx.Row`, so it gets a `connRow`-shaped stand-in that does
   `Err`/`DriverBytes`/`Next`/`Scan`/`Close` in one call.
2. **Real PostgreSQL.** The same functions against a 10k-row `ns_` table over loopback.

Isolated timings are medians of 10 runs at `rows=10000` on an Apple M1 Max. `allocs/row`
is exact and stable. `LOC` is **body only**: no signature, doc comment or closing brace,
since those are identical across variants. `cognit`/`cyclo` are `gocognit`/`gocyclo`; the
repo lints `gocognit` at 15.

One caveat on the complexity columns: both tools count branches inside closures, but
neither charges for the callback indirection, so a branch-free callback scores 0/1 and
looks simpler than it reads. Weigh `cognit` with `LOC`, not alone.

`golangci-lint` reports nothing on `variants.go`. The only `//nolint` is the `gosec`
int-conversion suppression `queryVersionsIfPresent` already carries.

---

## Shape 1 — `key, value, version` → `[]*committerpb.Row`

`readQueryRows`. **Today:**

```go
func readQueryRows(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	rows := make([]*committerpb.Row, 0, expectedSize)
	for r.Next() {
		v := &committerpb.Row{}
		if err := r.Scan(&v.Key, &v.Value, &v.Version); err != nil {
			return nil, err
		}
		rows = append(rows, v)
	}
	return rows, r.Err()
}
```

**`manual-hoisted`** — fastest, fewest allocations. The `scan` slice must be hoisted too,
not just the variables: `Scan` is variadic behind an interface, so a fresh argument slice
per row would otherwise be heap-allocated.

```go
func readQueryRows(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	var key, value []byte
	var version uint64
	scan := []any{&key, &value, &version}
	rows := make([]*committerpb.Row, 0, expectedSize)
	for r.Next() {
		if err := r.Scan(scan...); err != nil {
			return nil, err
		}
		rows = append(rows, &committerpb.Row{Key: key, Value: value, Version: version})
	}
	return rows, r.Err()
}
```

**`pgx.ForEachRow`** — same result, pgx spelling:

```go
func readQueryRows(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	var key, value []byte
	var version uint64
	rows := make([]*committerpb.Row, 0, expectedSize)
	_, err := pgx.ForEachRow(r, []any{&key, &value, &version}, func() error {
		rows = append(rows, &committerpb.Row{Key: key, Value: value, Version: version})
		return nil
	})
	return rows, err
}
```

**`pgx.AppendRows` / `pgx.CollectRows` with a closure** — what the issue proposes. The
closure must build a fresh message per row, which puts the variadic `[]any` back on the
per-row path and cancels the saving.

```go
func readQueryRows(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	return pgx.AppendRows(make([]*committerpb.Row, 0, expectedSize), r,
		func(row pgx.CollectableRow) (*committerpb.Row, error) {
			v := &committerpb.Row{}
			return v, row.Scan(&v.Key, &v.Value, &v.Version)
		})
}

// pgx.CollectRows is the same minus the size hint, so it also re-grows the result slice.
func readQueryRows(r pgx.Rows, _ int) ([]*committerpb.Row, error) {
	return pgx.CollectRows(r, func(row pgx.CollectableRow) (*committerpb.Row, error) {
		v := &committerpb.Row{}
		return v, row.Scan(&v.Key, &v.Value, &v.Version)
	})
}
```

**The mappers.** These drop `expectedSize`, which all three callers supply:

```go
func readQueryRows(r pgx.Rows, _ int) ([]*committerpb.Row, error) {
	return pgx.CollectRows(r, pgx.RowToAddrOfStructByName[committerpb.Row])
}

func readQueryRows(r pgx.Rows, _ int) ([]*committerpb.Row, error) {
	return pgx.CollectRows(r, pgx.RowToAddrOfStructByPos[committerpb.Row])
}
```

**The mappers via `pgx.AppendRows`** — one more line, keeps the size hint. Recommended:

```go
func readQueryRows(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	return pgx.AppendRows(make([]*committerpb.Row, 0, expectedSize), r,
		pgx.RowToAddrOfStructByName[committerpb.Row])
}

func readQueryRows(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	return pgx.AppendRows(make([]*committerpb.Row, 0, expectedSize), r,
		pgx.RowToAddrOfStructByPos[committerpb.Row])
}
```

At `rows=10000`, the size the hot caller reaches:

| variant | µs/op | vs today | allocs/row | B/op | LOC | cognit | cyclo |
|---|---|---|---|---|---|---|---|
| `manual-before` (today) | 1211 | — | 4.0 | 3,122,019 | 9 | 3 | 3 |
| **`manual-hoisted`** | **1007** | **−16.8%** | **3.0** | 2,642,124 | 11 | 3 | 3 |
| `pgx.ForEachRow` | 1011 | −16.5% | 3.0 | 2,642,125 | 8 | 0 | 1 |
| `pgx.AppendRows` | 1222 | +1.0% | 4.0 | 3,122,018 | 5 | 0 | 1 |
| `pgx.CollectRows` | 1281 | +5.8% | 4.0 | 3,350,435 | 4 | 0 | 1 |
| `pgx.AppendRows`+`RowToAddrOfStructByPos` | 1793 | +48.1% | 4.0 | 3,122,019 | 2 | 0 | 1 |
| `pgx.RowToAddrOfStructByPos` | 1834 | +51.5% | 4.0 | 3,350,435 | **1** | 0 | 1 |
| `pgx.AppendRows`+`RowToAddrOfStructByName` | 2357 | +94.7% | 5.0 | 3,362,019 | 2 | 0 | 1 |
| `pgx.RowToAddrOfStructByName` | 2399 | +98.2% | 5.0 | 3,590,436 | **1** | 0 | 1 |

Pairing a mapper with `pgx.AppendRows` costs one line and recovers the 228 KB that
re-growing an unsized slice wastes. `ByPos` then matches today's byte count exactly.

At `rows=1`, what `queryPolicies` and `queryConfig` hint at:

| variant | µs/op | vs today | allocs/op |
|---|---|---|---|
| `manual-before` (today) | 0.30 | — | 7 |
| `manual-hoisted` | 0.37 | **+23.1%** | 10 |
| `pgx.ForEachRow` | 0.38 | +25.5% | 10 |
| `pgx.AppendRows`+`RowToAddrOfStructByPos` | 0.39 | +28.2% | 7 |
| `pgx.AppendRows`+`RowToAddrOfStructByName` | 0.46 | +51.7% | 8 |

Hoisting regresses single-row reads: the hoisted destinations and `scan` slice are pure
overhead with no second row to amortise them, and today's code scans straight into the
message's fields. Two of three callers are in that regime, but both are config reads. The
mappers have no such crossover.

### Control: the same comparison on a plain struct

`pgx.RowToStructByName` returns `T` by value, which is unsafe for a protobuf message, so
the proto variants above use `RowToAddrOf*`. This control shows the reflection cost is
not an artifact of that indirection:

```go
type plainRow struct {
	Key     []byte
	Value   []byte
	Version uint64
}

func plainRowsStructByName(r pgx.Rows, expectedSize int) ([]plainRow, error) {
	return pgx.AppendRows(make([]plainRow, 0, expectedSize), r, pgx.RowToStructByName[plainRow])
}
```

| variant | µs/op | vs today | allocs/row | LOC | cognit |
|---|---|---|---|---|---|
| `manual-before` | 1172 | — | 4.0 | 9 | 3 |
| `pgx.RowToStructByPos` | 1847 | +57.6% | 4.0 | 1 | 0 |
| `pgx.RowToStructByName` | 2382 | +103.3% | 5.0 | 1 | 0 |

---

## Shape 2 — `tx_id, status, height` → `[]*committerpb.TxStatus`

`readTxStatusRows` and `readStatusWithHeight` — the same loop with different error
wrapping. **Today:**

```go
func readTxStatusRows(r pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error) {
	rows := make([]*committerpb.TxStatus, 0, expectedSize)
	for r.Next() {
		var id []byte
		var status int32
		var height []byte

		if err := r.Scan(&id, &status, &height); err != nil {
			return nil, errors.Wrap(err, "failed to read rows from the query result")
		}

		ht, _, err := servicepb.NewHeightFromBytes(height)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create height")
		}

		rows = append(rows, &committerpb.TxStatus{
			Ref:    committerpb.NewTxRef(string(id), ht.BlockNum, ht.TxNum),
			Status: committerpb.Status(status),
		})
	}
	return rows, r.Err()
}
```

This is the worst offender. `id`, `status` and `height` are declared *inside* the loop and
their addresses go into an interface-typed variadic `Scan`, so all three escape and the
`[]any` is rebuilt every iteration. That is 4 extra allocations and ~100 extra bytes per
row.

**`manual-hoisted`** — same 18 lines, four moved:

```go
func readTxStatusRows(r pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error) {
	var id, height []byte
	var status int32
	scan := []any{&id, &status, &height}
	rows := make([]*committerpb.TxStatus, 0, expectedSize)
	for r.Next() {
		if err := r.Scan(scan...); err != nil {
			return nil, errors.Wrap(err, "failed to read rows from the query result")
		}
		ht, _, err := servicepb.NewHeightFromBytes(height)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create height")
		}
		rows = append(rows, &committerpb.TxStatus{
			Ref:    committerpb.NewTxRef(string(id), ht.BlockNum, ht.TxNum),
			Status: committerpb.Status(status),
		})
	}
	return rows, r.Err()
}
```

**`pgx.ForEachRow`:**

```go
func readTxStatusRows(r pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error) {
	var id, height []byte
	var status int32
	rows := make([]*committerpb.TxStatus, 0, expectedSize)
	_, err := pgx.ForEachRow(r, []any{&id, &status, &height}, func() error {
		ht, _, err := servicepb.NewHeightFromBytes(height)
		if err != nil {
			return errors.Wrap(err, "failed to create height")
		}
		rows = append(rows, &committerpb.TxStatus{
			Ref:    committerpb.NewTxRef(string(id), ht.BlockNum, ht.TxNum),
			Status: committerpb.Status(status),
		})
		return nil
	})
	return rows, err
}
```

**`pgx.AppendRows`** — the per-row locals move into the closure, so they still escape
every row and nothing is saved:

```go
func readTxStatusRows(r pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error) {
	return pgx.AppendRows(make([]*committerpb.TxStatus, 0, expectedSize), r,
		func(row pgx.CollectableRow) (*committerpb.TxStatus, error) {
			var id []byte
			var status int32
			var height []byte
			if err := row.Scan(&id, &status, &height); err != nil {
				return nil, errors.Wrap(err, "failed to read rows from the query result")
			}
			ht, _, err := servicepb.NewHeightFromBytes(height)
			if err != nil {
				return nil, errors.Wrap(err, "failed to create height")
			}
			return &committerpb.TxStatus{
				Ref:    committerpb.NewTxRef(string(id), ht.BlockNum, ht.TxNum),
				Status: committerpb.Status(status),
			}, nil
		})
}
```

At `rows=10000`, the size the two batched callers reach:

| variant | µs/op | vs today | allocs/row | LOC | cognit | cyclo |
|---|---|---|---|---|---|---|
| `manual-before` (today) | 2234 | — | 10.0 | 18 | 5 | 4 |
| **`manual-hoisted`** | **1507** | **−32.6%** | **6.0** | 18 | 5 | 4 |
| `pgx.ForEachRow` | 1524 | −31.8% | 6.0 | 15 | 2 | 2 |
| `pgx.AppendRows` | 2178 | −2.5% | 10.0 | 17 | 4 | 3 |
| the mappers | — | *not expressible* | | | | |

At `rows=1`, what the two snapshot callers pass:

| variant | µs/op | vs today | allocs/op |
|---|---|---|---|
| `manual-before` (today) | 0.34 | — | 13 |
| `manual-hoisted` | 0.35 | **+1.6%** | 13 |
| `pgx.AppendRows` | 0.37 | +7.6% | 13 |
| `pgx.ForEachRow` | 0.38 | +10.0% | 13 |

No crossover here, unlike shape 1: the allocation count is identical at one row, because
today's code already declares the destinations inside the loop and hoisting only moves
them. `pgx.ForEachRow` is the one shape where the pgx helper has a readability argument —
3 lines shorter and better on both complexity metrics for the same runtime.

The mappers cannot express this shape. Three columns collapse into a nested `TxRef`, and
`height` needs `servicepb.NewHeightFromBytes`. Measured:
`pgx.RowToAddrOfStructByName[committerpb.TxStatus]` fails with `struct doesn't have
corresponding row field tx_id`.

---

## Shape 3 — `key, version` → `map[string]uint64`

Here the caller inventory changes the answer, so read the measurement and its
applicability separately.

`readTwoItems` is a shared generic helper with four callers. Only
`queryVersionsIfPresent` folds the pair into a map, and it is the only hot one.
`readNamespacePolicies` needs both slices. `readConfigTX` and `snapshot.go:162` take
`_, values` and discard the keys. **So `readTwoItems` cannot be replaced.** What follows
applies to one of its four callers.

**Today** — the helper plus that caller's fold. Neither slice is pre-sized:

```go
// service/vc/database.go
func readTwoItems[T1, T2 any](r pgx.Rows) (items1 []T1, items2 []T2, err error) {
	for r.Next() {
		var i1 T1
		var i2 T2
		if err := r.Scan(&i1, &i2); err != nil {
			return nil, nil, errors.Wrap(err, "failed while scanning a row")
		}
		items1 = append(items1, i1)
		items2 = append(items2, i2)
	}
	return items1, items2, errors.Wrap(r.Err(), "failed while reading from rows")
}

// ... and in queryVersionsIfPresent:
foundKeys, foundVersions, err := retryQueryAndReadTwoItems[[]byte, int64](ctx, d, query, queryKeys)
kToV := make(keyToVersion)
for i, key := range foundKeys {
	kToV[string(key)] = uint64(foundVersions[i])
}
```

**`manual-directToMap`** — a dedicated reader for that one caller. `readTwoItems` stays
untouched for the other three:

```go
func readKeyVersions(r pgx.Rows, expectedSize int) (keyToVersion, error) {
	var k []byte
	var v int64
	scan := []any{&k, &v}
	kToV := make(keyToVersion, expectedSize)
	for r.Next() {
		if err := r.Scan(scan...); err != nil {
			return nil, errors.Wrap(err, "failed while scanning a row")
		}
		kToV[string(k)] = uint64(v) //nolint:gosec // non-negative by DB constraint.
	}
	return kToV, errors.Wrap(r.Err(), "failed while reading from rows")
}
```

**`pgx.ForEachRow`:**

```go
func readKeyVersions(r pgx.Rows, expectedSize int) (keyToVersion, error) {
	var k []byte
	var v int64
	kToV := make(keyToVersion, expectedSize)
	_, err := pgx.ForEachRow(r, []any{&k, &v}, func() error {
		kToV[string(k)] = uint64(v) //nolint:gosec // non-negative by DB constraint.
		return nil
	})
	return kToV, err
}
```

| variant | µs/op | vs today | allocs/row | LOC | cognit | cyclo |
|---|---|---|---|---|---|---|
| `manual-before` (helper + fold) | 1749 | — | 5.0 | 19 | 5 | 5 |
| **`manual-directToMap`** | **695** | **−60.3%** | **2.0** | 11 | 3 | 3 |
| `pgx.ForEachRow` | 706 | −59.6% | 2.0 | 8 | 0 | 1 |

At one row the advantage disappears (+3.4%, same allocations), so the gain tracks batch
size — which suits this caller, driven by a validation batch's read set.

**The LOC column is not a saving.** `readTwoItems` must stay for its three other callers,
so this adds an 11-line reader rather than shrinking a 19-line one: net **+7 lines**. The
19/11 figures measure work per call, not the diff.

Also surfaced, not proposed here: `readConfigTX` and `snapshot.go:162` discard the keys
they ask for, so they could take a one-column reader.

---

## Shape 4 — one `BYTEA[]`/`INT[]` cell → `[]T`

`readArrayResult`. This is the only shape where the two variants take different
interfaces: today's reader takes `pgx.Row` (from `QueryRow`), and `pgx.CollectOneRow`
needs `pgx.Rows` (from `Query`). **Today:**

```go
func readArrayResult[T any](r pgx.Row) (res []T, err error) {
	err = r.Scan(&res)
	return res, errors.Wrap(err, "failed while scanning a row")
}
```

**`pgx.CollectOneRow`** — note the parameter type change:

```go
func readArrayResult[T any](r pgx.Rows) ([]T, error) {
	return pgx.CollectOneRow(r, pgx.RowTo[[]T])
}
```

| variant | µs/op | vs today | allocs/op | LOC | cognit |
|---|---|---|---|---|---|
| `pgx.Row.Scan` (today) | 137.0 | — | 13 | 3 | 0 |
| `pgx.CollectOneRow` | 137.4 | +0.3% | 13 | 1 | 0 |

A wash: the array arrives as one column and is decoded in one bulk codec call either way,
so neither variant has a per-element loop to optimise. Today's number is measured through
a `pgx.Row` that does the full one-row lifecycle inside its own `Scan`, as pgx's `connRow`
does, so it is charged for the wrapper it really pays.

---

## Against real PostgreSQL, 10k-row query

Each row is one `testing.Benchmark` run, not a median. Read the timings as indicative and
the allocation counts as exact.

| variant | µs/op | vs today | allocs/op |
|---|---|---|---|
| `next-only` — no decoding at all | 22,013 | +7.6% | 10,036 |
| `manual-before` | 20,462 | — | 50,039 |
| `manual-hoisted` | 21,922 | +7.1% | 40,043 |
| `pgx.ForEachRow` | 20,399 | −0.3% | 40,043 |
| `pgx.AppendRows` | 20,919 | +2.2% | 50,039 |
| `pgx.CollectRows` | 20,494 | +0.2% | 50,053 |
| `pgx.RowToAddrOfStructByPos` | 20,559 | **+0.5%** | 50,053 |
| `pgx.RowToAddrOfStructByName` | 21,317 | **+4.2%** | 60,053 |

Allocation counts reproduce the isolated numbers exactly: 40,043 / 50,039 / 60,053, i.e.
3, 4 and 5 per row plus fixed overhead.

The timings establish a noise floor, not a ranking. `manual-hoisted` and
`pgx.ForEachRow` are allocation-identical and do the same work, yet land 7.5% apart. And
`next-only`, which decodes nothing, came out slowest of all. The spread is about ±7%,
which swallows every difference: a 10k-key query costs ~20–22 ms, of which parsing is
~1 ms, so even `RowToAddrOfStructByName`'s +96% on parsing is ~4% of the query.

**That is the ceiling on this discussion. None of these choices is a throughput cliff,
and the +0.5% / +4.2% above mean "indistinguishable from today", not a measured
ordering.**

Allocation volume is what does not wash out: 10–20k per query, every batch. That is where
the variants really differ — subject to the caveat that its consequence is unmeasured.

## Where the mappers fit

Cost is unobservable end-to-end, so it should not decide this. What limits these two
functions is where they fit at all:

| reader | mapper? | why |
|---|---|---|
| `readQueryRows` | **yes** | 3 columns map 1:1 onto `Key`/`Value`/`Version` |
| `readTxStatusRows`, `readStatusWithHeight` | no | columns collapse into a nested `TxRef`; `height` needs custom decoding |
| `readTwoItems` / `queryVersionsIfPresent` | no | `CollectRows[T]` returns one slice; the caller wants a map |
| `readArrayResult` | n/a | single column; already a 3-line body |

One constraint on which mapper. The by-value variants copy protobuf messages:
`pgx.RowToStructByName[T]` returns `T`, and `AppendRows` copies it into the slice,
copying `protoimpl.MessageState`. protobuf-go forbids that. `go vet`'s copylocks pass does
not catch it, because the copy sits in pgx's generic code where `T` is still a type
parameter, and it does not visibly break in a narrow test — a silent hazard. For proto
rows, only `pgx.RowToAddrOfStructByName` and `pgx.RowToAddrOfStructByPos` are safe.

### How the mappers fail when schema or struct drifts

These readers are on the main path, and reflection moves some breakage from compile time
to run time. So:

- **`RowToAddrOfStructByPos` fails loudly on a field-count change.** pgx compares column
  count to destination count, so a regenerated message with an extra field errors with
  `number of field descriptions must equal number of destinations, got 3 and 4`.
- **It is silent under a same-count reordering**, where `key` and `value` are both `BYTEA`
  so no type error fires. Today's positional `r.Scan(&v.Key, &v.Value, &v.Version)`
  corrupts identically, so this is not a new failure mode.
- **`RowToAddrOfStructByName` is more robust than today's code.** It matches on column
  name, so it tolerates the SQL template's columns being reordered — which today's
  positional `Scan` silently would not — and fails loudly on an unmatched field.

Tests: `TestStructByPosFailsLoudlyOnFieldCountChange`,
`TestStructByPosIsSilentOnSameCountReordering`, `TestStructByNameSurvivesBothChanges`.

Where the cost comes from: both mappers call `setupStructScanTargets` per row —
`reflect.ValueOf(...).Elem()`, then `FieldByIndex().Addr().Interface()` per field, plus a
fresh `[]any`. `ByName` also rebuilds its cache key via `joinFieldNames(fldDescs)` every
row: a 24-byte string allocation plus a `sync.Map` lookup, to re-derive a mapping that
cannot have changed. That is the whole gap between `ByPos` (+48%) and `ByName` (+96%).

## Why reusing scan destinations is safe

Hoisting means every row decodes into the same `key`/`value` variables. If pgtype decoded
`BYTEA` in place, all collected rows would alias the last row's bytes. It does not:

```go
func (scanPlanBinaryBytesToBytes) Scan(src []byte, dst any) error {
	dstBuf := dst.(*[]byte)
	if src == nil {
		*dstBuf = nil
		return nil
	}

	*dstBuf = make([]byte, len(src))
	copy(*dstBuf, src)
	return nil
}
```

That per-row allocation is also why the hoisted variants still show 3 allocations per row
rather than 1. The two `[]byte` columns are freshly allocated regardless; hoisting removes
the escaping slice headers and the variadic `[]any`.

`TestReusedScanTargetsDoNotAlias` asserts that adjacent rows hold distinct backing
arrays, and fails if a future pgx version starts decoding into the existing buffer.

---

<sup>Benchmark sources aren't committed yet; happy to include them in the PR or drop
them.</sup>
