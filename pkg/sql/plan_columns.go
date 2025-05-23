// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
)

var noColumns = make(colinfo.ResultColumns, 0)

// planColumns returns the signature of rows logically computed
// by the given planNode.
// The signature consists of the list of columns with
// their name and type.
//
// The length of the returned slice is guaranteed to be equal to the
// length of the tuple returned by the planNode's Values() method
// during local execution.
//
// The returned slice is *not* mutable. To modify the result column
// set, implement a separate recursion (e.g. needed_columns.go) or use
// planMutableColumns defined below.
func planColumns(plan planNode) colinfo.ResultColumns {
	return getPlanColumns(plan, false)
}

// planMutableColumns is similar to planColumns() but returns a
// ResultColumns slice that can be modified by the caller.
func planMutableColumns(plan planNode) colinfo.ResultColumns {
	return getPlanColumns(plan, true)
}

// getPlanColumns implements the logic for the
// planColumns/planMutableColumns functions. The mut argument
// indicates whether the slice should be mutable (mut=true) or not.
func getPlanColumns(plan planNode, mut bool) colinfo.ResultColumns {
	switch n := plan.(type) {

	// Nodes that define their own schema.
	case *delayedNode:
		return n.columns
	case *groupNode:
		return n.columns
	case *joinNode:
		return n.columns
	case *ordinalityNode:
		return n.columns
	case *renderNode:
		return n.columns
	case *scanNode:
		return n.columns
	case *unionNode:
		return n.columns
	case *valuesNode:
		return n.columns
	case *vectorMutationSearchNode:
		return n.columns
	case *vectorSearchNode:
		return n.columns
	case *virtualTableNode:
		return n.columns
	case *windowNode:
		return n.columns
	case *showTraceNode:
		return n.columns
	case *zeroNode:
		return n.columns
	case *deleteNode:
		return n.columns
	case *deleteSwapNode:
		return n.columns
	case *updateNode:
		return n.columns
	case *updateSwapNode:
		return n.columns
	case *insertNode:
		return n.columns
	case *insertFastPathNode:
		return n.columns
	case *upsertNode:
		return n.columns
	case *indexJoinNode:
		return n.columns
	case *projectSetNode:
		return n.columns
	case *applyJoinNode:
		return n.columns
	case *lookupJoinNode:
		return n.columns
	case *zigzagJoinNode:
		return n.columns
	case *showTenantNode:
		return n.columns
	case *vTableLookupJoinNode:
		return n.columns
	case *invertedFilterNode:
		return n.columns
	case *invertedJoinNode:
		return n.columns
	case *showFingerprintsNode:
		return n.columns
	case *callNode:
		return n.getResultColumns()
	case *checkExternalConnectionNode:
		return n.columns

	// Nodes with a fixed schema.
	case *scrubNode:
		return n.getColumns(mut, colinfo.ScrubColumns)
	case *explainDDLNode:
		return n.getColumns(mut, colinfo.ExplainPlanColumns)
	case *explainPlanNode:
		return n.getColumns(mut, colinfo.ExplainPlanColumns)
	case *explainVecNode:
		return n.getColumns(mut, colinfo.ExplainPlanColumns)
	case *relocateNode:
		return n.getColumns(mut, colinfo.AlterTableRelocateColumns)
	case *relocateRange:
		return n.getColumns(mut, colinfo.AlterRangeRelocateColumns)
	case *scatterNode:
		return n.getColumns(mut, colinfo.AlterTableScatterColumns)
	case *splitNode:
		return n.getColumns(mut, colinfo.AlterTableSplitColumns)
	case *unsplitNode:
		return n.getColumns(mut, colinfo.AlterTableUnsplitColumns)
	case *unsplitAllNode:
		return n.getColumns(mut, colinfo.AlterTableUnsplitColumns)
	case *showTraceReplicaNode:
		return n.getColumns(mut, colinfo.ShowReplicaTraceColumns)
	case *sequenceSelectNode:
		return n.getColumns(mut, colinfo.SequenceSelectColumns)
	case *exportNode:
		return n.getColumns(mut, colinfo.ExportColumns)
	case *completionsNode:
		return n.getColumns(mut, colinfo.ShowCompletionsColumns)

	// The columns in the hookFnNode are returned by the hook function; we don't
	// know if they can be modified in place or not.
	case *hookFnNode:
		return n.getColumns(mut, n.header)

	// Nodes that have the same schema as their source or their
	// valueNode helper.
	case *bufferNode:
		return getPlanColumns(n.input, mut)
	case *distinctNode:
		return getPlanColumns(n.input, mut)
	case *fetchNode:
		return n.cursor.Types()
	case *filterNode:
		return getPlanColumns(n.input, mut)
	case *max1RowNode:
		return getPlanColumns(n.input, mut)
	case *limitNode:
		return getPlanColumns(n.input, mut)
	case *spoolNode:
		return getPlanColumns(n.input, mut)
	case *serializeNode:
		return getPlanColumns(n.source, mut)
	case *saveTableNode:
		return getPlanColumns(n.input, mut)
	case *scanBufferNode:
		return getPlanColumns(n.buffer, mut)
	case *sortNode:
		return getPlanColumns(n.input, mut)
	case *topKNode:
		return getPlanColumns(n.input, mut)
	case *recursiveCTENode:
		return getPlanColumns(n.input, mut)

	case *showVarNode:
		return colinfo.ResultColumns{
			{Name: n.name, Typ: types.String},
		}
	case *rowSourceToPlanNode:
		return n.columns
	case *cdcValuesNode:
		return n.columns

	case *identifySystemNode:
		return n.getColumns(mut, colinfo.IdentifySystemColumns)
	}

	// Every other node has no columns in their results.
	return noColumns
}

// planTypes returns the types schema of the rows produced by this planNode. See
// comments on planColumns for more details.
func planTypes(plan planNode) []*types.T {
	columns := planColumns(plan)
	typs := make([]*types.T, len(columns))
	for i := range typs {
		typs[i] = columns[i].Typ
	}
	return typs
}

// optColumnsSlot is a helper struct for nodes with a static signature.
// It allows instances to reuse a common (shared) ResultColumns slice as long as
// no read/write access is requested to the slice via planMutableColumns.
type optColumnsSlot struct {
	columns colinfo.ResultColumns
}

func (c *optColumnsSlot) getColumns(mut bool, cols colinfo.ResultColumns) colinfo.ResultColumns {
	if c.columns != nil {
		return c.columns
	}
	if !mut {
		return cols
	}
	c.columns = make(colinfo.ResultColumns, len(cols))
	copy(c.columns, cols)
	return c.columns
}
