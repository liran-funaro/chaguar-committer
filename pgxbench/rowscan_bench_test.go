/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package pgxbench

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgconn"
	"github.com/yugabyte/pgx/v5/pgtype"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

const (
	keySize   = 32
	valueSize = 128
)

var rowCounts = []int{1, 128, 1024, 10000}

// Each benchmark table holds the variant functions from variants.go, so the code
// quoted in the report is the code measured. The function-valued table field is a
// deliberate exception to the repo's "no callbacks as parameters" rule: the
// function under test *is* the parameter here.

func BenchmarkQueryRows(b *testing.B) {
	for _, tc := range []struct {
		name string
		read func(rows pgx.Rows, expectedSize int) ([]*committerpb.Row, error)
	}{
		{name: nameBefore, read: queryRowsBefore},
		{name: nameHoisted, read: queryRowsHoisted},
		{name: nameForEachRow, read: queryRowsForEachRow},
		{name: nameAppendRows, read: queryRowsAppendRows},
		{name: nameCollectRows, read: queryRowsCollectRows},
		{name: nameStructByName, read: queryRowsStructByName},
		{name: nameStructByPos, read: queryRowsStructByPos},
		{name: nameAppendRows + "+RowToAddrOfStructByName", read: queryRowsStructByNameSized},
		{name: nameAppendRows + "+RowToAddrOfStructByPos", read: queryRowsStructByPosSized},
	} {
		b.Run("variant="+tc.name, func(b *testing.B) {
			forEachCount(b, newKeyValueVersionRows, func(b *testing.B, r *fakeRows, n int) {
				b.Helper()
				rows, err := tc.read(r, n)
				if err != nil {
					b.Fatal(err)
				}
				sinkRows = rows
			})
		})
	}
}

func BenchmarkPlainRows(b *testing.B) {
	for _, tc := range []struct {
		name string
		read func(rows pgx.Rows, expectedSize int) ([]plainRow, error)
	}{
		{name: nameBefore, read: plainRowsBefore},
		{name: "pgx." + "RowToStructByName", read: plainRowsStructByName},
		{name: "pgx." + "RowToStructByPos", read: plainRowsStructByPos},
	} {
		b.Run("variant="+tc.name, func(b *testing.B) {
			forEachCount(b, newKeyValueVersionRows, func(b *testing.B, r *fakeRows, n int) {
				b.Helper()
				rows, err := tc.read(r, n)
				if err != nil {
					b.Fatal(err)
				}
				sinkPlain = rows
			})
		})
	}
}

func BenchmarkTxStatus(b *testing.B) {
	for _, tc := range []struct {
		name string
		read func(rows pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error)
	}{
		{name: nameBefore, read: txStatusBefore},
		{name: nameHoisted, read: txStatusHoisted},
		{name: nameForEachRow, read: txStatusForEachRow},
		{name: nameAppendRows, read: txStatusAppendRows},
	} {
		b.Run("variant="+tc.name, func(b *testing.B) {
			forEachCount(b, newTxStatusRows, func(b *testing.B, r *fakeRows, n int) {
				b.Helper()
				rows, err := tc.read(r, n)
				if err != nil {
					b.Fatal(err)
				}
				sinkStatus = rows
			})
		})
	}
}

func BenchmarkKeyVersion(b *testing.B) {
	for _, tc := range []struct {
		name string
		read func(rows pgx.Rows, expectedSize int) (map[string]uint64, error)
	}{
		{name: nameBefore, read: keyVersionBefore},
		{name: nameDirectToMap, read: keyVersionDirectToMap},
		{name: nameForEachRow, read: keyVersionForEachRow},
	} {
		b.Run("variant="+tc.name, func(b *testing.B) {
			forEachCount(b, newKeyVersionRows, func(b *testing.B, r *fakeRows, n int) {
				b.Helper()
				kToV, err := tc.read(r, n)
				if err != nil {
					b.Fatal(err)
				}
				sinkMap = kToV
			})
		})
	}
}

// BenchmarkArrayResult covers the VC's single-row array readers. The "before"
// variant takes a pgx.Row, so it is handed a fakeRow -- which, like pgx's connRow,
// does Next/Scan/Close inside its own Scan.
func BenchmarkArrayResult(b *testing.B) {
	b.Run("variant=pgx.Row.Scan", func(b *testing.B) {
		forEachCount(b, newIntArrayRow, func(b *testing.B, r *fakeRows, _ int) {
			b.Helper()
			res, err := arrayResultBefore(fakeRow{rows: r})
			if err != nil {
				b.Fatal(err)
			}
			sinkInts = res
		})
	})
	b.Run("variant=pgx.CollectOneRow", func(b *testing.B) {
		forEachCount(b, newIntArrayRow, func(b *testing.B, r *fakeRows, _ int) {
			b.Helper()
			res, err := arrayResultCollectOneRow(r)
			if err != nil {
				b.Fatal(err)
			}
			sinkInts = res
		})
	})
}

// Sinks keep the compiler from eliding the decoded results.
var (
	sinkRows   []*committerpb.Row
	sinkPlain  []plainRow
	sinkStatus []*committerpb.TxStatus
	sinkInts   []int
	sinkMap    map[string]uint64
)

// forEachCount runs one variant across every row count, rebuilding the result set
// per sub-benchmark and rewinding it per iteration.
func forEachCount(
	b *testing.B,
	newRows func(b testing.TB, n int) *fakeRows,
	run func(b *testing.B, r *fakeRows, n int),
) {
	b.Helper()
	for _, n := range rowCounts {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			r := newRows(b, n)
			b.ReportAllocs()
			for b.Loop() {
				r.reset()
				run(b, r, n)
			}
		})
	}
}

// newKeyValueVersionRows builds the wire image of `SELECT key, value, version FROM ns_x`.
func newKeyValueVersionRows(tb testing.TB, n int) *fakeRows {
	tb.Helper()
	rows := make([][][]byte, n)
	for i := range rows {
		key := make([]byte, keySize)
		binary.BigEndian.PutUint64(key, uint64(i))
		value := make([]byte, valueSize)
		binary.BigEndian.PutUint64(value, uint64(i))
		version := make([]byte, 8)
		binary.BigEndian.PutUint64(version, uint64(i%1000))
		rows[i] = [][]byte{key, value, version}
	}
	return newFakeRows(tb, rows,
		field("key", pgtype.ByteaOID),
		field("value", pgtype.ByteaOID),
		field("version", pgtype.Int8OID),
	)
}

// newTxStatusRows builds the wire image of `SELECT tx_id, status, height FROM tx_status`.
func newTxStatusRows(tb testing.TB, n int) *fakeRows {
	tb.Helper()
	rows := make([][][]byte, n)
	for i := range rows {
		txID := make([]byte, keySize)
		binary.BigEndian.PutUint64(txID, uint64(i))
		status := make([]byte, 4)
		binary.BigEndian.PutUint32(status, 1)
		height := (&servicepb.Height{BlockNum: uint64(i / 100), TxNum: uint32(i % 100)}).ToBytes()
		rows[i] = [][]byte{txID, status, height}
	}
	return newFakeRows(tb, rows,
		field("tx_id", pgtype.ByteaOID),
		field("status", pgtype.Int4OID),
		field("height", pgtype.ByteaOID),
	)
}

// newKeyVersionRows builds the wire image of the queryVersions template's result.
func newKeyVersionRows(tb testing.TB, n int) *fakeRows {
	tb.Helper()
	rows := make([][][]byte, n)
	for i := range rows {
		key := make([]byte, keySize)
		binary.BigEndian.PutUint64(key, uint64(i))
		version := make([]byte, 8)
		binary.BigEndian.PutUint64(version, uint64(i%1000))
		rows[i] = [][]byte{key, version}
	}
	return newFakeRows(tb, rows, field("key", pgtype.ByteaOID), field("version", pgtype.Int8OID))
}

// newIntArrayRow builds the wire image of a stored procedure returning INT[].
func newIntArrayRow(tb testing.TB, n int) *fakeRows {
	tb.Helper()
	return newFakeRows(tb, [][][]byte{{encodeInt4Array(n)}}, field("result", pgtype.Int4ArrayOID))
}

// encodeInt4Array writes the PostgreSQL binary representation of a 1-dimensional
// INT[] holding 1..n.
func encodeInt4Array(n int) []byte {
	buf := make([]byte, 0, 20+n*8)
	buf = binary.BigEndian.AppendUint32(buf, 1) // dimensions
	buf = binary.BigEndian.AppendUint32(buf, 0) // no NULLs
	buf = binary.BigEndian.AppendUint32(buf, uint32(pgtype.Int4OID))
	buf = binary.BigEndian.AppendUint32(buf, uint32(n)) //nolint:gosec // n is a small test size.
	buf = binary.BigEndian.AppendUint32(buf, 1)         // lower bound
	for i := 1; i <= n; i++ {
		buf = binary.BigEndian.AppendUint32(buf, 4)
		buf = binary.BigEndian.AppendUint32(buf, uint32(i))
	}
	return buf
}

func newFakeRows(tb testing.TB, rows [][][]byte, fields ...pgconn.FieldDescription) *fakeRows {
	tb.Helper()
	r := &fakeRows{typeMap: pgtype.NewMap(), fieldDesc: fields, rows: rows}
	r.reset()
	// Sanity-check that the hand-built wire image decodes through the real codecs,
	// so a benchmark can never report a fast path that silently failed to parse.
	require.True(tb, r.Next())
	values, err := r.Values()
	require.NoError(tb, err)
	require.Len(tb, values, len(fields))
	r.reset()
	return r
}

func field(name string, oid uint32) pgconn.FieldDescription {
	return pgconn.FieldDescription{Name: name, DataTypeOID: oid, Format: pgtype.BinaryFormatCode}
}
