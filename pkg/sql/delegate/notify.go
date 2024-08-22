// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package delegate

import (
	"fmt"
	"os"

	"github.com/cockroachdb/cockroach/pkg/sql/lexbase"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

func (d *delegator) delegateNotify(n *tree.Notify) (tree.Statement, error) {
	stmt := fmt.Sprintf(
		`UPSERT INTO system.notifications (channel, payload, pid) VALUES (%s, %s, %d)`,
		lexbase.EscapeSQLString(n.ChannelName.String()),
		lexbase.EscapeSQLString(n.Payload.String()),
		// TODO: cockroach session ids are uint128s, but the pid is an int32. so we cant really do anything correct here..
		// could alternatively internally map sessionids to int32s with some sort of table or something.
		// `UPSERT INTO system.notifications (channel, payload, pid) SELECT %s, %s, session_id FROM [SHOW session_id]`
		os.Getpid(), // TODO: need an extendedEvalContext.SessionID, but d doesnt have that
	)
	return d.parse(stmt)
}
