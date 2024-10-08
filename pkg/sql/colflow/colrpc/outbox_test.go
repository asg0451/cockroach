// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package colrpc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexecargs"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexectestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecerror"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecop"
	"github.com/cockroachdb/cockroach/pkg/sql/colmem"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/stretchr/testify/require"
)

func TestOutboxCatchesPanics(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()

	// Use the release-build panic-catching behavior instead of the
	// crdb_test-build behavior.
	defer colexecerror.ProductionBehaviorForTests()()

	var (
		input    = colexecop.NewBatchBuffer()
		typs     = []*types.T{types.Int}
		rpcLayer = makeMockFlowStreamRPCLayer()
	)
	input.Init(ctx)
	outbox, err := NewOutbox(&execinfra.FlowCtx{Gateway: false}, 0 /* processorID */, testAllocator, testMemAcc, colexecargs.OpWithMetaInfo{Root: input}, typs, nil /* getStats */)
	require.NoError(t, err)

	// This test relies on the fact that BatchBuffer panics when there are no
	// batches to return. Verify this assumption.
	require.Panics(t, func() { input.Next() })

	// The actual test verifies that the Outbox handles input execution tree
	// panics by not panicking and returning.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		outbox.runWithStream(ctx, rpcLayer.client, nil /* flowCtxCancel */, nil /* outboxCtxCancel */)
		wg.Done()
	}()

	inboxMemAccount := testMemMonitor.MakeBoundAccount()
	defer inboxMemAccount.Close(ctx)
	inbox, err := NewInbox(colmem.NewAllocator(ctx, &inboxMemAccount, coldata.StandardColumnFactory), typs, execinfrapb.StreamID(0))
	require.NoError(t, err)

	streamHandlerErrCh := handleStream(ctx, inbox, rpcLayer.server, func() { close(rpcLayer.server.csChan) })

	// The outbox will be sending the panic as eagerly. This Next call will
	// propagate the panic.
	err = colexecerror.CatchVectorizedRuntimeError(func() {
		inbox.Init(ctx)
		inbox.Next()
	})
	require.Error(t, err)

	// Expect no metadata.
	meta := inbox.DrainMeta()
	require.True(t, len(meta) == 0)

	require.True(t, testutils.IsError(err, "runtime error: index out of range"), err)

	require.NoError(t, <-streamHandlerErrCh)
	wg.Wait()
}

func TestOutboxDrainsMetadataSources(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()

	var (
		input = colexecop.NewBatchBuffer()
		typs  = []*types.T{types.Int}
	)

	// Define common function that returns both an Outbox and a pointer to a
	// uint32 that is set atomically when the outbox drains a metadata source.
	newOutboxWithMetaSources := func() (*Outbox, *uint32, error) {
		var sourceDrained uint32
		outbox, err := NewOutbox(
			&execinfra.FlowCtx{Gateway: false},
			0, /* processorID */
			testAllocator,
			testMemAcc,
			colexecargs.OpWithMetaInfo{
				Root: input,
				MetadataSources: []colexecop.MetadataSource{
					colexectestutils.CallbackMetadataSource{
						DrainMetaCb: func() []execinfrapb.ProducerMetadata {
							atomic.StoreUint32(&sourceDrained, 1)
							return nil
						},
					},
				},
			},
			typs,
			nil, /* getStats */
		)
		if err != nil {
			return nil, nil, err
		}
		return outbox, &sourceDrained, nil
	}

	t.Run("AfterSuccessfulRun", func(t *testing.T) {
		rpcLayer := makeMockFlowStreamRPCLayer()
		outboxMemAccount := testMemMonitor.MakeBoundAccount()
		defer outboxMemAccount.Close(ctx)
		outbox, sourceDrained, err := newOutboxWithMetaSources()
		require.NoError(t, err)

		b := testAllocator.NewMemBatchWithMaxCapacity(typs)
		input.Add(b, typs)

		// Close the csChan to unblock the Recv goroutine (we don't need it for this
		// test).
		close(rpcLayer.client.csChan)
		outbox.runWithStream(ctx, rpcLayer.client, nil /* flowCtxCancel */, nil /* outboxCtxCancel */)

		require.True(t, atomic.LoadUint32(sourceDrained) == 1)
	})

	// This is similar to TestOutboxCatchesPanics, but focuses on verifying that
	// the Outbox drains its metadata sources even after an error.
	t.Run("AfterOutboxError", func(t *testing.T) {
		// Use the release-build panic-catching behavior instead of the
		// crdb_test-build behavior.
		defer colexecerror.ProductionBehaviorForTests()()

		// This test, similar to TestOutboxCatchesPanics, relies on the fact that
		// a BatchBuffer panics when there are no batches to return.
		require.Panics(t, func() { input.Next() })

		rpcLayer := makeMockFlowStreamRPCLayer()
		outbox, sourceDrained, err := newOutboxWithMetaSources()
		require.NoError(t, err)

		close(rpcLayer.client.csChan)
		outbox.runWithStream(ctx, rpcLayer.client, nil /* flowCtxCancel */, nil /* outboxCtxCancel */)

		require.True(t, atomic.LoadUint32(sourceDrained) == 1)
	})
}
