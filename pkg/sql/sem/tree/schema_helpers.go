// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tree

import "slices"

// IsSetOrResetSchemaLocked returns true if `n` contains a command to
// set/reset "schema_locked" storage parameter.
func IsSetOrResetSchemaLocked(n Statement) bool {
	alterStmt, ok := n.(*AlterTable)
	if !ok {
		return false
	}
	for _, cmd := range alterStmt.Cmds {
		switch cmd := cmd.(type) {
		case *AlterTableSetStorageParams:
			if cmd.StorageParams.GetVal("schema_locked") != nil {
				return true
			}
		case *AlterTableResetStorageParams:
			if slices.Contains(cmd.Params, "schema_locked") {
				return true
			}
		}
	}
	return false
}

// IsAllowedLDRSchemaChange returns true if the schema change statement is
// allowed to occur while the table is being referenced by a logical data
// replication job as a destination table.
func IsAllowedLDRSchemaChange(n Statement) bool {
	switch s := n.(type) {
	case *CreateIndex:
		// Only allow non-unique and non-partial indexes to be created. A unique or
		// partial index on a destination table could cause inserts to fail.
		return !s.Unique && s.Predicate == nil
	case *DropIndex:
		return true
	}
	return false
}
