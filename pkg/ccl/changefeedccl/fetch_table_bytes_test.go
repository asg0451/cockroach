// Copyright 2023 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package changefeedccl_test

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/mocks"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TODO: add test for experimental / non enterpise feeds?
// TODO: add test for deleted table

// TODO: do this in a less silly way. the complication comes from calling the
// function both from a sql statement and from the changefeed frontier.
type queryFn func(ctx context.Context, opName string, override sessiondata.InternalExecutorOverride, stmt string, qargs ...interface{}) (eval.InternalRows, error)

func (f queryFn) QueryIteratorEx(ctx context.Context, opName string, override sessiondata.InternalExecutorOverride, stmt string, qargs ...interface{}) (eval.InternalRows, error) {
	return f(ctx, opName, override, stmt, qargs...)
}

func TestFetchChangefeedBillingBytesE2E(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	params := base.TestServerArgs{}
	s := serverutils.StartServerOnly(t, params)
	defer s.Stopper().Stop(ctx)

	execCfg := s.ExecutorConfig().(sql.ExecutorConfig)
	execCtx, close := sql.MakeJobExecContext(ctx, "test", username.NodeUserName(), &sql.MemoryMetrics{}, &execCfg)
	defer close()

	// add some changefeed jobs
	// TODO: use the changefeed test utils / factory stuff
	// registry := s.JobRegistry().(*jobs.Registry)
	stmt := "set cluster setting kv.rangefeed.enabled = true"
	_, err := s.SystemLayer().ExecutorConfig().(sql.ExecutorConfig).InternalDB.Executor().Exec(ctx, "test", nil, stmt)
	require.NoError(t, err)

	ie := s.InternalExecutor().(*sql.InternalExecutor)

	stmt = "CREATE DATABASE testdb"
	_, err = ie.ExecEx(ctx, "test-create-db", nil, sessiondata.InternalExecutorOverride{User: username.NodeUserName()}, stmt)
	require.NoError(t, err)

	stmt = "CREATE TABLE testdb.test as SELECT generate_series(1, 1000) AS id"
	_, err = ie.ExecEx(ctx, "test-create-table", nil, sessiondata.InternalExecutorOverride{User: username.NodeUserName()}, stmt)
	require.NoError(t, err)

	stmt = `CREATE CHANGEFEED FOR TABLE testdb.test INTO 'null://' WITH initial_scan='no';`
	_, err = ie.ExecEx(ctx, "test-create-cf", nil, sessiondata.InternalExecutorOverride{User: username.NodeUserName()}, stmt)
	require.NoError(t, err)

	var querier queryFn = func(ctx context.Context, opName string, override sessiondata.InternalExecutorOverride, stmt string, qargs ...interface{}) (eval.InternalRows, error) {
		var it eval.InternalRows
		var err error
		err = execCtx.ExecCfg().InternalDB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
			it, err = txn.QueryIteratorEx(ctx, opName, txn.KV(), override, stmt, qargs...)
			return err
		})
		return it, err
	}

	res, err := changefeedccl.FetchChangefeedBillingBytes(ctx, querier, &execCfg)
	require.NoError(t, err)
	assert.NotZero(t, res)
}

func TestFetchChangefeedBillingBytes(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	params := base.TestServerArgs{}
	s := serverutils.StartServerOnly(t, params)
	defer s.Stopper().Stop(ctx)

	execCfg := s.ExecutorConfig().(sql.ExecutorConfig)
	execCtx, close := sql.MakeJobExecContext(ctx, "test", username.NodeUserName(), &sql.MemoryMetrics{}, &execCfg)
	defer close()
	_ = execCtx

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Set up details.
	// Ensure that it can parse multiple kinds of changefeed details.
	detailses := []*jobspb.ChangefeedDetails{
		{
			TargetSpecifications: []jobspb.ChangefeedTargetSpecification{
				{
					Type:              jobspb.ChangefeedTargetSpecification_PRIMARY_FAMILY_ONLY,
					TableID:           101,
					StatementTimeName: "t1_primary",
				},
			},
		},
		{
			TargetSpecifications: []jobspb.ChangefeedTargetSpecification{
				{
					Type:              jobspb.ChangefeedTargetSpecification_COLUMN_FAMILY,
					TableID:           102,
					FamilyName:        "fam1",
					StatementTimeName: "t2_one_fam",
				},
			},
		},
		{
			TargetSpecifications: []jobspb.ChangefeedTargetSpecification{
				{
					Type:              jobspb.ChangefeedTargetSpecification_EACH_FAMILY,
					TableID:           103,
					StatementTimeName: "t3_each_fam",
				},
			},
		},
		{
			Tables: jobspb.ChangefeedTargets{
				104: {
					StatementTimeName: "t4_bare_table",
				},
				105: {
					StatementTimeName: "t5_dropped_table",
				},
			},
		},
	}
	rows := make([]tree.Datums, 0, len(detailses))
	for _, d := range detailses {
		payload := jobspb.Payload{Details: jobspb.WrapPayloadDetails(d)}
		bs, err := payload.Marshal()
		require.NoError(t, err)

		row := tree.Datums{tree.NewDInt(tree.DInt(42)), tree.NewDBytes(tree.DBytes(bs))}
		rows = append(rows, row)
	}
	it := &testIterator{rows: rows}
	defer require.True(t, it.closed)

	// Set up sizes.

	querier := mocks.NewMockTableBytesQuerier(ctrl)
	querier.EXPECT().QueryIteratorEx(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(it, nil).
		MinTimes(1)

	res, err := changefeedccl.FetchChangefeedBillingBytes(ctx, querier, &execCfg)
	require.NoError(t, err)
	assert.NotZero(t, res)
}

type testIterator struct {
	rows   []tree.Datums
	idx    int
	closed bool
}

func (t *testIterator) Close() error {
	t.closed = true
	return nil
}

func (t *testIterator) Cur() tree.Datums {
	return t.rows[t.idx]
}

func (t *testIterator) Next(context.Context) (bool, error) {
	t.idx++
	return t.idx < len(t.rows), nil
}

// testIterator implements eval.InternalRows
var _ eval.InternalRows = &testIterator{}
