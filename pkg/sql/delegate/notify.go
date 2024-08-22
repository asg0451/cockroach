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

	"github.com/cockroachdb/cockroach/pkg/sql/lexbase"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

func (d *delegator) delegateNotify(n *tree.Notify) (tree.Statement, error) {
	stmt := fmt.Sprintf(
		// notify -> upsert into system.notifications (channel, payload, pid) select %s, %s, pid from sessions_pids where session_id = (select session_id from [SHOW session_id]);
		`UPSERT INTO system.notifications (channel, payload, pid) SELECT %s, %s, pid FROM system.notifications_session_id_pids WHERE session_id = (SELECT session_id FROM [SHOW session_id])`,
		lexbase.EscapeSQLString(n.ChannelName.String()),
		lexbase.EscapeSQLString(n.Payload.String()),
		// TODO: cockroach session ids are stringified uint128s, but the pid is an int32. so we cant really do anything correct here..
		// could alternatively internally map sessionids to int32s with some sort of table or something.

		// set serial_normalization = sql_sequence_cached;
		// create table system.sessions_pids (session_id text primary key, pid serial4 not null);

		// listen -> insert into sessions_pids (session_id) select session_id from [SHOW session_id]; (and also register internally)
		// unlisten -> delete from sessions_pids where session_id = (select session_id from [SHOW session_id]); (and also unregister internally)
		// notify -> upsert into system.notifications (channel, payload, pid) select %s, %s, pid from sessions_pids where session_id = (select session_id from [SHOW session_id]);

		// `UPSERT INTO system.notifications (channel, payload, pid) SELECT %s, %s, session_id FROM [SHOW session_id]`

		// os.Getpid(),
	)
	return d.parse(stmt)
}
