/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// This file holds every row-reading variant under comparison, written the way it
// would appear in production -- same signatures and same error-wrapping style as
// the readers it replaces. The benchmarks call these functions directly, so the
// code quoted in the report is exactly the code measured, and `gocognit`/`gocyclo`
// see the same shape a reviewer would.
package pgxbench

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

// plainRow is a non-protobuf mirror of committerpb.Row. The struct-mapping helpers
// return T by value, which is not safe for a protobuf message (its MessageState
// must not be copied), so a plain struct is the only shape they can legally
// produce.
type plainRow struct {
	Key     []byte
	Value   []byte
	Version uint64
}

// ---------------------------------------------------------------------------
// Shape 1: `SELECT key, value, version` -> []*committerpb.Row
// Production original: service/query/query.go readQueryRows.
// ---------------------------------------------------------------------------

// queryRowsBefore is today's readQueryRows, verbatim.
func queryRowsBefore(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
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

// queryRowsHoisted scans into destinations declared once, then copies them into a
// fresh message per row. pgtype allocates a new slice for every BYTEA column, so
// the rows do not alias (TestReusedScanTargetsDoNotAlias).
//
// The []any must be hoisted too, not just the variables: Scan is variadic and
// reached through an interface, so a fresh argument slice would otherwise be
// heap-allocated on every row.
func queryRowsHoisted(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
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

func queryRowsForEachRow(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	var key, value []byte
	var version uint64
	rows := make([]*committerpb.Row, 0, expectedSize)
	_, err := pgx.ForEachRow(r, []any{&key, &value, &version}, func() error {
		rows = append(rows, &committerpb.Row{Key: key, Value: value, Version: version})
		return nil
	})
	return rows, err
}

func queryRowsAppendRows(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	return pgx.AppendRows(make([]*committerpb.Row, 0, expectedSize), r,
		func(row pgx.CollectableRow) (*committerpb.Row, error) {
			v := &committerpb.Row{}
			return v, row.Scan(&v.Key, &v.Value, &v.Version)
		})
}

func queryRowsCollectRows(r pgx.Rows, _ int) ([]*committerpb.Row, error) {
	return pgx.CollectRows(r, func(row pgx.CollectableRow) (*committerpb.Row, error) {
		v := &committerpb.Row{}
		return v, row.Scan(&v.Key, &v.Value, &v.Version)
	})
}

func queryRowsStructByName(r pgx.Rows, _ int) ([]*committerpb.Row, error) {
	return pgx.CollectRows(r, pgx.RowToAddrOfStructByName[committerpb.Row])
}

func queryRowsStructByPos(r pgx.Rows, _ int) ([]*committerpb.Row, error) {
	return pgx.CollectRows(r, pgx.RowToAddrOfStructByPos[committerpb.Row])
}

// queryRowsStructByNameSized and the Pos variant below keep the caller's size hint,
// which CollectRows discards -- all three readQueryRows callers pass one.
func queryRowsStructByNameSized(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	return pgx.AppendRows(make([]*committerpb.Row, 0, expectedSize), r,
		pgx.RowToAddrOfStructByName[committerpb.Row])
}

func queryRowsStructByPosSized(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	return pgx.AppendRows(make([]*committerpb.Row, 0, expectedSize), r,
		pgx.RowToAddrOfStructByPos[committerpb.Row])
}

// plainRowsBefore and the two below repeat the comparison on a non-proto struct,
// so the reflection cost is measured against a by-value baseline rather than
// through the AddrOf indirection.
func plainRowsBefore(r pgx.Rows, expectedSize int) ([]plainRow, error) {
	rows := make([]plainRow, 0, expectedSize)
	for r.Next() {
		var v plainRow
		if err := r.Scan(&v.Key, &v.Value, &v.Version); err != nil {
			return nil, err
		}
		rows = append(rows, v)
	}
	return rows, r.Err()
}

func plainRowsStructByName(r pgx.Rows, expectedSize int) ([]plainRow, error) {
	return pgx.AppendRows(make([]plainRow, 0, expectedSize), r, pgx.RowToStructByName[plainRow])
}

func plainRowsStructByPos(r pgx.Rows, expectedSize int) ([]plainRow, error) {
	return pgx.AppendRows(make([]plainRow, 0, expectedSize), r, pgx.RowToStructByPos[plainRow])
}

// ---------------------------------------------------------------------------
// Shape 2: `SELECT tx_id, status, height` -> []*committerpb.TxStatus
// Production originals: service/query/query.go readTxStatusRows and
// service/vc/database.go readStatusWithHeight (same loop, different wrapping).
// ---------------------------------------------------------------------------

// txStatusBefore is today's readTxStatusRows, verbatim.
func txStatusBefore(r pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error) {
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

// txStatusHoisted is txStatusBefore with the three scan destinations and the
// variadic argument slice declared once instead of per row.
func txStatusHoisted(r pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error) {
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

func txStatusForEachRow(r pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error) {
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

func txStatusAppendRows(r pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error) {
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

// ---------------------------------------------------------------------------
// Shape 3: `SELECT key, version` -> map[string]uint64
// Production original: service/vc/database.go queryVersionsIfPresent, which calls
// the generic readTwoItems and then folds the two slices into a map. The "before"
// here inlines both halves so the comparison covers the same total work.
// ---------------------------------------------------------------------------

func keyVersionBefore(r pgx.Rows, _ int) (map[string]uint64, error) {
	var keys [][]byte
	var versions []int64
	for r.Next() {
		var k []byte
		var v int64
		if err := r.Scan(&k, &v); err != nil {
			return nil, errors.Wrap(err, "failed while scanning a row")
		}
		keys = append(keys, k)
		versions = append(versions, v)
	}
	if err := r.Err(); err != nil {
		return nil, errors.Wrap(err, "failed while reading from rows")
	}
	kToV := make(map[string]uint64)
	for i, key := range keys {
		kToV[string(key)] = uint64(versions[i]) //nolint:gosec // non-negative by DB constraint.
	}
	return kToV, nil
}

func keyVersionDirectToMap(r pgx.Rows, expectedSize int) (map[string]uint64, error) {
	var k []byte
	var v int64
	scan := []any{&k, &v}
	kToV := make(map[string]uint64, expectedSize)
	for r.Next() {
		if err := r.Scan(scan...); err != nil {
			return nil, errors.Wrap(err, "failed while scanning a row")
		}
		kToV[string(k)] = uint64(v) //nolint:gosec // non-negative by DB constraint.
	}
	return kToV, errors.Wrap(r.Err(), "failed while reading from rows")
}

func keyVersionForEachRow(r pgx.Rows, expectedSize int) (map[string]uint64, error) {
	var k []byte
	var v int64
	kToV := make(map[string]uint64, expectedSize)
	_, err := pgx.ForEachRow(r, []any{&k, &v}, func() error {
		kToV[string(k)] = uint64(v) //nolint:gosec // non-negative by DB constraint.
		return nil
	})
	return kToV, err
}

// ---------------------------------------------------------------------------
// Shape 4: one row, one BYTEA[]/INT[] column -> []T
// Production original: service/vc/database.go readArrayResult.
// ---------------------------------------------------------------------------

// arrayResultBefore is today's readArrayResult, specialised to int.
func arrayResultBefore(r pgx.Row) ([]int, error) {
	var res []int
	err := r.Scan(&res)
	return res, errors.Wrap(err, "failed while scanning a row")
}

func arrayResultCollectOneRow(r pgx.Rows) ([]int, error) {
	return pgx.CollectOneRow(r, pgx.RowTo[[]int])
}
