/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package pgxbench

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"
)

// widenedRow simulates the target struct gaining a field -- what a regenerated
// protobuf message can do. Column order is still (key, value, version).
type widenedRow struct {
	Key     []byte
	Extra   []byte // new field, inserted before Value
	Value   []byte
	Version uint64
}

// reorderedRow simulates fields being reordered without changing their count.
type reorderedRow struct {
	Value   []byte
	Key     []byte
	Version uint64
}

// TestStructByPosFailsLoudlyOnFieldCountChange records that a positional mapper is
// NOT silently corrupting on the realistic breakage -- a field being added. pgx's
// Scan compares column count to destination count and refuses.
func TestStructByPosFailsLoudlyOnFieldCountChange(t *testing.T) {
	t.Parallel()
	r := newKeyValueVersionRows(t, 4)
	_, err := pgx.CollectRows(r, pgx.RowToStructByPos[widenedRow])
	require.ErrorContains(t, err, "number of field descriptions must equal number of destinations")
	t.Logf("RowToStructByPos on a widened struct: %v", err)
}

// TestStructByPosIsSilentOnSameCountReordering is the one case where position
// matching does corrupt quietly: the field count still matches, and key/value are
// both BYTEA so no type error fires. Note that today's explicit
// r.Scan(&v.Key, &v.Value, &v.Version) is positional too and would corrupt
// identically -- so this is not a regression the mapper introduces.
func TestStructByPosIsSilentOnSameCountReordering(t *testing.T) {
	t.Parallel()
	r := newKeyValueVersionRows(t, 4)
	got, err := pgx.CollectRows(r, pgx.RowToStructByPos[reorderedRow])
	require.NoError(t, err)

	r.reset()
	want, err := pgx.CollectRows(r, pgx.RowToStructByName[plainRow])
	require.NoError(t, err)

	// The key column landed in Value and vice versa, with no error reported.
	require.Equal(t, want[1].Key, got[1].Value, "key column landed in Value")
	require.Equal(t, want[1].Value, got[1].Key, "value column landed in Key")
	t.Log("RowToStructByPos silently swapped key and value on same-count reordering")
}

// TestStructByNameSurvivesBothChanges is the contrast: name matching fails loudly on
// an unmatched field, and is robust to reordering on either side.
func TestStructByNameSurvivesBothChanges(t *testing.T) {
	t.Parallel()
	r := newKeyValueVersionRows(t, 4)
	_, err := pgx.CollectRows(r, pgx.RowToStructByName[widenedRow])
	require.ErrorContains(t, err, "cannot find field Extra in returned row")

	// The lax variant accepts the extra field, and every named column still lands
	// in the right place.
	r.reset()
	lax, err := pgx.CollectRows(r, pgx.RowToStructByNameLax[widenedRow])
	require.NoError(t, err)
	r.reset()
	want, err := pgx.CollectRows(r, pgx.RowToStructByName[plainRow])
	require.NoError(t, err)
	require.Equal(t, want[1].Key, lax[1].Key)
	require.Equal(t, want[1].Value, lax[1].Value)
	require.Equal(t, want[1].Version, lax[1].Version)
	require.Nil(t, lax[1].Extra)

	// Reordering the struct's fields changes nothing, because matching is by name.
	r.reset()
	reordered, err := pgx.CollectRows(r, pgx.RowToStructByName[reorderedRow])
	require.NoError(t, err)
	require.Equal(t, want[1].Key, reordered[1].Key)
	require.Equal(t, want[1].Value, reordered[1].Value)
}
