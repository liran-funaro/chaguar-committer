/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Package pgxbench isolates the per-row cost of the pgx v5 row-collection APIs
// (CollectRows/AppendRows/ForEachRow/RowToStructBy*) against the manual
// `for rows.Next() { rows.Scan(...) }` loop used on the VC and query hot paths.
//
// fakeRows is a faithful stand-in for pgx's *baseRows: it holds pre-encoded wire
// bytes and decodes them through a real *pgtype.Map with the same scan-plan
// caching that baseRows.Scan does, so the only difference between benchmark
// variants is the collection API itself -- no network, no server, no variance.
package pgxbench

import (
	"reflect"

	"github.com/cockroachdb/errors"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgconn"
	"github.com/yugabyte/pgx/v5/pgtype"
)

// fakeRows replays a fixed result set through the real pgtype decoders.
type fakeRows struct {
	typeMap   *pgtype.Map
	fieldDesc []pgconn.FieldDescription
	rows      [][][]byte
	idx       int

	// Mirrors baseRows: the scan plan is computed once per result set and reused
	// for every row as long as the destination types are unchanged.
	scanPlans []pgtype.ScanPlan
	scanTypes []reflect.Type
}

func (r *fakeRows) reset() {
	r.idx = -1
	r.scanPlans = nil
	r.scanTypes = nil
}

// fakeRow stands in for pgx's connRow -- the pgx.Row that QueryRow returns, whose
// Scan performs the entire single-row lifecycle in one call. Without this, a
// pgx.Row-taking reader benchmarked against a *fakeRows would skip the Err check,
// the DriverBytes guard, Next and Close, and so understate its own cost.
type fakeRow struct {
	rows *fakeRows
}

func (r fakeRow) Scan(dest ...any) error {
	if err := r.rows.Err(); err != nil {
		return err
	}
	for _, d := range dest {
		if _, ok := d.(*pgtype.DriverBytes); ok {
			r.rows.Close()
			return errors.New("cannot scan into *pgtype.DriverBytes from QueryRow")
		}
	}
	if !r.rows.Next() {
		if r.rows.Err() == nil {
			return pgx.ErrNoRows
		}
		return r.rows.Err()
	}
	// connRow discards this error and reports rows.Err() instead, because
	// baseRows.Scan records it via rows.fatal; returning it directly is equivalent
	// on success and stricter on failure.
	if err := r.rows.Scan(dest...); err != nil {
		return err
	}
	r.rows.Close()
	return r.rows.Err()
}

func (*fakeRows) Close()                                         {}
func (*fakeRows) Err() error                                     { return nil }
func (*fakeRows) CommandTag() pgconn.CommandTag                  { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return r.fieldDesc }
func (*fakeRows) Conn() *pgx.Conn                                { return nil }
func (r *fakeRows) RawValues() [][]byte                          { return r.rows[r.idx] }

func (r *fakeRows) Next() bool {
	r.idx++
	return r.idx < len(r.rows)
}

func (r *fakeRows) Values() ([]any, error) {
	values := make([]any, 0, len(r.fieldDesc))
	for i := range r.fieldDesc {
		fd := &r.fieldDesc[i]
		dt, ok := r.typeMap.TypeForOID(fd.DataTypeOID)
		if !ok {
			values = append(values, r.rows[r.idx][i])
			continue
		}
		v, err := dt.Codec.DecodeValue(r.typeMap, fd.DataTypeOID, fd.Format, r.rows[r.idx][i])
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}

// Scan replicates baseRows.Scan, including the RowScanner fast path used by
// pgx.RowToStructBy*, the arity guards, and the per-column scan-plan cache.
func (r *fakeRows) Scan(dest ...any) error {
	values := r.rows[r.idx]

	if len(r.fieldDesc) != len(values) {
		return errors.Newf("number of field descriptions must equal number of values, got %d and %d",
			len(r.fieldDesc), len(values))
	}

	if len(dest) == 1 {
		if rc, ok := dest[0].(pgx.RowScanner); ok {
			return rc.ScanRow(r)
		}
	}

	// This guard is what makes a positional mapper fail loudly rather than silently
	// when the target struct's field count stops matching the column count.
	if len(r.fieldDesc) != len(dest) {
		return errors.Newf("number of field descriptions must equal number of destinations, got %d and %d",
			len(r.fieldDesc), len(dest))
	}

	if r.scanPlans == nil {
		r.scanPlans = make([]pgtype.ScanPlan, len(values))
		r.scanTypes = make([]reflect.Type, len(values))
		for i := range dest {
			r.scanPlans[i] = r.typeMap.PlanScan(r.fieldDesc[i].DataTypeOID, r.fieldDesc[i].Format, dest[i])
			r.scanTypes[i] = reflect.TypeOf(dest[i])
		}
	}

	for i, dst := range dest {
		if dst == nil {
			continue
		}
		if r.scanTypes[i] != reflect.TypeOf(dst) {
			r.scanPlans[i] = r.typeMap.PlanScan(r.fieldDesc[i].DataTypeOID, r.fieldDesc[i].Format, dest[i])
			r.scanTypes[i] = reflect.TypeOf(dest[i])
		}
		if err := r.scanPlans[i].Scan(values[i], dst); err != nil {
			return err
		}
	}
	return nil
}
