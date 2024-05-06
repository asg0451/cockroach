package changefeedccl

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/volatility"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

// type txner interface {
// 	Txn(ctx context.Context, f func(context.Context, isql.Txn) error, opts ...isql.TxnOption) error
// }

type querier interface {
	QueryIteratorEx(ctx context.Context, opName string, override sessiondata.InternalExecutorOverride, stmt string, qargs ...interface{}) (eval.InternalRows, error)
}

// FetchChangefeedBillingBytes fetches the total number of bytes of data watched
// by all changefeeds. It counts tables that are watched by multiple changefeeds
// multiple times.
func FetchChangefeedBillingBytes(ctx context.Context, querier querier, execCfg *sql.ExecutorConfig) (int64, error) {
	deets, err := getChangefeedDetails(ctx, querier)
	if err != nil {
		return 0, err
	}

	feedsTableIds := make(map[int][]descpb.ID, len(deets))
	tableIDs := make([]descpb.ID, 0, len(deets))
	for cdi, cd := range deets {
		// check both possible locations for table data due to older proto version. TODO: is this still necessary?
		// inspired by AllTargets in changefeedccl/changefeed.go
		if len(cd.TargetSpecifications) > 0 {
			for _, ts := range cd.TargetSpecifications {
				if ts.TableID > 0 {
					feedsTableIds[cdi] = append(feedsTableIds[cdi], ts.TableID)
					tableIDs = append(tableIDs, ts.TableID)
				}
			}
		} else {
			for id := range cd.Tables {
				feedsTableIds[cdi] = append(feedsTableIds[cdi], id)
				tableIDs = append(tableIDs, id)
			}
		}
	}

	tableSizes, err := fetchTableSizes(ctx, execCfg, tableIDs)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, tableIds := range feedsTableIds {
		for _, id := range tableIds {
			total += tableSizes[id]
		}
	}
	return total, nil
}

func fetchTableSizes(ctx context.Context, execCfg *sql.ExecutorConfig, tableIDs []descpb.ID) (map[descpb.ID]int64, error) {
	tableSizes := make(map[descpb.ID]int64, len(tableIDs))

	type spanInfo struct {
		span  roachpb.Span
		table descpb.ID
	}

	// build list of spans for all tables
	spanSizes := make(map[string]spanInfo, len(tableIDs))
	spans := make([]roachpb.Span, 0, len(tableIDs))
	for _, id := range tableIDs {
		// fetch table descriptor
		var desc catalog.TableDescriptor
		fetchTableDesc := func(
			ctx context.Context, txn isql.Txn, descriptors *descs.Collection,
		) error {
			tableDesc, err := descriptors.ByID(txn.KV()).WithoutNonPublic().Get().Table(ctx, id)
			if err != nil {
				return err
			}
			desc = tableDesc
			return nil
		}
		if err := sql.DescsTxn(ctx, execCfg, fetchTableDesc); err != nil {
			if errors.Is(err, catalog.ErrDescriptorDropped) {
				// if the table was dropped, we can ignore it this cycle
				continue
			}
			return nil, err
		}

		// TODO: do we need to count the sizes of other indexes?
		span := desc.PrimaryIndexSpan(execCfg.Codec)
		spans = append(spans, span)
		spanSizes[span.String()] = spanInfo{span: span, table: id}
	}

	// fetch span stats and fill in table sizes
	resp, err := execCfg.TenantStatusServer.SpanStats(
		ctx,
		&roachpb.SpanStatsRequest{
			NodeID: "0", // fan out
			Spans:  spans,
		},
	)
	if err != nil {
		return nil, err
	}
	for spanStr, stats := range resp.SpanToStats {
		si := spanSizes[spanStr]
		tableSizes[si.table] += stats.ApproximateTotalStats.LiveBytes
	}

	return tableSizes, nil
}

const changefeedDetailsQuery = `
	SELECT j.id, ji.value
	FROM system.jobs j JOIN system.job_info ji ON j.id = ji.job_id
	WHERE status IN ('running', 'paused') AND job_type = 'CHANGEFEED'
		AND info_key = '` + jobs.LegacyPayloadKey + `'
`

// getChangefeedDetails fetches the changefeed details for all changefeeds.
func getChangefeedDetails(ctx context.Context, querier querier) ([]*jobspb.ChangefeedDetails, error) {
	var deets []*jobspb.ChangefeedDetails

	it, err := querier.QueryIteratorEx(ctx, "changefeeds_billing_payloads", sessiondata.NodeUserSessionDataOverride, changefeedDetailsQuery)
	if err != nil {
		return nil, err
	}
	defer func() { _ = it.Close() }()

	var ok bool
	for ok, err = it.Next(ctx); ok; ok, err = it.Next(ctx) {
		row := it.Cur()
		id, payloadBs := int64(tree.MustBeDInt(row[0])), tree.MustBeDBytes(row[1])
		var payload jobspb.Payload
		if err := payload.Unmarshal([]byte(payloadBs)); err != nil {
			return nil, errors.WithDetailf(err, "failed to unmarshal payload for job %d", id)
		}

		details := payload.GetDetails()
		if details == nil {
			return nil, errors.AssertionFailedf("no details for job %d", id)
		}
		cfDetails, ok := details.(*jobspb.Payload_Changefeed)
		if !ok {
			return nil, errors.AssertionFailedf("unexpected details type %T for job %d", details, id)
		}
		deets = append(deets, cfDetails.Changefeed)
	}

	return deets, err
}

// Setup the sql builtin. This is used for either one-off validation or for
// metrics collection itelf, depending on which approach we go with.
func init() {
	overload := tree.Overload{
		Types:      tree.ParamTypes{},
		ReturnType: tree.FixedReturnType(types.Int),
		Fn: func(ctx context.Context, evalCtx *eval.Context, args tree.Datums) (tree.Datum, error) {
			sum, err := FetchChangefeedBillingBytes(ctx, evalCtx.Planner, evalCtx.Planner.ExecutorConfig().(*sql.ExecutorConfig))
			if err != nil {
				return nil, pgerror.Wrap(err, pgcode.Internal, ``)
			}
			return tree.NewDInt(tree.DInt(sum)), nil
		},
		Class:      tree.NormalClass,
		Info:       "TODO",
		Volatility: volatility.Volatile,
	}

	utilccl.RegisterCCLBuiltin("crdb_internal.changefeed_table_bytes",
		`Returns the sum of bytes watched per table per changefeed`, overload)
}
