package changefeedccl

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/cockroachdb/crlib/crtime"
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
	if f, ok := s.(interface {
		FinalizeFilesUpTo(context.Context, hlc.Timestamp) error
	}); ok {
		require.NoError(t, f.FinalizeFilesUpTo(ctx, hlc.MaxTimestamp))
	}

	// Expect at least one file-closed callback with the path payload.
	require.NotEmpty(t, got)
	// Sanity check payload format (JSON with path field).
	require.Contains(t, string(bytes.Join(got, []byte{'\n'})), "\"path\"")
}

func TestIcebergSink_PartitionedFinalizeAndFence(t *testing.T) {
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

	warehouseDir := filepath.Join(testDir(t), "wh2")
	esURL := sinkURIForNodelocal(t, warehouseDir)
	iceURL, err := url.Parse("iceberg://rest.local/ns1/tpart")
	require.NoError(t, err)
	iceSinkURL := &changefeedbase.SinkURL{URL: iceURL}

	// Provide a partition spec with two fields to verify directory layout and spec_id propagation.
	specJSON := `{"spec":{"spec_id":1,"fields":[{"name":"ts_day","source":"ts","transform":"day"},{"name":"id_bucket","source":"id","transform":"bucket[4]"}]},"warehouse":"` + esURL.String() + `"}`
	opts := changefeedbase.IcebergSinkOptions{JSONConfig: changefeedbase.SinkSpecificJSONConfig([]byte(specJSON))}

	sf, err := span.MakeFrontier(roachpb.Span{Key: []byte("a"), EndKey: []byte("b")})
	require.NoError(t, err)
	oracle := &changeAggregatorLowerBoundOracle{sf: sf}

	s, err := makeIcebergSink(ctx, iceSinkURL, 1, settings, oracle, esFromURI, user, jobspb.JobID(7), changefeedbase.Targets{}, changefeedbase.EncodingOptions{Format: changefeedbase.OptFormatParquet, Envelope: changefeedbase.OptEnvelopeBare}, nil, opts)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	// Capture FileClosed payloads.
	var got [][]byte
	if p, ok := s.(interface{ RegisterFileClosedHandler(func([]byte)) }); ok {
		p.RegisterFileClosedHandler(func(b []byte) { got = append(got, append([]byte(nil), b...)) })
	}

	// Manually insert an open file with partition values and finalize via fence.
	topic := makeIcebergTopic(t, "tpart")
	key := icebergFileKey{tableID: topic.GetTopicIdentifier().TableID, familyID: topic.GetTopicIdentifier().FamilyID, schemaID: uint32(topic.GetVersion()), partKey: "ts_day=2025-01-02/id_bucket=3"}
	of := &icebergOpenFile{
		key:        key,
		topic:      topic,
		schemaID:   key.schemaID,
		created:    crtime.NowMono(),
		oldestMVCC: hlc.Timestamp{WallTime: 1},
		latestMVCC: hlc.Timestamp{WallTime: 2},
		numRows:    5,
		partition:  map[string]string{"ts_day": "2025-01-02", "id_bucket": "3"},
	}
	// Seed a small buffer as file content.
	_, _ = of.buf.Write([]byte("abc"))
	s.(*icebergSink).files.ReplaceOrInsert(key, of)

	// Finalize all files up to resolved R; expect one FileClosed.
	R := hlc.Timestamp{WallTime: 123, Logical: 0}
	require.NoError(t, s.(interface {
		FinalizeFilesUpTo(context.Context, hlc.Timestamp) error
	}).FinalizeFilesUpTo(ctx, R))
	require.NotEmpty(t, got)

	// Validate JSON payload fields roughly.
	var payload map[string]any
	require.NoError(t, json.Unmarshal(got[len(got)-1], &payload))
	path := payload["path"].(string)
	require.Contains(t, path, "ns1/tpart/crdb_job=7/ts_day=2025-01-02/id_bucket=3/")
	require.Contains(t, path, "epoch=123-0/")
	// Spec id propagated
	require.EqualValues(t, float64(1), payload["spec_id"]) // json numbers decode as float64
	require.EqualValues(t, float64(5), payload["record_count"])
}
