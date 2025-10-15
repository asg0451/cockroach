// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package changefeedccl

import (
	"context"
	"net/url"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/kvevent"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
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
}

func (s *icebergSink) getConcreteType() sinkType { return sinkTypeIceberg }
func (s *icebergSink) Dial() error               { return nil }
func (s *icebergSink) Close() error              { return nil }

func (s *icebergSink) EmitRow(
	ctx context.Context,
	topic TopicDescriptor,
	key, value []byte,
	updated, mvcc hlc.Timestamp,
	_ kvevent.Alloc,
	_headers rowHeaders,
) error {
	// TODO: implement iceberg writer
	s.m.recordOneMessage()(mvcc, len(key)+len(value), sinkDoesNotCompress)
	return nil
}

func (s *icebergSink) EmitResolvedTimestamp(ctx context.Context, _ Encoder, _ hlc.Timestamp) error {
	s.m.recordResolvedCallback()()
	return nil
}

func (s *icebergSink) Flush(ctx context.Context) error {
	s.m.recordFlushRequestCallback()()
	return nil
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

func parseIcebergConfig(u *changefeedbase.SinkURL) (icebergConfig, error) {
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

	// Required query params
	cfg.warehouseURI = u.ConsumeParam("warehouse")
	if cfg.warehouseURI == "" {
		return cfg, errors.Newf("scheme %s requires parameter %s", changefeedbase.SinkSchemeIceberg, "warehouse")
	}

	// Optional params
	cfg.equalityDeleteBy = u.ConsumeParam("equality_delete_by")
	cfg.authKind = strings.ToLower(u.ConsumeParam("auth"))
	cfg.token = u.ConsumeParam("token")
	cfg.clientID = u.ConsumeParam("client_id")
	cfg.clientSecret = u.ConsumeParam("client_secret")
	cfg.scope = u.ConsumeParam("scope")

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

	// Validate no unknown params remain (defensive; helps catch typos)
	if rem := u.RemainingQueryParams(); len(rem) > 0 {
		return cfg, errors.Newf("invalid query parameters %v for scheme %s", rem, changefeedbase.SinkSchemeIceberg)
	}
	return cfg, nil
}

// makeIcebergSink constructs a minimal iceberg sink from the sink URL.
// URL structure is expected to be: iceberg://<rest-endpoint>/<namespace>/<table>?warehouse=...&...
func makeIcebergSink(
	_ context.Context,
	u *changefeedbase.SinkURL,
	_ changefeedbase.Targets,
	enc changefeedbase.EncodingOptions,
	mb metricsRecorderBuilder,
) (Sink, error) {
	if enc.Format != changefeedbase.OptFormatParquet {
		return nil, errors.Newf("%s sink requires %s=parquet", changefeedbase.SinkSchemeIceberg, changefeedbase.OptFormat)
	}
	if _, err := parseIcebergConfig(u); err != nil {
		return nil, err
	}
	m := mb(requiresResourceAccounting)
	return &icebergSink{m: m}, nil
}
