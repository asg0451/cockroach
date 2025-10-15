// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package changefeedccl

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/RaduBerinde/btreemap"
	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdcevent"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/kvevent"
	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/parquet"
	"github.com/cockroachdb/crlib/crtime"
	"github.com/cockroachdb/errors"
)

// isIcebergSink returns true if the provided URL indicates the iceberg sink.
func isIcebergSink(u *url.URL) bool {
	return u.Scheme == changefeedbase.SinkSchemeIceberg
}

// icebergSink is a placeholder implementation that compiles and allows
// end-to-end plumbing while the full sink is implemented.
type icebergSink struct {
	m metricsRecorder
	// optional callback to notify aggregator on file close (Iceberg mode)
	fileClosedHandlers []func([]byte)

	// storage and context
	es       cloud.ExternalStorage
	settings *cluster.Settings
	oracle   timestampLowerBoundOracle
	jobID    jobspb.JobID
	nodeID   base.SQLInstanceID

	// config
	cfg         icebergConfig
	targetMaxSz int64

	// optional partition spec (from config for MVP)
	spec   PartitionSpec
	specID int32

	// per-topic keyed open file state
	files *btreemap.BTreeMap[icebergFileKey, *icebergOpenFile]

	// sequence for data file naming
	seq uint64
}

type icebergFileKey struct {
	tableID  descpb.ID
	familyID descpb.FamilyID
	schemaID uint32
	// partKey encodes partition name=value pairs in a stable order for the file
	// series. Empty if no partition spec is configured.
	partKey string
}

func icebergKeyCmp(a, b icebergFileKey) int {
	if c := cmp.Compare(a.tableID, b.tableID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.familyID, b.familyID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.schemaID, b.schemaID); c != 0 {
		return c
	}
	return cmp.Compare(a.partKey, b.partKey)
}

type icebergOpenFile struct {
	key        icebergFileKey
	topic      TopicDescriptor
	schemaID   uint32
	created    crtime.Mono
	oldestMVCC hlc.Timestamp
	latestMVCC hlc.Timestamp
	buf        bytes.Buffer
	writer     *parquetWriter
	numRows    int
	// partition holds evaluated partition values for this open file.
	partition map[string]string
}

func (s *icebergSink) getConcreteType() sinkType { return sinkTypeIceberg }
func (s *icebergSink) Dial() error               { return nil }
func (s *icebergSink) Close() error              { return nil }

// For parquet, EncodeAndEmitRow is used via SinkWithEncoder; EmitRow should not be called.
func (s *icebergSink) EmitRow(
	ctx context.Context,
	topic TopicDescriptor,
	key, value []byte,
	updated, mvcc hlc.Timestamp,
	_ kvevent.Alloc,
	_headers rowHeaders,
) error {
	return errors.AssertionFailedf("EmitRow unimplemented by iceberg sink; parquet path uses EncodeAndEmitRow")
}

func (s *icebergSink) EmitResolvedTimestamp(ctx context.Context, _ Encoder, _ hlc.Timestamp) error {
	s.m.recordResolvedCallback()()
	return nil
}

func (s *icebergSink) Flush(ctx context.Context) error {
	done := s.m.recordFlushRequestCallback()
	defer done()
	if s.files == nil {
		return nil
	}
	// Only flush Parquet buffered data; do not finalize or upload.
	vals := s.files.Ascend(btreemap.Min[icebergFileKey](), btreemap.Max[icebergFileKey]())
	for _, of := range vals {
		if of.numRows == 0 || of.writer == nil {
			continue
		}
		if err := of.writer.flush(); err != nil {
			return err
		}
	}
	return nil
}

// FileClosedProvider allows the aggregator to register a callback for file-closed events.
type FileClosedProvider interface {
	RegisterFileClosedHandler(func([]byte))
}

// ResolvedFence allows the aggregator to request that the sink finalize all
// files up to the provided resolved timestamp before emitting the resolved.
type ResolvedFence interface {
	FinalizeFilesUpTo(context.Context, hlc.Timestamp) error
}

// RegisterFileClosedHandler implements FileClosedProvider.
func (s *icebergSink) RegisterFileClosedHandler(h func([]byte)) {
	s.fileClosedHandlers = append(s.fileClosedHandlers, h)
}

type icebergConfig struct {
	restEndpoint     string
	namespace        string
	table            string
	warehouseURI     string
	equalityDeleteBy string // CSV of column names for MVP; parsed later
	// auth parameters (placeholders for MVP)
	authKind     string
	token        string
	clientID     string
	clientSecret string
	scope        string
}

type icebergJSONConfig struct {
	Warehouse    string `json:"warehouse"`
	Auth         string `json:"auth"`
	Token        string `json:"token"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Scope        string `json:"scope"`
	// Optional Iceberg partition spec and id
	Spec   json.RawMessage `json:"spec"`
	SpecID int             `json:"spec_id"`
}

func parseIcebergConfig(
	u *changefeedbase.SinkURL,
	jsonStr changefeedbase.SinkSpecificJSONConfig,
	equalityDeleteBy string,
) (icebergConfig, error) {
	cfg := icebergConfig{}
	if u.Host == "" {
		return cfg, errors.Newf("missing REST endpoint host for scheme %s", changefeedbase.SinkSchemeIceberg)
	}
	cfg.restEndpoint = u.Host

	// Expect at least /<namespace>/<table>
	p := strings.Trim(u.Path, "/")
	parts := []string{}
	if p != "" {
		parts = strings.Split(p, "/")
	}
	if len(parts) < 2 {
		return cfg, errors.Newf("invalid iceberg URI path %q: expected /<namespace>/<table>", u.Path)
	}
	cfg.namespace = parts[len(parts)-2]
	cfg.table = parts[len(parts)-1]

	// Parse JSON config for required/optional settings.
	var jc icebergJSONConfig
	if jsonStr != `` {
		if err := json.Unmarshal([]byte(jsonStr), &jc); err != nil {
			return cfg, errors.Wrap(err, "invalid iceberg_sink_config json")
		}
	}
	cfg.warehouseURI = jc.Warehouse
	if cfg.warehouseURI == "" {
		return cfg, errors.Newf("%s requires %s in %s", changefeedbase.SinkSchemeIceberg, "warehouse", changefeedbase.OptIcebergSinkConfig)
	}

	cfg.equalityDeleteBy = equalityDeleteBy
	cfg.authKind = strings.ToLower(jc.Auth)
	cfg.token = jc.Token
	cfg.clientID = jc.ClientID
	cfg.clientSecret = jc.ClientSecret
	cfg.scope = jc.Scope

	switch cfg.authKind {
	case "", "none":
		// no auth
	case "bearer":
		if cfg.token == "" {
			return cfg, errors.Newf("auth=bearer requires token parameter")
		}
	case "oauth":
		if cfg.clientID == "" || cfg.clientSecret == "" {
			return cfg, errors.Newf("auth=oauth requires client_id and client_secret")
		}
	default:
		return cfg, errors.Newf("unsupported auth kind %q; use none|bearer|oauth", cfg.authKind)
	}

	// Validate no unknown query params are supplied (use JSON instead)
	if rem := u.RemainingQueryParams(); len(rem) > 0 {
		return cfg, errors.Newf("invalid query parameters %v for scheme %s; use %s instead", rem, changefeedbase.SinkSchemeIceberg, changefeedbase.OptIcebergSinkConfig)
	}
	return cfg, nil
}

// makeIcebergSink constructs a minimal iceberg sink from the sink URL.
// URL structure is expected to be: iceberg://<rest-endpoint>/<namespace>/<table>
func makeIcebergSink(
	ctx context.Context,
	u *changefeedbase.SinkURL,
	srcID base.SQLInstanceID,
	settings *cluster.Settings,
	oracle timestampLowerBoundOracle,
	makeExternalStorageFromURI cloud.ExternalStorageFromURIFactory,
	user username.SQLUsername,
	jobID jobspb.JobID,
	_ changefeedbase.Targets,
	enc changefeedbase.EncodingOptions,
	mb metricsRecorderBuilder,
	opts changefeedbase.IcebergSinkOptions,
) (Sink, error) {
	if enc.Format != changefeedbase.OptFormatParquet {
		return nil, errors.Newf("%s sink requires %s=parquet", changefeedbase.SinkSchemeIceberg, changefeedbase.OptFormat)
	}
	cfg, err := parseIcebergConfig(u, opts.JSONConfig, opts.EqualityDeleteBy)
	if err != nil {
		return nil, err
	}
	// Open ExternalStorage for the warehouse URI.
	es, err := makeExternalStorageFromURI(ctx, cfg.warehouseURI, user, cloud.WithIOAccountingInterceptor(nil), cloud.WithClientName("cdc"))
	if err != nil {
		return nil, err
	}
	m := mb(es.RequiresExternalIOAccounting())
	s := &icebergSink{
		m:           m,
		es:          es,
		settings:    settings,
		oracle:      oracle,
		jobID:       jobID,
		nodeID:      srcID,
		cfg:         cfg,
		targetMaxSz: 16 << 20, // 16MB
		files:       btreemap.New[icebergFileKey, *icebergOpenFile](8, icebergKeyCmp),
	}
	// Parse optional partition spec from options JSON.
	if opts.JSONConfig != `` {
		var jc icebergJSONConfig
		// Best-effort parse; ignore errors here since config may not include spec.
		_ = json.Unmarshal([]byte(opts.JSONConfig), &jc)
		if len(jc.Spec) > 0 {
			spec, specID, err := parsePartitionSpecJSON([]byte(jc.Spec), jc.SpecID)
			if err != nil {
				return nil, err
			}
			s.spec = spec
			s.specID = int32(specID)
		}
	}
	return s, nil
}

// EncodeAndEmitRow implements SinkWithEncoder for parquet path.
func (s *icebergSink) EncodeAndEmitRow(
	ctx context.Context,
	updatedRow cdcevent.Row,
	prevRow cdcevent.Row,
	topic TopicDescriptor,
	updated, mvcc hlc.Timestamp,
	encodingOpts changefeedbase.EncodingOptions,
	_ kvevent.Alloc,
) error {
	if s.files == nil {
		return errors.New("cannot EncodeAndEmitRow on a closed sink")
	}
	// Evaluate partition values if a spec is configured.
	var parts map[string]string
	var partKey string
	if len(s.spec.Fields) > 0 {
		p, err := EvaluatePartition(updatedRow, s.spec)
		if err != nil {
			return err
		}
		parts = p
		// Build a stable partKey of name=value pairs in spec order.
		b := strings.Builder{}
		for i, f := range s.spec.Fields {
			if v, ok := parts[f.Name]; ok {
				if i > 0 {
					b.WriteByte('/')
				}
				b.WriteString(f.Name)
				b.WriteByte('=')
				b.WriteString(v)
			}
		}
		partKey = b.String()
	}
	key := icebergFileKey{tableID: topic.GetTopicIdentifier().TableID, familyID: topic.GetTopicIdentifier().FamilyID, schemaID: uint32(topic.GetVersion()), partKey: partKey}
	_, of, _ := s.files.Get(key)
	if of == nil {
		of = &icebergOpenFile{key: key, topic: topic, schemaID: key.schemaID, created: crtime.NowMono(), oldestMVCC: mvcc, latestMVCC: mvcc, partition: parts}
		pw, err := newParquetWriterFromRow(updatedRow, &of.buf, encodingOpts, parquet.WithCompressionCodec(parquet.CompressionZSTD))
		if err != nil {
			return err
		}
		of.writer = pw
		s.files.ReplaceOrInsert(key, of)
	}
	if of.oldestMVCC.IsEmpty() || mvcc.Less(of.oldestMVCC) {
		of.oldestMVCC = mvcc
	}
	if of.latestMVCC.Less(mvcc) {
		of.latestMVCC = mvcc
	}
	if err := of.writer.addData(updatedRow, prevRow, updated, mvcc); err != nil {
		return err
	}
	of.numRows++
	// If buffered + written exceeds target, flush row group and finalize file.
	if int64(of.buf.Len())+of.writer.estimatedBufferedBytes() > s.targetMaxSz {
		if err := of.writer.flush(); err != nil {
			return err
		}
		if err := s.finalizeAndUpload(ctx, of, updated); err != nil {
			return err
		}
		// Replace with a fresh file for the same key
		newOf := &icebergOpenFile{key: key, topic: topic, schemaID: key.schemaID, created: crtime.NowMono(), oldestMVCC: hlc.MaxTimestamp, latestMVCC: hlc.MinTimestamp, partition: parts}
		pw, err := newParquetWriterFromRow(updatedRow, &newOf.buf, encodingOpts, parquet.WithCompressionCodec(parquet.CompressionZSTD))
		if err != nil {
			return err
		}
		newOf.writer = pw
		s.files.ReplaceOrInsert(key, newOf)
	}
	return nil
}

func (s *icebergSink) finalizeAndUpload(ctx context.Context, of *icebergOpenFile, epoch hlc.Timestamp) error {
	if of.writer != nil {
		if err := of.writer.close(); err != nil {
			return err
		}
	}
	// Build deterministic path: <warehouse>/<ns>/<table>/crdb_job=<job_id>/<partition...>/epoch=<R>/shard=<worker_id>/data-<seq>.parquet
	seq := atomic.AddUint64(&s.seq, 1)
	epochPart := fmt.Sprintf("epoch=%d-%d", epoch.WallTime, epoch.Logical)
	shardPart := fmt.Sprintf("shard=%d", s.nodeID)
	fileName := fmt.Sprintf("data-%08x.parquet", seq)
	var pathElems []string
	pathElems = append(pathElems, s.cfg.namespace, s.cfg.table, fmt.Sprintf("crdb_job=%d", s.jobID))
	if len(s.spec.Fields) > 0 && len(of.partition) > 0 {
		for _, f := range s.spec.Fields {
			if v, ok := of.partition[f.Name]; ok {
				pathElems = append(pathElems, fmt.Sprintf("%s=%s", f.Name, v))
			}
		}
	}
	pathElems = append(pathElems, epochPart, shardPart, fileName)
	dest := filepath.Join(pathElems...)
	// Write file to warehouse via ExternalStorage
	if err := cloud.WriteFile(ctx, s.es, dest, bytes.NewReader(of.buf.Bytes())); err != nil {
		return err
	}
	// Emit FileClosed as protobuf (no metrics yet). Delay proto dependency by
	// encoding payload via helper to avoid requiring generated code in this file.
	payload := marshalFileClosed(dest, epoch, fmt.Sprintf("%s.%s", s.cfg.namespace, s.cfg.table), int32(s.nodeID), of.schemaID, s.specID, int64(of.numRows), int64(of.buf.Len()), of.partition)
	for _, h := range s.fileClosedHandlers {
		h(payload)
	}
	// record metrics and mark as finalized
	s.m.recordEmittedBatch(of.created, of.numRows, of.oldestMVCC, of.buf.Len(), sinkDoesNotCompress)
	of.numRows = 0
	return nil
}

// FinalizeFilesUpTo implements ResolvedFence. It finalizes open files whose latest
// MVCC is <= resolved and emits FileClosed for them. Remaining files stay open.
func (s *icebergSink) FinalizeFilesUpTo(ctx context.Context, resolved hlc.Timestamp) error {
	if s.files == nil {
		return nil
	}
	vals := s.files.Ascend(btreemap.Min[icebergFileKey](), btreemap.Max[icebergFileKey]())
	for _, of := range vals {
		if of.numRows == 0 {
			continue
		}
		if of.latestMVCC.IsEmpty() || resolved.IsEmpty() {
			continue
		}
		if of.latestMVCC.LessEq(resolved) {
			if of.writer != nil {
				if err := of.writer.flush(); err != nil {
					return err
				}
			}
			if err := s.finalizeAndUpload(ctx, of, resolved); err != nil {
				return err
			}
			s.files.Delete(of.key)
		}
	}
	return nil
}
