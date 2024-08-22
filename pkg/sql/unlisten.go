// Copyright 2022 The Cockroach Authors.
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

type unlistenNode struct {
	n *tree.Unlisten
}

func (un *unlistenNode) Close(_ context.Context)             {}
func (un *unlistenNode) Next(params runParams) (bool, error) { return false, nil }
func (un *unlistenNode) Values() tree.Datums                 { return nil }
func (un *unlistenNode) startExec(params runParams) error {
	if !params.extendedEvalCtx.TxnImplicit {
		return unimplemented.New("unlisten", "cannot be used inside a transaction")
	}

	sessionID := params.extendedEvalCtx.SessionID

	// TODO: a session can listen to multiple channels, so we should only remove if we're the last one.
	_, err := params.p.execCfg.InternalDB.Executor().ExecEx(
		params.ctx,
		"delete-session-pid",
		params.p.txn,
		sessiondata.InternalExecutorOverride{},
		"delete from system.notifications_session_id_pids where session_id = $1",
		sessionID.String(),
	)
	if err != nil {
		return fmt.Errorf("failed to delete session pid: %w", err)
	}

	registry := params.p.execCfg.PGListenerRegistry
	if un.n.Star {
		registry.RemoveAllListeners(params.ctx, notify.ListenerID(sessionID))
	} else {
		registry.RemoveListener(params.ctx, notify.ListenerID(sessionID), un.n.ChannelName.String())
	}
	return nil
}

var _ planNode = &unlistenNode{}

func (p *planner) Unlisten(ctx context.Context, n *tree.Unlisten) (planNode, error) {
	return &unlistenNode{n: n}, nil
}
