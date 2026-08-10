/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package pgxbench

// Variant labels, shared by the isolated and real-database harnesses so a name can
// only be spelled one way. They appear verbatim in benchmark output and in the
// analysis write-up's tables.
const (
	nameBefore       = "manual-before"
	nameHoisted      = "manual-hoisted"
	nameDirectToMap  = "manual-directToMap"
	nameForEachRow   = "pgx.ForEachRow"
	nameAppendRows   = "pgx.AppendRows"
	nameCollectRows  = "pgx.CollectRows"
	nameStructByName = "pgx.RowToAddrOfStructByName"
	nameStructByPos  = "pgx.RowToAddrOfStructByPos"
)
