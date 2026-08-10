/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package pgxbench

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// TestStructByNameOnProto checks whether the reflection-based struct mappers can
// even target committerpb.Row, and what happens to a proto message that the
// by-value variant copies into the result slice.
func TestStructByNameOnProto(t *testing.T) {
	t.Parallel()
	r := newKeyValueVersionRows(t, 3)

	byName, err := pgx.CollectRows(r, pgx.RowToAddrOfStructByName[committerpb.Row])
	require.NoError(t, err)
	require.Len(t, byName, 3)
	t.Logf("AddrOfStructByName row 0: key=%x version=%d", byName[0].Key, byName[0].Version)

	r.reset()
	byPos, err := pgx.CollectRows(r, pgx.RowToAddrOfStructByPos[committerpb.Row])
	require.NoError(t, err)
	require.Len(t, byPos, 3)
	t.Logf("AddrOfStructByPos row 0: key=%x version=%d", byPos[0].Key, byPos[0].Version)

	// The by-value variant copies protoimpl.MessageState into the result slice.
	r.reset()
	byValue, err := pgx.CollectRows(r, pgx.RowToStructByName[committerpb.Row])
	require.NoError(t, err)
	require.Len(t, byValue, 3)

	// Exercise the copied messages through the proto runtime.
	encoded, err := proto.Marshal(&byValue[0])
	require.NoError(t, err)
	t.Logf("marshalled copied message: %x", encoded)
	t.Logf("String() of copied message: %s", byValue[0].String())
	require.True(t, proto.Equal(&byValue[0], byName[0]))
}

// TestPlainStructMapping is the control: the same helpers on a non-proto struct.
func TestPlainStructMapping(t *testing.T) {
	t.Parallel()
	r := newKeyValueVersionRows(t, 3)
	byName, err := pgx.CollectRows(r, pgx.RowToStructByName[plainRow])
	require.NoError(t, err)
	require.Len(t, byName, 3)

	r.reset()
	byPos, err := pgx.CollectRows(r, pgx.RowToStructByPos[plainRow])
	require.NoError(t, err)
	require.Equal(t, byName, byPos)
}

// TestTxStatusShapeCannotBeStructMapped documents that the tx_status readers have
// no 1:1 column-to-field shape: the three columns collapse into a nested TxRef,
// and height is a custom varint encoding the mappers cannot decode.
func TestTxStatusShapeCannotBeStructMapped(t *testing.T) {
	t.Parallel()
	r := newTxStatusRows(t, 3)
	_, err := pgx.CollectRows(r, pgx.RowToAddrOfStructByName[committerpb.TxStatus])
	require.Error(t, err)
	t.Logf("RowToAddrOfStructByName[TxStatus] error: %v", err)
}

// TestReusedScanTargetsDoNotAlias guards the one hazard in reusing scan
// destinations across rows (which is what ForEachRow does, and where its saving
// comes from): if pgtype decoded []byte columns in place, every collected row
// would end up sharing the last row's bytes. pgtype's bytea scan plan allocates a
// fresh slice per row, so the rows stay independent -- this test fails loudly if
// that ever changes.
func TestReusedScanTargetsDoNotAlias(t *testing.T) {
	t.Parallel()
	const n = 8
	r := newKeyValueVersionRows(t, n)

	var key, value []byte
	var version uint64
	var collected []*committerpb.Row
	_, err := pgx.ForEachRow(r, []any{&key, &value, &version}, func() error {
		collected = append(collected, &committerpb.Row{Key: key, Value: value, Version: version})
		return nil
	})
	require.NoError(t, err)
	require.Len(t, collected, n)

	// Every row must keep its own distinct key, and no two rows may share a
	// backing array.
	r.reset()
	expected, err := pgx.CollectRows(r, func(row pgx.CollectableRow) (*committerpb.Row, error) {
		v := &committerpb.Row{}
		return v, row.Scan(&v.Key, &v.Value, &v.Version)
	})
	require.NoError(t, err)
	require.Len(t, expected, n)
	for i := range collected {
		require.Equal(t, expected[i].Key, collected[i].Key, "row %d key", i)
		require.Equal(t, expected[i].Value, collected[i].Value, "row %d value", i)
		require.Equal(t, expected[i].Version, collected[i].Version, "row %d version", i)
		if i > 0 {
			require.NotSame(t, &collected[i-1].Key[0], &collected[i].Key[0],
				"rows %d and %d share a key backing array", i-1, i)
		}
	}
}
