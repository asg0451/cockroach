// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltelemetry"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
)

// dropOwnedByNode represents a DROP OWNED BY <role(s)> statement.
type dropOwnedByNode struct {
	// TODO(angelaw): Uncomment when implementing - commenting out due to linting error.
	//n *tree.DropOwnedBy
}

func (p *planner) DropOwnedBy(ctx context.Context) (planNode, error) {
	if err := checkSchemaChangeEnabled(
		ctx,
		p.ExecCfg(),
		"DROP OWNED BY",
	); err != nil {
		return nil, err
	}
	telemetry.Inc(sqltelemetry.CreateDropOwnedByCounter())
	// TODO(angelaw): Implementation.
	return nil, unimplemented.NewWithIssue(55381, "drop owned by is not yet implemented")
}

func (n *dropOwnedByNode) startExec(params runParams) error {
	// TODO(angelaw): Implementation.
	return nil
}
func (n *dropOwnedByNode) Next(runParams) (bool, error) { return false, nil }
func (n *dropOwnedByNode) Values() tree.Datums          { return tree.Datums{} }
func (n *dropOwnedByNode) Close(context.Context)        {}
