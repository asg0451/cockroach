// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package nodedialer

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv/kvbase"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/rpc/rpcbase"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	circuit2 "github.com/cockroachdb/cockroach/pkg/util/circuit"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metamorphic"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
	"google.golang.org/grpc"
	"storj.io/drpc"
)

// An AddressResolver translates NodeIDs into addresses.
type AddressResolver func(roachpb.NodeID) (net.Addr, roachpb.Locality, error)

// A Dialer wraps an *rpc.Context for dialing based on node IDs. For each node,
// it maintains a circuit breaker that prevents rapid connection attempts and
// provides hints to the callers on whether to log the outcome of the operation.
type Dialer struct {
	rpcContext   *rpc.Context
	resolver     AddressResolver
	testingKnobs DialerTestingKnobs
}

// DialerOpt contains configuration options for a Dialer.
type DialerOpt struct {
	// TestingKnobs contains testing utilities.
	TestingKnobs DialerTestingKnobs
}

// DialerTestingKnobs contains dialer testing options.
type DialerTestingKnobs struct {
	// TestingNoLocalClientOptimization, if set, disables the optimization about
	// using a direct client for the local node instead of going through gRPC. For
	// one, the behavior on cancellation of the client RPC ctx is different: when
	// going through gRPC, the framework watches for client ctx cancellation and
	// interrupts the RPC. When bypassing gRPC, the client ctx is passed directly
	// to the RPC handler.
	TestingNoLocalClientOptimization bool
}

// ModuleTestingKnobs implements the ModuleTestingKnobs interface.
func (DialerTestingKnobs) ModuleTestingKnobs() {}

// New initializes a Dialer.
func New(rpcContext *rpc.Context, resolver AddressResolver) *Dialer {
	return &Dialer{
		rpcContext: rpcContext,
		resolver:   resolver,
	}
}

// NewWithOpt initializes a Dialer and allows passing in configuration options.
func NewWithOpt(rpcContext *rpc.Context, resolver AddressResolver, opt DialerOpt) *Dialer {
	d := New(rpcContext, resolver)
	d.testingKnobs = opt.TestingKnobs
	return d
}

// Stopper returns this node dialer's Stopper.
// TODO(bdarnell): This is a bit of a hack for kv/transport_race.go
func (n *Dialer) Stopper() *stop.Stopper {
	return n.rpcContext.Stopper
}

// Silence lint warning because this method is only used in race builds.
var _ = (*Dialer).Stopper

// Dial returns a grpc connection to the given node. It logs whenever the
// node first becomes unreachable or reachable.
func (n *Dialer) Dial(
	ctx context.Context, nodeID roachpb.NodeID, class rpcbase.ConnectionClass,
) (_ *grpc.ClientConn, err error) {
	if n == nil || n.resolver == nil {
		return nil, errors.New("no node dialer configured")
	}
	gc, _, err := dial(ctx, n.resolver, n.rpcContext.GRPCDialNode, nodeID, class, true /* checkBreaker */)
	if err != nil {
		return gc, errors.Wrapf(err, "gRPC")
	}
	return gc, err
}

// DRPCDial returns a drpc connection to the given node. It logs whenever the
// node first becomes unreachable or reachable. This method is similar to
// Dial, but it dials a DRPC connection instead of a gRPC connection.
func (n *Dialer) DRPCDial(
	ctx context.Context, nodeID roachpb.NodeID, class rpcbase.ConnectionClass,
) (drpc.Conn, error) {
	if n == nil || n.resolver == nil {
		return nil, errors.New("no node dialer configured")
	}
	dc, _, err := dial(ctx, n.resolver, n.rpcContext.DRPCDialNode, nodeID, class, true /* checkBreaker */)
	if err != nil {
		return nil, errors.Wrapf(err, "DRPC")
	}
	return dc, err
}

// DialNoBreaker is like Dial, but will not check the circuit breaker before
// trying to connect. This function should only be used when there is good
// reason to believe that the node is reachable.
func (n *Dialer) DialNoBreaker(
	ctx context.Context, nodeID roachpb.NodeID, class rpcbase.ConnectionClass,
) (_ *grpc.ClientConn, err error) {
	if n == nil || n.resolver == nil {
		return nil, errors.New("no node dialer configured")
	}
	gc, _, err := dial(ctx, n.resolver, n.rpcContext.GRPCDialNode, nodeID, class, false /* checkBreaker */)
	if err != nil {
		return nil, errors.Wrapf(err, "gRPC")
	}
	return gc, err
}

// DRPCDialNoBreaker is like DRPCDial, but will not check the circuit breaker
// before trying to connect. This function should only be used when there is
// good reason to believe that the node is reachable.
func (n *Dialer) DRPCDialNoBreaker(
	ctx context.Context, nodeID roachpb.NodeID, class rpcbase.ConnectionClass,
) (drpc.Conn, error) {
	if n == nil || n.resolver == nil {
		return nil, errors.New("no node dialer configured")
	}
	dc, _, err := dial(ctx, n.resolver, n.rpcContext.DRPCDialNode, nodeID, class, false /* checkBreaker */)
	if err != nil {
		return nil, errors.Wrapf(err, "DRPC")
	}
	return dc, err
}

// DialInternalClient is a specialization of DialClass for callers that
// want a kvpb.InternalClient. This supports an optimization to bypass the
// network for the local node.
//
// For a more contextualized explanation, see the comment that decorates
// (*rpc.Context).loopbackDialFn.
func (n *Dialer) DialInternalClient(
	ctx context.Context, nodeID roachpb.NodeID, class rpcbase.ConnectionClass,
) (rpc.RestrictedInternalClient, error) {
	if n == nil || n.resolver == nil {
		return nil, errors.New("no node dialer configured")
	}
	{
		// If we're dialing the local node, don't go through gRPC.
		localClient := n.rpcContext.GetLocalInternalClientForAddr(nodeID)
		if localClient != nil && !n.testingKnobs.TestingNoLocalClientOptimization {
			log.VEvent(ctx, 2, kvbase.RoutingRequestLocallyMsg)
			return localClient, nil
		}
	}

	var client rpc.RestrictedInternalClient
	useStreamPoolClient := shouldUseBatchStreamPoolClient(ctx, n.rpcContext.Settings)
	if !rpc.ExperimentalDRPCEnabled.Get(&n.rpcContext.Settings.SV) {
		gc, conn, err := dial(ctx, n.resolver, n.rpcContext.GRPCDialNode, nodeID, class, true /* checkBreaker */)
		if err != nil {
			return nil, errors.Wrapf(err, "gRPC")
		}
		client = newBaseInternalClient(gc)
		if useStreamPoolClient {
			client = newBatchStreamPoolClient(conn.BatchStreamPool())
		}
	} else {
		dc, conn, err := dial(ctx, n.resolver, n.rpcContext.DRPCDialNode, nodeID, class, true /* checkBreaker */)
		if err != nil {
			return nil, errors.Wrapf(err, "DRPC")
		}
		// TODO(server): gRPC version of batch stream pool implements
		// rpc.RestrictedInternalClient and is allocation-optimized,
		// whereas here we allocate a new throw-away
		// unaryDRPCBatchServiceToInternalAdapter.
		var pool *rpc.DRPCBatchStreamPool
		if useStreamPoolClient {
			pool = conn.BatchStreamPool()
		}
		client = &unaryDRPCBatchServiceToInternalAdapter{
			kvBatchClient:      kvpb.NewDRPCKVBatchClientAdapter(dc),
			muxRangeFeedClient: kvpb.NewDRPCRangeFeedClientAdapter(dc),
			drpcStreamPool:     pool,
		}
	}

	client = maybeWrapInTracingClient(ctx, client)
	return client, nil
}

// dial performs the dialing of the remote connection. If checkBreaker
// is set (which it usually is), circuit breakers for the peer will be
// checked.
func dial[Conn io.Closer](
	ctx context.Context,
	resolver AddressResolver,
	dialFn func(addr string, nodeID roachpb.NodeID, locality roachpb.Locality, class rpcbase.ConnectionClass) *rpc.Connection[Conn],
	nodeID roachpb.NodeID,
	class rpcbase.ConnectionClass,
	checkBreaker bool,
) (Conn, *rpc.Connection[Conn], error) {
	var nilConn Conn
	const ctxWrapMsg = "dial"

	addr, locality, err := resolver(nodeID)
	if err != nil {
		return nilConn, nil, err
	}
	// Don't trip the breaker if we're already canceled.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nilConn, nil, errors.Wrap(ctxErr, ctxWrapMsg)
	}

	log.VEventf(ctx, 2, "sending request to %s", addr)
	rpcConn := dialFn(addr.String(), nodeID, locality, class)
	connect := rpcConn.Connect
	if !checkBreaker {
		connect = rpcConn.ConnectNoBreaker
	}
	conn, err := connect(ctx)
	if err != nil {
		return nilConn, nil, errors.Wrapf(err, "failed to connect to n%d at %v", nodeID, addr)
	}
	return conn, rpcConn, nil
}

// ConnHealth returns nil if we have an open connection of the request
// class to the given node that succeeded on its most recent heartbeat.
// Returns circuit.ErrBreakerOpen if the breaker is tripped, otherwise
// ErrNoConnection if no connection to the node currently exists.
func (n *Dialer) ConnHealth(nodeID roachpb.NodeID, class rpcbase.ConnectionClass) error {
	if n == nil || n.resolver == nil {
		return errors.New("no node dialer configured")
	}
	addr, _, err := n.resolver(nodeID)
	if err != nil {
		return err
	}
	return n.rpcContext.ConnHealth(addr.String(), nodeID, class)
}

// ConnHealthTryDial returns nil if we have an open connection of the request
// class to the given node that succeeded on its most recent heartbeat. If no
// healthy connection is found, it will attempt to dial the node.
//
// This exists for components that do not themselves actively maintain RPC
// connections to remote nodes, e.g. DistSQL. However, it can cause significant
// latency if the remote node is unresponsive (e.g. if the server/VM is shut
// down), and should be avoided in latency-sensitive code paths. Preferably,
// this should be replaced by some other mechanism to maintain RPC connections.
// See also: https://github.com/cockroachdb/cockroach/issues/70111
// TODO(baptist): This method is poorly named and confusing. It is used as a
// "hint" to use a connection if it already exists, but simultaneously kick off
// a connection attempt in the background if it doesn't and always return
// immediately. It is only used today by DistSQL and it should probably be
// removed and moved into that code. Also, as of #99191, we have stateful
// circuit breakers that probe in the background and so whatever exactly it
// is the caller really wants can likely be achieved by more direct means.
func (n *Dialer) ConnHealthTryDial(nodeID roachpb.NodeID, class rpcbase.ConnectionClass) error {
	err := n.ConnHealth(nodeID, class)
	if err == nil {
		return err
	}
	addr, locality, err := n.resolver(nodeID)
	if err != nil {
		return err
	}
	// NB: This will always return `ErrNotHeartbeated` since the heartbeat will
	// not be done by the time `Health` is called since GRPCDialNode is async.
	return n.rpcContext.GRPCDialNode(addr.String(), nodeID, locality, class).Health()
}

// ConnHealthTryDialInstance returns nil if we have an open connection of the
// rpc.DefaultClass to the given sqlinstance that succeeded on its most recent
// heartbeat. If no healthy connection is found, it will attempt to dial the
// instance.
func (n *Dialer) ConnHealthTryDialInstance(id base.SQLInstanceID, addr string) error {
	if n == nil {
		return errors.New("no node dialer configured")
	}
	if err := n.rpcContext.ConnHealth(
		addr, roachpb.NodeID(id), rpcbase.DefaultClass); err == nil {
		return nil
	}
	return n.rpcContext.GRPCDialPod(addr, id, roachpb.Locality{}, rpcbase.DefaultClass).Health()
}

// GetCircuitBreaker retrieves the circuit breaker for connections to the
// given node. The breaker should not be mutated as this affects all connections
// dialing to that node through this NodeDialer.
func (n *Dialer) GetCircuitBreaker(
	nodeID roachpb.NodeID, class rpcbase.ConnectionClass,
) (*circuit2.Breaker, bool) {
	addr, _, err := n.resolver(nodeID)
	if err != nil {
		return nil, false
	}
	return n.rpcContext.GetBreakerForAddr(nodeID, class, addr)
}

// Latency returns the exponentially weighted moving average latency to the
// given node ID. Returns a latency of 0 with no error if we don't have enough
// samples to compute a reliable average.
func (n *Dialer) Latency(nodeID roachpb.NodeID) (time.Duration, error) {
	if n == nil || n.resolver == nil {
		return 0, errors.AssertionFailedf("no node dialer configured")
	}
	if n.rpcContext.RemoteClocks == nil {
		return 0, errors.AssertionFailedf("can't call Latency in a client command")
	}
	latency, ok := n.rpcContext.RemoteClocks.Latency(nodeID)
	if !ok {
		latency = 0
	}
	return latency, nil
}

// baseInternalClient is a wrapper around a grpc.ClientConn that implements the
// RestrictedInternalClient interface. By calling kvpb.NewInternalClient on each
// RPC invocation, that function can be inlined and the returned internalClient
// object (which itself is just a wrapper) never needs to be allocated on the
// heap.
type baseInternalClient grpc.ClientConn

func newBaseInternalClient(conn *grpc.ClientConn) rpc.RestrictedInternalClient {
	return (*baseInternalClient)(conn)
}

func (c *baseInternalClient) asConn() *grpc.ClientConn {
	return (*grpc.ClientConn)(c)
}

// Batch implements the RestrictedInternalClient interface.
func (c *baseInternalClient) Batch(
	ctx context.Context, ba *kvpb.BatchRequest,
) (*kvpb.BatchResponse, error) {
	return kvpb.NewGRPCInternalClientAdapter(c.asConn()).Batch(ctx, ba)
}

// MuxRangeFeed implements the RestrictedInternalClient interface.
func (c *baseInternalClient) MuxRangeFeed(
	ctx context.Context,
) (kvpb.RPCInternal_MuxRangeFeedClient, error) {
	return kvpb.NewGRPCInternalClientAdapter(c.asConn()).MuxRangeFeed(ctx)
}

var batchStreamPoolingEnabled = settings.RegisterBoolSetting(
	settings.ApplicationLevel,
	"rpc.batch_stream_pool.enabled",
	"if true, use pooled gRPC streams to execute Batch RPCs",
	metamorphic.ConstantWithTestBool("rpc.batch_stream_pool.enabled", true),
)

func shouldUseBatchStreamPoolClient(ctx context.Context, st *cluster.Settings) bool {
	return batchStreamPoolingEnabled.Get(&st.SV)
}

// batchStreamPoolClient is a client that sends Batch RPCs using a pooled
// BatchStream RPC stream. Pooling these streams allows for reuse of gRPC
// resources, as opposed to native unary RPCs, which create a new stream and
// throw it away for each unary request (see grpc.invoke).
type batchStreamPoolClient rpc.BatchStreamPool

func newBatchStreamPoolClient(pool *rpc.BatchStreamPool) rpc.RestrictedInternalClient {
	return (*batchStreamPoolClient)(pool)
}

func (c *batchStreamPoolClient) asPool() *rpc.BatchStreamPool {
	return (*rpc.BatchStreamPool)(c)
}

// Batch implements the RestrictedInternalClient interface, using the pooled
// streams in the BatchStreamPool to issue the Batch RPC.
func (c *batchStreamPoolClient) Batch(
	ctx context.Context, ba *kvpb.BatchRequest,
) (*kvpb.BatchResponse, error) {
	return c.asPool().Send(ctx, ba)
}

// MuxRangeFeed implements the RestrictedInternalClient interface.
func (c *batchStreamPoolClient) MuxRangeFeed(
	ctx context.Context,
) (kvpb.RPCInternal_MuxRangeFeedClient, error) {
	return kvpb.NewGRPCInternalClientAdapter(c.asPool().Conn()).MuxRangeFeed(ctx)
}

// tracingInternalClient wraps a RestrictedInternalClient and fills in trace
// information on Batch RPCs.
//
// Note that tracingInternalClient is not used to wrap the internalClientAdapter
// - local RPCs don't need this tracing functionality.
type tracingInternalClient struct {
	rpc.RestrictedInternalClient
}

func maybeWrapInTracingClient(
	ctx context.Context, client rpc.RestrictedInternalClient,
) rpc.RestrictedInternalClient {
	sp := tracing.SpanFromContext(ctx)
	if sp != nil {
		return &tracingInternalClient{RestrictedInternalClient: client}
	}
	return client
}

// Batch overrides the Batch RPC client method and fills in tracing information.
func (c *tracingInternalClient) Batch(
	ctx context.Context, ba *kvpb.BatchRequest,
) (*kvpb.BatchResponse, error) {
	sp := tracing.SpanFromContext(ctx)
	if sp != nil {
		ba = ba.ShallowCopy()
		ba.TraceInfo = sp.Meta().ToProto()
	}
	return c.RestrictedInternalClient.Batch(ctx, ba)
}
