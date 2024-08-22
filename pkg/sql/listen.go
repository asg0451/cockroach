// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/notify"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
)

type listenNode struct {
	n *tree.Listen
}

func (ln *listenNode) Close(_ context.Context)             {}
func (ln *listenNode) Next(params runParams) (bool, error) { return false, nil }
func (ln *listenNode) Values() tree.Datums                 { return nil }
func (ln *listenNode) startExec(params runParams) error {
	if !params.extendedEvalCtx.TxnImplicit {
		return unimplemented.New("listen", "cannot be used inside a transaction")
	}

	sessionID := params.extendedEvalCtx.SessionID

	// TODO: a session can listen to multiple channels, so we should only add if we're the first.
	row, err := params.p.execCfg.InternalDB.Executor().QueryRowEx(
		params.ctx,
		"insert-session-pid",
		params.p.txn,
		sessiondata.InternalExecutorOverride{},
		"insert into system.notifications_session_id_pids (session_id) values ($1) returning pid",
		sessionID.String(),
	)
	if err != nil {
		return fmt.Errorf("failed to insert session pid: %w", err)
	}
	pid := int32(tree.MustBeDInt(row[0]))
	fmt.Printf("pid: %d\n", pid)

	registry := params.p.execCfg.PGListenerRegistry
	registry.AddListener(params.ctx, notify.ListenerID(sessionID), pid, ln.n.ChannelName.String(), params.p.notificationSender)
	return nil
}

var _ planNode = &listenNode{}

func (p *planner) Listen(ctx context.Context, n *tree.Listen) (planNode, error) {
	return &listenNode{n: n}, nil
}
