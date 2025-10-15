// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package changefeedccl

import (
	"context"
	"encoding/json"
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

type icebergJSONConfig struct {
	Warehouse    string `json:"warehouse"`
	Auth         string `json:"auth"`
	Token        string `json:"token"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Scope        string `json:"scope"`
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
	_ context.Context,
	u *changefeedbase.SinkURL,
	_ changefeedbase.Targets,
	enc changefeedbase.EncodingOptions,
	mb metricsRecorderBuilder,
	opts changefeedbase.IcebergSinkOptions,
) (Sink, error) {
	if enc.Format != changefeedbase.OptFormatParquet {
		return nil, errors.Newf("%s sink requires %s=parquet", changefeedbase.SinkSchemeIceberg, changefeedbase.OptFormat)
	}
	if _, err := parseIcebergConfig(u, opts.JSONConfig, opts.EqualityDeleteBy); err != nil {
		return nil, err
	}
	m := mb(requiresResourceAccounting)
	return &icebergSink{m: m}, nil
}
