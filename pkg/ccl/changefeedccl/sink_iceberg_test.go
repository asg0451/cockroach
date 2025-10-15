package changefeedccl

import (
	"bytes"
	"context"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/blobs"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdcevent"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/cloud"
	_ "github.com/cockroachdb/cockroach/pkg/cloud/impl" // register cloud storage providers
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/span"
	"github.com/stretchr/testify/require"
)

// makeIcebergTopic is a minimal helper to construct a TopicDescriptor for tests.
func makeIcebergTopic(t *testing.T, name string) TopicDescriptor {
	t.Helper()
	d := tabledesc.NewBuilder(&descpb.TableDescriptor{Name: name, ID: 1001}).BuildImmutableTable()
	md := makeMetadata(d)
	spec := changefeedbase.Target{
		Type:              jobspb.ChangefeedTargetSpecification_PRIMARY_FAMILY_ONLY,
		DescID:            d.GetID(),
		StatementTimeName: changefeedbase.StatementTimeName(name),
	}
	return &tableDescriptorTopic{Metadata: md, spec: spec}
}

// makeSimpleRow constructs a tiny cdcevent.Row for tests.
func makeSimpleRow(t *testing.T, tableName string, ts hlc.Timestamp) cdcevent.Row {
	t.Helper()
	d := tabledesc.NewBuilder(&descpb.TableDescriptor{Name: tableName, ID: 1001}).BuildImmutableTable()
	md := makeMetadata(d)
	return cdcevent.Row{EventDescriptor: &cdcevent.EventDescriptor{Metadata: md}, MvccTimestamp: ts}
}

func sinkURIForNodelocal(t *testing.T, dir string) *changefeedbase.SinkURL {
	t.Helper()
	u, err := url.Parse("nodelocal://0/" + filepath.ToSlash(dir))
	require.NoError(t, err)
	return &changefeedbase.SinkURL{URL: u}
}

func TestIcebergSink_BasicParquetAndFileClosed(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	externalIODir, cleanup := testutils.TempDir(t)
	defer cleanup()

	settings := cluster.MakeTestingClusterSettings()
	user := username.RootUserName()

	clientFactory := blobs.TestBlobServiceClient(externalIODir)
	esFromURI := func(ctx context.Context, uri string, user username.SQLUsername, opts ...cloud.ExternalStorageOption) (cloud.ExternalStorage, error) {
		return cloud.ExternalStorageFromURI(ctx, uri, base.ExternalIODirConfig{}, settings, clientFactory, user, nil, nil, cloud.NilMetrics, opts...)
	}

	// Build a minimal iceberg sink pointing to a nodelocal warehouse.
	warehouseDir := filepath.Join(testDir(t), "wh")
	esURL := sinkURIForNodelocal(t, warehouseDir)
	iceURL, err := url.Parse("iceberg://rest.local/ns1/t1")
	require.NoError(t, err)
	iceSinkURL := &changefeedbase.SinkURL{URL: iceURL}

	// Inject minimal JSON config with warehouse path.
	opts := changefeedbase.IcebergSinkOptions{JSONConfig: changefeedbase.SinkSpecificJSONConfig([]byte(`{"warehouse":"` + esURL.String() + `"}`))}

	// Minimal oracle (unused for now); use a dummy span frontier.
	sf, err := span.MakeFrontier(roachpb.Span{Key: []byte("a"), EndKey: []byte("b")})
	require.NoError(t, err)
	oracle := &changeAggregatorLowerBoundOracle{sf: sf}

	s, err := makeIcebergSink(ctx, iceSinkURL, 1, settings, oracle, esFromURI, user, jobspb.JobID(42), changefeedbase.Targets{}, changefeedbase.EncodingOptions{Format: changefeedbase.OptFormatParquet, Envelope: changefeedbase.OptEnvelopeBare}, nil, opts)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	// Register a file-closed handler and capture calls.
	var got [][]byte
	if p, ok := s.(interface{ RegisterFileClosedHandler(func([]byte)) }); ok {
		p.RegisterFileClosedHandler(func(b []byte) { got = append(got, append([]byte(nil), b...)) })
	}

	// Encode a trivial parquet row and flush.
	topic := makeIcebergTopic(t, "t1")
	row := makeSimpleRow(t, "t1", hlc.Timestamp{WallTime: 1})
	require.NoError(t, s.(SinkWithEncoder).EncodeAndEmitRow(ctx, row, cdcevent.Row{}, topic, hlc.Timestamp{WallTime: 2}, row.MvccTimestamp, changefeedbase.EncodingOptions{Format: changefeedbase.OptFormatParquet, Envelope: changefeedbase.OptEnvelopeBare}, zeroAlloc))
	require.NoError(t, s.Flush(ctx))

	// Expect at least one file-closed callback with the path payload.
	require.NotEmpty(t, got)
	// Sanity check payload format.
	require.Contains(t, string(bytes.Join(got, []byte{'\n'})), "path=")
}
