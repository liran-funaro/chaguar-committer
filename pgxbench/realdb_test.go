/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package pgxbench

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"

	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

// TestRealDBRowScanCost measures each row-collection variant against a real
// PostgreSQL instance, so the per-row deltas measured in the isolated benchmarks
// can be weighed against the query's total cost (round trip + planning +
// execution + wire transfer). The `next_only` variant reads the result set
// without decoding it and is the floor: everything above it is parsing.
//
// It is a Test rather than a Benchmark because the DB fixtures need *testing.T;
// the measurements themselves run through testing.Benchmark.
func TestRealDBRowScanCost(t *testing.T) {
	t.Parallel()
	pool := setupBenchTable(t)

	for _, n := range []int{1, 128, 1024, 10000} {
		keys := benchKeys(n)
		t.Logf("--- rows=%d ---", n)
		for _, tc := range []struct {
			name string
			read func(rows pgx.Rows, expectedSize int) ([]*committerpb.Row, error)
		}{
			{name: "next-only (no decode)", read: readNextOnly},
			{name: nameBefore, read: queryRowsBefore},
			{name: nameHoisted, read: queryRowsHoisted},
			{name: nameForEachRow, read: queryRowsForEachRow},
			{name: nameAppendRows, read: queryRowsAppendRows},
			{name: nameCollectRows, read: queryRowsCollectRows},
			{name: nameStructByPos, read: queryRowsStructByPos},
			{name: nameStructByName, read: queryRowsStructByName},
		} {
			res := benchmarkAgainstDB(pool, keys, n, tc.read)
			t.Logf("%-30s %10.1f µs/op %10d B/op %8d allocs/op",
				tc.name,
				float64(res.NsPerOp())/1000,
				res.AllocedBytesPerOp(),
				res.AllocsPerOp(),
			)
		}
	}
}

// benchmarkAgainstDB times one reader over a real query returning n rows. The query is
// inside the timed loop on purpose: the point of this harness is to weigh the reader's
// cost against the round trip it is attached to.
func benchmarkAgainstDB(
	pool *pgxpool.Pool,
	keys [][]byte,
	n int,
	read func(rows pgx.Rows, expectedSize int) ([]*committerpb.Row, error),
) testing.BenchmarkResult {
	return testing.Benchmark(func(b *testing.B) {
		b.Helper()
		b.ReportAllocs()
		for b.Loop() {
			rows, err := pool.Query(b.Context(), benchQuery, keys)
			if err != nil {
				b.Fatal(err)
			}
			out, readErr := read(rows, n)
			if readErr != nil {
				b.Fatal(readErr)
			}
			sinkRows = out
			rows.Close()
			if err := rows.Err(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// readNextOnly drains the result set without decoding it: the floor against which
// every decoding variant is measured.
func readNextOnly(rows pgx.Rows, _ int) ([]*committerpb.Row, error) {
	count := 0
	for rows.Next() {
		count++
	}
	sinkInt = count
	return nil, rows.Err()
}

var sinkInt int

const (
	benchTableDDL = `CREATE TABLE IF NOT EXISTS ns_bench (
	key     BYTEA NOT NULL PRIMARY KEY,
	value   BYTEA DEFAULT NULL,
	version BIGINT DEFAULT 0::BIGINT NOT NULL CHECK (version >= 0)
);`
	// Mirrors service/query/query_tmpl.sql.
	benchQuery = `SELECT key, value, version FROM ns_bench WHERE key = any($1);`
	benchRows  = 10000
)

// setupBenchTable creates ns_bench with benchRows rows shaped like real state.
func setupBenchTable(t *testing.T) *pgxpool.Pool {
	t.Helper()
	conn := testdb.PrepareTestEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	pool, err := statedb.NewPool(ctx, &statedb.Config{
		Endpoints:      conn.Endpoints,
		Username:       conn.User,
		Password:       conn.Password,
		Database:       conn.Database,
		MaxConnections: 10,
		MinConnections: 1,
		LoadBalance:    conn.LoadBalance,
		TLS:            conn.TLS,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, benchTableDDL)
	require.NoError(t, err)

	keys := benchKeys(benchRows)
	values := make([][]byte, benchRows)
	versions := make([]int64, benchRows)
	for i := range values {
		values[i] = make([]byte, valueSize)
		copy(values[i], keys[i])
		versions[i] = int64(i % 1000)
	}
	_, err = pool.Exec(ctx, `INSERT INTO ns_bench (key, value, version)
SELECT * FROM unnest($1::BYTEA[], $2::BYTEA[], $3::BIGINT[]) ON CONFLICT DO NOTHING;`,
		keys, values, versions)
	require.NoError(t, err)

	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM ns_bench;`).Scan(&count))
	require.Equal(t, benchRows, count)
	t.Logf("ns_bench populated with %d rows", count)
	return pool
}

// benchKeys returns the first n keys, matching what setupBenchTable inserted.
func benchKeys(n int) [][]byte {
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = fmt.Appendf(nil, "%0*d", keySize, i)
	}
	return keys
}
