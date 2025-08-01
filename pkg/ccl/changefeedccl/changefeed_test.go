// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package changefeedccl

import (
	"context"
	gosql "database/sql"
	"encoding/base64"
	gojson "encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/cockroachdb/cockroach-go/v2/crdb"
	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdceval"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdcevent"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdctest"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/checkpoint"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/kvevent"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/resolvedspan"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/schemafeed/schematestutils"
	_ "github.com/cockroachdb/cockroach/pkg/ccl/kvccl/kvtenantccl" // multi-tenant tests
	_ "github.com/cockroachdb/cockroach/pkg/ccl/multiregionccl"    // locality-related table mutations
	_ "github.com/cockroachdb/cockroach/pkg/ccl/partitionccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl"
	_ "github.com/cockroachdb/cockroach/pkg/cloud/impl" // registers cloud storage providers
	"github.com/cockroachdb/cockroach/pkg/internal/sqlsmith"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/kvcoord"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/closedts"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/desctestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/flowinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/randgen"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/storage/fs"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/jobutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/listenerutil"
	"github.com/cockroachdb/cockroach/pkg/testutils/pgurlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/cidr"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/json"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/eventpb"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/randident"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/span"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing/tracingpb"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/dustin/go-humanize"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testServerRegion = "us-east-1"

func TestChangefeedBasics(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, foo)

		// 'initial' is skipped because only the latest value ('updated') is
		// emitted by the initial scan.
		assertPayloads(t, foo, []string{
			`foo: [0]->{"after": {"a": 0, "b": "updated"}}`,
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b')`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"a": 1, "b": "a"}}`,
			`foo: [2]->{"after": {"a": 2, "b": "b"}}`,
		})

		sqlDB.Exec(t, `UPSERT INTO foo VALUES (2, 'c'), (3, 'd')`)
		assertPayloads(t, foo, []string{
			`foo: [2]->{"after": {"a": 2, "b": "c"}}`,
			`foo: [3]->{"after": {"a": 3, "b": "d"}}`,
		})

		sqlDB.Exec(t, `DELETE FROM foo WHERE a = 1`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": null}`,
		})
	}

	cdcTest(t, testFn, feedTestForceSink("kafka"))
	cdcTest(t, testFn, feedTestForceSink("enterprise"))
	cdcTest(t, testFn, feedTestForceSink("webhook"))
	cdcTest(t, testFn, feedTestForceSink("pubsub"))
	cdcTest(t, testFn, feedTestForceSink("sinkless"))
	cdcTest(t, testFn, feedTestForceSink("cloudstorage"))

	// NB running TestChangefeedBasics, which includes a DELETE, with
	// cloudStorageTest is a regression test for #36994.
}

func TestChangefeedBasicQuery(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)
		foo := feed(t, f, `CREATE CHANGEFEED AS SELECT *, event_op() AS op, cdc_prev FROM foo`)
		defer closeFeed(t, foo)

		// 'initial' is skipped because only the latest value ('updated') is
		// emitted by the initial scan.
		assertPayloads(t, foo, []string{
			`foo: [0]->{"a": 0, "b": "updated", "cdc_prev": null, "op": "insert"}`,
		})
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b')`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"a": 1, "b": "a", "cdc_prev": null, "op": "insert"}`,
			`foo: [2]->{"a": 2, "b": "b", "cdc_prev": null, "op": "insert"}`,
		})
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (2, 'c'), (3, 'd')`)
		assertPayloads(t, foo, []string{
			`foo: [2]->{"a": 2, "b": "c", "cdc_prev": {"a": 2, "b": "b"}, "op": "update"}`,
			`foo: [3]->{"a": 3, "b": "d", "cdc_prev": null, "op": "insert"}`,
		})
		// Deleted rows with bare envelope are emitted with only
		// the key columns set.
		sqlDB.Exec(t, `DELETE FROM foo WHERE a = 1`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"a": 1, "b": null, "cdc_prev": {"a": 1, "b": "a"}, "op": "delete"}`,
		})
	}

	cdcTest(t, testFn)
}

// TestChangefeedIdentifyDependentTablesForProtecting identifies (system) tables
// that are accessed in the course of running a changefeed and ensures that they
// are all in the list of tables we protect with PTS. It does this by running a
// sinkless changefeed and capturing a trace of its execution. For completeness,
// it includes a cdc query in the changefeed, under the assumption that doing so
// will require more tables.
func TestChangefeedIdentifyDependentTablesForProtecting(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cfCreated := atomic.Bool{}

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		foo := feed(t, f, `CREATE CHANGEFEED AS SELECT *, event_op() AS op, cdc_prev FROM foo`)
		defer closeFeed(t, foo)
		cfCreated.Store(true)

		assertPayloads(t, foo, []string{
			`foo: [0]->{"a": 0, "b": "initial", "cdc_prev": null, "op": "insert"}`,
		})

		// Do some operations so the changefeed does some scanning.
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (2, 'c'), (3, 'd')`)
		sqlDB.Exec(t, `DELETE FROM foo WHERE a = 1`)

		assertPayloads(t, foo, []string{
			`foo: [1]->{"a": 1, "b": "a", "cdc_prev": null, "op": "insert"}`,
			`foo: [2]->{"a": 2, "b": "b", "cdc_prev": null, "op": "insert"}`,
			`foo: [2]->{"a": 2, "b": "c", "cdc_prev": {"a": 2, "b": "b"}, "op": "update"}`,
			`foo: [3]->{"a": 3, "b": "d", "cdc_prev": null, "op": "insert"}`,
			`foo: [1]->{"a": 1, "b": null, "cdc_prev": {"a": 1, "b": "a"}, "op": "delete"}`,
		})
	}

	trimRx := regexp.MustCompile(`executing (.*), \[txn:.*`)
	tableIdRx := regexp.MustCompile(`(/Tenant/[0-9]+)?/(Table|NamespaceTable)/([0-9]+)`)

	tableIDsAccessed := map[catid.DescID]struct{}{}

	// Parse trace logs of the following format into table ids and add them to our map:
	// `executing Scan [/Tenant/10/Table/3/1,/Tenant/10/Table/3/2), Scan [/Tenant/10/NamespaceTable/30/1,/Tenant/10/NamespaceTable/30/2), ...`
	// This seems a bit brittle, but it's the best we can do currently.
	noteExecutingScansLog := func(msg string) {
		trimmed := trimRx.FindStringSubmatch(msg)[1]
		spanStmts := strings.Split(trimmed, ", ")
		for _, spanStmt := range spanStmts {
			spanStmt = strings.Replace(spanStmt, "Scan ", "", 1)
			startEnd := strings.Split(strings.Trim(spanStmt, "[)"), ",")
			require.Len(t, startEnd, 2)

			start := startEnd[0]
			matches := tableIdRx.FindStringSubmatch(start)
			require.NotEmpty(t, matches)

			tableID, err := strconv.ParseInt(matches[3], 10, 64)
			require.NoError(t, err)
			tableIDsAccessed[catid.DescID(tableID)] = struct{}{}
		}
	}

	traceCb := func(trace tracingpb.Recording, stmt string) {
		if !cfCreated.Load() {
			return
		}
		if !strings.HasPrefix(stmt, "CREATE CHANGEFEED") {
			return
		}
		for _, span := range trace {
			for _, log := range span.Logs {
				msg := log.Message.StripMarkers()
				if !strings.Contains(msg, "executing Scan") {
					continue
				}
				noteExecutingScansLog(msg)
			}
		}
	}

	cdcTest(t, testFn, withKnobsFn(func(knobs *base.TestingKnobs) {
		knobs.JobsTestingKnobs = jobs.NewTestingKnobsWithShortIntervals()
		if knobs.SQLExecutor == nil {
			knobs.SQLExecutor = &sql.ExecutorTestingKnobs{}
		}
		knobs.SQLExecutor.(*sql.ExecutorTestingKnobs).WithStatementTrace = traceCb
	}), feedTestForceSink("sinkless"))

	// NOTE: not all the required tables will necessarily show up in every run due to caching. However we should always see SOME tables.
	require.NotEmpty(t, tableIDsAccessed)

	var unexpectedTableIDs []catid.DescID
	for id := range tableIDsAccessed {
		if !slices.Contains(systemTablesToProtect, id) {
			unexpectedTableIDs = append(unexpectedTableIDs, id)
		}
	}
	require.Empty(t, unexpectedTableIDs)
}

// Same test as TestChangefeedBasicQuery, but using wrapped envelope with CDC query.
func TestChangefeedBasicQueryWrapped(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)
		// Currently, parquet format (which may be injected by feed() call),  doesn't
		// know how to handle tuple types (cdc_prev); so, force JSON format.
		foo := feed(t, f, `
CREATE CHANGEFEED WITH envelope='wrapped', format='json', diff
AS SELECT b||a AS ba, event_op() AS op  FROM foo`)
		defer closeFeed(t, foo)

		// 'initial' is skipped because only the latest value ('updated') is
		// emitted by the initial scan.
		assertPayloads(t, foo, []string{
			`foo: [0]->{"after": {"ba": "updated0", "op": "insert"}, "before": null}`,
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b')`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"ba": "a1", "op": "insert"}, "before": null}`,
			`foo: [2]->{"after": {"ba": "b2", "op": "insert"}, "before": null}`,
		})

		// Wrapped envelope results in "before" having entire previous row state -- *not* projection.
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (2, 'c'), (3, 'd')`)
		assertPayloads(t, foo, []string{
			`foo: [2]->{"after": {"ba": "c2", "op": "update"}, "before": {"a": 2, "b": "b"}}`,
			`foo: [3]->{"after": {"ba": "d3", "op": "insert"}, "before": null}`,
		})

		sqlDB.Exec(t, `DELETE FROM foo WHERE a = 1`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": null, "before": {"a": 1, "b": "a"}}`,
		})
	}

	cdcTest(t, testFn, feedTestForceSink("webhook"))
}

// Same test as TestChangefeedBasicQueryWrapped, but this time using AVRO.
func TestChangefeedBasicQueryWrappedAvro(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)
		foo := feed(t, f, `
CREATE CHANGEFEED WITH envelope='wrapped', format='avro', diff
AS SELECT *, event_op() AS op  FROM foo`)
		defer closeFeed(t, foo)

		// 'initial' is skipped because only the latest value ('updated') is
		// emitted by the initial scan.
		assertPayloads(t, foo, []string{
			`foo: {"a":{"long":0}}->{"after":{"foo":{"a":{"long":0},"b":{"string":"updated"},"op":{"string":"insert"}}},"before":null}`,
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b')`)
		assertPayloads(t, foo, []string{
			`foo: {"a":{"long":1}}->{"after":{"foo":{"a":{"long":1},"b":{"string":"a"},"op":{"string":"insert"}}},"before":null}`,
			`foo: {"a":{"long":2}}->{"after":{"foo":{"a":{"long":2},"b":{"string":"b"},"op":{"string":"insert"}}},"before":null}`,
		})

		sqlDB.Exec(t, `UPSERT INTO foo VALUES (2, 'c'), (3, 'd')`)
		assertPayloads(t, foo, []string{
			`foo: {"a":{"long":2}}->{"after":{"foo":{"a":{"long":2},"b":{"string":"c"},"op":{"string":"update"}}},"before":{"foo_before":{"a":{"long":2},"b":{"string":"b"}}}}`,
			`foo: {"a":{"long":3}}->{"after":{"foo":{"a":{"long":3},"b":{"string":"d"},"op":{"string":"insert"}}},"before":null}`,
		})

		sqlDB.Exec(t, `DELETE FROM foo WHERE a = 1`)
		assertPayloads(t, foo, []string{
			`foo: {"a":{"long":1}}->{"after":null,"before":{"foo_before":{"a":{"long":1},"b":{"string":"a"}}}}`,
		})
	}

	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestRLSBlocking(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE rls (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO rls VALUES (0, 'initial')`)
		sqlDB.Exec(t, `INSERT INTO rls VALUES (1, 'second')`)

		// Make sure CDC query cannot start if table is RLS enabled.
		sqlDB.Exec(t, `ALTER TABLE rls ENABLE ROW LEVEL SECURITY`)
		expErrSubstr := "CDC queries are not supported on tables with row-level security enabled"
		expectErrCreatingFeed(t, f, `CREATE CHANGEFEED AS SELECT * FROM rls WHERE a != 0`, expErrSubstr)

		// Ensure that CDC query fails after creating if table becomes RLS enabled.
		sqlDB.Exec(t, "ALTER TABLE rls DISABLE ROW LEVEL SECURITY")
		tf := feed(t, f, `CREATE CHANGEFEED AS SELECT * FROM rls WHERE a != 0`)
		defer closeFeed(t, tf)
		assertPayloads(t, tf, []string{
			`rls: [1]->{"a": 1, "b": "second"}`,
		})
		sqlDB.Exec(t, `ALTER TABLE rls ENABLE ROW LEVEL SECURITY`)
		sqlDB.Exec(t, `INSERT INTO rls VALUES (2, 'third')`)
		_, err := readNextMessages(context.Background(), tf, 1)
		require.Error(t, err)
		require.Contains(t, err.Error(), expErrSubstr)
	}

	cdcTest(t, testFn, withAllowChangefeedErr("expects terminal error"))
}

func TestToJSONAsChangefeed(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo values (1, 'hello')`)
		sqlDB.CheckQueryResults(t,
			`SELECT crdb_internal.to_json_as_changefeed_with_flags(foo.*) from foo`,
			[][]string{{`{"after": {"a": 1, "b": "hello"}}`}},
		)
		sqlDB.CheckQueryResults(t,
			`SELECT crdb_internal.to_json_as_changefeed_with_flags(foo.*, 'updated', 'diff') from foo`,
			[][]string{{`{"after": {"a": 1, "b": "hello"}, "before": null, "updated": "0.0000000000"}`}},
		)

		sqlDB.CheckQueryResults(t,
			`SELECT crdb_internal.to_json_as_changefeed_with_flags(foo.*, 'updated', 'envelope=row') from foo`,
			[][]string{{`{"__crdb__": {"updated": "0.0000000000"}, "a": 1, "b": "hello"}`}},
		)

		sqlDB.ExpectErrWithTimeout(t, `unknown envelope: lobster`,
			`SELECT crdb_internal.to_json_as_changefeed_with_flags(foo.*, 'updated', 'envelope=lobster') from foo`)
	}

	cdcTest(t, testFn)
}

// TestChangefeedProgressMetrics tests the changefeed.aggregator_progress and
// changefeed.checkpoint_progress metrics.
func TestChangefeedProgressMetrics(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Verify the aggmetric functional gauges work correctly
	t.Run("aggregate functional gauge", func(t *testing.T) {
		cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
			registry := s.Server.JobRegistry().(*jobs.Registry)
			metrics := registry.MetricsStruct().Changefeed.(*Metrics)
			defaultSLI, err := metrics.getSLIMetrics(defaultSLIScope)
			require.NoError(t, err)
			sliA, err := metrics.getSLIMetrics("scope_a")
			require.NoError(t, err)
			sliB, err := metrics.getSLIMetrics("scope_b")
			require.NoError(t, err)

			defaultSLI.mu.checkpoint[5] = hlc.Timestamp{WallTime: 1}

			sliA.mu.checkpoint[1] = hlc.Timestamp{WallTime: 2}
			sliA.mu.checkpoint[2] = hlc.Timestamp{WallTime: 5}
			sliA.mu.checkpoint[3] = hlc.Timestamp{WallTime: 0} // Zero timestamp should be ignored.

			sliB.mu.checkpoint[1] = hlc.Timestamp{WallTime: 4}
			sliB.mu.checkpoint[2] = hlc.Timestamp{WallTime: 9}

			// Ensure each scope gets the correct value
			require.Equal(t, int64(1), defaultSLI.CheckpointProgress.Value())
			require.Equal(t, int64(2), sliA.CheckpointProgress.Value())
			require.Equal(t, int64(4), sliB.CheckpointProgress.Value())

			// Ensure the value progresses upon changefeed progress
			defaultSLI.mu.checkpoint[5] = hlc.Timestamp{WallTime: 20}
			require.Equal(t, int64(20), defaultSLI.CheckpointProgress.Value())

			// Ensure the value updates correctly upon changefeeds completing
			delete(sliB.mu.checkpoint, 1)
			require.Equal(t, int64(9), sliB.CheckpointProgress.Value())
			delete(sliB.mu.checkpoint, 2)
			require.Equal(t, int64(0), sliB.CheckpointProgress.Value())

			// Ensure the aggregate value is correct after progress / completion
			require.Equal(t, int64(2), metrics.AggMetrics.CheckpointProgress.Value())
			sliA.mu.checkpoint[1] = hlc.Timestamp{WallTime: 30}
			require.Equal(t, int64(5), metrics.AggMetrics.CheckpointProgress.Value())
			delete(sliA.mu.checkpoint, 2)
			require.Equal(t, int64(20), metrics.AggMetrics.CheckpointProgress.Value())
			delete(defaultSLI.mu.checkpoint, 5)
			require.Equal(t, int64(30), metrics.AggMetrics.CheckpointProgress.Value())
			delete(sliA.mu.checkpoint, 1)
			require.Equal(t, int64(0), metrics.AggMetrics.CheckpointProgress.Value())
		})
	})

	// Verify that ids must be registered to have an effect.
	t.Run("id registration", func(t *testing.T) {
		cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
			registry := s.Server.JobRegistry().(*jobs.Registry)
			metrics := registry.MetricsStruct().Changefeed.(*Metrics)
			sliA, err := metrics.getSLIMetrics("scope_a")
			require.NoError(t, err)

			unregisteredID := int64(999)
			id1 := sliA.claimId()
			id2 := sliA.claimId()
			id3 := sliA.claimId()
			sliA.setResolved(unregisteredID, hlc.Timestamp{WallTime: 0})
			sliA.setResolved(id1, hlc.Timestamp{WallTime: 1})
			sliA.setResolved(id2, hlc.Timestamp{WallTime: 2})
			sliA.setResolved(id3, hlc.Timestamp{WallTime: 3})

			sliA.setCheckpoint(unregisteredID, hlc.Timestamp{WallTime: 0})
			sliA.setCheckpoint(id1, hlc.Timestamp{WallTime: 1})
			sliA.setCheckpoint(id2, hlc.Timestamp{WallTime: 2})
			sliA.setCheckpoint(id3, hlc.Timestamp{WallTime: 3})

			require.Equal(t, int64(1), metrics.AggMetrics.CheckpointProgress.Value())
			require.Equal(t, int64(1), metrics.AggMetrics.AggregatorProgress.Value())

			sliA.closeId(id1)

			require.Equal(t, int64(2), metrics.AggMetrics.CheckpointProgress.Value())
			require.Equal(t, int64(2), metrics.AggMetrics.AggregatorProgress.Value())

		})
	})

	// Verify that a changefeed updates the timestamps as it progresses
	t.Run("running changefeed", func(t *testing.T) {
		cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
			sqlDB := sqlutils.MakeSQLRunner(s.DB)

			sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
			fooA := feed(t, f, `CREATE CHANGEFEED FOR foo WITH metrics_label='label_a', resolved='100ms'`)

			registry := s.Server.JobRegistry().(*jobs.Registry)
			metrics := registry.MetricsStruct().Changefeed.(*Metrics)
			sliA, err := metrics.getSLIMetrics("label_a")
			require.NoError(t, err)

			// Verify that aggregator_progress has recurring updates
			var lastTimestamp int64 = 0
			for i := 0; i < 3; i++ {
				testutils.SucceedsSoon(t, func() error {
					progress := sliA.AggregatorProgress.Value()
					if progress > lastTimestamp {
						lastTimestamp = progress
						return nil
					}
					return errors.Newf("waiting for aggregator_progress to advance from %d (value=%d)",
						lastTimestamp, progress)
				})
			}

			// Verify that checkpoint_progress has recurring updates
			for i := 0; i < 3; i++ {
				testutils.SucceedsSoon(t, func() error {
					progress := sliA.CheckpointProgress.Value()
					if progress > lastTimestamp {
						lastTimestamp = progress
						return nil
					}
					return errors.Newf("waiting for checkpoint_progress to advance from %d (value=%d)",
						lastTimestamp, progress)
				})
			}

			// Verify that max_behind_nanos has recurring updates
			var lastValue int64 = 0
			for i := 0; i < 3; i++ {
				testutils.SucceedsSoon(t, func() error {
					value := sliA.MaxBehindNanos.Value()
					if value != lastValue {
						lastValue = value
						return nil
					}
					return errors.Newf("waiting for max_behind_nanos to update %d",
						lastValue)
				})
			}

			sliB, err := registry.MetricsStruct().Changefeed.(*Metrics).getSLIMetrics("label_b")
			require.Equal(t, int64(0), sliB.AggregatorProgress.Value())
			fooB := feed(t, f, `CREATE CHANGEFEED FOR foo WITH metrics_label='label_b', resolved='100ms'`)
			defer closeFeed(t, fooB)
			require.NoError(t, err)
			// Verify that aggregator_progress has recurring updates
			testutils.SucceedsSoon(t, func() error {
				progress := sliB.AggregatorProgress.Value()
				if progress > 0 {
					return nil
				}
				return errors.Newf("waiting for second aggregator_progress to advance (value=%d)", progress)
			})

			closeFeed(t, fooA)
			testutils.SucceedsSoon(t, func() error {
				aggregatorProgress := sliA.AggregatorProgress.Value()
				checkpointProgress := sliA.CheckpointProgress.Value()
				maxBehindNanos := sliA.MaxBehindNanos.Value()
				if aggregatorProgress == 0 && checkpointProgress == 0 && maxBehindNanos == 0 {
					return nil
				}
				return errors.Newf("waiting for progress metrics to be 0 (ap=%d, cp=%d)",
					aggregatorProgress, checkpointProgress)
			})
		})
	})
}

func TestChangefeedIdleness(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		changefeedbase.IdleTimeout.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 3*time.Second)

		registry := s.Server.JobRegistry().(*jobs.Registry)
		currentlyIdle := registry.MetricsStruct().JobMetrics[jobspb.TypeChangefeed].CurrentlyIdle
		// Use a wait group for cases when the number of idle changefeeds temporarily
		// decreases, to avoid a race condition where the changefeed becomes idle
		// before the idleness is checked.
		var wg sync.WaitGroup
		waitForIdleCount := func(numIdle int64) {
			wg.Add(1)
			testutils.SucceedsSoon(t, func() error {
				if currentlyIdle.Value() != numIdle {
					return fmt.Errorf("expected (%+v) idle changefeeds, found (%+v)", numIdle, currentlyIdle.Value())
				}
				return nil
			})
			wg.Done()
		}
		done := make(chan bool)
		workload := func() {
			for {
				select {
				case <-done:
					return
				default:
					sqlDB.Exec(t, `INSERT INTO foo VALUES (0)`)
					sqlDB.Exec(t, `DELETE FROM foo WHERE a = 0`)
					sqlDB.Exec(t, `INSERT INTO bar VALUES (0)`)
					sqlDB.Exec(t, `DELETE FROM bar WHERE b = 0`)
				}
			}
		}

		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `CREATE TABLE bar (b INT PRIMARY KEY)`)
		cf1 := feed(t, f, "CREATE CHANGEFEED FOR TABLE foo WITH resolved='10ms'") // higher resolved frequency for faster test
		cf2 := feed(t, f, "CREATE CHANGEFEED FOR TABLE bar WITH resolved='10ms'")
		defer closeFeed(t, cf1)

		go workload()
		go waitForIdleCount(0)
		wg.Wait()
		done <- true
		waitForIdleCount(2) // Both should eventually be considered idle

		jobFeed := cf2.(cdctest.EnterpriseTestFeed)
		require.NoError(t, jobFeed.Pause())
		waitForIdleCount(1) // Paused jobs aren't considered idle

		require.NoError(t, jobFeed.Resume())
		waitForIdleCount(2) // Resumed job should eventually become idle

		closeFeed(t, cf2)
		waitForIdleCount(1) // The cancelled changefeed isn't considered idle

		go workload()
		go waitForIdleCount(0)
		wg.Wait()
		done <- true
		waitForIdleCount(1)

	}, feedTestEnterpriseSinks)
}

// TestChangefeedSendError validates that SendErrors do not fail the changefeed
// as they can occur in normal situations such as a cluster update
func TestChangefeedSendError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0)`)

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		// Allow triggering a single sendError
		sendErrorCh := make(chan error, 1)
		knobs.FeedKnobs.OnRangeFeedValue = func() error {
			select {
			case err := <-sendErrorCh:
				return err
			default:
				return nil
			}
		}

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, foo)

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (2)`)
		sendErrorCh <- kvcoord.TestNewSendError("test sendError")
		sqlDB.Exec(t, `INSERT INTO foo VALUES (3)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (4)`)

		// Changefeed should've been retried due to the SendError
		registry := s.Server.JobRegistry().(*jobs.Registry)
		sli, err := registry.MetricsStruct().Changefeed.(*Metrics).getSLIMetrics(defaultSLIScope)
		require.NoError(t, err)
		retryCounter := sli.ErrorRetries
		testutils.SucceedsSoon(t, func() error {
			if retryCounter.Value() < 1 {
				return fmt.Errorf("no retry has occured")
			}
			return nil
		})

		assertPayloads(t, foo, []string{
			`foo: [0]->{"after": {"a": 0}}`,
			`foo: [1]->{"after": {"a": 1}}`,
			`foo: [2]->{"after": {"a": 2}}`,
			`foo: [3]->{"after": {"a": 3}}`,
			`foo: [4]->{"after": {"a": 4}}`,
		})
	}, feedTestEnterpriseSinks, withAllowChangefeedErr("injects error"))
}

func TestChangefeedBasicConfluentKafka(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)

		foo := feed(t, f,
			fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH format=%s`, changefeedbase.OptFormatAvro))
		defer closeFeed(t, foo)

		// 'initial' is skipped because only the latest value ('updated') is
		// emitted by the initial scan.
		assertPayloads(t, foo, []string{
			`foo: {"a":{"long":0}}->{"after":{"foo":{"a":{"long":0},"b":{"string":"updated"}}}}`,
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b')`)
		assertPayloads(t, foo, []string{
			`foo: {"a":{"long":1}}->{"after":{"foo":{"a":{"long":1},"b":{"string":"a"}}}}`,
			`foo: {"a":{"long":2}}->{"after":{"foo":{"a":{"long":2},"b":{"string":"b"}}}}`,
		})

		sqlDB.Exec(t, `UPSERT INTO foo VALUES (2, 'c'), (3, 'd')`)
		assertPayloads(t, foo, []string{
			`foo: {"a":{"long":2}}->{"after":{"foo":{"a":{"long":2},"b":{"string":"c"}}}}`,
			`foo: {"a":{"long":3}}->{"after":{"foo":{"a":{"long":3},"b":{"string":"d"}}}}`,
		})

		sqlDB.Exec(t, `DELETE FROM foo WHERE a = 1`)
		assertPayloads(t, foo, []string{
			`foo: {"a":{"long":1}}->{"after":null}`,
		})
	}

	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestChangefeedQuotedTableNameTopicName(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE "MyTable" (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO "MyTable" VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO "MyTable" VALUES (0, 'updated')`)

		foo := feed(t, f,
			`CREATE CHANGEFEED FOR d.public."MyTable" WITH diff, full_table_name`)
		defer closeFeed(t, foo)

		// The topic name should be d.public.MyTable and not d.public._u0022_MyTable_u0022_
		// or d.public."MyTable".
		assertPayloads(t, foo, []string{
			`d.public.MyTable: [0]->{"after": {"a": 0, "b": "updated"}, "before": null}`,
		})
	}

	cdcTest(t, testFn)
}

// TestChangefeedQuotedIdentifiersTopicName is similar to
// TestChangefeedQuotedTableNameTopicName, but for quoted identifiers
// in the SELECT clause instead of the table name.
func TestChangefeedQuotedIdentifiersTopicName(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE mytable (
			id INT PRIMARY KEY, 
			"SomeField" JSONB,
			"AnotherField" JSONB
		)`)

		sqlDB.Exec(t, `INSERT INTO mytable VALUES (
			1, 
			'{"PropA": "value1", "prop_b": "value2"}'::jsonb,
			'{"PropC": "value3", "prop_d": "value4"}'::jsonb
		)`)

		sqlDB.Exec(t, `INSERT INTO mytable VALUES (
			2, 
			'{"PropA": "value5", "prop_b": "value6"}'::jsonb,
			'{"PropC": "value7", "prop_d": "value8"}'::jsonb
		)`)

		foo := feed(t, f, `CREATE CHANGEFEED WITH diff, full_table_name, on_error=pause, envelope=wrapped AS SELECT 
			id,
			"SomeField"->>'PropA' AS "PropA",
			"SomeField"->>'prop_b' AS "PropB",
			"AnotherField"->>'PropC' AS "PropC",
			"AnotherField"->>'prop_d' AS "PropD"
		FROM public.mytable`)
		defer closeFeed(t, foo)

		// The topic should show up as d.public.mytable and not as
		// d.public.u0022_mytable_u0022 or d.public."MyTable".
		assertPayloads(t, foo, []string{
			`d.public.mytable: [1]->{"after": {"PropA": "value1", "PropB": "value2", "PropC": "value3", "PropD": "value4", "id": 1}, "before": null}`,
			`d.public.mytable: [2]->{"after": {"PropA": "value5", "PropB": "value6", "PropC": "value7", "PropD": "value8", "id": 2}, "before": null}`,
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedDiff(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH diff`)
		defer closeFeed(t, foo)

		// 'initial' is skipped because only the latest value ('updated') is
		// emitted by the initial scan.
		assertPayloads(t, foo, []string{
			`foo: [0]->{"after": {"a": 0, "b": "updated"}, "before": null}`,
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b')`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"a": 1, "b": "a"}, "before": null}`,
			`foo: [2]->{"after": {"a": 2, "b": "b"}, "before": null}`,
		})

		sqlDB.Exec(t, `UPSERT INTO foo VALUES (2, 'c'), (3, 'd')`)
		assertPayloads(t, foo, []string{
			`foo: [2]->{"after": {"a": 2, "b": "c"}, "before": {"a": 2, "b": "b"}}`,
			`foo: [3]->{"after": {"a": 3, "b": "d"}, "before": null}`,
		})

		sqlDB.Exec(t, `DELETE FROM foo WHERE a = 1`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": null, "before": {"a": 1, "b": "a"}}`,
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'new a')`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"a": 1, "b": "new a"}, "before": null}`,
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedTenants(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	kvServer, kvSQLdb, cleanup := startTestFullServer(t, makeOptions(t, withArgsFn(func(args *base.TestServerArgs) {
		args.ExternalIODirConfig.DisableOutbound = true
	})))
	defer cleanup()

	tenantArgs := base.TestTenantArgs{
		// crdb_internal.create_tenant called by StartTenant
		TenantID: serverutils.TestTenantID(),
		// Non-enterprise changefeeds are currently only
		// disabled by setting DisableOutbound true
		// everywhere.
		ExternalIODirConfig: base.ExternalIODirConfig{
			DisableOutbound: true,
		},
		UseDatabase: `d`,
	}

	tenantServer, tenantDB := serverutils.StartTenant(t, kvServer, tenantArgs)
	tenantSQL := sqlutils.MakeSQLRunner(tenantDB)
	tenantSQL.ExecMultiple(t, strings.Split(tenantSetupStatements, ";")...)
	tenantSQL.Exec(t, `CREATE TABLE foo_in_tenant (pk INT PRIMARY KEY)`)
	t.Run("changefeed on non-tenant table fails", func(t *testing.T) {
		kvSQL := sqlutils.MakeSQLRunner(kvSQLdb)
		kvSQL.Exec(t, `CREATE TABLE d.foo (pk INT PRIMARY KEY)`)

		tenantSQL.ExpectErr(t, `table "foo" does not exist`,
			`CREATE CHANGEFEED FOR foo`,
		)
	})
	t.Run("sinkful changefeed fails", func(t *testing.T) {
		tenantSQL.ExpectErr(t, "Outbound IO is disabled by configuration, cannot create changefeed into kafka",
			`CREATE CHANGEFEED FOR foo_in_tenant INTO 'kafka://does-not-matter'`,
		)
	})
	t.Run("sinkless changefeed works", func(t *testing.T) {
		coreFeedFactory, cleanup := makeFeedFactory(t, "sinkless", tenantServer, tenantDB)
		defer cleanup()
		tenantSQL.Exec(t, `INSERT INTO foo_in_tenant VALUES (1)`)
		feed := feed(t, coreFeedFactory, `CREATE CHANGEFEED FOR foo_in_tenant`)
		assertPayloads(t, feed, []string{
			`foo_in_tenant: [1]->{"after": {"pk": 1}}`,
		})
	})
}

func TestMissingTableErr(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, cleanup := makeServer(t)
	defer cleanup()

	t.Run("changefeed on non existing table fails", func(t *testing.T) {
		kvSQL := sqlutils.MakeSQLRunner(s.DB)
		kvSQL.ExpectErr(t, `^pq: failed to resolve targets in the CHANGEFEED stmt: table "foo" does not exist`,
			`CREATE CHANGEFEED FOR foo`,
		)
	})
}

func TestChangefeedTenantsExternalIOEnabled(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, cleanup := makeTenantServer(t, withArgsFn(func(args *base.TestServerArgs) {
		args.ExternalIODirConfig.DisableOutbound = true
	}))
	defer cleanup()

	tenantSQL := sqlutils.MakeSQLRunner(s.DB)
	tenantSQL.Exec(t, `CREATE TABLE foo_in_tenant (pk INT PRIMARY KEY)`)

	t.Run("sinkful changefeed works", func(t *testing.T) {
		f, cleanup := makeFeedFactory(t, "kafka", s.Server, s.DB)
		defer cleanup()
		tenantSQL.Exec(t, `INSERT INTO foo_in_tenant VALUES (1)`)
		feed := feed(t, f, `CREATE CHANGEFEED FOR foo_in_tenant`)
		defer closeFeed(t, feed)
		assertPayloads(t, feed, []string{
			`foo_in_tenant: [1]->{"after": {"pk": 1}}`,
		})
	})
}

func TestChangefeedEnvelope(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a')`)

		t.Run(`envelope=row`, func(t *testing.T) {
			foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH envelope='row'`)
			defer closeFeed(t, foo)
			assertPayloads(t, foo, []string{`foo: [1]->{"a": 1, "b": "a"}`})
		})
		t.Run(`envelope=deprecated_row`, func(t *testing.T) {
			foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH envelope='deprecated_row'`)
			defer closeFeed(t, foo)
			assertPayloads(t, foo, []string{`foo: [1]->{"a": 1, "b": "a"}`})
		})
		t.Run(`envelope=key_only`, func(t *testing.T) {
			foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH envelope='key_only'`)
			defer closeFeed(t, foo)
			assertPayloads(t, foo, []string{`foo: [1]->`})
		})
		t.Run(`envelope=wrapped`, func(t *testing.T) {
			foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH envelope='wrapped'`)
			defer closeFeed(t, foo)
			assertPayloads(t, foo, []string{`foo: [1]->{"after": {"a": 1, "b": "a"}}`})
		})
		t.Run(`envelope=wrapped,key_in_value`, func(t *testing.T) {
			foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH key_in_value, envelope='wrapped'`)
			defer closeFeed(t, foo)
			assertPayloads(t, foo, []string{`foo: [1]->{"after": {"a": 1, "b": "a"}, "key": [1]}`})
		})
		// TODO(#139660): add envelope=enriched here
	}

	// some sinks are incompatible with envelope
	cdcTest(t, testFn, feedTestRestrictSinks("sinkless", "enterprise", "kafka"))
}

func TestChangefeedFullTableName(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a')`)

		t.Run(`envelope=row`, func(t *testing.T) {
			foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH full_table_name`)
			defer closeFeed(t, foo)
			assertPayloads(t, foo, []string{`d.public.foo: [1]->{"after": {"a": 1, "b": "a"}}`})
		})
	})
}

func TestChangefeedMultiTable(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a')`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO bar VALUES (2, 'b')`)

		fooAndBar := feed(t, f, `CREATE CHANGEFEED FOR foo, bar`)
		defer closeFeed(t, fooAndBar)

		assertPayloads(t, fooAndBar, []string{
			`foo: [1]->{"after": {"a": 1, "b": "a"}}`,
			`bar: [2]->{"after": {"a": 2, "b": "b"}}`,
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedCursor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	require.NoError(t, log.SetVModule("event_processing=3,blocking_buffer=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		// NB: The test server is a single node and the hlc clock is a
		// singleton. Any transaction (ie. `INSERT INTO`) or call to
		// s.Server.Clock().Now() will share this clock. Any read of the clock
		// increments its current logical time. Thus, the operations below which
		// happen in sequence will have strictly increasing logical timestamps.
		beforeInsert := s.Server.Clock().Now()
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'before')`)
		insertTimestamp := s.Server.Clock().Now()

		tsLogical := s.Server.Clock().Now()
		tsClock := timeutil.FromUnixNanos(tsLogical.WallTime)

		sqlDB.Exec(t, `INSERT INTO foo VALUES (2, 'after')`)

		// Sanity check that operations happened in the expected order.
		require.True(t, beforeInsert.Less(insertTimestamp) && insertTimestamp.Less(tsLogical) && tsLogical.Less(s.Server.Clock().Now()),
			fmt.Sprintf("beforeInsert: %s, insertTimestamp: %s, tsLogical: %s", beforeInsert, insertTimestamp, tsLogical))

		t.Run("negative cursor", func(t *testing.T) {
			// The below function is currently used to test negative timestamp in cursor i.e of the form
			// "-3us".
			// Using this function we can calculate the difference with the time that was before
			// the insert statement, which is set as the new cursor value inside createChangefeedJobRecord
			calculateCursor := func(currentTime *hlc.Timestamp) string {
				//  Should convert to microseconds as that is the maximum precision we support
				diff := (beforeInsert.WallTime - currentTime.WallTime) / 1000
				diffStr := strconv.FormatInt(diff, 10) + "us"
				return diffStr
			}

			knobs := s.TestingKnobs.DistSQL.(*execinfra.TestingKnobs).Changefeed.(*TestingKnobs)
			knobs.OverrideCursor = calculateCursor

			// The "-3 days" is a placeholder here - it will be replaced with actual difference
			// in createChangefeedJobRecord
			fooInterval := feed(t, f, `CREATE CHANGEFEED FOR foo WITH cursor=$1`, "-3 days")
			defer closeFeed(t, fooInterval)
			assertPayloads(t, fooInterval, []string{
				`foo: [1]->{"after": {"a": 1, "b": "before"}}`,
				`foo: [2]->{"after": {"a": 2, "b": "after"}}`,
			})

			// We do not need to override for the remaining cases
			knobs.OverrideCursor = nil
		})

		t.Run("decimal cursor", func(t *testing.T) {
			fooLogical := feed(t, f, `CREATE CHANGEFEED FOR foo WITH cursor=$1`, eval.TimestampToDecimalDatum(tsLogical).String())
			defer closeFeed(t, fooLogical)
			assertPayloads(t, fooLogical, []string{
				`foo: [2]->{"after": {"a": 2, "b": "after"}}`,
			})

			// Check that the cursor is properly hooked up to the job statement
			// time. The sinkless tests currently don't have a way to get the
			// statement timestamp, so only verify this for enterprise.
			if e, ok := fooLogical.(cdctest.EnterpriseTestFeed); ok {
				var bytes []byte
				sqlDB.QueryRow(t, jobutils.JobPayloadByIDQuery, e.JobID()).Scan(&bytes)
				var payload jobspb.Payload
				require.NoError(t, protoutil.Unmarshal(bytes, &payload))
				require.Equal(t, tsLogical, payload.GetChangefeed().StatementTime)
			}
		})

		t.Run("nanos cursor", func(t *testing.T) {
			nanosStr := strconv.FormatInt(tsClock.UnixNano(), 10)
			fooNanosStr := feed(t, f, `CREATE CHANGEFEED FOR foo WITH cursor=$1`, nanosStr)
			defer closeFeed(t, fooNanosStr)
			assertPayloads(t, fooNanosStr, []string{
				`foo: [2]->{"after": {"a": 2, "b": "after"}}`,
			})
		})

		t.Run("datetime cursor", func(t *testing.T) {
			timeStr := tsClock.Format(`2006-01-02 15:04:05.999999`)
			fooString := feed(t, f, `CREATE CHANGEFEED FOR foo WITH cursor=$1`, timeStr)
			defer closeFeed(t, fooString)
			assertPayloads(t, fooString, []string{
				`foo: [2]->{"after": {"a": 2, "b": "after"}}`,
			})
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedTimestamps(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		ctx := context.Background()
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0)`)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH updated, resolved`, optOutOfMetamorphicEnrichedEnvelope{reason: "this test calls testfeed.Next() directly"})
		defer closeFeed(t, foo)

		// Grab the first non resolved-timestamp row.
		var row0 *cdctest.TestFeedMessage
		for {
			var err error
			row0, err = foo.Next()
			assert.NoError(t, err)
			if len(row0.Value) > 0 {
				break
			}
		}

		// If this changefeed uses jobs (and thus stores a ChangefeedDetails), get
		// the statement timestamp from row0 and verify that they match. Otherwise,
		// just skip the row.
		if jf, ok := foo.(cdctest.EnterpriseTestFeed); ok {
			d, err := jf.Details()
			assert.NoError(t, err)
			expected := `{"after": {"a": 0}, "updated": "` + d.StatementTime.AsOfSystemTime() + `"}`
			assert.Equal(t, expected, string(row0.Value))
		}

		// Assert the remaining key using assertPayloads, since we know the exact
		// timestamp expected.
		var ts1 string
		if err := crdb.ExecuteTx(ctx, s.DB, nil /* txopts */, func(tx *gosql.Tx) error {
			return tx.QueryRow(
				`INSERT INTO foo VALUES (1) RETURNING cluster_logical_timestamp()`,
			).Scan(&ts1)
		}); err != nil {
			t.Fatal(err)
		}
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"a": 1}, "updated": "` + ts1 + `"}`,
		})

		// Check that we eventually get a resolved timestamp greater than ts1.
		parsed := parseTimeToHLC(t, ts1)
		for {
			if resolved, _ := expectResolvedTimestamp(t, foo); parsed.Less(resolved) {
				break
			}
		}
	}

	cdcTest(t, testFn)
}

func TestChangefeedMVCCTimestamps(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE mvcc_timestamp_test_table (id UUID PRIMARY KEY DEFAULT gen_random_uuid())`)

		rowCount := 5
		expectedPayloads := make([]string, rowCount)
		for i := 0; i < rowCount; i++ {
			row := sqlDB.QueryRow(t, `INSERT INTO mvcc_timestamp_test_table VALUES (DEFAULT) RETURNING id, cluster_logical_timestamp()`)

			var id string
			var mvccTimestamp string
			row.Scan(&id, &mvccTimestamp)
			expectedPayloads[i] = fmt.Sprintf(`mvcc_timestamp_test_table: ["%[1]s"]->{"after": {"id": "%[1]s"}, "mvcc_timestamp": "%[2]s"}`,
				id, mvccTimestamp)
		}

		changeFeed := feed(t, f, `CREATE CHANGEFEED FOR mvcc_timestamp_test_table WITH mvcc_timestamp`)
		defer closeFeed(t, changeFeed)
		assertPayloads(t, changeFeed, expectedPayloads)
	}

	cdcTest(t, testFn)
}

func TestChangefeedMVCCTimestampsAvro(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE mvcc_timestamp_test_table (id UUID PRIMARY KEY DEFAULT gen_random_uuid())`)

		const rowCount = 5
		expectedPayloads := make([]string, rowCount)
		for i := 0; i < rowCount; i++ {
			row := sqlDB.QueryRow(t, `INSERT INTO mvcc_timestamp_test_table VALUES (DEFAULT) RETURNING id, cluster_logical_timestamp()`)

			var id string
			var mvccTimestamp string
			row.Scan(&id, &mvccTimestamp)
			expectedPayloads[i] = fmt.Sprintf(`mvcc_timestamp_test_table: {"id":{"string":"%[1]s"}}->{"after":{"mvcc_timestamp_test_table":{"id":{"string":"%[1]s"}}},"mvcc_timestamp":{"string":"%[2]s"}}`,
				id, mvccTimestamp)
		}

		changeFeed := feed(t, f, `CREATE CHANGEFEED FOR mvcc_timestamp_test_table WITH mvcc_timestamp, format='avro'`)
		defer closeFeed(t, changeFeed)
		assertPayloads(t, changeFeed, expectedPayloads)
	}

	cdcTest(t, testFn, feedTestForceSink(`kafka`))
}

func TestChangefeedResolvedFrequency(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)

		const freq = 10 * time.Millisecond
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH resolved=$1`, freq.String())
		defer closeFeed(t, foo)

		// We get each resolved timestamp notification once in each partition.
		// Grab the first `2 * #partitions`, sort because we might get all from
		// one partition first, and compare the first and last.
		resolved := make([]hlc.Timestamp, 2*len(foo.Partitions()))
		for i := range resolved {
			resolved[i], _ = expectResolvedTimestamp(t, foo)
		}
		sort.Slice(resolved, func(i, j int) bool { return resolved[i].Less(resolved[j]) })
		first, last := resolved[0], resolved[len(resolved)-1]

		if d := last.GoTime().Sub(first.GoTime()); d < freq {
			t.Errorf(`expected %s between resolved timestamps, but got %s`, freq, d)
		}
	}

	cdcTest(t, testFn)
}

func TestChangefeedRandomExpressions(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	skip.UnderStress(t)
	skip.UnderRace(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		rng, _ := randutil.NewTestRand()
		tblName := "seed"
		defer s.DB.Close()

		setup := sqlsmith.Setups[tblName](rng)
		sqlDB.ExecMultiple(t, setup...)

		// TODO: PopulateTableWithRandData doesn't work with enums
		dropEnumQry := "ALTER TABLE seed DROP COLUMN _enum;"
		sqlDB.Exec(t, dropEnumQry)

		// Attempt to insert a few more than our target 100 values, since there's a
		// small chance we may not succeed that many inserts.
		numInserts := 110
		inserts := make([]string, 0, numInserts)
		for rows := 0; rows < 100; {
			var err error
			var newRows int
			if newRows, err = randgen.PopulateTableWithRandData(rng, s.DB, tblName, numInserts, &inserts); err != nil {
				t.Fatal(err)
			}
			rows += newRows
		}

		limitQry := "DELETE FROM seed WHERE rowid NOT IN (SELECT rowid FROM seed ORDER BY rowid LIMIT 100);"
		sqlDB.Exec(t, limitQry)

		// Put the enums back. enum_range('hi'::greeting)[rowid%7] will give nulls when rowid%7=0 or 6.
		addEnumQry := "ALTER TABLE seed ADD COLUMN _enum greeting;"
		sqlDB.Exec(t, addEnumQry)
		populateEnumQry := "UPDATE seed SET _enum = enum_range('hi'::greeting)[rowid%7];"
		sqlDB.Exec(t, populateEnumQry)
		// Get values to log setup.
		t.Logf("setup:\n%s\n%s\n%s\n%s\n%s\n%s",
			strings.Join(setup, "\n"),
			dropEnumQry,
			strings.Join(inserts, "\n"),
			limitQry,
			addEnumQry,
			populateEnumQry)

		queryGen, err := sqlsmith.NewSmither(s.DB, rng,
			sqlsmith.DisableWith(),
			sqlsmith.DisableMutations(),
			sqlsmith.DisableLimits(),
			sqlsmith.DisableAggregateFuncs(),
			sqlsmith.DisableWindowFuncs(),
			sqlsmith.DisableJoins(),
			sqlsmith.DisableUDFs(),
			sqlsmith.DisableIndexHints(),
			sqlsmith.SetScalarComplexity(0.5),
			sqlsmith.SetComplexity(0.5),
		)
		require.NoError(t, err)
		defer queryGen.Close()
		numNonTrivialTestRuns := 0
		n := 150
		whereClausesChecked := make(map[string]struct{}, n)
		for i := 0; i < n; i++ {
			query := queryGen.Generate()
			where, ok := getWhereClause(query)
			if !ok {
				continue
			}
			if _, alreadyChecked := whereClausesChecked[where]; alreadyChecked {
				continue
			}
			whereClausesChecked[where] = struct{}{}
			query = "SELECT array_to_string(IFNULL(array_agg(distinct rowid),'{}'),'|') FROM seed WHERE " + where
			t.Log(query)
			timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Second*2)
			rows := s.DB.QueryRowContext(timeoutCtx, query)
			var expectedRowIDsStr string
			if err := rows.Scan(&expectedRowIDsStr); err != nil {
				t.Logf("Skipping query %s because error %s", query, err)
				cancel()
				continue
			}
			cancel()
			expectedRowIDs := strings.Split(expectedRowIDsStr, "|")
			if expectedRowIDsStr == "" {
				t.Logf("Skipping predicate %s because it returned no rows", where)
				continue
			}
			createStmt := `CREATE CHANGEFEED WITH schema_change_policy='stop' AS SELECT rowid FROM seed WHERE ` + where
			t.Logf("Expecting statement %s to emit %d events", createStmt, len(expectedRowIDs))
			seedFeed, err := f.Feed(createStmt)
			if err != nil {
				t.Logf("Test tolerating create changefeed error: %s", err.Error())
				if seedFeed != nil {
					closeFeedIgnoreError(t, seedFeed)
				}
				continue
			}
			assertedPayloads := make([]string, len(expectedRowIDs))
			for i, id := range expectedRowIDs {
				assertedPayloads[i] = fmt.Sprintf(`seed: [%s]->{"rowid": %s}`, id, id)
			}
			err = assertPayloadsBaseErr(context.Background(), seedFeed, assertedPayloads, false, false, nil, changefeedbase.OptEnvelopeWrapped)
			closeFeedIgnoreError(t, seedFeed)
			if err != nil {
				// Skip errors that may come up during SQL execution. If the SQL query
				// didn't fail with these errors, it's likely because the query was built in
				// a way that did not have to execute on the row that caused the error, but
				// the CDC query did.
				// Since we get the error that caused the changefeed job to
				// fail from scraping the job status and creating a new
				// error, we unfortunately don't have the pgcode and have to
				// rely on known strings.
				validPgErrs := []string{
					"argument is not an object",
					"cannot subtract infinite dates",
					"dwithin distance cannot be less than zero",
					"error parsing EWKB",
					"error parsing EWKT",
					"error parsing GeoJSON",
					"expected LineString",
					"geometry type is unsupported",
					"invalid escape string",
					"invalid regular expression",
					"no locations to init GEOS",
					"parameter has to be of type Point",
					"regexp compilation failed",
					"result out of range",
					"should be of length",
					"unknown DateStyle parameter",
				}
				containsKnownPgErr := func(e error) (interface{}, bool) {
					for _, v := range validPgErrs {
						if strings.Contains(e.Error(), v) {
							return nil, true
						}
					}
					return nil, false
				}
				if _, contains := errors.If(err, containsKnownPgErr); contains {
					t.Logf("Skipping statement %s because it encountered pgerror %s", createStmt, err)
					continue
				}

				t.Fatal(err)
			}
			numNonTrivialTestRuns++
		}
		require.Greater(t, numNonTrivialTestRuns, 0, "Expected >0 predicates to be nontrivial out of %d attempts", n)
		t.Logf("%d predicates checked: all had the same result in SELECT and CHANGEFEED", numNonTrivialTestRuns)

	}

	cdcTest(t, testFn, feedTestForceSink(`kafka`))
}

// getWhereClause extracts the predicate from a randomly generated SQL statement.
func getWhereClause(query string) (string, bool) {
	var p parser.Parser
	stmts, err := p.Parse(query)
	if err != nil {
		return "", false
	}
	if len(stmts) != 1 {
		return "", false
	}
	selectStmt, ok := stmts[0].AST.(*tree.Select).Select.(*tree.SelectClause)
	if !ok {
		return "", false
	}
	if selectStmt.Where == nil || len(selectStmt.From.Tables) == 0 {
		return "", false
	}
	// Replace all table references with "seed" because we're not using the FROM clause so we can't reference aliases.
	replaced, err := tree.SimpleVisit(selectStmt.Where.Expr, func(expr tree.Expr) (recurse bool, newExpr tree.Expr, err error) {
		if ci, ok := expr.(*tree.ColumnItem); ok {
			newCI := *ci
			newCI.TableName = &tree.UnresolvedObjectName{NumParts: 1, Parts: [3]string{``, ``, `seed`}}
			expr = &newCI
		}
		if un, ok := expr.(*tree.UnresolvedName); ok && un.NumParts > 1 {
			un.Parts[un.NumParts-1] = `seed`
		}
		return true, expr, nil
	})
	return replaced.String(), err == nil
}

// Test how Changefeeds react to schema changes that do not require a backfill
// operation.
func TestChangefeedInitialScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	noInitialScanTests := map[string]string{
		`no cursor - no initial scan`:     `CREATE CHANGEFEED FOR no_initial_scan WITH no_initial_scan, resolved='1s'`,
		`no cursor - no initial backfill`: `CREATE CHANGEFEED FOR no_initial_scan WITH initial_scan = 'no', resolved='1s'`,
	}

	initialScanTests := map[string]string{
		`cursor - with initial scan`:     `CREATE CHANGEFEED FOR initial_scan WITH initial_scan, resolved='1s', cursor='%s'`,
		`cursor - with initial backfill`: `CREATE CHANGEFEED FOR initial_scan WITH initial_scan = 'yes', resolved='1s', cursor='%s'`,
	}

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		for testName, changefeedStmt := range noInitialScanTests {
			t.Run(testName, func(t *testing.T) {
				sqlDB.Exec(t, `CREATE TABLE no_initial_scan (a INT PRIMARY KEY)`)
				defer sqlDB.Exec(t, `DROP TABLE no_initial_scan`)
				sqlDB.Exec(t, `INSERT INTO no_initial_scan VALUES (1)`)

				noInitialScan := feed(t, f, changefeedStmt)
				defer closeFeed(t, noInitialScan)

				expectResolvedTimestamp(t, noInitialScan)

				sqlDB.Exec(t, `INSERT INTO no_initial_scan VALUES (2)`)
				assertPayloads(t, noInitialScan, []string{
					`no_initial_scan: [2]->{"after": {"a": 2}}`,
				})
			})
		}

		for testName, changefeedStmtFormat := range initialScanTests {
			t.Run(testName, func(t *testing.T) {
				sqlDB.Exec(t, `CREATE TABLE initial_scan (a INT PRIMARY KEY)`)
				defer sqlDB.Exec(t, `DROP TABLE initial_scan`)
				sqlDB.Exec(t, `INSERT INTO initial_scan VALUES (1), (2), (3)`)
				var tsStr string
				var i int
				sqlDB.QueryRow(t, `SELECT count(*), cluster_logical_timestamp() from initial_scan`).Scan(&i, &tsStr)
				initialScan := feed(t, f, fmt.Sprintf(changefeedStmtFormat, tsStr))
				defer closeFeed(t, initialScan)

				assertPayloads(t, initialScan, []string{
					`initial_scan: [1]->{"after": {"a": 1}}`,
					`initial_scan: [2]->{"after": {"a": 2}}`,
					`initial_scan: [3]->{"after": {"a": 3}}`,
				})
				sqlDB.Exec(t, `INSERT INTO initial_scan VALUES (4)`)
				assertPayloads(t, initialScan, []string{
					`initial_scan: [4]->{"after": {"a": 4}}`,
				})
			})
		}
	}

	cdcTest(t, testFn)
}

// TestChangefeedLaggingRangesMetrics tests the behavior of the
// changefeed.lagging_ranges metric.
func TestChangefeedLaggingRangesMetrics(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		// Ensure a fast closed timestamp interval so ranges can catch up fast.
		kvserver.RangeFeedRefreshInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 20*time.Millisecond)
		closedts.SideTransportCloseInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 20*time.Millisecond)
		closedts.TargetDuration.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 20*time.Millisecond)

		skipMu := syncutil.Mutex{}
		skippedRanges := map[string]struct{}{}
		numRanges := 10
		numRangesToSkip := int64(4)
		var stopSkip atomic.Bool
		// `shouldSkip` continuously skips checkpoints for the first `numRangesToSkip` ranges it sees.
		// skipping is disabled by setting `stopSkip` to true.
		shouldSkip := func(event *kvpb.RangeFeedEvent) bool {
			if stopSkip.Load() {
				return false
			}
			switch event.GetValue().(type) {
			case *kvpb.RangeFeedCheckpoint:
				sp := event.Checkpoint.Span
				skipMu.Lock()
				defer skipMu.Unlock()
				if _, ok := skippedRanges[sp.String()]; ok || int64(len(skippedRanges)) < numRangesToSkip {
					skippedRanges[sp.String()] = struct{}{}
					return true
				}
			}
			return false
		}

		knobs := s.TestingKnobs.DistSQL.(*execinfra.TestingKnobs).Changefeed.(*TestingKnobs)

		knobs.FeedKnobs.RangefeedOptions = append(knobs.FeedKnobs.RangefeedOptions, kvcoord.TestingWithOnRangefeedEvent(
			func(ctx context.Context, s roachpb.Span, _ int64, event *kvpb.RangeFeedEvent) (skip bool, _ error) {
				return shouldSkip(event), nil
			}),
		)

		registry := s.Server.JobRegistry().(*jobs.Registry)
		sli1, err := registry.MetricsStruct().Changefeed.(*Metrics).getSLIMetrics("t1")
		require.NoError(t, err)
		laggingRangesTier1 := sli1.LaggingRanges
		sli2, err := registry.MetricsStruct().Changefeed.(*Metrics).getSLIMetrics("t2")
		require.NoError(t, err)
		laggingRangesTier2 := sli2.LaggingRanges

		assertLaggingRanges := func(tier string, expected int64) {
			testutils.SucceedsWithin(t, func() error {
				var laggingRangesObserved int64
				if tier == "t1" {
					laggingRangesObserved = laggingRangesTier1.Value()
				} else {
					laggingRangesObserved = laggingRangesTier2.Value()
				}
				if laggingRangesObserved != expected {
					return fmt.Errorf("expected %d lagging ranges, but found %d", expected, laggingRangesObserved)
				}
				return nil
			}, 10*time.Second)
		}

		sqlDB.Exec(t, fmt.Sprintf(`
		  CREATE TABLE foo (key INT PRIMARY KEY);
		  INSERT INTO foo (key) SELECT * FROM generate_series(1, %d);
		  ALTER TABLE foo SPLIT AT (SELECT * FROM generate_series(1, %d, 1));
  		`, numRanges, numRanges-1))
		sqlDB.CheckQueryResults(t, `SELECT count(*) FROM [SHOW RANGES FROM TABLE foo]`,
			[][]string{{fmt.Sprint(numRanges)}},
		)

		const laggingRangesOpts = `lagging_ranges_threshold="250ms", lagging_ranges_polling_interval="25ms"`
		feed1Tier1 := feed(t, f,
			fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH initial_scan='no', metrics_label="t1", %s`, laggingRangesOpts))
		feed2Tier1 := feed(t, f,
			fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH initial_scan='no', metrics_label="t1", %s`, laggingRangesOpts))
		feed3Tier2 := feed(t, f,
			fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH initial_scan='no', metrics_label="t2", %s`, laggingRangesOpts))

		assertLaggingRanges("t1", numRangesToSkip*2)
		assertLaggingRanges("t2", numRangesToSkip)

		stopSkip.Store(true)
		assertLaggingRanges("t1", 0)
		assertLaggingRanges("t2", 0)

		stopSkip.Store(false)
		assertLaggingRanges("t1", numRangesToSkip*2)
		assertLaggingRanges("t2", numRangesToSkip)

		require.NoError(t, feed1Tier1.Close())
		assertLaggingRanges("t1", numRangesToSkip)
		assertLaggingRanges("t2", numRangesToSkip)

		require.NoError(t, feed2Tier1.Close())
		assertLaggingRanges("t1", 0)
		assertLaggingRanges("t2", numRangesToSkip)

		require.NoError(t, feed3Tier2.Close())
		assertLaggingRanges("t1", 0)
		assertLaggingRanges("t2", 0)
	}
	// Can't run on tenants due to lack of SPLIT AT support (#54254)
	cdcTest(t, testFn, feedTestNoTenants, feedTestEnterpriseSinks)
}

func TestChangefeedTotalRangesMetric(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		registry := s.Server.JobRegistry().(*jobs.Registry)
		metrics := registry.MetricsStruct().Changefeed.(*Metrics)
		defaultSLI, err := metrics.getSLIMetrics(defaultSLIScope)
		require.NoError(t, err)
		totalRanges := defaultSLI.TotalRanges

		// Total ranges should start at zero.
		require.Zero(t, totalRanges.Value())

		assertTotalRanges := func(expected int64) {
			testutils.SucceedsSoon(t, func() error {
				if actual := totalRanges.Value(); expected != actual {
					return errors.Newf("expected total ranges to be %d, but got %d", expected, actual)
				}
				return nil
			})
		}

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, "CREATE TABLE foo (x int)")

		// We expect one range after creating a changefeed on a single table.
		fooFeed := feed(t, f, "CREATE CHANGEFEED FOR foo WITH lagging_ranges_polling_interval='1s'")
		assertTotalRanges(1)

		// We expect total ranges to be zero again after pausing the changefeed.
		require.NoError(t, fooFeed.(cdctest.EnterpriseTestFeed).Pause())
		assertTotalRanges(0)

		// We once again expect one range after resuming the changefeed.
		require.NoError(t, fooFeed.(cdctest.EnterpriseTestFeed).Resume())
		assertTotalRanges(1)

		// We expect two ranges after starting another changefeed on a single table.
		barFeed := feed(t, f, "CREATE CHANGEFEED FOR foo WITH lagging_ranges_polling_interval='1s'")
		assertTotalRanges(2)

		// We expect there to still be one range after cancelling one of the changefeeds.
		require.NoError(t, fooFeed.Close())
		assertTotalRanges(1)

		// We expect there to be no ranges left after cancelling the other changefeed.
		require.NoError(t, barFeed.Close())
		assertTotalRanges(0)
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks)
}

func TestChangefeedBackfillObservability(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		knobs := s.TestingKnobs.DistSQL.(*execinfra.TestingKnobs).Changefeed.(*TestingKnobs)
		registry := s.Server.JobRegistry().(*jobs.Registry)
		sli, err := registry.MetricsStruct().Changefeed.(*Metrics).getSLIMetrics(defaultSLIScope)
		require.NoError(t, err)
		pendingRanges := sli.BackfillPendingRanges

		// Create a table with multiple ranges
		numRanges := 10
		rowsPerRange := 20
		sqlDB.Exec(t, fmt.Sprintf(`
  CREATE TABLE foo (key INT PRIMARY KEY);
  INSERT INTO foo (key) SELECT * FROM generate_series(1, %d);
  ALTER TABLE foo SPLIT AT (SELECT * FROM generate_series(%d, %d, %d));
  `, numRanges*rowsPerRange, rowsPerRange, (numRanges-1)*rowsPerRange, rowsPerRange))
		sqlDB.CheckQueryResults(t, `SELECT count(*) FROM [SHOW RANGES FROM TABLE foo]`,
			[][]string{{fmt.Sprint(numRanges)}},
		)

		// Allow control of the scans
		scanCtx, scanCancel := context.WithCancel(context.Background())
		scanChan := make(chan struct{})
		knobs.FeedKnobs.BeforeScanRequest = func(b *kv.Batch) error {
			select {
			case <-scanCtx.Done():
				return scanCtx.Err()
			case <-scanChan:
				return nil
			}
		}

		require.Equal(t, pendingRanges.Value(), int64(0))
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, foo)

		// Progress the initial backfill halfway through its ranges
		for i := 0; i < numRanges/2; i++ {
			scanChan <- struct{}{}
		}
		testutils.SucceedsSoon(t, func() error {
			count := pendingRanges.Value()
			if count != int64(numRanges/2) {
				return fmt.Errorf("range count %d should be %d", count, numRanges/2)
			}
			return nil
		})

		// Ensure that the pending count is cleared if the backfill completes
		// regardless of successful scans
		scanCancel()
		testutils.SucceedsSoon(t, func() error {
			count := pendingRanges.Value()
			if count > 0 {
				return fmt.Errorf("range count %d should be 0", count)
			}
			return nil
		})
	}

	// Can't run on tenants due to lack of SPLIT AT support (#54254)
	cdcTest(t, testFn, feedTestNoTenants, feedTestEnterpriseSinks)
}

func TestChangefeedUserDefinedTypes(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		_ = maybeDisableDeclarativeSchemaChangesForTest(t, sqlDB)

		// Set up a type and table.
		sqlDB.Exec(t, `CREATE TYPE t AS ENUM ('hello', 'howdy', 'hi')`)
		sqlDB.Exec(t, `CREATE TABLE tt (x INT PRIMARY KEY, y t)`)
		sqlDB.Exec(t, `INSERT INTO tt VALUES (0, 'hello')`)

		// Open up the changefeed.
		cf := feed(t, f, `CREATE CHANGEFEED FOR tt`)
		defer closeFeed(t, cf)

		assertPayloads(t, cf, []string{
			`tt: [0]->{"after": {"x": 0, "y": "hello"}}`,
		})

		sqlDB.Exec(t, `INSERT INTO tt VALUES (1, 'howdy'), (2, 'hi')`)
		assertPayloads(t, cf, []string{
			`tt: [1]->{"after": {"x": 1, "y": "howdy"}}`,
			`tt: [2]->{"after": {"x": 2, "y": "hi"}}`,
		})

		// Alter the type and insert a new value.
		sqlDB.Exec(t, `ALTER TYPE t ADD VALUE 'hiya'`)
		sqlDB.Exec(t, `INSERT INTO tt VALUES (3, 'hiya')`)
		assertPayloads(t, cf, []string{
			`tt: [3]->{"after": {"x": 3, "y": "hiya"}}`,
		})

		// If we create a new type and add that type to tt, it should be picked
		// up by the schema feed.
		sqlDB.Exec(t, `CREATE TYPE t2 AS ENUM ('bye', 'cya')`)
		sqlDB.Exec(t, `ALTER TABLE tt ADD COLUMN z t2 DEFAULT 'bye'`)
		sqlDB.Exec(t, `INSERT INTO tt VALUES (4, 'hello', 'cya')`)

		assertPayloads(t, cf, []string{
			`tt: [0]->{"after": {"x": 0, "y": "hello", "z": "bye"}}`,
			`tt: [1]->{"after": {"x": 1, "y": "howdy", "z": "bye"}}`,
			`tt: [2]->{"after": {"x": 2, "y": "hi", "z": "bye"}}`,
			`tt: [3]->{"after": {"x": 3, "y": "hiya", "z": "bye"}}`,
			`tt: [4]->{"after": {"x": 4, "y": "hello", "z": "cya"}}`,
		})

		// If we rename a value in an existing type, it doesn't count as a change
		// but the rename is reflected in future changes.
		sqlDB.Exec(t, `ALTER TYPE t RENAME VALUE 'hi' TO 'yo'`)
		sqlDB.Exec(t, `UPDATE tt SET z='cya' where x=2`)

		assertPayloads(t, cf, []string{
			`tt: [2]->{"after": {"x": 2, "y": "yo", "z": "cya"}}`,
		})

	}

	cdcTest(t, testFn)
}

// If the schema_change_policy is 'stop' and we drop columns which are not
// targeted by the changefeed, it should not stop.
func TestNoStopAfterNonTargetColumnDrop(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE hasfams (id int primary key, a string, b string, c string, FAMILY id_a (id, a), FAMILY b_and_c (b, c))`)
		sqlDB.Exec(t, `INSERT INTO hasfams values (0, 'a', 'b', 'c')`)

		// Open up the changefeed.
		cf := feed(t, f, `CREATE CHANGEFEED FOR TABLE hasfams FAMILY b_and_c WITH schema_change_policy='stop'`,
			optOutOfMetamorphicEnrichedEnvelope{"requires families"})
		defer closeFeed(t, cf)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"b": "b", "c": "c"}}`,
		})

		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN a`)
		sqlDB.Exec(t, `INSERT INTO hasfams VALUES (1, 'b1', 'c1')`)

		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [1]->{"after": {"b": "b1", "c": "c1"}}`,
		})

		// Check that dropping a watched column still stops the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN b`)
		msg, err := cf.Next()
		require.True(t, testutils.IsError(err, `schema change occurred at`),
			`expected "schema change occurred at ..." got: msg=%s, err=%+v`, msg, err)
	}

	runWithAndWithoutRegression141453(t, testFn, func(t *testing.T, testFn cdcTestFn) {
		cdcTest(t, testFn)
	})
}

func TestChangefeedProjectionDelete(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (id int primary key, a string)`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'a')`)
		foo := feed(t, f, `CREATE CHANGEFEED WITH envelope='wrapped' AS SELECT * FROM foo`)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{`foo: [0]->{"after": {"a": "a", "id": 0}}`})
		sqlDB.Exec(t, `DELETE FROM foo WHERE id = 0`)
		assertPayloads(t, foo, []string{
			`foo: [0]->{"after": null}`,
		})
	}
	cdcTest(t, testFn, feedTestForceSink("cloudstorage"))
}

// Regression test for https://github.com/cockroachdb/cockroach/issues/106358
// Ensure that changefeeds upgraded from the version that did not set job record
// cluster ID continue functioning.
func TestChangefeedCanResumeWhenClusterIDMissing(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (id int primary key, a string)`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'a')`)
		foo := feed(t, f, `CREATE CHANGEFEED WITH envelope='wrapped' AS SELECT * FROM foo`)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{`foo: [0]->{"after": {"a": "a", "id": 0}}`})
		jobFeed := foo.(cdctest.EnterpriseTestFeed)

		// Pause the job and delete the row.
		require.NoError(t, jobFeed.Pause())
		sqlDB.Exec(t, `DELETE FROM foo WHERE id = 0`)

		// clear out creation cluster id.
		jobRegistry := s.Server.JobRegistry().(*jobs.Registry)
		require.NoError(t, func() error {
			job, err := jobRegistry.LoadJob(context.Background(), jobFeed.JobID())
			if err != nil {
				return err
			}
			return job.NoTxn().Update(context.Background(), func(txn isql.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
				md.Payload.CreationClusterID = uuid.Nil
				ju.UpdatePayload(md.Payload)
				return nil
			})
		}())

		// Resume; we expect to see deleted row.
		require.NoError(t, jobFeed.Resume())
		assertPayloads(t, foo, []string{
			`foo: [0]->{"after": null}`,
		})

		// The job payload now has clusterID set.
		job, err := jobRegistry.LoadJob(context.Background(), jobFeed.JobID())
		require.NoError(t, err)
		require.NotEqual(t, uuid.Nil, job.Payload().CreationClusterID)
	}
	cdcTest(t, testFn, feedTestEnterpriseSinks)
}

// If we drop columns which are not targeted by the changefeed, it should not backfill.
func TestNoBackfillAfterNonTargetColumnDrop(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE hasfams (id int primary key, a string, b string, c string, FAMILY id_a (id, a), FAMILY b_and_c (b, c))`)
		sqlDB.Exec(t, `INSERT INTO hasfams values (0, 'a', 'b', 'c')`)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}

		// Open up the changefeed.
		// We specify `updated` so that identical messages with different timestamps
		// aren't filtered out as duplicates. The appearance of such messages would
		// indicate that a backfill did happen even though it should not have.
		cf := feed(t, f, `CREATE CHANGEFEED FOR TABLE hasfams FAMILY b_and_c WITH updated`, args...)
		defer closeFeed(t, cf)
		assertPayloadsStripTs(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"b": "b", "c": "c"}}`,
		})

		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN a`)
		sqlDB.Exec(t, `INSERT INTO hasfams VALUES (1, 'b1', 'c1')`)
		assertPayloadsStripTs(t, cf, []string{
			`hasfams.b_and_c: [1]->{"after": {"b": "b1", "c": "c1"}}`,
		})

		// Check that dropping a watched column still backfills.
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN c`)
		assertPayloadsStripTs(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"b": "b"}}`,
			`hasfams.b_and_c: [1]->{"after": {"b": "b1"}}`,
		})
	}

	runWithAndWithoutRegression141453(t, testFn, func(t *testing.T, testFn cdcTestFn) {
		cdcTest(t, testFn)
	})
}

func TestChangefeedColumnDropsWithFamilyAndNonFamilyTargets(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE hasfams (id int primary key, a string, b string, c string, FAMILY id_a (id, a), FAMILY b_and_c (b, c))`)
		sqlDB.Exec(t, `CREATE TABLE nofams (id int primary key, a string, b string, c string)`)
		sqlDB.Exec(t, `INSERT INTO hasfams values (0, 'a', 'b', 'c')`)
		sqlDB.Exec(t, `INSERT INTO nofams values (0, 'a', 'b', 'c')`)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}
		// Open up the changefeed.
		cf := feed(t, f, `CREATE CHANGEFEED FOR TABLE hasfams FAMILY b_and_c, TABLE nofams`, args...)
		defer closeFeed(t, cf)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"b": "b", "c": "c"}}`,
			`nofams: [0]->{"after": {"a": "a", "b": "b", "c": "c", "id": 0}}`,
		})

		// Dropping an unwatched column from hasfams does not affect the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN a`)
		sqlDB.Exec(t, `INSERT INTO hasfams VALUES (1, 'b1', 'c1')`)
		sqlDB.Exec(t, `INSERT INTO nofams VALUES (1, 'a1', 'b1', 'c1')`)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [1]->{"after": {"b": "b1", "c": "c1"}}`,
			`nofams: [1]->{"after": {"a": "a1", "b": "b1", "c": "c1", "id": 1}}`,
		})

		// Check that dropping a watched column will backfill the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN b`)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"c": "c"}}`,
			`hasfams.b_and_c: [1]->{"after": {"c": "c1"}}`,
		})

		// Check that dropping a watched column will backfill the changefeed.
		sqlDB.Exec(t, `ALTER TABLE nofams DROP COLUMN b`)
		assertPayloads(t, cf, []string{
			`nofams: [0]->{"after": {"a": "a", "c": "c", "id": 0}}`,
			`nofams: [1]->{"after": {"a": "a1", "c": "c1", "id": 1}}`,
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedColumnDropsOnMultipleFamiliesWithTheSameName(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE hasfams (id int primary key, a string, b string, c string, FAMILY id_a (id, a), FAMILY b_and_c (b, c))`)
		sqlDB.Exec(t, `CREATE TABLE alsohasfams (id int primary key, a string, b string, c string, FAMILY id_a (id, a), FAMILY b_and_c (b, c))`)
		sqlDB.Exec(t, `INSERT INTO hasfams values (0, 'a', 'b', 'c')`)
		sqlDB.Exec(t, `INSERT INTO alsohasfams values (0, 'a', 'b', 'c')`)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}

		// Open up the changefeed.
		cf := feed(t, f, `CREATE CHANGEFEED FOR TABLE hasfams FAMILY b_and_c, TABLE alsohasfams FAMILY id_a`, args...)
		defer closeFeed(t, cf)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"b": "b", "c": "c"}}`,
			`alsohasfams.id_a: [0]->{"after": {"a": "a", "id": 0}}`,
		})

		// Dropping an unwatched column from hasfams does not affect the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN a`)
		sqlDB.Exec(t, `INSERT INTO hasfams VALUES (1, 'b1', 'c1')`)
		sqlDB.Exec(t, `INSERT INTO alsohasfams VALUES (1, 'a1', 'b1', 'c1')`)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [1]->{"after": {"b": "b1", "c": "c1"}}`,
			`alsohasfams.id_a: [1]->{"after": {"a": "a1", "id": 1}}`,
		})

		// Check that dropping a watched column will backfill the changefeed.
		sqlDB.Exec(t, `ALTER TABLE alsohasfams DROP COLUMN a`)
		assertPayloads(t, cf, []string{
			`alsohasfams.id_a: [0]->{"after": {"id": 0}}`,
			`alsohasfams.id_a: [1]->{"after": {"id": 1}}`,
		})

		// Check that dropping a watched column will backfill the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN b`)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"c": "c"}}`,
			`hasfams.b_and_c: [1]->{"after": {"c": "c1"}}`,
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedColumnDropsOnTheSameTableWithMultipleFamilies(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE hasfams (id int primary key, a string, b string, c string, FAMILY id_a (id, a), FAMILY b_and_c (b, c))`)
		sqlDB.Exec(t, `INSERT INTO hasfams values (0, 'a', 'b', 'c')`)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}
		// Open up the changefeed.
		cf := feed(t, f, `CREATE CHANGEFEED FOR TABLE hasfams FAMILY id_a, TABLE hasfams FAMILY b_and_c`, args...)
		defer closeFeed(t, cf)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"b": "b", "c": "c"}}`,
			`hasfams.id_a: [0]->{"after": {"a": "a", "id": 0}}`,
		})

		// Check that dropping a watched column will backfill the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN a`)
		assertPayloads(t, cf, []string{
			`hasfams.id_a: [0]->{"after": {"id": 0}}`,
		})

		// Check that dropping a watched column will backfill the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN b`)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"c": "c"}}`,
		})
	}

	runWithAndWithoutRegression141453(t, testFn, func(t *testing.T, testFn cdcTestFn) {
		cdcTest(t, testFn)
	})
}

func TestChangefeedColumnDropsOnTheSameTableWithMultipleFamiliesWithManualSchemaLocked(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE hasfams (id int primary key, a string, b string, c string, FAMILY id_a (id, a), FAMILY b_and_c (b, c))`)
		sqlDB.Exec(t, `INSERT INTO hasfams values (0, 'a', 'b', 'c')`)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}
		// Open up the changefeed.
		cf := feed(t, f, `CREATE CHANGEFEED FOR TABLE hasfams FAMILY id_a, TABLE hasfams FAMILY b_and_c`, args...)
		defer closeFeed(t, cf)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"b": "b", "c": "c"}}`,
			`hasfams.id_a: [0]->{"after": {"a": "a", "id": 0}}`,
		})

		// Check that dropping a watched column will backfill the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams SET (schema_locked=false)`)
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN a`)
		sqlDB.Exec(t, `ALTER TABLE hasfams SET (schema_locked=true)`)
		assertPayloads(t, cf, []string{
			`hasfams.id_a: [0]->{"after": {"id": 0}}`,
		})

		// Check that dropping a watched column will backfill the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams SET (schema_locked=false)`)
		sqlDB.Exec(t, `ALTER TABLE hasfams DROP COLUMN b`)
		sqlDB.Exec(t, `ALTER TABLE hasfams SET (schema_locked=true)`)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"c": "c"}}`,
		})
	}

	runWithAndWithoutRegression141453(t, testFn, func(t *testing.T, testFn cdcTestFn) {
		cdcTest(t, testFn)
	})
}

func TestNoStopAfterNonTargetAddColumnWithBackfill(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE hasfams (id INT PRIMARY KEY, a STRING, b STRING, c STRING, FAMILY id_a (id, a), FAMILY b_and_c (b, c))`)
		sqlDB.Exec(t, `INSERT INTO hasfams values (0, 'a', 'b', 'c')`)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}

		// Open up the changefeed.
		cf := feed(t, f, `CREATE CHANGEFEED FOR TABLE hasfams FAMILY b_and_c WITH schema_change_policy='stop'`, args...)

		defer closeFeed(t, cf)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [0]->{"after": {"b": "b", "c": "c"}}`,
		})

		// Adding a column with a backfill to the default column family does not stop the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams ADD COLUMN d STRING DEFAULT 'default'`)
		sqlDB.Exec(t, `INSERT INTO hasfams VALUES (1, 'a1', 'b1', 'c1', 'd1')`)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [1]->{"after": {"b": "b1", "c": "c1"}}`,
		})

		// Adding a column with a backfill to a non-target column family does not stop the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams ADD COLUMN e STRING DEFAULT 'default' FAMILY id_a`)
		sqlDB.Exec(t, `INSERT INTO hasfams VALUES (2, 'a2', 'b2', 'c2', 'd2', 'e2')`)
		assertPayloads(t, cf, []string{
			`hasfams.b_and_c: [2]->{"after": {"b": "b2", "c": "c2"}}`,
		})

		// Check that adding a column to a watched family stops the changefeed.
		sqlDB.Exec(t, `ALTER TABLE hasfams ADD COLUMN f INT DEFAULT 0 FAMILY b_and_c`)
		if _, err := cf.Next(); !testutils.IsError(err, `schema change occurred at`) {
			t.Errorf(`expected "schema change occurred at ..." got: %+v`, err.Error())
		}
	}

	runWithAndWithoutRegression141453(t, testFn, func(t *testing.T, testFn cdcTestFn) {
		cdcTest(t, testFn)
	})
}

func TestChangefeedExternalIODisabled(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	t.Run("sinkful changefeeds not allowed with disabled external io", func(t *testing.T) {
		disallowedSinkProtos := []string{
			changefeedbase.SinkSchemeExperimentalSQL,
			changefeedbase.SinkSchemeKafka,
			changefeedbase.SinkSchemeNull, // Doesn't work because all sinkful changefeeds are disallowed
			// Cloud sink schemes
			"experimental-s3",
			"experimental-gs",
			"experimental-nodelocal",
			"experimental-http",
			"experimental-https",
			"experimental-azure",
		}
		ctx := context.Background()
		s, db, _ := serverutils.StartServer(t, base.TestServerArgs{
			DefaultTestTenant: base.TODOTestTenantDisabled,
			ExternalIODirConfig: base.ExternalIODirConfig{
				DisableOutbound: true,
			},
		})
		defer s.Stopper().Stop(ctx)
		sqlDB := sqlutils.MakeSQLRunner(db)
		sqlDB.ExecMultiple(t, strings.Split(serverSetupStatements, ";")...)
		sqlDB.Exec(t, "CREATE TABLE target_table (pk INT PRIMARY KEY)")
		for _, proto := range disallowedSinkProtos {
			sqlDB.ExpectErrWithTimeout(t, "Outbound IO is disabled by configuration, cannot create changefeed",
				"CREATE CHANGEFEED FOR target_table INTO $1",
				fmt.Sprintf("%s://does-not-matter", proto),
			)
		}
	})

	withDisabledOutbound := func(args *base.TestServerArgs) { args.ExternalIODirConfig.DisableOutbound = true }
	cdcTestNamed(t, "sinkless changfeeds are allowed with disabled external io", func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, "CREATE TABLE target_table (pk INT PRIMARY KEY)")
		sqlDB.Exec(t, "INSERT INTO target_table VALUES (1)")
		feed := feed(t, f, "CREATE CHANGEFEED FOR target_table")
		defer closeFeed(t, feed)
		assertPayloads(t, feed, []string{
			`target_table: [1]->{"after": {"pk": 1}}`,
		})
	}, feedTestForceSink("sinkless"), withArgsFn(withDisabledOutbound))
}

// Test how Changefeeds react to schema changes that do not require a backfill
// operation.
func TestChangefeedSchemaChangeNoBackfill(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	skip.UnderRace(t, "takes >1 min under race")

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		_ = maybeDisableDeclarativeSchemaChangesForTest(t, sqlDB)

		// Schema changes that predate the changefeed.
		t.Run(`historical`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE historical (a INT PRIMARY KEY, b STRING DEFAULT 'before')`)
			var start string
			sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp()`).Scan(&start)
			sqlDB.Exec(t, `INSERT INTO historical (a, b) VALUES (0, '0')`)
			sqlDB.Exec(t, `INSERT INTO historical (a) VALUES (1)`)
			sqlDB.Exec(t, `ALTER TABLE historical ALTER COLUMN b SET DEFAULT 'after'`)
			sqlDB.Exec(t, `INSERT INTO historical (a) VALUES (2)`)
			sqlDB.Exec(t, `ALTER TABLE historical ADD COLUMN c INT`)
			sqlDB.Exec(t, `INSERT INTO historical (a) VALUES (3)`)
			sqlDB.Exec(t, `INSERT INTO historical (a, c) VALUES (4, 14)`)
			historical := feed(t, f, `CREATE CHANGEFEED FOR historical WITH cursor=$1`, start)
			defer closeFeed(t, historical)
			assertPayloads(t, historical, []string{
				`historical: [0]->{"after": {"a": 0, "b": "0"}}`,
				`historical: [1]->{"after": {"a": 1, "b": "before"}}`,
				`historical: [2]->{"after": {"a": 2, "b": "after"}}`,
				`historical: [3]->{"after": {"a": 3, "b": "after", "c": null}}`,
				`historical: [4]->{"after": {"a": 4, "b": "after", "c": 14}}`,
			})
		})

		t.Run(`add column`, func(t *testing.T) {
			// NB: the default is a nullable column
			sqlDB.Exec(t, `CREATE TABLE add_column (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `INSERT INTO add_column VALUES (1)`)
			addColumn := feed(t, f, `CREATE CHANGEFEED FOR add_column`)
			defer closeFeed(t, addColumn)
			assertPayloads(t, addColumn, []string{
				`add_column: [1]->{"after": {"a": 1}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_column ADD COLUMN b STRING`)
			sqlDB.Exec(t, `INSERT INTO add_column VALUES (2, '2')`)
			assertPayloads(t, addColumn, []string{
				`add_column: [2]->{"after": {"a": 2, "b": "2"}}`,
			})
		})

		t.Run(`rename column`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE rename_column (a INT PRIMARY KEY, b STRING)`)
			sqlDB.Exec(t, `INSERT INTO rename_column VALUES (1, '1')`)
			renameColumn := feed(t, f, `CREATE CHANGEFEED FOR rename_column`)
			defer closeFeed(t, renameColumn)
			assertPayloads(t, renameColumn, []string{
				`rename_column: [1]->{"after": {"a": 1, "b": "1"}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE rename_column RENAME COLUMN b TO c`)
			sqlDB.Exec(t, `INSERT INTO rename_column VALUES (2, '2')`)
			assertPayloads(t, renameColumn, []string{
				`rename_column: [2]->{"after": {"a": 2, "c": "2"}}`,
			})
		})

		t.Run(`add default`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_default (a INT PRIMARY KEY, b STRING)`)
			sqlDB.Exec(t, `INSERT INTO add_default (a, b) VALUES (1, '1')`)
			addDefault := feed(t, f, `CREATE CHANGEFEED FOR add_default`)
			defer closeFeed(t, addDefault)
			sqlDB.Exec(t, `ALTER TABLE add_default ALTER COLUMN b SET DEFAULT 'd'`)
			sqlDB.Exec(t, `INSERT INTO add_default (a) VALUES (2)`)
			assertPayloads(t, addDefault, []string{
				`add_default: [1]->{"after": {"a": 1, "b": "1"}}`,
				`add_default: [2]->{"after": {"a": 2, "b": "d"}}`,
			})
		})

		t.Run(`drop default`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE drop_default (a INT PRIMARY KEY, b STRING DEFAULT 'd')`)
			sqlDB.Exec(t, `INSERT INTO drop_default (a) VALUES (1)`)
			dropDefault := feed(t, f, `CREATE CHANGEFEED FOR drop_default`)
			defer closeFeed(t, dropDefault)
			sqlDB.Exec(t, `ALTER TABLE drop_default ALTER COLUMN b DROP DEFAULT`)
			sqlDB.Exec(t, `INSERT INTO drop_default (a) VALUES (2)`)
			assertPayloads(t, dropDefault, []string{
				`drop_default: [1]->{"after": {"a": 1, "b": "d"}}`,
				`drop_default: [2]->{"after": {"a": 2, "b": null}}`,
			})
		})

		t.Run(`drop not null`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE drop_notnull (a INT PRIMARY KEY, b STRING NOT NULL)`)
			sqlDB.Exec(t, `INSERT INTO drop_notnull VALUES (1, '1')`)
			dropNotNull := feed(t, f, `CREATE CHANGEFEED FOR drop_notnull`)
			defer closeFeed(t, dropNotNull)
			sqlDB.Exec(t, `ALTER TABLE drop_notnull ALTER b DROP NOT NULL`)
			sqlDB.Exec(t, `INSERT INTO drop_notnull VALUES (2, NULL)`)
			assertPayloads(t, dropNotNull, []string{
				`drop_notnull: [1]->{"after": {"a": 1, "b": "1"}}`,
				`drop_notnull: [2]->{"after": {"a": 2, "b": null}}`,
			})
		})

		t.Run(`checks`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE checks (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `INSERT INTO checks VALUES (1)`)
			checks := feed(t, f, `CREATE CHANGEFEED FOR checks`)
			defer closeFeed(t, checks)
			sqlDB.Exec(t, `ALTER TABLE checks ADD CONSTRAINT c CHECK (a < 5) NOT VALID`)
			sqlDB.Exec(t, `INSERT INTO checks VALUES (2)`)
			sqlDB.Exec(t, `ALTER TABLE checks VALIDATE CONSTRAINT c`)
			sqlDB.Exec(t, `INSERT INTO checks VALUES (3)`)
			sqlDB.Exec(t, `ALTER TABLE checks DROP CONSTRAINT c`)
			sqlDB.Exec(t, `INSERT INTO checks VALUES (6)`)
			assertPayloads(t, checks, []string{
				`checks: [1]->{"after": {"a": 1}}`,
				`checks: [2]->{"after": {"a": 2}}`,
				`checks: [3]->{"after": {"a": 3}}`,
				`checks: [6]->{"after": {"a": 6}}`,
			})
		})

		t.Run(`add index`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_index (a INT PRIMARY KEY, b STRING)`)
			sqlDB.Exec(t, `INSERT INTO add_index VALUES (1, '1')`)
			addIndex := feed(t, f, `CREATE CHANGEFEED FOR add_index`)
			defer closeFeed(t, addIndex)
			sqlDB.Exec(t, `CREATE INDEX b_idx ON add_index (b)`)
			sqlDB.Exec(t, `SELECT * FROM add_index@b_idx`)
			sqlDB.Exec(t, `INSERT INTO add_index VALUES (2, '2')`)
			assertPayloads(t, addIndex, []string{
				`add_index: [1]->{"after": {"a": 1, "b": "1"}}`,
				`add_index: [2]->{"after": {"a": 2, "b": "2"}}`,
			})
		})

		t.Run(`unique`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE "unique" (a INT PRIMARY KEY, b STRING)`)
			sqlDB.Exec(t, `INSERT INTO "unique" VALUES (1, '1')`)
			unique := feed(t, f, `CREATE CHANGEFEED FOR "unique"`)
			defer closeFeed(t, unique)
			sqlDB.Exec(t, `ALTER TABLE "unique" ADD CONSTRAINT u UNIQUE (b)`)
			sqlDB.Exec(t, `INSERT INTO "unique" VALUES (2, '2')`)
			assertPayloads(t, unique, []string{
				`unique: [1]->{"after": {"a": 1, "b": "1"}}`,
				`unique: [2]->{"after": {"a": 2, "b": "2"}}`,
			})
		})

		t.Run(`alter default`, func(t *testing.T) {
			sqlDB.Exec(
				t, `CREATE TABLE alter_default (a INT PRIMARY KEY, b STRING DEFAULT 'before')`)
			sqlDB.Exec(t, `INSERT INTO alter_default (a) VALUES (1)`)
			alterDefault := feed(t, f, `CREATE CHANGEFEED FOR alter_default`)
			defer closeFeed(t, alterDefault)
			sqlDB.Exec(t, `ALTER TABLE alter_default ALTER COLUMN b SET DEFAULT 'after'`)
			sqlDB.Exec(t, `INSERT INTO alter_default (a) VALUES (2)`)
			assertPayloads(t, alterDefault, []string{
				`alter_default: [1]->{"after": {"a": 1, "b": "before"}}`,
				`alter_default: [2]->{"after": {"a": 2, "b": "after"}}`,
			})
		})

		// Test adding a column with explicitly setting the default value to be NULL
		t.Run(`add column with DEFAULT NULL`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE t (id INT PRIMARY KEY)`)
			sqlDB.Exec(t, `INSERT INTO t VALUES (1)`)
			defaultNull := feed(t, f, `CREATE CHANGEFEED FOR t`)
			defer closeFeed(t, defaultNull)
			sqlDB.Exec(t, `ALTER TABLE t ADD COLUMN c INT DEFAULT NULL`)
			sqlDB.Exec(t, `INSERT INTO t VALUES (2, 2)`)
			assertPayloads(t, defaultNull, []string{
				// Verify that no column backfill occurs
				`t: [1]->{"after": {"id": 1}}`,
				`t: [2]->{"after": {"c": 2, "id": 2}}`,
			})
		})
	}

	cdcTest(t, testFn)

	log.FlushFiles()
	entries, err := log.FetchEntriesFromFiles(0, math.MaxInt64, 1, regexp.MustCompile("cdc ux violation"),
		log.WithFlattenedSensitiveData)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatalf("Found violation of CDC's guarantees: %v", entries)
	}
}

// TestChangefeedLaggingSpanCheckpointing tests checkpointing when the highwater
// does not advance due to specific spans lagging behind.
func TestChangefeedLaggingSpanCheckpointing(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, db, stopServer := startTestFullServer(t, makeOptions(t, feedTestNoTenants))
	defer stopServer()
	sqlDB := sqlutils.MakeSQLRunner(db)

	knobs := s.TestingKnobs().
		DistSQL.(*execinfra.TestingKnobs).
		Changefeed.(*TestingKnobs)

	// Initialize table with 20 ranges.
	sqlDB.Exec(t, `
  CREATE TABLE foo (key INT PRIMARY KEY);
  INSERT INTO foo (key) SELECT * FROM generate_series(1, 1000);
  ALTER TABLE foo SPLIT AT (SELECT * FROM generate_series(1, 1000, 50));
  `)

	fooDesc := desctestutils.TestingGetPublicTableDescriptor(s.DB(), s.Codec(), "d", "foo")
	tableSpan := fooDesc.PrimaryIndexSpan(s.Codec())

	// Checkpoint progress frequently, allow a large enough checkpoint, and
	// reduce the lag threshold to allow lag checkpointing to trigger
	changefeedbase.SpanCheckpointInterval.Override(
		context.Background(), &s.ClusterSettings().SV, 10*time.Millisecond)
	changefeedbase.SpanCheckpointMaxBytes.Override(
		context.Background(), &s.ClusterSettings().SV, 100<<20 /* 100 MiB */)
	changefeedbase.SpanCheckpointLagThreshold.Override(
		context.Background(), &s.ClusterSettings().SV, 10*time.Millisecond)

	// We'll start the changefeed with the cursor set to the current time (not insert time).
	// NB: The changefeed created in this test doesn't actually send any message events.
	var tsStr string
	sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp() from foo`).Scan(&tsStr)
	cursor := parseTimeToHLC(t, tsStr)
	t.Logf("cursor: %v", cursor)

	// Rangefeed will skip some of the checkpoints to simulate lagging spans.
	var laggingSpans roachpb.SpanGroup
	nonLaggingSpans := make(map[string]int)
	var numSpans int
	knobs.FeedKnobs.ShouldSkipCheckpoint = func(checkpoint *kvpb.RangeFeedCheckpoint) (skip bool) {
		// Skip spans for the whole table.
		if checkpoint.Span.Equal(tableSpan) {
			return true
		}
		// Skip spans that we already picked to be lagging.
		if laggingSpans.Encloses(checkpoint.Span) {
			return true
		}
		// Skip additional updates for every 3rd non-lagging span so that we have
		// a few spans lagging at a second timestamp above the cursor.
		if i, ok := nonLaggingSpans[checkpoint.Span.String()]; ok {
			return i%3 == 0
		}
		numSpans++
		// Skip updates for every 3rd span so that we have a few spans lagging
		// at the cursor.
		if numSpans%3 == 0 {
			laggingSpans.Add(checkpoint.Span)
			return true
		}
		nonLaggingSpans[checkpoint.Span.String()] = len(nonLaggingSpans) + 1
		return false
	}

	var jobID jobspb.JobID
	sqlDB.QueryRow(t,
		`CREATE CHANGEFEED FOR foo INTO 'null://' WITH resolved='50ms', no_initial_scan, cursor=$1`, tsStr,
	).Scan(&jobID)

	// Helper to read job progress
	jobRegistry := s.JobRegistry().(*jobs.Registry)
	loadProgress := func() jobspb.Progress {
		job, err := jobRegistry.LoadJob(context.Background(), jobID)
		require.NoError(t, err)
		return job.Progress()
	}

	// We should eventually checkpoint some spans that are ahead of the highwater.
	// We'll wait until we have two unique timestamps.
	testutils.SucceedsSoon(t, func() error {
		progress := loadProgress()
		cp := maps.Collect(loadCheckpoint(t, progress).All())
		if len(cp) >= 2 {
			return nil
		}
		return errors.New("waiting for checkpoint with two different timestamps")
	})

	sqlDB.Exec(t, "PAUSE JOB $1", jobID)
	waitForJobState(sqlDB, t, jobID, jobs.StatePaused)

	// We expect highwater to be 0 (because we skipped some spans) or exactly cursor
	// (this is mostly due to racy updates sent from aggregators to the frontier).
	// However, the checkpoint timestamp should be at least at the cursor.
	progress := loadProgress()
	require.True(t, progress.GetHighWater().IsEmpty() || progress.GetHighWater().Equal(cursor),
		"expected empty highwater or %s, found %s", cursor, progress.GetHighWater())
	spanLevelCheckpoint := loadCheckpoint(t, progress)
	require.NotNil(t, spanLevelCheckpoint)
	require.True(t, cursor.LessEq(spanLevelCheckpoint.MinTimestamp()))

	// Construct a reverse index from spans to timestamps.
	spanTimestamps := make(map[string]hlc.Timestamp)
	for ts, spans := range spanLevelCheckpoint.All() {
		for _, s := range spans {
			spanTimestamps[s.String()] = ts
		}
	}

	var rangefeedStartedOnce bool
	var incorrectCheckpointErr error
	knobs.FeedKnobs.OnRangeFeedStart = func(spans []kvcoord.SpanTimePair) {
		// We only need to check the first rangefeed restart since
		// any additional restarts (likely due to transient errors)
		// may be using newer span-level checkpoints than the one
		// we saved after the last pause.
		if rangefeedStartedOnce {
			return
		}

		setErr := func(stp kvcoord.SpanTimePair, expectedTS hlc.Timestamp) {
			incorrectCheckpointErr = errors.Newf(
				"rangefeed for span %s expected to start @%s, started @%s instead",
				stp.Span, expectedTS, stp.StartAfter)
		}

		// Verify that the start time for each span is correct.
		for _, sp := range spans {
			if checkpointTS := spanTimestamps[sp.Span.String()]; checkpointTS.IsSet() {
				// Any span in the checkpoint should be resumed at its checkpoint timestamp.
				if !sp.StartAfter.Equal(checkpointTS) {
					setErr(sp, checkpointTS)
				}
			} else {
				// Any spans not in the checkpoint should be at the cursor.
				if !sp.StartAfter.Equal(cursor) {
					setErr(sp, cursor)
				}
			}
		}

		rangefeedStartedOnce = true
	}
	knobs.FeedKnobs.ShouldSkipCheckpoint = nil

	sqlDB.Exec(t, "RESUME JOB $1", jobID)
	waitForJobState(sqlDB, t, jobID, jobs.StateRunning)

	// Wait until highwater advances past cursor.
	testutils.SucceedsSoon(t, func() error {
		progress := loadProgress()
		if hw := progress.GetHighWater(); hw != nil && cursor.Less(*hw) {
			return nil
		}
		return errors.New("waiting for checkpoint advance")
	})

	sqlDB.Exec(t, "PAUSE JOB $1", jobID)
	waitForJobState(sqlDB, t, jobID, jobs.StatePaused)
	// Verify the rangefeed started. This guards against the testing knob
	// not being called, which was happening in earlier versions of the code.
	require.True(t, rangefeedStartedOnce)
	// Verify we didn't see incorrect timestamps when resuming.
	require.NoError(t, incorrectCheckpointErr)
}

// Test checkpointing during schema change backfills that can be paused and
// resumed multiple times during execution
func TestChangefeedSchemaChangeBackfillCheckpoint(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	rnd, seed := randutil.NewPseudoRand()
	t.Logf("random seed: %d", seed)

	// This test asserts that a second checkpoint made after resumption does its
	// best to not lose information from the first checkpoint, therefore the
	// maxCheckpointSize should be large enough to hold both without any
	// truncation
	var maxCheckpointSize int64 = 100 << 20

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		usingLegacySchemaChanger := maybeDisableDeclarativeSchemaChangesForTest(t, sqlDB)
		// NB: For the `ALTER TABLE foo ADD COLUMN ... DEFAULT` schema change,
		// the expected boundary is different depending on if we are using the
		// legacy schema changer or not.
		expectedBoundaryType := jobspb.ResolvedSpan_RESTART
		if usingLegacySchemaChanger {
			expectedBoundaryType = jobspb.ResolvedSpan_BACKFILL
		}

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		// Initialize table
		sqlDB.Exec(t, `CREATE TABLE foo(key INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo (key) SELECT * FROM generate_series(1, 1000)`)

		// Ensure Scan Requests are always small enough that we receive multiple
		// resolved events during a backfill
		knobs.FeedKnobs.BeforeScanRequest = func(b *kv.Batch) error {
			b.Header.MaxSpanRequestKeys = 50
			return nil
		}

		// Setup changefeed job details, avoid relying on initial scan functionality
		baseFeed := feed(t, f, `CREATE CHANGEFEED FOR foo WITH resolved='100ms', min_checkpoint_frequency='100ms', no_initial_scan`)
		jobFeed := baseFeed.(cdctest.EnterpriseTestFeed)
		jobRegistry := s.Server.JobRegistry().(*jobs.Registry)

		// Ensure events are consumed for sinks that don't buffer (ex: Kafka)
		g := ctxgroup.WithContext(context.Background())
		g.Go(func() error {
			for {
				_, err := baseFeed.Next()
				if err != nil {
					return err
				}
			}
		})
		defer func() {
			closeFeed(t, baseFeed)
			_ = g.Wait()
		}()

		// Helper to read job progress
		loadProgress := func() jobspb.Progress {
			jobID := jobFeed.JobID()
			job, err := jobRegistry.LoadJob(context.Background(), jobID)
			require.NoError(t, err)
			return job.Progress()
		}

		// Ensure the changefeed has begun normal execution
		testutils.SucceedsSoon(t, func() error {
			prog := loadProgress()
			if p := prog.GetHighWater(); p != nil && !p.IsEmpty() {
				t.Logf("highwater: %s", p)
				return nil
			}
			return errors.New("waiting for highwater")
		})

		// Pause job and setup overrides to force a checkpoint
		require.NoError(t, jobFeed.Pause())

		// Checkpoint progress frequently, and set the checkpoint size limit.
		changefeedbase.SpanCheckpointInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 1)
		changefeedbase.SpanCheckpointMaxBytes.Override(
			context.Background(), &s.Server.ClusterSettings().SV, maxCheckpointSize)

		var tableSpan roachpb.Span
		refreshTableSpan := func() {
			fooDesc := desctestutils.TestingGetPublicTableDescriptor(
				s.SystemServer.DB(), s.Codec, "d", "foo")
			tableSpan = fooDesc.PrimaryIndexSpan(s.Codec)
		}

		// FilterSpanWithMutation should ensure that once the backfill begins, the following resolved events
		// that are for that backfill (are of the timestamp right after the backfill timestamp) resolve some
		// but not all of the time, which results in a checkpoint eventually being created
		numGaps := 0
		var backfillTimestamp hlc.Timestamp
		var initialCheckpoint roachpb.SpanGroup
		var foundCheckpoint int32
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			// Stop resolving anything after checkpoint set to avoid eventually resolving the full span
			if initialCheckpoint.Len() > 0 {
				return true, nil
			}

			// A backfill begins when the associated resolved event arrives, which has a
			// timestamp such that all backfill spans have a timestamp of
			// timestamp.Next().
			if r.BoundaryType == expectedBoundaryType {
				// NB: We wait until the schema change is public before looking
				// up the table span. When using the declarative schema changer,
				// the table span will be different before and after the schema
				// change due to a primary index swap.
				refreshTableSpan()
				backfillTimestamp = r.Timestamp
				t.Logf("backfill timestamp: %s", backfillTimestamp)
				return false, nil
			}

			// Check if we've set a checkpoint yet
			progress := loadProgress()
			if spanLevelCheckpoint := loadCheckpoint(t, progress); spanLevelCheckpoint != nil {
				minCheckpointTS := spanLevelCheckpoint.MinTimestamp()
				// Checkpoint timestamp should be the timestamp of the spans from the backfill
				if !minCheckpointTS.Equal(backfillTimestamp.Next()) {
					return false, changefeedbase.WithTerminalError(
						errors.AssertionFailedf("expected checkpoint timestamp %s, found %s", backfillTimestamp, minCheckpointTS))
				}
				initialCheckpoint = makeSpanGroupFromCheckpoint(t, spanLevelCheckpoint)
				atomic.StoreInt32(&foundCheckpoint, 1)
			}

			// Filter non-backfill-related spans
			if !r.Timestamp.Equal(backfillTimestamp.Next()) {
				// Only allow spans prior to a valid backfillTimestamp to avoid moving past the backfill
				return backfillTimestamp.IsSet() && !r.Timestamp.LessEq(backfillTimestamp.Next()), nil
			}

			// At the end of a backfill, kv feed will emit a resolved span for the whole table.
			// Filter this out because we would like to leave gaps.
			if r.Span.Equal(tableSpan) {
				return true, nil
			}

			// Ensure that we have at least 2 gaps, so when a second checkpoint happens later in this test,
			// the second checkpoint can still leave at least one gap.
			if numGaps >= 2 {
				return rnd.Intn(10) > 7, nil
			}
			numGaps += 1
			return true, nil
		}

		require.NoError(t, jobFeed.Resume())
		sqlDB.Exec(t, `ALTER TABLE foo ADD COLUMN b STRING DEFAULT 'd'`)

		// Wait for a checkpoint to have been set
		testutils.SucceedsSoon(t, func() error {
			if atomic.LoadInt32(&foundCheckpoint) != 0 {
				return nil
			}
			return errors.New("waiting for checkpoint")
		})
		t.Logf("initial checkpoint: %s", initialCheckpoint.Slice())

		require.NoError(t, jobFeed.Pause())

		// All spans up to the backfill event should've been resolved, therefore the
		// highwater mark should be that of the backfill event
		progress := loadProgress()
		h := progress.GetHighWater()
		require.True(t, h.Equal(backfillTimestamp))

		// We ensure that if the job is resumed, it builds off of the existing
		// checkpoint, not resolving any already-checkpointed-spans while also
		// setting a new checkpoint that contains both initially checkpointed spans
		// as well as the newly resolved ones
		var secondCheckpoint roachpb.SpanGroup
		foundCheckpoint = 0
		numGaps = 0
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			// Stop resolving anything after second checkpoint set to avoid backfill completion
			if secondCheckpoint.Len() > 0 {
				return true, nil
			}

			// Once we've set a checkpoint that covers new spans, record it
			progress := loadProgress()
			if spanLevelCheckpoint := loadCheckpoint(t, progress); spanLevelCheckpoint != nil {
				currentCheckpoint := makeSpanGroupFromCheckpoint(t, spanLevelCheckpoint)
				// Ensure that the second checkpoint both contains all spans in the first checkpoint as well as new spans
				if currentCheckpoint.Encloses(initialCheckpoint.Slice()...) && !initialCheckpoint.Encloses(currentCheckpoint.Slice()...) {
					secondCheckpoint = currentCheckpoint
					atomic.StoreInt32(&foundCheckpoint, 1)
				}
			}

			// Filter non-backfill-related spans
			if !r.Timestamp.Equal(backfillTimestamp.Next()) {
				// Only allow spans prior to a valid backfillTimestamp to avoid moving past the backfill
				return backfillTimestamp.IsSet() && !r.Timestamp.LessEq(backfillTimestamp.Next()), nil
			}

			require.Falsef(t, initialCheckpoint.Encloses(r.Span), "second backfill should not resolve checkpointed span")

			// At the end of a backfill, kv feed will emit a resolved span for the whole table.
			// Filter this out because we would like to leave at least one gap.
			if r.Span.Equal(tableSpan) {
				return true, nil
			}

			// Ensure there is at least one gap so that we can receive resolved spans later.
			if numGaps >= 1 {
				return rnd.Intn(10) > 7, nil
			}
			numGaps += 1
			return true, nil
		}

		require.NoError(t, jobFeed.Resume())
		testutils.SucceedsSoon(t, func() error {
			if atomic.LoadInt32(&foundCheckpoint) != 0 {
				return nil
			}
			return errors.New("waiting for second checkpoint")
		})
		t.Logf("second checkpoint: %s", secondCheckpoint.Slice())

		require.NoError(t, jobFeed.Pause())
		for _, span := range initialCheckpoint.Slice() {
			require.Truef(t, secondCheckpoint.Contains(span.Key), "second checkpoint should contain all values in first checkpoint")
		}

		// Collect spans we attempt to resolve after when we resume.
		var resolved []roachpb.Span
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			resolved = append(resolved, r.Span)
			return false, nil
		}

		// Resume job.
		require.NoError(t, jobFeed.Resume())

		// checkpoint should eventually be gone once backfill completes.
		testutils.SucceedsSoon(t, func() error {
			progress := loadProgress()
			if loadCheckpoint(t, progress) != nil {
				return errors.New("checkpoint still non-empty")
			}
			return nil
		})

		// Pause job to avoid race on the resolved array
		require.NoError(t, jobFeed.Pause())

		// Verify that none of the resolved spans after resume were checkpointed.
		t.Logf("Table Span: %s, Second Checkpoint: %v, Resolved Spans: %v", tableSpan, secondCheckpoint, resolved)
		for _, sp := range resolved {
			require.Falsef(t, !sp.Equal(tableSpan) && secondCheckpoint.Contains(sp.Key), "span should not have been resolved: %s", sp)
		}
	}

	cdcTestWithSystem(t, testFn, feedTestEnterpriseSinks)

	log.FlushFiles()
	entries, err := log.FetchEntriesFromFiles(0, math.MaxInt64, 1,
		regexp.MustCompile("cdc ux violation"), log.WithFlattenedSensitiveData)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatalf("Found violation of CDC's guarantees: %v", entries)
	}
}

// Test schema changes that require a backfill when the backfill option is
// allowed when using the legacy schema changer.
//
// TODO: remove this test when the legacy schema changer is  deprecated.
func TestChangefeedSchemaChangeAllowBackfill_Legacy(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		t.Log("using legacy schema changer")
		sqlDB.Exec(t, "SET create_table_with_schema_locked=false")
		sqlDB.Exec(t, "SET use_declarative_schema_changer='off'")
		sqlDB.Exec(t, "SET CLUSTER SETTING  sql.defaults.use_declarative_schema_changer='off'")
		sqlDB.Exec(t, "SET CLUSTER SETTING  sql.defaults.create_table_with_schema_locked='false'")

		// Expected semantics:
		//
		// 1) DROP COLUMN
		// If the table descriptor is at version 1 when the `ALTER TABLE` stmt is issued,
		// we expect the changefeed level backfill to be triggered at the `ModificationTime` of
		// version 2 of the said descriptor. This is because this is the descriptor
		// version at which the dropped column stops being visible to SELECTs. Note that
		// this means we will see row updates resulting from the schema-change level
		// backfill _after_ the changefeed level backfill.
		//
		// 2) ADD COLUMN WITH DEFAULT & ADD COLUMN AS ... STORED
		// If the table descriptor is at version 1 when the `ALTER TABLE` stmt is issued,
		// we expect the changefeed level backfill to be triggered at the
		// `ModificationTime` of version 4 of said descriptor. This is because this is the
		// descriptor version which makes the schema-change level backfill for the
		// newly-added column public. This means we wil see row updates resulting from the
		// schema-change level backfill _before_ the changefeed level backfill.

		t.Run(`add column with default`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_column_def (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `INSERT INTO add_column_def VALUES (1)`)
			sqlDB.Exec(t, `INSERT INTO add_column_def VALUES (2)`)
			addColumnDef := feed(t, f, `CREATE CHANGEFEED FOR add_column_def WITH updated`)
			defer closeFeed(t, addColumnDef)
			assertPayloadsStripTs(t, addColumnDef, []string{
				`add_column_def: [1]->{"after": {"a": 1}}`,
				`add_column_def: [2]->{"after": {"a": 2}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_column_def ADD COLUMN b STRING DEFAULT 'd'`)
			ts := schematestutils.FetchDescVersionModificationTime(t, s.TestServer.Server,
				`d`, `public`, `add_column_def`, 4)

			// Schema change backfill
			assertPayloadsStripTs(t, addColumnDef, []string{
				`add_column_def: [1]->{"after": {"a": 1}}`,
				`add_column_def: [2]->{"after": {"a": 2}}`,
			})
			// Changefeed level backfill
			assertPayloads(t, addColumnDef, []string{
				fmt.Sprintf(`add_column_def: [1]->{"after": {"a": 1, "b": "d"}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
				fmt.Sprintf(`add_column_def: [2]->{"after": {"a": 2, "b": "d"}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
			})
		})

		t.Run(`add column computed`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_col_comp (a INT PRIMARY KEY, b INT AS (a + 5) STORED)`)
			sqlDB.Exec(t, `INSERT INTO add_col_comp VALUES (1)`)
			sqlDB.Exec(t, `INSERT INTO add_col_comp (a) VALUES (2)`)
			addColComp := feed(t, f, `CREATE CHANGEFEED FOR add_col_comp WITH updated`)
			defer closeFeed(t, addColComp)
			assertPayloadsStripTs(t, addColComp, []string{
				`add_col_comp: [1]->{"after": {"a": 1, "b": 6}}`,
				`add_col_comp: [2]->{"after": {"a": 2, "b": 7}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_col_comp ADD COLUMN c INT AS (a + 10) STORED`)
			assertPayloadsStripTs(t, addColComp, []string{
				`add_col_comp: [1]->{"after": {"a": 1, "b": 6}}`,
				`add_col_comp: [2]->{"after": {"a": 2, "b": 7}}`,
			})
			ts := schematestutils.FetchDescVersionModificationTime(t, s.TestServer.Server,
				`d`, `public`, `add_col_comp`, 4)

			assertPayloads(t, addColComp, []string{
				fmt.Sprintf(`add_col_comp: [1]->{"after": {"a": 1, "b": 6, "c": 11}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
				fmt.Sprintf(`add_col_comp: [2]->{"after": {"a": 2, "b": 7, "c": 12}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
			})
		})

		t.Run(`drop column`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE drop_column (a INT PRIMARY KEY, b STRING)`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (1, '1')`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (2, '2')`)
			dropColumn := feed(t, f, `CREATE CHANGEFEED FOR drop_column WITH updated`)
			defer closeFeed(t, dropColumn)
			assertPayloadsStripTs(t, dropColumn, []string{
				`drop_column: [1]->{"after": {"a": 1, "b": "1"}}`,
				`drop_column: [2]->{"after": {"a": 2, "b": "2"}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE drop_column DROP COLUMN b`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (3)`)

			// since the changefeed level backfill (which flushes the sink before
			// the backfill) occurs before the schema-change backfill for a drop
			// column, the order in which the sink receives both backfills is
			// uncertain. the only guarantee here is per-key ordering guarantees,
			// so we must check both backfills in the same assertion.
			assertPayloadsPerKeyOrderedStripTs(t, dropColumn, []string{
				// Changefeed level backfill for DROP COLUMN b.
				`drop_column: [1]->{"after": {"a": 1}}`,
				`drop_column: [2]->{"after": {"a": 2}}`,
				// Schema-change backfill for DROP COLUMN b.
				`drop_column: [1]->{"after": {"a": 1}}`,
				`drop_column: [2]->{"after": {"a": 2}}`,
				// Insert 3 into drop_column
				`drop_column: [3]->{"after": {"a": 3}}`,
			})
		})

		t.Run(`multiple alters`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE multiple_alters (a INT PRIMARY KEY, b STRING)`)
			sqlDB.Exec(t, `INSERT INTO multiple_alters VALUES (1, '1')`)
			sqlDB.Exec(t, `INSERT INTO multiple_alters VALUES (2, '2')`)

			// Set up a hook to pause the changfeed on the next emit.
			var wg sync.WaitGroup
			waitSinkHook := func(_ context.Context) error {
				wg.Wait()
				return nil
			}
			knobs := s.TestingKnobs.
				DistSQL.(*execinfra.TestingKnobs).
				Changefeed.(*TestingKnobs)
			knobs.BeforeEmitRow = waitSinkHook

			multipleAlters := feed(t, f, `CREATE CHANGEFEED FOR multiple_alters WITH updated`)
			defer closeFeed(t, multipleAlters)
			assertPayloadsStripTs(t, multipleAlters, []string{
				`multiple_alters: [1]->{"after": {"a": 1, "b": "1"}}`,
				`multiple_alters: [2]->{"after": {"a": 2, "b": "2"}}`,
			})

			// Wait on the next emit, queue up three ALTERs. The next poll process
			// will see all of them at once.
			wg.Add(1)
			waitForSchemaChange(t, sqlDB, `ALTER TABLE multiple_alters DROP COLUMN b`)
			waitForSchemaChange(t, sqlDB, `ALTER TABLE multiple_alters ADD COLUMN c STRING DEFAULT 'cee'`)
			waitForSchemaChange(t, sqlDB, `ALTER TABLE multiple_alters ADD COLUMN d STRING DEFAULT 'dee'`)
			wg.Done()

			// assertions are grouped this way because the sink is flushed prior
			// to a changefeed level backfill, ensuring all messages are received
			// at the start of the assertion
			assertPayloadsPerKeyOrderedStripTs(t, multipleAlters, []string{
				// Changefeed level backfill for DROP COLUMN b.
				`multiple_alters: [1]->{"after": {"a": 1}}`,
				`multiple_alters: [2]->{"after": {"a": 2}}`,
				// Schema-change backfill for DROP COLUMN b.
				`multiple_alters: [1]->{"after": {"a": 1}}`,
				`multiple_alters: [2]->{"after": {"a": 2}}`,
				// Schema-change backfill for ADD COLUMN c.
				`multiple_alters: [1]->{"after": {"a": 1}}`,
				`multiple_alters: [2]->{"after": {"a": 2}}`,
			})
			assertPayloadsPerKeyOrderedStripTs(t, multipleAlters, []string{
				// Changefeed level backfill for ADD COLUMN c.
				`multiple_alters: [1]->{"after": {"a": 1, "c": "cee"}}`,
				`multiple_alters: [2]->{"after": {"a": 2, "c": "cee"}}`,
				// Schema change level backfill for ADD COLUMN d.
				`multiple_alters: [1]->{"after": {"a": 1, "c": "cee"}}`,
				`multiple_alters: [2]->{"after": {"a": 2, "c": "cee"}}`,
			})
			ts := schematestutils.FetchDescVersionModificationTime(t, s.TestServer.Server,
				`d`, `public`, `multiple_alters`, 10)
			// Changefeed level backfill for ADD COLUMN d.
			assertPayloads(t, multipleAlters, []string{
				// Backfill no-ops for column D (C schema change is complete)
				// TODO(dan): Track duplicates more precisely in sinklessFeed/tableFeed.
				// Scan output for column C
				fmt.Sprintf(`multiple_alters: [1]->{"after": {"a": 1, "c": "cee", "d": "dee"}, "updated": "%s"}`, ts.AsOfSystemTime()),
				fmt.Sprintf(`multiple_alters: [2]->{"after": {"a": 2, "c": "cee", "d": "dee"}, "updated": "%s"}`, ts.AsOfSystemTime()),
			})
		})
	}

	cdcTestWithSystem(t, testFn)

	log.FlushFiles()
	entries, err := log.FetchEntriesFromFiles(0, math.MaxInt64, 1,
		regexp.MustCompile("cdc ux violation"), log.WithFlattenedSensitiveData)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatalf("Found violation of CDC's guarantees: %v", entries)
	}
}

// TestChangefeedSchemaChangeAllowBackfill tests schema changes that require a
// backfill when the backfill option is allowed.
func TestChangefeedSchemaChangeAllowBackfill(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		// Expected semantics:
		//
		// 1) DROP COLUMN
		//
		// If the table descriptor is at version 1 when the `ALTER TABLE` stmt is issued,
		// we expect the changefeed level backfill to be triggered at the `ModificationTime` of
		// version 2 of the said descriptor. This is because this is the descriptor
		// version at which the dropped column stops being visible to SELECTs.
		//
		// 2) ADD COLUMN WITH DEFAULT & ADD COLUMN AS ... STORED
		//
		// If the table descriptor is at version 1 when the `ALTER TABLE` stmt
		// is issued, we expect the backfill to be triggered at the
		// `ModificationTime` of version 7 of said descriptor. This is because
		// this is the descriptor version at which the KV-level backfill is finished and
		// the primary index swap takes place to make the newly-added column public.

		t.Run(`add column with default`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_column_def (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `INSERT INTO add_column_def VALUES (1)`)
			sqlDB.Exec(t, `INSERT INTO add_column_def VALUES (2)`)
			addColumnDef := feed(t, f, `CREATE CHANGEFEED FOR add_column_def WITH updated`)
			defer closeFeed(t, addColumnDef)
			assertPayloadsStripTs(t, addColumnDef, []string{
				`add_column_def: [1]->{"after": {"a": 1}}`,
				`add_column_def: [2]->{"after": {"a": 2}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_column_def ADD COLUMN b STRING DEFAULT 'd'`)
			ts := schematestutils.FetchDescVersionModificationTime(t, s.Server, `d`, `public`, `add_column_def`, 7)
			assertPayloads(t, addColumnDef, []string{
				fmt.Sprintf(`add_column_def: [1]->{"after": {"a": 1, "b": "d"}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
				fmt.Sprintf(`add_column_def: [2]->{"after": {"a": 2, "b": "d"}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
			})
		})

		t.Run(`add column computed`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_col_comp (a INT PRIMARY KEY, b INT AS (a + 5) STORED)`)
			sqlDB.Exec(t, `INSERT INTO add_col_comp VALUES (1)`)
			sqlDB.Exec(t, `INSERT INTO add_col_comp (a) VALUES (2)`)
			addColComp := feed(t, f, `CREATE CHANGEFEED FOR add_col_comp WITH updated`)
			defer closeFeed(t, addColComp)
			assertPayloadsStripTs(t, addColComp, []string{
				`add_col_comp: [1]->{"after": {"a": 1, "b": 6}}`,
				`add_col_comp: [2]->{"after": {"a": 2, "b": 7}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_col_comp ADD COLUMN c INT AS (a + 10) STORED`)
			ts := schematestutils.FetchDescVersionModificationTime(t, s.Server, `d`, `public`, `add_col_comp`, 7)
			assertPayloads(t, addColComp, []string{
				fmt.Sprintf(`add_col_comp: [1]->{"after": {"a": 1, "b": 6, "c": 11}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
				fmt.Sprintf(`add_col_comp: [2]->{"after": {"a": 2, "b": 7, "c": 12}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
			})
		})

		t.Run(`drop column`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE drop_column (a INT PRIMARY KEY, b STRING)`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (1, '1')`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (2, '2')`)
			dropColumn := feed(t, f, `CREATE CHANGEFEED FOR drop_column WITH updated`)
			defer closeFeed(t, dropColumn)
			assertPayloadsStripTs(t, dropColumn, []string{
				`drop_column: [1]->{"after": {"a": 1, "b": "1"}}`,
				`drop_column: [2]->{"after": {"a": 2, "b": "2"}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE drop_column DROP COLUMN b`)
			ts := schematestutils.FetchDescVersionModificationTime(t, s.Server, `d`, `public`, `drop_column`, 6)

			// Backfill for DROP COLUMN b.
			assertPayloads(t, dropColumn, []string{
				fmt.Sprintf(`drop_column: [1]->{"after": {"a": 1}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
				fmt.Sprintf(`drop_column: [2]->{"after": {"a": 2}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
			})

			// Insert 3 into drop_column.
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (3)`)
			assertPayloadsStripTs(t, dropColumn, []string{
				`drop_column: [3]->{"after": {"a": 3}}`,
			})
		})

		t.Run(`multiple alters`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE multiple_alters (a INT PRIMARY KEY, b STRING) WITH (schema_locked = false)`)
			sqlDB.Exec(t, `INSERT INTO multiple_alters VALUES (1, '1')`)
			sqlDB.Exec(t, `INSERT INTO multiple_alters VALUES (2, '2')`)

			// Set up a hook to pause the changfeed on the next emit.
			var wg sync.WaitGroup
			waitSinkHook := func(_ context.Context) error {
				wg.Wait()
				return nil
			}
			knobs := s.TestingKnobs.
				DistSQL.(*execinfra.TestingKnobs).
				Changefeed.(*TestingKnobs)
			knobs.BeforeEmitRow = waitSinkHook

			multipleAlters := feed(t, f, `CREATE CHANGEFEED FOR multiple_alters WITH updated`)
			defer closeFeed(t, multipleAlters)
			assertPayloadsStripTs(t, multipleAlters, []string{
				`multiple_alters: [1]->{"after": {"a": 1, "b": "1"}}`,
				`multiple_alters: [2]->{"after": {"a": 2, "b": "2"}}`,
			})

			// Wait on the next emit, queue up three ALTERs. The next poll process
			// will see all of them at once.
			wg.Add(1)
			waitForSchemaChange(t, sqlDB, `ALTER TABLE multiple_alters DROP COLUMN b`)
			waitForSchemaChange(t, sqlDB, `ALTER TABLE multiple_alters ADD COLUMN c STRING DEFAULT 'cee'`)
			waitForSchemaChange(t, sqlDB, `ALTER TABLE multiple_alters ADD COLUMN d STRING DEFAULT 'dee'`)
			wg.Done()

			// When dropping the column, the desc goes from version 1->9 with the schema change being visible at
			// version 2. Then, when adding column c, it goes from 9->17, with the schema change being visible at
			// the 7th step (version 15). Finally, when adding column d, it goes from 17->25 ith the schema change
			// being visible at the 7th step (version 23).
			// TODO(#142936): Investigate if this descriptor version hardcoding is sound.
			dropTS := schematestutils.FetchDescVersionModificationTime(t, s.Server, `d`, `public`, `multiple_alters`, 6)
			addTS := schematestutils.FetchDescVersionModificationTime(t, s.Server, `d`, `public`, `multiple_alters`, 15)
			addTS2 := schematestutils.FetchDescVersionModificationTime(t, s.Server, `d`, `public`, `multiple_alters`, 23)

			assertPayloads(t, multipleAlters, []string{
				fmt.Sprintf(`multiple_alters: [1]->{"after": {"a": 1}, "updated": "%s"}`, dropTS.AsOfSystemTime()),
				fmt.Sprintf(`multiple_alters: [2]->{"after": {"a": 2}, "updated": "%s"}`, dropTS.AsOfSystemTime()),
				fmt.Sprintf(`multiple_alters: [1]->{"after": {"a": 1, "c": "cee"}, "updated": "%s"}`, addTS.AsOfSystemTime()),
				fmt.Sprintf(`multiple_alters: [2]->{"after": {"a": 2, "c": "cee"}, "updated": "%s"}`, addTS.AsOfSystemTime()),
				fmt.Sprintf(`multiple_alters: [1]->{"after": {"a": 1, "c": "cee", "d": "dee"}, "updated": "%s"}`, addTS2.AsOfSystemTime()),
				fmt.Sprintf(`multiple_alters: [2]->{"after": {"a": 2, "c": "cee", "d": "dee"}, "updated": "%s"}`, addTS2.AsOfSystemTime()),
			})
		})
	}

	runWithAndWithoutRegression141453(t, testFn, func(t *testing.T, testFn cdcTestFn) {
		cdcTest(t, testFn)
	})

	log.FlushFiles()
	entries, err := log.FetchEntriesFromFiles(0, math.MaxInt64, 1,
		regexp.MustCompile("cdc ux violation"), log.WithFlattenedSensitiveData)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatalf("Found violation of CDC's guarantees: %v", entries)
	}
}

// TestChangefeedSchemaChangeBackfillScope tests that when a changefeed is watching multiple tables and only
// one needs a backfill, we only see backfill rows emitted for that one table.
func TestChangefeedSchemaChangeBackfillScope(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		usingLegacySchemaChanger := maybeDisableDeclarativeSchemaChangesForTest(t, sqlDB)

		t.Run(`add column with default`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_column_def (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `CREATE TABLE no_def_change (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `INSERT INTO add_column_def VALUES (1)`)
			sqlDB.Exec(t, `INSERT INTO add_column_def VALUES (2)`)
			sqlDB.Exec(t, `INSERT INTO no_def_change VALUES (3)`)
			combinedFeed := feed(t, f, `CREATE CHANGEFEED FOR add_column_def, no_def_change WITH updated`)
			defer closeFeed(t, combinedFeed)
			assertPayloadsStripTs(t, combinedFeed, []string{
				`add_column_def: [1]->{"after": {"a": 1}}`,
				`add_column_def: [2]->{"after": {"a": 2}}`,
				`no_def_change: [3]->{"after": {"a": 3}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_column_def ADD COLUMN b STRING DEFAULT 'd'`)

			var ts hlc.Timestamp
			if usingLegacySchemaChanger {
				// Schema change becomes public at version 4.
				ts = schematestutils.FetchDescVersionModificationTime(t, s.TestServer.Server,
					`d`, `public`, `add_column_def`, 4)
				// The legacy schema changer rewrites KVs in place, so we see
				// an additional backfill before the changefeed-level backfill.
				assertPayloadsStripTs(t, combinedFeed, []string{
					`add_column_def: [1]->{"after": {"a": 1}}`,
					`add_column_def: [2]->{"after": {"a": 2}}`,
				})
			} else {
				// The primary index swap occurs at version 7.
				ts = schematestutils.FetchDescVersionModificationTime(t, s.TestServer.Server,
					`d`, `public`, `add_column_def`, 7)
			}
			assertPayloads(t, combinedFeed, []string{
				fmt.Sprintf(`add_column_def: [1]->{"after": {"a": 1, "b": "d"}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
				fmt.Sprintf(`add_column_def: [2]->{"after": {"a": 2, "b": "d"}, "updated": "%s"}`,
					ts.AsOfSystemTime()),
			})
		})

	}

	cdcTestWithSystem(t, testFn)
	log.FlushFiles()
	entries, err := log.FetchEntriesFromFiles(0, math.MaxInt64, 1,
		regexp.MustCompile("cdc ux violation"), log.WithFlattenedSensitiveData)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatalf("Found violation of CDC's guarantees: %v", entries)
	}
}

// Regression test for #34314
func TestChangefeedAfterSchemaChangeBackfill(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE after_backfill (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO after_backfill VALUES (0)`)
		sqlDB.Exec(t, `ALTER TABLE after_backfill ADD COLUMN b INT DEFAULT 1`)
		sqlDB.Exec(t, `INSERT INTO after_backfill VALUES (2, 3)`)
		afterBackfill := feed(t, f, `CREATE CHANGEFEED FOR after_backfill`)
		defer closeFeed(t, afterBackfill)
		assertPayloads(t, afterBackfill, []string{
			`after_backfill: [0]->{"after": {"a": 0, "b": 1}}`,
			`after_backfill: [2]->{"after": {"a": 2, "b": 3}}`,
		})
	}

	cdcTest(t, testFn)
	log.FlushFiles()
	entries, err := log.FetchEntriesFromFiles(0, math.MaxInt64, 1,
		regexp.MustCompile("cdc ux violation"), log.WithFlattenedSensitiveData)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatalf("Found violation of CDC's guarantees: %v", entries)
	}
}

func TestChangefeedEachColumnFamily(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {

		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		// Table with 2 column families.
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING, FAMILY most (a,b), FAMILY only_c (c))`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog', 'cat')`)

		// Must specify WITH split_column_families
		sqlDB.ExpectErrWithTimeout(t, `multiple column families`, `CREATE CHANGEFEED FOR foo`)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH split_column_families`, args...)
		defer closeFeed(t, foo)

		assertPayloads(t, foo, []string{
			`foo.most: [0]->{"after": {"a": 0, "b": "dog"}}`,
			`foo.only_c: [0]->{"after": {"c": "cat"}}`,
		})

		// No messages for unaffected column families.
		sqlDB.Exec(t, `UPDATE foo SET c='lion' WHERE a=0`)
		sqlDB.Exec(t, `UPDATE foo SET c='tiger' WHERE a=0`)
		assertPayloads(t, foo, []string{
			`foo.only_c: [0]->{"after": {"c": "lion"}}`,
			`foo.only_c: [0]->{"after": {"c": "tiger"}}`,
		})

		// No messages on insert for families where no non-null values were set.
		sqlDB.Exec(t, `INSERT INTO foo values (1, 'puppy', null)`)
		sqlDB.Exec(t, `INSERT INTO foo values (2, null, 'kitten')`)
		assertPayloads(t, foo, []string{
			`foo.most: [1]->{"after": {"a": 1, "b": "puppy"}}`,
			`foo.most: [2]->{"after": {"a": 2, "b": null}}`,
			`foo.only_c: [2]->{"after": {"c": "kitten"}}`,
		})

		sqlDB.Exec(t, `DELETE FROM foo WHERE a>0`)

		// Deletes send a message for each column family.
		fooWithDiff := feed(t, f, `CREATE CHANGEFEED FOR foo WITH split_column_families, diff`, args...)
		defer closeFeed(t, fooWithDiff)
		sqlDB.Exec(t, `DELETE FROM foo WHERE a=0`)
		assertPayloads(t, fooWithDiff, []string{
			`foo.most: [0]->{"after": {"a": 0, "b": "dog"}, "before": null}`,
			`foo.only_c: [0]->{"after": {"c": "tiger"}, "before": null}`,
			`foo.most: [0]->{"after": null, "before": {"a": 0, "b": "dog"}}`,
			`foo.only_c: [0]->{"after": null, "before": {"c": "tiger"}}`,
		})

		// Table with a second column family added after the changefeed starts.
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY, FAMILY f_a (a))`)
		sqlDB.Exec(t, `INSERT INTO bar VALUES (0)`)
		bar := feed(t, f, `CREATE CHANGEFEED FOR bar`, args...)
		defer closeFeed(t, bar)
		assertPayloads(t, bar, []string{
			`bar: [0]->{"after": {"a": 0}}`,
		})
		sqlDB.Exec(t, `ALTER TABLE bar ADD COLUMN b STRING CREATE FAMILY f_b`)
		sqlDB.Exec(t, `INSERT INTO bar VALUES (1)`)
		if _, err := bar.Next(); !testutils.IsError(err, `created on a table with a single column family`) {
			t.Errorf(`expected "column family" error got: %+v`, err)
		}
	}

	cdcTest(t, testFn, withAllowChangefeedErr("expects terminal error"))
}

func TestChangefeedSingleColumnFamily(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		// Table with 2 column families.
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING, d STRING, FAMILY most (a,b), FAMILY rest (c, d))`)
		sqlDB.Exec(t, `INSERT INTO foo(a,b,c) values (0, 'dog', 'cat')`)
		sqlDB.Exec(t, `INSERT INTO foo(a,b,c) values (1, 'dollar', 'cent')`)

		sqlDB.ExpectErrWithTimeout(t, `nosuchfamily`, `CREATE CHANGEFEED FOR foo FAMILY nosuchfamily`)

		// TODO(#145927): unskip this when we have family or topic info in enriched feeds.
		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}

		fooMost := feed(t, f, `CREATE CHANGEFEED FOR foo FAMILY most`, args...)
		defer closeFeed(t, fooMost)
		assertPayloads(t, fooMost, []string{
			`foo.most: [0]->{"after": {"a": 0, "b": "dog"}}`,
			`foo.most: [1]->{"after": {"a": 1, "b": "dollar"}}`,
		})

		fooRest := feed(t, f, `CREATE CHANGEFEED FOR foo FAMILY rest`, args...)
		defer closeFeed(t, fooRest)
		assertPayloads(t, fooRest, []string{
			`foo.rest: [0]->{"after": {"c": "cat", "d": null}}`,
			`foo.rest: [1]->{"after": {"c": "cent", "d": null}}`,
		})

		fooBoth := feed(t, f, `CREATE CHANGEFEED FOR foo FAMILY rest, foo FAMILY most`, args...)
		defer closeFeed(t, fooBoth)
		assertPayloads(t, fooBoth, []string{
			`foo.most: [0]->{"after": {"a": 0, "b": "dog"}}`,
			`foo.rest: [0]->{"after": {"c": "cat", "d": null}}`,
			`foo.most: [1]->{"after": {"a": 1, "b": "dollar"}}`,
			`foo.rest: [1]->{"after": {"c": "cent", "d": null}}`,
		})

		sqlDB.Exec(t, `DELETE FROM foo WHERE a = 0`)
		assertPayloads(t, fooBoth, []string{
			`foo.most: [0]->{"after": null}`,
			`foo.rest: [0]->{"after": null}`,
		})

	}
	cdcTest(t, testFn)
}

func TestChangefeedSingleColumnFamilySchemaChanges(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	// requireErrorSoon times out after 30 seconds
	skip.UnderStress(t)
	skip.UnderRace(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		// Table with 2 column families.
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING, FAMILY most (a,b), FAMILY rest (c))`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog', 'cat')`)

		arg := optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families; see #145927"}
		fooMost := feed(t, f, `CREATE CHANGEFEED FOR foo FAMILY most`, arg)
		defer closeFeed(t, fooMost)
		assertPayloads(t, fooMost, []string{
			`foo.most: [0]->{"after": {"a": 0, "b": "dog"}}`,
		})

		fooRest := feed(t, f, `CREATE CHANGEFEED FOR foo FAMILY rest`, arg)
		defer closeFeed(t, fooRest)
		assertPayloads(t, fooRest, []string{
			`foo.rest: [0]->{"after": {"c": "cat"}}`,
		})

		// Add a column to an existing family, it shows up in the feed for that family
		sqlDB.Exec(t, `ALTER TABLE foo ADD COLUMN more int DEFAULT 11 FAMILY most`)
		assertPayloads(t, fooMost, []string{
			`foo.most: [0]->{"after": {"a": 0, "b": "dog", "more": 11}}`,
		})

		// Removing all columns in a watched family fails the feed
		waitForSchemaChange(t, sqlDB, `ALTER TABLE foo DROP column c`)
		requireTerminalErrorSoon(context.Background(), t, fooRest,
			regexp.MustCompile(`CHANGEFEED targeting nonexistent or removed column family rest of table foo`))
	}

	runWithAndWithoutRegression141453(t, testFn, func(t *testing.T, testFn cdcTestFn) {
		cdcTest(t, testFn, withAllowChangefeedErr("expects terminal error"))
	}, withMaybeUseLegacySchemaChanger())
}

func TestChangefeedEachColumnFamilySchemaChanges(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		// Table with 2 column families.
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING, FAMILY f1 (a,b), FAMILY f2 (c))`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog', 'cat')`)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH split_column_families`, args...)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{
			`foo.f1: [0]->{"after": {"a": 0, "b": "dog"}}`,
			`foo.f2: [0]->{"after": {"c": "cat"}}`,
		})

		// Add a column to an existing family
		sqlDB.Exec(t, `ALTER TABLE foo ADD COLUMN d string DEFAULT 'hi' FAMILY f2`)
		assertPayloads(t, foo, []string{
			`foo.f2: [0]->{"after": {"c": "cat", "d": "hi"}}`,
		})

		// Add a column to a new family.
		// Behavior here is a little wonky with default values in a way
		// that's likely to change with declarative schema changer,
		// so not asserting anything either way about that.
		sqlDB.Exec(t, `ALTER TABLE foo ADD COLUMN e string CREATE FAMILY f3`)
		sqlDB.Exec(t, `UPDATE foo SET e='hello' WHERE a=0`)
		assertPayloads(t, foo, []string{
			`foo.f3: [0]->{"after": {"e": "hello"}}`,
		})
	}

	runWithAndWithoutRegression141453(t, testFn, func(t *testing.T, testFn cdcTestFn) {
		cdcTest(t, testFn)
	}, withMaybeUseLegacySchemaChanger())
}

func TestCoreChangefeedRequiresSelectPrivilege(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		rootDB := sqlutils.MakeSQLRunner(s.DB)
		rootDB.Exec(t, `CREATE USER user1`)
		rootDB.Exec(t, `CREATE TYPE type_a as enum ('a')`)
		rootDB.Exec(t, `CREATE TABLE table_a (id int, type type_a)`)
		rootDB.Exec(t, `CREATE TABLE table_b (id int, type type_a)`)
		rootDB.Exec(t, `INSERT INTO table_a(id) values (0)`)

		expectSuccess := func(stmt string) {
			successfulFeed := feed(t, f, stmt)
			defer closeFeed(t, successfulFeed)
			_, err := successfulFeed.Next()
			require.NoError(t, err)
		}

		asUser(t, f, `user1`, func(_ *sqlutils.SQLRunner) {
			expectErrCreatingFeed(t, f, `CREATE CHANGEFEED FOR table_a`,
				`user user1 requires the SELECT privilege on all target tables to be able to run a core changefeed`)
		})
		rootDB.Exec(t, `GRANT SELECT ON table_a TO user1`)
		asUser(t, f, `user1`, func(_ *sqlutils.SQLRunner) {
			expectSuccess(`CREATE CHANGEFEED FOR table_a`)
		})
		asUser(t, f, `user1`, func(_ *sqlutils.SQLRunner) {
			expectErrCreatingFeed(t, f, `CREATE CHANGEFEED FOR table_a, table_b`,
				`user user1 requires the SELECT privilege on all target tables to be able to run a core changefeed`)
		})

		rootDB.Exec(t, `GRANT SELECT ON table_b TO user1`)
		asUser(t, f, `user1`, func(_ *sqlutils.SQLRunner) {
			expectSuccess(`CREATE CHANGEFEED FOR table_a, table_b`)
		})
	}
	cdcTest(t, testFn, feedTestForceSink("sinkless"))
}

// TODO(#94757): remove CONTROLCHANGEFEED entirely
func TestControlChangefeedRoleOption(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		rootDB := sqlutils.MakeSQLRunner(s.DB)
		rootDB.Exec(t, `CREATE USER user1 WITH CONTROLCHANGEFEED`)
		rootDB.Exec(t, `CREATE TYPE type_a as enum ('a')`)
		rootDB.Exec(t, `CREATE TABLE table_a (id int, type type_a)`)
		rootDB.Exec(t, `CREATE TABLE table_b (id int, type type_a)`)
		rootDB.Exec(t, `INSERT INTO table_a(id) values (0)`)

		expectSuccess := func(stmt string) {
			successfulFeed := feed(t, f, stmt)
			defer closeFeed(t, successfulFeed)
			_, err := successfulFeed.Next()
			require.NoError(t, err)
		}

		asUser(t, f, `user1`, func(_ *sqlutils.SQLRunner) {
			expectErrCreatingFeed(t, f, `CREATE CHANGEFEED FOR table_a, table_b`,
				`pq: user user1 with CONTROLCHANGEFEED role option requires the SELECT privilege on all target tables to be able to run an enterprise changefeed`)
		})
		rootDB.Exec(t, `GRANT SELECT ON table_a TO user1`)
		asUser(t, f, `user1`, func(_ *sqlutils.SQLRunner) {
			expectErrCreatingFeed(t, f, `CREATE CHANGEFEED FOR table_a, table_b`,
				`pq: user user1 with CONTROLCHANGEFEED role option requires the SELECT privilege on all target tables to be able to run an enterprise changefeed`)
		})
		rootDB.Exec(t, `GRANT SELECT ON table_b TO user1`)
		asUser(t, f, `user1`, func(_ *sqlutils.SQLRunner) {
			expectSuccess(`CREATE CHANGEFEED FOR table_a`)
		})
	}
	cdcTest(t, testFn, feedTestOmitSinks("sinkless"))
}

func TestChangefeedCreateAuthorizationWithChangefeedPriv(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s, db, _ := serverutils.StartServer(t, base.TestServerArgs{
		DefaultTestTenant: base.TODOTestTenantDisabled,
		Knobs: base.TestingKnobs{
			JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
			DistSQL: &execinfra.TestingKnobs{
				Changefeed: &TestingKnobs{
					WrapSink: func(s Sink, _ jobspb.JobID) Sink {
						if _, ok := s.(*externalConnectionKafkaSink); ok {
							return s
						}
						return &externalConnectionKafkaSink{sink: s, ignoreDialError: true}
					},
				},
			},
		},
	})
	defer s.Stopper().Stop(ctx)

	rootDB := sqlutils.MakeSQLRunner(db)
	rootDB.Exec(t, `CREATE USER user1`)
	rootDB.Exec(t, `CREATE TYPE type_a as enum ('a')`)
	rootDB.Exec(t, `CREATE TABLE table_a (id int, type type_a)`)
	rootDB.Exec(t, `CREATE TABLE table_b (id int, type type_a)`)
	rootDB.Exec(t, `INSERT INTO table_a(id) values (0)`)
	rootDB.Exec(t, `SET CLUSTER SETTING kv.rangefeed.enabled = true`)
	enableEnterprise := utilccl.TestingDisableEnterprise()
	enableEnterprise()

	withUser := func(t *testing.T, user string, fn func(*sqlutils.SQLRunner)) {
		password := `password`
		rootDB.Exec(t, fmt.Sprintf(`ALTER USER %s WITH PASSWORD '%s'`, user, password))

		pgURL := url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(user, password),
			Host:   s.SQLAddr(),
		}
		db2, err := gosql.Open("postgres", pgURL.String())
		if err != nil {
			t.Fatal(err)
		}
		defer db2.Close()
		userDB := sqlutils.MakeSQLRunner(db2)

		fn(userDB)
	}

	rootDB.Exec(t, `CREATE EXTERNAL CONNECTION "nope" AS 'kafka://nope'`)

	withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
		userDB.ExpectErr(t,
			"user user1 requires the CHANGEFEED privilege on all target tables to be able to run an enterprise changefeed",
			"CREATE CHANGEFEED for table_a, table_b INTO 'external://nope'",
		)
	})
	rootDB.Exec(t, "GRANT CHANGEFEED ON table_a TO user1")
	withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
		userDB.ExpectErr(t,
			"user user1 requires the CHANGEFEED privilege on all target tables to be able to run an enterprise changefeed",
			"CREATE CHANGEFEED for table_a, table_b INTO 'external://nope'",
		)
	})
	rootDB.Exec(t, "GRANT CHANGEFEED ON table_b TO user1")
	withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
		userDB.Exec(t,
			"CREATE CHANGEFEED for table_a, table_b INTO 'external://nope'",
		)
	})

	// With require_external_connection_sink enabled, the user requires USAGE on the external connection.
	rootDB.Exec(t, "SET CLUSTER SETTING changefeed.permissions.require_external_connection_sink.enabled = true")
	withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
		userDB.ExpectErr(t,
			"pq: the CHANGEFEED privilege on all tables can only be used with external connection sinks",
			"CREATE CHANGEFEED for table_a, table_b INTO 'kafka://nope'",
		)
	})
	rootDB.Exec(t, "GRANT USAGE ON EXTERNAL CONNECTION nope to user1")
	withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
		userDB.Exec(t,
			"CREATE CHANGEFEED for table_a, table_b INTO 'external://nope'",
		)
	})
	rootDB.Exec(t, "SET CLUSTER SETTING changefeed.permissions.require_external_connection_sink.enabled = false")
}

func TestChangefeedGrant(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		rootDB := sqlutils.MakeSQLRunner(s.DB)
		rootDB.Exec(t, `create user guest`)

		// CHANGEFEED can be granted as a default privilege on all new tables in a schema
		rootDB.ExecMultiple(t,
			`ALTER DEFAULT PRIVILEGES IN SCHEMA d.public GRANT CHANGEFEED ON TABLES TO guest`,
			`CREATE TABLE table_c (id int primary key)`,
			`INSERT INTO table_c values (0)`,
		)

		// SHOW GRANTS includes CHANGEFEED privileges.
		var count int
		rootDB.QueryRow(t, `select count(*) from [show grants] where privilege_type = 'CHANGEFEED';`).Scan(&count)
		require.Greater(t, count, 0, `Number of CHANGEFEED grants`)

	}
	cdcTest(t, testFn)
}

// TestChangefeedJobControl tests if a user can control a changefeed
// based on their permissions.
func TestChangefeedJobControl(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		ChangefeedJobPermissionsTestSetup(t, s)

		createFeed := func(stmt string) (cdctest.EnterpriseTestFeed, func()) {
			successfulFeed := feed(t, f, stmt)
			closeCf := func() {
				closeFeed(t, successfulFeed)
			}
			_, err := successfulFeed.Next()
			require.NoError(t, err)
			return successfulFeed.(cdctest.EnterpriseTestFeed), closeCf
		}

		// Create a changefeed and assert who can control the job.
		var currentFeed cdctest.EnterpriseTestFeed
		var closeCf func()
		asUser(t, f, `feedCreator`, func(_ *sqlutils.SQLRunner) {
			currentFeed, closeCf = createFeed(`CREATE CHANGEFEED FOR table_a, table_b`)
		})
		asUser(t, f, `adminUser`, func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t, "PAUSE job $1", currentFeed.JobID())
			waitForJobState(userDB, t, currentFeed.JobID(), "paused")
			userDB.Exec(t, "ALTER JOB $1 OWNER TO feedowner", currentFeed.JobID())
		})
		asUser(t, f, `userWithAllGrants`, func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t, "RESUME job $1", currentFeed.JobID())
			waitForJobState(userDB, t, currentFeed.JobID(), "running")
		})
		asUser(t, f, `jobController`, func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t, "RESUME job $1", currentFeed.JobID())
			waitForJobState(userDB, t, currentFeed.JobID(), "running")
		})
		asUser(t, f, `userWithSomeGrants`, func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t, "does not have privileges for job", "PAUSE job $1", currentFeed.JobID())
		})
		asUser(t, f, `regularUser`, func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t, "does not have privileges for job", "PAUSE job $1", currentFeed.JobID())
		})
		closeCf()

		// No one can modify changefeeds created by admins, except for admins.
		// In this case, the root user creates the changefeed.
		asUser(t, f, "adminUser", func(runner *sqlutils.SQLRunner) {
			currentFeed, closeCf = createFeed(`CREATE CHANGEFEED FOR table_a, table_b`)
		})
		asUser(t, f, `adminUser`, func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t, "PAUSE job $1", currentFeed.JobID())
			waitForJobState(userDB, t, currentFeed.JobID(), "paused")
		})
		asUser(t, f, `userWithAllGrants`, func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t, "only admins can control jobs owned by other admins", "PAUSE job $1", currentFeed.JobID())
		})
		asUser(t, f, `jobController`, func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t, "only admins can control jobs owned by other admins", "PAUSE job $1", currentFeed.JobID())
		})
		closeCf()
	}

	// Only enterprise sinks create jobs.
	cdcTest(t, testFn, feedTestEnterpriseSinks)
}

func TestChangefeedCustomKey(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'dog', 'cat')`)
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH key_column='b', unordered`,
			optOutOfMetamorphicEnrichedEnvelope{reason: "custom key not supported in test framework"})
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{
			`foo: ["dog"]->{"after": {"a": 0, "b": "dog", "c": "cat"}}`,
		})
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'dog', 'zebra')`)
		assertPayloads(t, foo, []string{
			`foo: ["dog"]->{"after": {"a": 1, "b": "dog", "c": "zebra"}}`,
		})
		sqlDB.Exec(t, `ALTER TABLE foo RENAME COLUMN b to b2`)
		requireTerminalErrorSoon(context.Background(), t, foo, regexp.MustCompile(`required column b not present`))
	}
	cdcTest(t, testFn, feedTestForceSink("kafka"), withAllowChangefeedErr("expects error"))
}

// Reproduce issue for #114196. This test verifies that changefeed with custom
// key column works with CDC queries correctly.
func TestChangefeedCustomKeyColumnWithCDCQuery(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	tests := map[string]struct {
		shouldSkip           bool
		createTableStmt      string
		createChangeFeedStmt string
		stmts                []string
		payloadsAfterStmts   []string
	}{
		`select_star`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='b', unordered AS SELECT * FROM foo`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog', 'cat')`, `INSERT INTO foo VALUES (1, 'dog1', 'cat1')`},
			payloadsAfterStmts:   []string{`foo: ["dog"]->{"a": 0, "b": "dog", "c": "cat"}`, `foo: ["dog1"]->{"a": 1, "b": "dog1", "c": "cat1"}`},
		},
		`select_with_filter`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='b', unordered AS SELECT * FROM foo WHERE b='dog'`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog', 'cat')`, `INSERT INTO foo VALUES (1, 'dog1', 'cat1')`},
			payloadsAfterStmts:   []string{`foo: ["dog"]->{"a": 0, "b": "dog", "c": "cat"}`},
		},
		`select_multiple_columns`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='c', unordered AS SELECT b, c FROM foo WHERE b='dog'`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog', 'cat')`, `INSERT INTO foo VALUES (1, 'dog1', 'cat1')`},
			payloadsAfterStmts:   []string{`foo: ["cat"]->{"b": "dog", "c": "cat"}`},
		},
		`custom_key_with_created_column`: {
			shouldSkip:           true,
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='double_b', unordered AS SELECT concat(b, c) AS double_b FROM foo`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog', 'cat')`, `INSERT INTO foo VALUES (1, 'dog1', 'cat1')`},
			payloadsAfterStmts:   []string{`foo: ["cat"]->{"c": "cat"}`},
		},
		`select_star_with_builtin_funcs`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='c', unordered AS SELECT *, concat(b, c) FROM foo`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog', 'cat')`, `INSERT INTO foo VALUES (1, 'dog1', 'cat1')`},
			payloadsAfterStmts:   []string{`foo: ["cat"]->{"a": 0, "b": "dog", "c": "cat", "concat": "dogcat"}`, `foo: ["cat1"]->{"a": 1, "b": "dog1", "c": "cat1", "concat": "dog1cat1"}`},
		},
		`select_stored_column`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING, d STRING AS (concat(b, c)) STORED)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='d', unordered AS SELECT * FROM foo WHERE d='dog1cat1'`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog', 'cat')`, `INSERT INTO foo VALUES (1, 'dog1', 'cat1')`},
			payloadsAfterStmts:   []string{`foo: ["dog1cat1"]->{"a": 1, "b": "dog1", "c": "cat1", "d": "dog1cat1"}`},
		},
		`select_virtual_column`: {
			shouldSkip:           true,
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING, d STRING AS (concat(b, c)) VIRTUAL)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='d', unordered AS SELECT d FROM foo WHERE d='dog1cat1'`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog', 'cat')`, `INSERT INTO foo VALUES (1, 'dog1', 'cat1')`},
			payloadsAfterStmts:   []string{`foo: ["dog1cat1"]->{"a": 1, "b": "dog1", "c": "cat1", "d": "dog1cat1"}`},
		},
		`select_with_filter_IN`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='b', unordered AS SELECT * FROM foo WHERE b IN ('dog', 'dog1')`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog')`, `INSERT INTO foo VALUES (1, 'dog1')`},
			payloadsAfterStmts:   []string{`foo: ["dog"]->{"a": 0, "b": "dog"}`, `foo: ["dog1"]->{"a": 1, "b": "dog1"}`},
		},
		`select_with_delete`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='b', unordered AS SELECT *, event_op() = 'delete' AS deleted FROM foo WHERE 'hello' != 'world'`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog')`, `INSERT INTO foo VALUES (1, 'dog1')`, `DELETE FROM foo WHERE a=1`},
			payloadsAfterStmts:   []string{`foo: [null]->{"a": 1, "b": null, "deleted": true}`, `foo: ["dog"]->{"a": 0, "b": "dog", "deleted": false}`, `foo: ["dog1"]->{"a": 1, "b": "dog1", "deleted": false}`},
		},
		`select_with_filter_delete`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='b', unordered AS SELECT * FROM foo WHERE event_op() = 'delete'`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog')`, `INSERT INTO foo VALUES (1, 'dog1')`, `DELETE FROM foo WHERE a=1`},
			payloadsAfterStmts:   []string{`foo: [null]->{"a": 1, "b": null}`},
		},
		`select_with_cdc_prev`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='b', unordered AS SELECT *, (cdc_prev) FROM foo`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog')`, `UPSERT INTO foo VALUES (0,'dog1')`},
			payloadsAfterStmts:   []string{`foo: ["dog"]->{"a": 0, "b": "dog", "cdc_prev": null}`, `foo: ["dog1"]->{"a": 0, "b": "dog1", "cdc_prev": {"a": 0, "b": "dog"}}`},
		},
		`select_with_filter_cdc_prev`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='b', unordered AS SELECT b FROM foo WHERE (cdc_prev).a = 0`,
			stmts:                []string{`INSERT INTO foo VALUES (0, 'dog')`, `UPSERT INTO foo VALUES (0, 'dog1')`},
			payloadsAfterStmts:   []string{`foo: ["dog1"]->{"b": "dog1"}`},
		},
		`select_with_hidden_column`: {
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING NOT VISIBLE)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='b', unordered AS SELECT b FROM foo`,
			stmts:                []string{`INSERT INTO foo(a,b) VALUES (0, 'dog')`, `INSERT INTO foo(a,b) VALUES (1, 'dog1')`},
			payloadsAfterStmts:   []string{`foo: ["dog"]->{"b": "dog"}`, `foo: ["dog1"]->{"b": "dog1"}`},
		},
		`select_with_cdc_prev_column`: {
			shouldSkip:           true,
			createTableStmt:      `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`,
			createChangeFeedStmt: `CREATE CHANGEFEED WITH key_column='cdc_prev.a', unordered AS SELECT * FROM foo`,
			stmts:                []string{`INSERT INTO foo(a,b) VALUES (0, 'dog')`, `INSERT INTO foo(a,b) VALUES (1, 'dog1')`},
			payloadsAfterStmts:   []string{`foo: ["dog"]->{"b": "dog"}`, `foo: ["dog1"]->{"b": "dog1"}`},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.shouldSkip {
				t.Logf("skipping this test because %s is currently not supported; "+
					"see #115267 for more details", name)
				return
			}
			testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
				sqlDB := sqlutils.MakeSQLRunner(s.DB)
				sqlDB.Exec(t, test.createTableStmt)
				foo := feed(t, f, test.createChangeFeedStmt)
				defer closeFeed(t, foo)
				for _, stmt := range test.stmts {
					sqlDB.Exec(t, stmt)
				}
				assertPayloads(t, foo, test.payloadsAfterStmts)
			}
			cdcTest(t, testFn, feedTestForceSink("kafka"))
		})
	}
}

func TestChangefeedCustomKeyAvro(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog', 'cat')`)
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH format='avro', key_column='b', unordered`)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{
			`foo: {"b":{"string":"dog"}}->{"after":{"foo":{"a":{"long":0},"b":{"string":"dog"},"c":{"string":"cat"}}}}`,
		})
	}
	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestChangefeedColumnFamilyAvro(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, c STRING, FAMILY most (a,b), FAMILY justc (c))`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog', 'cat')`)
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH split_column_families, format=avro`)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{
			`foo.most: {"a":{"long":0}}->{"after":{"foo_u002e_most":{"a":{"long":0},"b":{"string":"dog"}}}}`,
			`foo.justc: {"a":{"long":0}}->{"after":{"foo_u002e_justc":{"c":{"string":"cat"}}}}`,
		})
	}
	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestChangefeedBareAvro(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog')`)
		foo := feed(t, f, `CREATE CHANGEFEED WITH format=avro, schema_change_policy=stop AS SELECT * FROM foo`)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{
			`foo: {"a":{"long":0}}->{"record":{"foo":{"a":{"long":0},"b":{"string":"dog"}}}}`,
		})
	}
	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func toJSON(t *testing.T, x any) string {
	t.Helper()
	// Our json library formats differently than the stdlib, and json.MakeJSON()
	// chokes on some inputs, so marshal it twice.
	bs, err := gojson.Marshal(x)
	require.NoError(t, err)
	j, err := json.ParseJSON(string(bs))
	require.NoError(t, err)
	return j.String()
}

func TestChangefeedEnriched(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	withField := func(fieldName string, schema map[string]any) map[string]any {
		s := maps.Clone(schema)
		s["field"] = fieldName
		return s
	}

	key := map[string]any{"a": 0}
	keySchema := map[string]any{
		"name": "foo.key",
		"fields": []map[string]any{
			{"field": "a", "optional": false, "type": "int64"},
		},
		"optional": false,
		"type":     "struct",
	}

	afterVal := map[string]any{"a": 0, "b": "dog"}
	afterSchema := map[string]any{
		"name":  "foo.after.value",
		"field": "after",
		"fields": []map[string]any{
			{"field": "a", "optional": false, "type": "int64"},
			{"field": "b", "optional": true, "type": "string"},
		},
		"optional": false,
		"type":     "struct",
	}

	payload := map[string]any{
		"after": afterVal,
		"op":    "c",
		// TODO(#139662): add `before`
		// NOTE: ts_ns is stripped by the test framework, and source is specified separately
	}

	// Create an enriched source provider with no data. The contents of source
	// will be tested in another test, we just want to make sure the structure &
	// schema is right here.
	esp, err := newEnrichedSourceProvider(changefeedbase.EncodingOptions{}, getTestingEnrichedSourceData())
	require.NoError(t, err)
	source, err := esp.GetJSON(cdcevent.TestingMakeEventRowFromEncDatums([]rowenc.EncDatum{}, nil, 0, false), eventContext{})
	require.NoError(t, err)

	var sourceMap map[string]any
	require.NoError(t, gojson.Unmarshal([]byte(source.String()), &sourceMap))

	sourceSchema, err := esp.KafkaConnectJSONSchema().AsJSON()
	require.NoError(t, err)
	var sourceSchemaMap map[string]any
	require.NoError(t, gojson.Unmarshal([]byte(sourceSchema.String()), &sourceSchemaMap))
	sourceSchemaMap["field"] = "source"

	tsNsSchema := map[string]any{"field": "ts_ns", "optional": false, "type": "int64"}
	opSchema := map[string]any{"field": "op", "optional": false, "type": "string"}

	cases := []struct {
		name                 string
		enrichedProperties   []string
		messageWithoutSource map[string]any
		withSource           bool
		expectedKey          map[string]any
		keyInValue           bool
	}{
		{
			name:                 "with nothing",
			messageWithoutSource: payload,
			expectedKey:          key,
		},
		{
			name:               "with schema",
			enrichedProperties: []string{"schema"},
			messageWithoutSource: map[string]any{
				"payload": payload,
				"schema": map[string]any{
					"name":     "cockroachdb.envelope",
					"optional": false,
					"fields": []map[string]any{
						afterSchema,
						tsNsSchema,
						opSchema,
					},
					"type": "struct",
				},
			},
			expectedKey: map[string]any{
				"payload": key,
				"schema":  keySchema,
			},
		},
		{
			name:                 "with source",
			enrichedProperties:   []string{"source"},
			messageWithoutSource: payload,
			withSource:           true,
			expectedKey:          key,
		},
		{
			name:               "with schema and source",
			enrichedProperties: []string{"schema", "source"},
			messageWithoutSource: map[string]any{
				"payload": payload,
				"schema": map[string]any{
					"name":     "cockroachdb.envelope",
					"optional": false,
					"fields": []map[string]any{
						afterSchema,
						sourceSchemaMap,
						tsNsSchema,
						opSchema,
					},
					"type": "struct",
				},
			},
			withSource: true,
			expectedKey: map[string]any{
				"payload": key,
				"schema":  keySchema,
			},
		},
		{
			name:       "with key_in_value",
			keyInValue: true,
			messageWithoutSource: map[string]any{
				"after": afterVal,
				"op":    "c",
				"key":   key,
			},
			expectedKey: key,
		},
		{
			name:               "with source and key_in_value",
			enrichedProperties: []string{"source"},
			keyInValue:         true,
			messageWithoutSource: map[string]any{
				"after": afterVal,
				"op":    "c",
				"key":   key,
			},
			withSource:  true,
			expectedKey: key,
		},
		{
			name:               "with schema and key_in_value",
			enrichedProperties: []string{"schema"},
			keyInValue:         true,
			messageWithoutSource: map[string]any{
				"payload": map[string]any{
					"after": afterVal,
					"op":    "c",
					// NOTE: this key does not have its schema in it here, because that would be redundant/strange.
					"key": key,
				},
				"schema": map[string]any{
					"name":     "cockroachdb.envelope",
					"optional": false,
					"fields": []map[string]any{
						afterSchema,
						withField("key", keySchema),
						tsNsSchema,
						opSchema,
					},
					"type": "struct",
				},
			},
			withSource: false,
			expectedKey: map[string]any{
				"payload": key,
				"schema":  keySchema,
			},
		},
		{
			name:               "with schema, source, and key_in_value",
			enrichedProperties: []string{"schema", "source"},
			keyInValue:         true,
			messageWithoutSource: map[string]any{
				"payload": map[string]any{
					"after": afterVal,
					"op":    "c",
					"key":   key,
				},
				"schema": map[string]any{
					"name":     "cockroachdb.envelope",
					"optional": false,
					"fields": []map[string]any{
						afterSchema,
						sourceSchemaMap,
						withField("key", keySchema),
						tsNsSchema,
						opSchema,
					},
					"type": "struct",
				},
			},
			withSource: true,
			expectedKey: map[string]any{
				"payload": key,
				"schema":  keySchema,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {

				// The webhook testfeed removes the key from the value, so skip keyInValue and schema'd tests for it.
				_, isWebhook := f.(*webhookFeedFactory)
				if isWebhook && (tc.keyInValue || slices.Contains(tc.enrichedProperties, "schema")) {
					return
				}

				sqlDB := sqlutils.MakeSQLRunner(s.DB)

				sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
				sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog')`)

				create := fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH envelope=enriched, enriched_properties='%s'`,
					strings.Join(tc.enrichedProperties, ","))
				if tc.keyInValue {
					create += ", key_in_value"
				}
				foo := feed(t, f, create)
				defer closeFeed(t, foo)

				// The webhook testfeed relies on source.table_name for assertion matching. See: #145927
				topic := "foo"
				if isWebhook && !slices.Contains(tc.enrichedProperties, "source") {
					topic = ""
				}

				assertion := fmt.Sprintf("%s: %s->%s", topic, toJSON(t, tc.expectedKey), toJSON(t, tc.messageWithoutSource))
				sourceAssertion := func(actualSource map[string]any) {
					if tc.withSource {
						// Just check the source's structure.
						require.ElementsMatch(t, slices.Collect(maps.Keys(sourceMap)), slices.Collect(maps.Keys(actualSource)))
					} else {
						require.Empty(t, actualSource)
					}
				}
				assertPayloadsEnriched(t, foo, []string{assertion}, sourceAssertion)
			}
			supportedSinks := []string{"kafka", "pubsub", "sinkless", "webhook"}
			for _, sink := range supportedSinks {
				cdcTest(t, testFn, feedTestForceSink(sink))
			}
		})
	}
}

// TestChangefeedsParallelEnriched tests that multiple changefeeds can run in
// parallel with the enriched envelope. It is most useful under race, to ensure
// that there is no accidental data races in the encoders and source providers.
func TestChangefeedsParallelEnriched(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	const numFeeds = 10
	const maxIterations = 1_000_000_000
	const maxRows = 100

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		db := sqlutils.MakeSQLRunner(s.DB)
		db.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		ctx, cancel := context.WithCancel(ctx)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			db := sqlutils.MakeSQLRunner(s.Server.SQLConn(t))
			var i int
			for i = 0; i < maxIterations && ctx.Err() == nil; i++ {
				db.Exec(t, `UPSERT INTO d.foo VALUES ($1, $2)`, i%maxRows, fmt.Sprintf("hello %d", i))
			}
		}()

		opts := `envelope='enriched'`

		_, isKafka := f.(*kafkaFeedFactory)
		useAvro := isKafka && rand.Intn(2) == 0
		if useAvro {
			t.Logf("using avro")
			opts += `, format='avro'`
		}
		var feeds []cdctest.TestFeed
		for range numFeeds {
			feed := feed(t, f, fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH %s`, opts))
			feeds = append(feeds, feed)
		}

		// consume from the feeds
		for _, feed := range feeds {
			feed := feed
			msgCount := 0

			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					_, err := feed.Next()
					if err != nil {
						if errors.Is(err, context.Canceled) {
							t.Errorf("error reading from feed: %v", err)
						}
						break
					}
					msgCount++
				}
				assert.GreaterOrEqual(t, msgCount, 0)
			}()
		}

		// let the feeds run for a few seconds
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			t.Fatalf("%v", ctx.Err())
		}

		cancel()

		for _, feed := range feeds {
			closeFeed(t, feed)
		}

		doneWaiting := make(chan struct{})
		go func() {
			defer close(doneWaiting)
			wg.Wait()
		}()
		select {
		case <-doneWaiting:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for goroutines to finish")
		}
	}
	// Sinkless testfeeds have some weird shutdown behaviours, so exclude them for now.
	cdcTest(t, testFn, feedTestRestrictSinks("kafka", "pubsub", "webhook"))
}

func TestChangefeedEnrichedAvro(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cases := []struct {
		name               string
		enrichedProperties []string
		withSource         bool
	}{
		{name: "none", enrichedProperties: []string{""}},
		{name: "with source", enrichedProperties: []string{"source"}, withSource: true},
		// no change in output from the first two -- the schema is part of the avro format
		{name: "with schema", enrichedProperties: []string{"schema"}},
		{name: "with schema and source", enrichedProperties: []string{"schema", "source"}, withSource: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
				sqlDB := sqlutils.MakeSQLRunner(s.DB)

				sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
				sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog')`)
				enrichedPropsStr := ""
				if len(tc.enrichedProperties) > 0 {
					enrichedPropsStr = fmt.Sprintf(", enriched_properties='%s'", strings.Join(tc.enrichedProperties, ","))
				}
				foo := feed(t, f, fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH envelope=enriched, format=avro, confluent_schema_registry='localhost:90909' %s`, enrichedPropsStr))
				defer closeFeed(t, foo)

				assertionKey := `{"a":{"long":0}}`
				assertionAfter := `"after": {"foo": {"a": {"long": 0}, "b": {"string": "dog"}}}`

				sourceAssertion := func(actualSource map[string]any) {
					if tc.withSource {
						require.NotNil(t, actualSource)
					} else {
						require.Nil(t, actualSource)
					}
				}
				assertPayloadsEnriched(t, foo, []string{
					fmt.Sprintf(`foo: %s->{%s, "op": {"string": "c"}}`,
						assertionKey, assertionAfter),
				}, sourceAssertion)
			}
			cdcTest(t, testFn, feedTestForceSink("kafka"))
		})
	}
}

func TestChangefeedEnrichedWithDiff(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cases := []struct {
		name      string
		options   []string
		sinks     []string
		assertion func(topic string) []string
	}{
		{
			name: "json with diff", options: []string{"diff", "format=json"}, sinks: []string{"kafka", "pubsub", "sinkless", "webhook", "cloudstorage"},
			assertion: func(topic string) []string {
				return []string{
					fmt.Sprintf(`%s: {"a": 0}->{"after": {"a": 0, "b": "dog"}, "before": null, "op": "c"}`, topic),
					fmt.Sprintf(`%s: {"a": 0}->{"after": {"a": 0, "b": "cat"}, "before": {"a": 0, "b": "dog"}, "op": "u"}`, topic),
					fmt.Sprintf(`%s: {"a": 0}->{"after": null, "before": {"a": 0, "b": "cat"}, "op": "d"}`, topic),
				}
			},
		},
		{
			name: "json without diff", options: []string{"format=json"}, sinks: []string{"kafka", "pubsub", "sinkless", "webhook"},
			assertion: func(topic string) []string {
				return []string{
					fmt.Sprintf(`%s: {"a": 0}->{"after": {"a": 0, "b": "dog"}, "op": "c"}`, topic),
					fmt.Sprintf(`%s: {"a": 0}->{"after": {"a": 0, "b": "cat"}, "op": "u"}`, topic),
					fmt.Sprintf(`%s: {"a": 0}->{"after": null, "op": "d"}`, topic),
				}
			},
		},
		{
			name: "avro with diff", options: []string{"diff", "format=avro"}, sinks: []string{"kafka"},
			assertion: func(topic string) []string {
				return []string{
					fmt.Sprintf(`%s: {"a":{"long":0}}->{"after": {"foo": {"a": {"long": 0}, "b": {"string": "dog"}}}, "before": null, "op": {"string": "c"}}`, topic),
					fmt.Sprintf(`%s: {"a":{"long":0}}->{"after": {"foo": {"a": {"long": 0}, "b": {"string": "cat"}}}, "before": {"foo_before": {"a": {"long": 0}, "b": {"string": "dog"}}}, "op": {"string": "u"}}`, topic),
					fmt.Sprintf(`%s: {"a":{"long":0}}->{"after": null, "before": {"foo_before": {"a": {"long": 0}, "b": {"string": "cat"}}}, "op": {"string": "d"}}`, topic),
				}
			},
		},
		{
			name: "avro without diff", options: []string{"format=avro"}, sinks: []string{"kafka"},
			assertion: func(topic string) []string {
				return []string{
					fmt.Sprintf(`%s: {"a":{"long":0}}->{"after": {"foo": {"a": {"long": 0}, "b": {"string": "dog"}}}, "op": {"string": "c"}}`, topic),
					fmt.Sprintf(`%s: {"a":{"long":0}}->{"after": {"foo": {"a": {"long": 0}, "b": {"string": "cat"}}}, "op": {"string": "u"}}`, topic),
					fmt.Sprintf(`%s: {"a":{"long":0}}->{"after": null, "op": {"string": "d"}}`, topic),
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
				sqlDB := sqlutils.MakeSQLRunner(s.DB)

				sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
				sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog')`)

				foo := feed(t, f, fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH envelope=enriched, %s`, strings.Join(tc.options, ", ")))
				defer closeFeed(t, foo)

				sqlDB.Exec(t, `UPDATE foo SET b = 'cat'`)
				sqlDB.Exec(t, `DELETE FROM foo WHERE b = 'cat'`)

				// TODO(#139660): the webhook sink forces topic_in_value, but
				// this is not supported by the enriched envelope type. We should adapt
				// the test framework to account for this.
				topic := "foo"
				if _, ok := foo.(*webhookFeed); ok {
					topic = ""
				}

				assertPayloadsEnriched(t, foo, tc.assertion(topic), nil)
			}
			for _, sink := range tc.sinks {
				cdcTest(t, testFn, feedTestForceSink(sink))
			}
		})
	}
}
func TestChangefeedEnrichedSourceWithDataAvro(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testutils.RunTrueAndFalse(t, "ts_{ns,hlc}", func(t *testing.T, withUpdated bool) {
		testutils.RunTrueAndFalse(t, "mvcc_ts", func(t *testing.T, withMVCCTS bool) {
			clusterName := "clusterName123"
			dbVersion := "v999.0.0"
			defer build.TestingOverrideVersion(dbVersion)()
			mkTestFn := func(sink string) func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
				return func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
					clusterID := s.Server.ExecutorConfig().(sql.ExecutorConfig).NodeInfo.LogicalClusterID().String()

					sqlDB := sqlutils.MakeSQLRunner(s.DB)

					sqlDB.Exec(t, `CREATE TABLE foo (i INT PRIMARY KEY)`)
					sqlDB.Exec(t, `INSERT INTO foo values (0)`)

					var tableID int
					sqlDB.QueryRow(t, `SELECT table_id FROM crdb_internal.tables WHERE name = 'foo' AND database_name = 'd'`).Scan(&tableID)

					stmt := `CREATE CHANGEFEED FOR foo WITH envelope=enriched, enriched_properties='source', format=avro`
					if withMVCCTS {
						stmt += ", mvcc_timestamp"
					}
					if withUpdated {
						stmt += ", updated"
					}
					testFeed := feed(t, f, stmt)
					defer closeFeed(t, testFeed)

					var jobID int64
					var nodeName string
					var sourceAssertion func(actualSource map[string]any)
					if ef, ok := testFeed.(cdctest.EnterpriseTestFeed); ok {
						jobID = int64(ef.JobID())
					}
					sqlDB.QueryRow(t, `SELECT value FROM crdb_internal.node_runtime_info where component = 'DB' and field = 'Host'`).Scan(&nodeName)

					sourceAssertion = func(actualSource map[string]any) {
						var nodeID any
						actualSourceValue := actualSource["source"].(map[string]any)
						nodeID = actualSourceValue["node_id"].(map[string]any)["string"]

						require.NotNil(t, nodeID)

						sourceNodeLocality := fmt.Sprintf(`region=%s`, testServerRegion)

						const dummyMvccTimestamp = "1234567890.0001"
						jobIDStr := strconv.FormatInt(jobID, 10)

						dummyUpdatedTSNS := 12345678900001000
						dummyUpdatedTSHLC :=
							hlc.Timestamp{WallTime: int64(dummyUpdatedTSNS), Logical: 0}.AsOfSystemTime()

						var assertion string
						assertionMap := map[string]any{
							"source": map[string]any{
								"changefeed_sink":        map[string]any{"string": sink},
								"cluster_id":             map[string]any{"string": clusterID},
								"cluster_name":           map[string]any{"string": clusterName},
								"crdb_internal_table_id": map[string]any{"int": tableID},
								"database_name":          map[string]any{"string": "d"},
								"db_version":             map[string]any{"string": dbVersion},
								"job_id":                 map[string]any{"string": jobIDStr},
								// Note that the field is still present in the avro schema, so it appears here as nil.
								"mvcc_timestamp":       nil,
								"node_id":              map[string]any{"string": nodeID},
								"origin":               map[string]any{"string": "cockroachdb"},
								"node_name":            map[string]any{"string": nodeName},
								"primary_keys":         map[string]any{"array": []any{"i"}},
								"schema_name":          map[string]any{"string": "public"},
								"source_node_locality": map[string]any{"string": sourceNodeLocality},
								"table_name":           map[string]any{"string": "foo"},
								"ts_ns":                nil,
								"ts_hlc":               nil,
							},
						}
						if withMVCCTS {
							mvccTsMap := actualSource["source"].(map[string]any)["mvcc_timestamp"].(map[string]any)
							assertReasonableMVCCTimestamp(t, mvccTsMap["string"].(string))

							mvccTsMap["string"] = dummyMvccTimestamp
							assertionMap["source"].(map[string]any)["mvcc_timestamp"] = map[string]any{"string": dummyMvccTimestamp}
						}
						if withUpdated {
							tsnsMap := actualSource["source"].(map[string]any)["ts_ns"].(map[string]any)
							tsns := tsnsMap["long"].(gojson.Number)
							tsnsInt, err := tsns.Int64()
							require.NoError(t, err)
							tsnsString := tsns.String()
							assertReasonableMVCCTimestamp(t, tsnsString)
							tsnsMap["long"] = dummyUpdatedTSNS
							assertionMap["source"].(map[string]any)["ts_ns"] = map[string]any{"long": dummyUpdatedTSNS}

							tshlcMap := actualSource["source"].(map[string]any)["ts_hlc"].(map[string]any)
							assertEqualTSNSHLCWalltime(t, tsnsInt, tshlcMap["string"].(string))

							tshlcMap["string"] = dummyUpdatedTSHLC
							assertionMap["source"].(map[string]any)["ts_hlc"] = map[string]any{"string": dummyUpdatedTSHLC}
						}
						assertion = toJSON(t, assertionMap)

						value, err := reformatJSON(actualSource)
						require.NoError(t, err)
						require.JSONEq(t, assertion, string(value))
					}

					assertPayloadsEnriched(t, testFeed, []string{`foo: {"i":{"long":0}}->{"after": {"foo": {"i": {"long": 0}}}, "op": {"string": "c"}}`}, sourceAssertion)
				}
			}
			testLocality := roachpb.Locality{
				Tiers: []roachpb.Tier{{
					Key:   "region",
					Value: testServerRegion,
				}}}
			cdcTest(t, mkTestFn("kafka"), feedTestForceSink("kafka"), feedTestUseClusterName(clusterName),
				feedTestUseLocality(testLocality))

		})
	})
}

func TestChangefeedEnrichedSourceWithDataJSON(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testutils.RunTrueAndFalse(t, "ts_{ns,hlc}", func(t *testing.T, withUpdated bool) {
		testutils.RunTrueAndFalse(t, "mvcc_ts", func(t *testing.T, withMVCCTS bool) {
			clusterName := "clusterName123"
			dbVersion := "v999.0.0"
			defer build.TestingOverrideVersion(dbVersion)()
			mkTestFn := func(sink string) func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
				return func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
					clusterID := s.Server.ExecutorConfig().(sql.ExecutorConfig).NodeInfo.LogicalClusterID().String()

					sqlDB := sqlutils.MakeSQLRunner(s.DB)

					sqlDB.Exec(t, `CREATE TABLE foo (i INT PRIMARY KEY)`)
					sqlDB.Exec(t, `INSERT INTO foo values (0)`)

					stmt := `CREATE CHANGEFEED FOR foo WITH envelope=enriched, enriched_properties='source', format=json`
					if withMVCCTS {
						stmt += ", mvcc_timestamp"
					}
					if withUpdated {
						stmt += ", updated"
					}
					testFeed := feed(t, f, stmt)
					defer closeFeed(t, testFeed)

					var jobID int64
					var nodeName string
					var tableID int

					var sourceAssertion func(actualSource map[string]any)
					if ef, ok := testFeed.(cdctest.EnterpriseTestFeed); ok {
						jobID = int64(ef.JobID())
					}
					sqlDB.QueryRow(t, `SELECT value FROM crdb_internal.node_runtime_info where component = 'DB' and field = 'Host'`).Scan(&nodeName)
					sqlDB.QueryRow(t, `SELECT table_id FROM crdb_internal.tables WHERE name = 'foo' AND database_name = 'd'`).Scan(&tableID)

					sourceAssertion = func(actualSource map[string]any) {
						nodeID := actualSource["node_id"]
						require.NotNil(t, nodeID)

						sourceNodeLocality := fmt.Sprintf(`region=%s`, testServerRegion)

						// There are some differences between how we specify sinks here and their actual names.
						if sink == "sinkless" {
							sink = sinkTypeSinklessBuffer.String()
						}

						const dummyMvccTimestamp = "1234567890.0001"
						jobIDStr := strconv.FormatInt(jobID, 10)

						dummyUpdatedTSNS := 12345678900001000
						dummyUpdatedTSHLC :=
							hlc.Timestamp{WallTime: int64(dummyUpdatedTSNS), Logical: 0}.AsOfSystemTime()

						var assertion string
						assertionMap := map[string]any{
							"cluster_id":             clusterID,
							"cluster_name":           clusterName,
							"crdb_internal_table_id": tableID,
							"db_version":             dbVersion,
							"job_id":                 jobIDStr,
							"node_id":                nodeID,
							"node_name":              nodeName,
							"origin":                 "cockroachdb",
							"changefeed_sink":        sink,
							"source_node_locality":   sourceNodeLocality,
							"database_name":          "d",
							"schema_name":            "public",
							"table_name":             "foo",
							"primary_keys":           []any{"i"},
						}
						if withMVCCTS {
							assertReasonableMVCCTimestamp(t, actualSource["mvcc_timestamp"].(string))
							actualSource["mvcc_timestamp"] = dummyMvccTimestamp
							assertionMap["mvcc_timestamp"] = dummyMvccTimestamp
						}
						if withUpdated {
							tsns := actualSource["ts_ns"].(gojson.Number)
							tsnsInt, err := tsns.Int64()
							require.NoError(t, err)
							assertReasonableMVCCTimestamp(t, tsns.String())
							actualSource["ts_ns"] = dummyUpdatedTSNS
							assertionMap["ts_ns"] = dummyUpdatedTSNS
							assertEqualTSNSHLCWalltime(t, tsnsInt, actualSource["ts_hlc"].(string))
							actualSource["ts_hlc"] = dummyUpdatedTSHLC
							assertionMap["ts_hlc"] = dummyUpdatedTSHLC
						}
						assertion = toJSON(t, assertionMap)

						value, err := reformatJSON(actualSource)
						require.NoError(t, err)
						require.JSONEq(t, assertion, string(value))
					}

					assertPayloadsEnriched(t, testFeed, []string{`foo: {"i": 0}->{"after": {"i": 0}, "op": "c"}`}, sourceAssertion)
				}
			}
			for _, sink := range []string{"kafka", "pubsub", "sinkless", "cloudstorage", "webhook"} {
				testLocality := roachpb.Locality{
					Tiers: []roachpb.Tier{{
						Key:   "region",
						Value: testServerRegion,
					}}}
				cdcTest(t, mkTestFn(sink), feedTestForceSink(sink), feedTestUseClusterName(clusterName),
					feedTestUseLocality(testLocality))
			}
		})
	})
}

func TestChangefeedEnrichedSourceSchemaInfo(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cases := []struct {
		name                          string
		format                        string
		expectedRow                   string
		expectedRowsAfterSchemaChange []string
	}{
		{
			name:        "json",
			format:      "json",
			expectedRow: `foo: {"a": 1, "b": "key1"}->{"after": {"a": 1, "b": "key1", "c": 100}, "op": "c"}`,
			expectedRowsAfterSchemaChange: []string{
				`foo: {"a": 1, "b": "key1"}->{"after": {"a": 1, "b": "key1", "c": 100, "d": "new_col"}, "op": "u"}`,
				`foo: {"a": 2, "b": "key2"}->{"after": {"a": 2, "b": "key2", "c": 200, "d": "new_value"}, "op": "c"}`,
			},
		},
		{
			name:        "avro",
			format:      "avro",
			expectedRow: `foo: {"a":{"long":1},"b":{"string":"key1"}}->{"after": {"foo": {"a": {"long": 1}, "b": {"string": "key1"}, "c": {"long": 100}}}, "op": {"string": "c"}}`,
			expectedRowsAfterSchemaChange: []string{
				`foo: {"a":{"long":1},"b":{"string":"key1"}}->{"after": {"foo": {"a": {"long": 1}, "b": {"string": "key1"}, "c": {"long": 100}, "d": {"string": "new_col"}}}, "op": {"string": "u"}}`,
				`foo: {"a":{"long":2},"b":{"string":"key2"}}->{"after": {"foo": {"a": {"long": 2}, "b": {"string": "key2"}, "c": {"long": 200}, "d": {"string": "new_value"}}}, "op": {"string": "c"}}`,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {

				var originalTableID int

				sqlDB := sqlutils.MakeSQLRunner(s.DB)

				sqlDB.Exec(t, `CREATE TABLE foo (a INT, b STRING, c INT, PRIMARY KEY (a, b))`)
				sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'key1', 100)`)

				stmt := fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH envelope=enriched, enriched_properties='source', format=%s`, testCase.format)
				foo := feed(t, f, stmt)
				defer closeFeed(t, foo)

				sourceAssertion := func(actualSource map[string]any) {
					if testCase.format == "avro" {
						actualSourceValue := actualSource["source"].(map[string]any)
						require.Equal(t, map[string]any{"string": "foo"}, actualSourceValue["table_name"])
						require.Equal(t, map[string]any{"string": "public"}, actualSourceValue["schema_name"])
						require.Equal(t, map[string]any{"string": "d"}, actualSourceValue["database_name"])
						require.Equal(t, map[string]any{"array": []any{"a", "b"}}, actualSourceValue["primary_keys"])

						num := actualSourceValue["crdb_internal_table_id"].(map[string]any)["int"].(gojson.Number)
						idInt, err := num.Int64()
						require.NoError(t, err)
						originalTableID = int(idInt)
					} else {
						require.Equal(t, "foo", actualSource["table_name"])
						require.Equal(t, "public", actualSource["schema_name"])
						require.Equal(t, "d", actualSource["database_name"])
						require.Equal(t, []any{"a", "b"}, actualSource["primary_keys"])

						num := actualSource["crdb_internal_table_id"].(gojson.Number)
						idInt, err := num.Int64()
						require.NoError(t, err)
						originalTableID = int(idInt)
					}
				}
				assertPayloadsEnriched(t, foo, []string{testCase.expectedRow}, sourceAssertion)

				sqlDB.Exec(t, `ALTER TABLE foo ADD COLUMN d STRING DEFAULT 'new_col'`)
				sqlDB.Exec(t, `INSERT INTO foo VALUES (2, 'key2', 200, 'new_value')`)

				sourceAssertionAfterSchemaChange := func(actualSource map[string]any) {
					if testCase.format == "avro" {
						actualSourceValue := actualSource["source"].(map[string]any)
						require.Equal(t, map[string]any{"string": "foo"}, actualSourceValue["table_name"])
						require.Equal(t, map[string]any{"string": "public"}, actualSourceValue["schema_name"])
						require.Equal(t, map[string]any{"string": "d"}, actualSourceValue["database_name"])
						require.Equal(t, map[string]any{"array": []any{"a", "b"}}, actualSourceValue["primary_keys"])

						num := actualSourceValue["crdb_internal_table_id"].(map[string]any)["int"].(gojson.Number)
						idInt, err := num.Int64()
						require.NoError(t, err)
						require.Equal(t, originalTableID, int(idInt))
					} else {
						require.Equal(t, "foo", actualSource["table_name"])
						require.Equal(t, "public", actualSource["schema_name"])
						require.Equal(t, "d", actualSource["database_name"])
						require.Equal(t, []any{"a", "b"}, actualSource["primary_keys"])

						num := actualSource["crdb_internal_table_id"].(gojson.Number)
						idInt, err := num.Int64()
						require.NoError(t, err)
						require.Equal(t, originalTableID, int(idInt))
					}
				}
				assertPayloadsEnriched(t, foo, testCase.expectedRowsAfterSchemaChange, sourceAssertionAfterSchemaChange)
			}

			cdcTest(t, testFn, feedTestRestrictSinks("kafka"))
		})
	}
}

func TestChangefeedEnrichedSourceSchemaInfoOnPrimaryKeyChange(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cases := []struct {
		name             string
		format           string
		expectedRow      string
		expectedRowAfter string
	}{
		{
			name:             "json",
			format:           "json",
			expectedRow:      `foo: {"a": 1}->{"after": {"a": 1, "b": "initial"}, "op": "c"}`,
			expectedRowAfter: `foo: {"b": "new_key"}->{"after": {"a": 2, "b": "new_key"}, "op": "c"}`,
		},
		{
			name:             "avro",
			format:           "avro",
			expectedRow:      `foo: {"a":{"long":1}}->{"after": {"foo": {"a": {"long": 1}, "b": {"string": "initial"}}}, "op": {"string": "c"}}`,
			expectedRowAfter: `foo: {"b":{"string":"new_key"}}->{"after": {"foo": {"a": {"long": 2}, "b": {"string": "new_key"}}}, "op": {"string": "c"}}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {

				var originalTableID int

				sqlDB := sqlutils.MakeSQLRunner(s.DB)

				sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
				sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'initial')`)

				stmt := fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH envelope=enriched, enriched_properties='source', format=%s`, testCase.format)
				foo := feed(t, f, stmt)
				defer closeFeed(t, foo)

				sourceAssertion := func(actualSource map[string]any) {
					if testCase.format == "avro" {
						actualSourceValue := actualSource["source"].(map[string]any)
						require.Equal(t, map[string]any{"string": "foo"}, actualSourceValue["table_name"])
						require.Equal(t, map[string]any{"string": "public"}, actualSourceValue["schema_name"])
						require.Equal(t, map[string]any{"string": "d"}, actualSourceValue["database_name"])
						require.Equal(t, map[string]any{"array": []any{"a"}}, actualSourceValue["primary_keys"])

						num := actualSourceValue["crdb_internal_table_id"].(map[string]any)["int"].(gojson.Number)
						idInt, err := num.Int64()
						require.NoError(t, err)
						originalTableID = int(idInt)
					} else {
						require.Equal(t, "foo", actualSource["table_name"])
						require.Equal(t, "public", actualSource["schema_name"])
						require.Equal(t, "d", actualSource["database_name"])
						require.Equal(t, []any{"a"}, actualSource["primary_keys"])

						num := actualSource["crdb_internal_table_id"].(gojson.Number)
						idInt, err := num.Int64()
						require.NoError(t, err)
						originalTableID = int(idInt)
					}
				}
				if testCase.format == "json" {
					assertPayloadsEnriched(t, foo, []string{testCase.expectedRow}, sourceAssertion)
				} else {
					assertPayloadsEnriched(t, foo, []string{testCase.expectedRow}, sourceAssertion)
				}

				sqlDB.Exec(t, `ALTER TABLE foo ALTER COLUMN b SET NOT NULL`)
				sqlDB.Exec(t, `ALTER TABLE foo ALTER PRIMARY KEY USING COLUMNS (b)`)
				sqlDB.Exec(t, `INSERT INTO foo VALUES (2, 'new_key')`)

				sourceAssertionAfterPKChange := func(actualSource map[string]any) {
					if testCase.format == "avro" {
						actualSourceValue := actualSource["source"].(map[string]any)
						require.Equal(t, map[string]any{"string": "foo"}, actualSourceValue["table_name"])
						require.Equal(t, map[string]any{"string": "public"}, actualSourceValue["schema_name"])
						require.Equal(t, map[string]any{"string": "d"}, actualSourceValue["database_name"])
						require.Equal(t, map[string]any{"array": []any{"b"}}, actualSourceValue["primary_keys"])

						num := actualSourceValue["crdb_internal_table_id"].(map[string]any)["int"].(gojson.Number)
						idInt, err := num.Int64()
						require.NoError(t, err)
						require.Equal(t, originalTableID, int(idInt))
					} else {
						require.Equal(t, "foo", actualSource["table_name"])
						require.Equal(t, "public", actualSource["schema_name"])
						require.Equal(t, "d", actualSource["database_name"])
						require.Equal(t, []any{"b"}, actualSource["primary_keys"])

						num := actualSource["crdb_internal_table_id"].(gojson.Number)
						idInt, err := num.Int64()
						require.NoError(t, err)
						require.Equal(t, originalTableID, int(idInt))
					}
				}
				assertPayloadsEnriched(t, foo, []string{testCase.expectedRowAfter}, sourceAssertionAfterPKChange)
			}

			cdcTest(t, testFn, feedTestForceSink("kafka"))
		})
	}
}

func TestChangefeedEnrichedTableIDStableOnRename(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (i INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH envelope=enriched, enriched_properties='source'`)
		defer closeFeed(t, foo)

		var originalTableID int

		// Capture first row (before rename)
		sourceAssertionBefore := func(actualSource map[string]any) {
			num := actualSource["crdb_internal_table_id"].(gojson.Number)
			id, err := num.Int64()
			require.NoError(t, err)
			originalTableID = int(id)
		}
		assertPayloadsEnriched(t, foo, []string{`foo: {"i": 1}->{"after": {"i": 1}, "op": "c"}`}, sourceAssertionBefore)

		// Rename table
		sqlDB.Exec(t, `ALTER TABLE foo RENAME TO bar`)
		sqlDB.Exec(t, `INSERT INTO bar VALUES (2)`)

		// Capture second row (after rename)
		sourceAssertionAfter := func(actualSource map[string]any) {
			num := actualSource["crdb_internal_table_id"].(gojson.Number)
			id, err := num.Int64()
			require.NoError(t, err)
			require.Equal(t, originalTableID, int(id))
		}
		assertPayloadsEnriched(t, foo, []string{`foo: {"i": 2}->{"after": {"i": 2}, "op": "c"}`}, sourceAssertionAfter)
	}

	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestChangefeedExpressionUsesSerializedSessionData(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.ExecMultiple(t,
			// Create target table in a different database.
			// Session data should be serialized to point to the
			// correct database.
			`CREATE DATABASE session`,
			`USE session`,
			`CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`,
			`INSERT INTO foo values (0, 'hello')`,
			`INSERT INTO foo values (1, 'howdy')`,
		)

		// Trigram similarity threshold should be 30%; so that "howdy" matches,
		// but hello doesn't.  This threshold should be serialized
		// in the changefeed jobs record, and correctly propagated to the aggregators.
		foo := feed(t, f, `CREATE CHANGEFEED WITH schema_change_policy=stop `+
			`AS SELECT * FROM foo WHERE b % 'how'`)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{`foo: [1]->{"a": 1, "b": "howdy"}`})
	}
	cdcTest(t, testFn, feedTestForceSink("kafka"), feedTestUseRootUserConnection)
}

func TestChangefeedBareJSON(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog')`)
		foo := feed(t, f, `CREATE CHANGEFEED WITH schema_change_policy=stop AS SELECT * FROM foo`)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{`foo: [0]->{"a": 0, "b": "dog"}`})
	}
	cdcTest(t, testFn, feedTestForceSink("kafka"))
	cdcTest(t, testFn, feedTestForceSink("enterprise"))
	cdcTest(t, testFn, feedTestForceSink("pubsub"))
	cdcTest(t, testFn, feedTestForceSink("sinkless"))
	cdcTest(t, testFn, feedTestForceSink("webhook"))
	cdcTest(t, testFn, feedTestForceSink("cloudstorage"))
}

func TestChangefeedExternalConnectionSchemaRegistry(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo values (0, 'dog')`)

		schemaReg := cdctest.StartTestSchemaRegistry()
		defer schemaReg.Close()

		name := fmt.Sprintf("schemareg%d", rand.Uint64())

		sqlDB.Exec(t, fmt.Sprintf(`CREATE EXTERNAL CONNECTION "%s" AS '%s'`, name, schemaReg.URL()))

		sql := fmt.Sprintf("CREATE CHANGEFEED WITH format=avro, confluent_schema_registry='external://%s' AS SELECT * FROM foo", name)

		foo := feed(t, f, sql)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{`foo: {"a":{"long":0}}->{"record":{"foo":{"a":{"long":0},"b":{"string":"dog"}}}}`})
	}
	// Test helpers for avro assume Kafka
	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestChangefeedAvroNotice(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, stop := makeServer(t)
	defer stop()
	schemaReg := cdctest.StartTestSchemaRegistry()
	defer schemaReg.Close()

	sqlDB := sqlutils.MakeSQLRunner(s.DB)
	sqlDB.Exec(t, "CREATE table foo (i int)")
	sqlDB.Exec(t, `INSERT INTO foo VALUES (0)`)

	sql := fmt.Sprintf("CREATE CHANGEFEED FOR d.foo INTO 'null://' WITH format=experimental_avro, confluent_schema_registry='%s'", schemaReg.URL())
	expectNotice(t, s.Server, sql, `avro is no longer experimental, use format=avro`)
}

func TestChangefeedResolvedNotice(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cluster, _, cleanup := startTestCluster(t)
	defer cleanup()
	s := cluster.Server(1)

	// Set the default min_checkpoint_frequency to 30 seconds for this test
	restoreDefault := changefeedbase.TestingSetDefaultMinCheckpointFrequency(30 * time.Second)
	defer restoreDefault()

	pgURL, cleanup := pgurlutils.PGUrl(t, s.SQLAddr(), t.Name(), url.User(username.RootUser))
	defer cleanup()
	pgBase, err := pq.NewConnector(pgURL.String())
	if err != nil {
		t.Fatal(err)
	}
	var actual string
	connector := pq.ConnectorWithNoticeHandler(pgBase, func(n *pq.Error) {
		actual = n.Message
	})

	dbWithHandler := gosql.OpenDB(connector)
	defer dbWithHandler.Close()

	sqlDB := sqlutils.MakeSQLRunner(dbWithHandler)

	sqlDB.Exec(t, `CREATE TABLE ☃ (i INT PRIMARY KEY)`)
	sqlDB.Exec(t, `INSERT INTO ☃ VALUES (0)`)

	t.Run("resolved<min_checkpoint_frequency", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/' WITH resolved='5s', min_checkpoint_frequency='10s'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `resolved (5s) messages will not be emitted more frequently than the configured min_checkpoint_frequency (10s), but may be emitted less frequently`, actual)
	})
	t.Run("resolved<min_checkpoint_frequency default", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/' WITH resolved='5s'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `resolved (5s) messages will not be emitted more frequently than the default min_checkpoint_frequency (30s), but may be emitted less frequently`, actual)
	})
	t.Run("resolved=min_checkpoint_frequency", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/' WITH resolved='5s', min_checkpoint_frequency='5s'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `changefeed will emit to topic _u2603_`, actual)
	})
	t.Run("resolved>min_checkpoint_frequency", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/' WITH resolved='10s', min_checkpoint_frequency='5s'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `changefeed will emit to topic _u2603_`, actual)
	})
	t.Run("resolved default", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/' WITH resolved, min_checkpoint_frequency='10s'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `resolved (0s by default) messages will not be emitted more frequently than the configured min_checkpoint_frequency (10s), but may be emitted less frequently`, actual)
	})
}

func TestChangefeedLowFrequencyNotices(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cluster, _, cleanup := startTestCluster(t)
	defer cleanup()
	s := cluster.Server(1)

	pgURL, cleanup := pgurlutils.PGUrl(t, s.SQLAddr(), t.Name(), url.User(username.RootUser))
	defer cleanup()
	pgBase, err := pq.NewConnector(pgURL.String())
	if err != nil {
		t.Fatal(err)
	}
	var actual string
	connector := pq.ConnectorWithNoticeHandler(pgBase, func(n *pq.Error) {
		actual = n.Message
	})

	dbWithHandler := gosql.OpenDB(connector)
	defer dbWithHandler.Close()

	sqlDB := sqlutils.MakeSQLRunner(dbWithHandler)

	sqlDB.Exec(t, `CREATE TABLE ☃ (i INT PRIMARY KEY)`)
	sqlDB.Exec(t, `INSERT INTO ☃ VALUES (0)`)

	t.Run("no options specified", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `changefeed will emit to topic _u2603_`, actual)
	})
	t.Run("normal resolved and min_checkpoint_frequency", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/' WITH resolved='10s', min_checkpoint_frequency='10s'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `changefeed will emit to topic _u2603_`, actual)
	})
	t.Run("low resolved timestamp", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/' WITH resolved='200ms'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `the 'resolved' timestamp interval (200ms) is very low; consider increasing it to at least 500ms`, actual)
	})
	t.Run("low min_checkpoint_frequency timestamp", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/' WITH min_checkpoint_frequency='200ms'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `the 'min_checkpoint_frequency' timestamp interval (200ms) is very low; consider increasing it to at least 500ms`, actual)
	})
}

func TestChangefeedOutputTopics(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cluster, _, cleanup := startTestCluster(t)
	defer cleanup()
	s := cluster.Server(1)

	pgURL, cleanup := pgurlutils.PGUrl(t, s.SQLAddr(), t.Name(), url.User(username.RootUser))
	defer cleanup()
	pgBase, err := pq.NewConnector(pgURL.String())
	if err != nil {
		t.Fatal(err)
	}
	var actual string
	connector := pq.ConnectorWithNoticeHandler(pgBase, func(n *pq.Error) {
		actual = n.Message
	})

	dbWithHandler := gosql.OpenDB(connector)
	defer dbWithHandler.Close()

	sqlDB := sqlutils.MakeSQLRunner(dbWithHandler)

	sqlDB.Exec(t, `CREATE TABLE ☃ (i INT PRIMARY KEY)`)
	sqlDB.Exec(t, `INSERT INTO ☃ VALUES (0)`)

	t.Run("kafka", func(t *testing.T) {
		actual = "(no notice)"
		f := makeKafkaFeedFactory(t, s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'kafka://does.not.matter/'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `changefeed will emit to topic _u2603_`, actual)
	})

	t.Run("pubsub v2", func(t *testing.T) {
		actual = "(no notice)"
		f := makePubsubFeedFactory(s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'gcpubsub://does.not.matter/'`)
		defer closeFeed(t, testFeed)
		// Pubsub doesn't sanitize the topic name.
		require.Equal(t, `changefeed will emit to topic ☃`, actual)
	})

	t.Run("webhooks does not emit anything", func(t *testing.T) {
		actual = "(no notice)"
		f := makePubsubFeedFactory(s, dbWithHandler)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR ☃ INTO 'webhook-https://does.not.matter/'`)
		defer closeFeed(t, testFeed)
		require.Equal(t, `(no notice)`, actual)
	})
}

// requireTerminalErrorSoon polls for the test feed for an error and asserts
// that the error matches the provided regex. This can either be a terminal
// error or an error encountered while parsing messages and doing testfeed
// things.
func requireTerminalErrorSoon(
	ctx context.Context, t *testing.T, f cdctest.TestFeed, errRegex *regexp.Regexp,
) {
	err := timeutil.RunWithTimeout(ctx, "requireTerminalErrorSoon", 30*time.Second, func(ctx context.Context) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				m, err := f.Next()
				if err != nil {
					assert.Regexp(t, errRegex, err)
					return nil
				}
				log.Infof(ctx, "waiting for error; skipping test feed message: %s", m.String())
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestChangefeedFailOnTableOffline(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	dataSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if _, err := w.Write([]byte("42,42\n")); err != nil {
				t.Logf("failed to write: %s", err.Error())
			}
		}
	}))
	defer dataSrv.Close()

	cdcTestNamedWithSystem(t, "import fails changefeed", func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sysDB := sqlutils.MakeSQLRunner(s.SystemServer.SQLConn(t))
		sysDB.Exec(t, "SET CLUSTER SETTING kv.closed_timestamp.target_duration = '50ms'")
		sqlDB.Exec(t, `CREATE TABLE for_import (a INT PRIMARY KEY, b INT) WITH (schema_locked=false)`)
		defer sqlDB.Exec(t, `DROP TABLE for_import`)
		sqlDB.Exec(t, `INSERT INTO for_import VALUES (0, NULL)`)
		forImport := feed(t, f, `CREATE CHANGEFEED FOR for_import `)
		defer closeFeed(t, forImport)
		assertPayloads(t, forImport, []string{
			`for_import: [0]->{"after": {"a": 0, "b": null}}`,
		})
		sqlDB.Exec(t, `IMPORT INTO for_import CSV DATA ($1)`, dataSrv.URL)
		requireTerminalErrorSoon(context.Background(), t, forImport,
			regexp.MustCompile(`CHANGEFEED cannot target offline table: for_import \(offline reason: "importing"\)`))
	}, withAllowChangefeedErr("expects terminal error"))

	cdcTestNamedWithSystem(t, "reverted import fails changefeed with earlier cursor", func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sysSQLDB := sqlutils.MakeSQLRunner(s.SystemDB)
		sysSQLDB.Exec(t, "SET CLUSTER SETTING kv.bulk_io_write.small_write_size = '1'")

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE for_import (a INT PRIMARY KEY, b INT)`)

		var start string
		sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp()`).Scan(&start)
		sqlDB.Exec(t, "INSERT INTO for_import VALUES (0, 10);")

		// Start an import job which will immediately pause after ingestion
		sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = 'import.after_ingest';")
		go func() {
			sqlDB.ExpectErrWithRetry(t, `pause point`, `IMPORT INTO for_import CSV DATA ($1);`, `result is ambiguous`, dataSrv.URL)
		}()
		sqlDB.CheckQueryResultsRetry(
			t,
			fmt.Sprintf(`SELECT count(*) FROM [SHOW JOBS] WHERE job_type='IMPORT' AND status='%s'`, jobs.StatePaused),
			[][]string{{"1"}},
		)

		// Cancel to trigger a revert and verify revert completion
		var jobID string
		sqlDB.QueryRow(t, `SELECT job_id FROM [SHOW JOBS] where job_type='IMPORT'`).Scan(&jobID)
		sqlDB.Exec(t, `CANCEL JOB $1`, jobID)
		sqlDB.CheckQueryResultsRetry(
			t,
			fmt.Sprintf(`SELECT count(*) FROM [SHOW JOBS] WHERE job_type='IMPORT' AND status='%s'`, jobs.StateCanceled),
			[][]string{{"1"}},
		)
		sqlDB.CheckQueryResultsRetry(t, "SELECT count(*) FROM for_import", [][]string{{"1"}})

		// Changefeed should fail regardless
		forImport := feed(t, f, `CREATE CHANGEFEED FOR for_import WITH cursor=$1`, start)
		defer closeFeed(t, forImport)
		requireTerminalErrorSoon(context.Background(), t, forImport,
			regexp.MustCompile(`CHANGEFEED cannot target offline table: for_import \(offline reason: "importing"\)`))
	}, withAllowChangefeedErr("expects terminal error"))
}

func TestChangefeedRestartMultiNode(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cluster, db, cleanup := startTestCluster(t)
	defer cleanup()

	sqlDB := sqlutils.MakeSQLRunner(db)
	sqlDB.Exec(t, `CREATE TABLE test_tab (a INT PRIMARY KEY, b INT UNIQUE NOT NULL) WITH (schema_locked = false)`)
	sqlDB.Exec(t, `INSERT INTO test_tab VALUES (0, 0)`)

	row := sqlDB.QueryRow(t, `SELECT range_id, lease_holder FROM [SHOW RANGES FROM TABLE test_tab WITH DETAILS] LIMIT 1`)
	var rangeID, leaseHolder int
	row.Scan(&rangeID, &leaseHolder)

	// Start the changefeed on a node other than the leaseholder
	// so that it is likely that the changeAggregator and
	// changeFrontier are on different nodes.
	feedServerID := ((leaseHolder - 1) + 1) % 3
	t.Logf("Range %d is on lease holder %d, running rangefeed on server %d (store id: %d)", rangeID, leaseHolder, feedServerID, cluster.Server(feedServerID).GetFirstStoreID())
	db = cluster.ServerConn(feedServerID)
	sqlDB = sqlutils.MakeSQLRunner(db)

	f := makeKafkaFeedFactory(t, cluster, db)
	feed := feed(t, f, "CREATE CHANGEFEED FOR test_tab WITH updated")
	defer closeFeed(t, feed)
	assertPayloadsStripTs(t, feed, []string{
		`test_tab: [0]->{"after": {"a": 0, "b": 0}}`,
	})

	waitForSchemaChange(t, sqlDB, `ALTER TABLE test_tab ALTER PRIMARY KEY USING COLUMNS (b)`)
	sqlDB.Exec(t, `INSERT INTO test_tab VALUES (1, 11)`)
	// No backfill, but we should see the newly insert value
	assertPayloadsStripTs(t, feed, []string{
		`test_tab: [11]->{"after": {"a": 1, "b": 11}}`,
	})

	waitForSchemaChange(t, sqlDB, `ALTER TABLE test_tab SET LOCALITY REGIONAL BY ROW`)
	// schema-changer backfill for the ADD COLUMN
	assertPayloadsStripTs(t, feed, []string{
		`test_tab: [0]->{"after": {"a": 0, "b": 0}}`,
		`test_tab: [11]->{"after": {"a": 1, "b": 11}}`,
	})
	// changefeed backfill for the ADD COLUMN
	assertPayloadsStripTs(t, feed, []string{
		`test_tab: ["us-east1", 0]->{"after": {"a": 0, "b": 0, "crdb_region": "us-east1"}}`,
		`test_tab: ["us-east1", 11]->{"after": {"a": 1, "b": 11, "crdb_region": "us-east1"}}`,
	})

	sqlDB.Exec(t, `INSERT INTO test_tab VALUES (2, 22)`)
	// Newly inserted data works
	assertPayloadsStripTs(t, feed, []string{
		`test_tab: ["us-east1", 22]->{"after": {"a": 2, "b": 22, "crdb_region": "us-east1"}}`,
	})
}

func TestChangefeedStopPolicyMultiNode(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t)

	cluster, db, cleanup := startTestCluster(t)
	defer cleanup()

	sqlDB := sqlutils.MakeSQLRunner(db)
	sqlDB.Exec(t, `CREATE TABLE test_tab (a INT PRIMARY KEY)`)
	sqlDB.Exec(t, `INSERT INTO test_tab VALUES (0)`)

	row := sqlDB.QueryRow(t, `SELECT range_id, lease_holder FROM [SHOW RANGES FROM TABLE test_tab WITH DETAILS] LIMIT 1`)
	var rangeID, leaseHolder int
	row.Scan(&rangeID, &leaseHolder)

	// Start the changefeed on a node other than the leaseholder
	// so that it is likely that the changeAggregator and
	// changeFrontier are on different nodes.
	feedServerID := ((leaseHolder - 1) + 1) % 3
	t.Logf("Range %d is on lease holder %d, running rangefeed on server %d (store id: %d)", rangeID, leaseHolder, feedServerID, cluster.Server(feedServerID).GetFirstStoreID())
	db = cluster.ServerConn(feedServerID)
	sqlDB = sqlutils.MakeSQLRunner(db)

	f := makeKafkaFeedFactory(t, cluster, db)
	feed := feed(t, f, "CREATE CHANGEFEED FOR test_tab WITH schema_change_policy='stop'")
	defer closeFeed(t, feed)
	sqlDB.Exec(t, `INSERT INTO test_tab VALUES (1)`)
	assertPayloads(t, feed, []string{
		`test_tab: [0]->{"after": {"a": 0}}`,
		`test_tab: [1]->{"after": {"a": 1}}`,
	})
	sqlDB.Exec(t, `ALTER TABLE test_tab ADD COLUMN b INT NOT NULL DEFAULT 0`)

	waitForSchemaChangeError := func(t *testing.T, f cdctest.TestFeed) {
		t.Helper()
		for {
			if _, err := f.Next(); err != nil {
				require.Contains(t, err.Error(), "schema change occurred at")
				break
			}
		}
	}
	waitForSchemaChangeError(t, feed)
}

func TestChangefeedWorksOnRBRChange(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFnJSON := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sysDB := sqlutils.MakeSQLRunner(s.SystemServer.SQLConn(t))
		sysDB.Exec(t, "SET CLUSTER SETTING kv.closed_timestamp.target_duration = '50ms'")
		t.Run("regional by row change works", func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE rbr (a INT PRIMARY KEY, b INT) WITH (schema_locked=false)`)
			defer sqlDB.Exec(t, `DROP TABLE rbr`)
			sqlDB.Exec(t, `INSERT INTO rbr VALUES (0, NULL)`)
			rbr := feed(t, f, `CREATE CHANGEFEED FOR rbr`)
			defer closeFeed(t, rbr)
			sqlDB.Exec(t, `INSERT INTO rbr VALUES (1, 2)`)
			assertPayloads(t, rbr, []string{
				`rbr: [0]->{"after": {"a": 0, "b": null}}`,
				`rbr: [1]->{"after": {"a": 1, "b": 2}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE rbr SET LOCALITY REGIONAL BY ROW`)
			assertPayloads(t, rbr, []string{
				`rbr: ["us-east-1", 0]->{"after": {"a": 0, "b": null, "crdb_region": "us-east-1"}}`,
				`rbr: ["us-east-1", 1]->{"after": {"a": 1, "b": 2, "crdb_region": "us-east-1"}}`,
			})
		})
	}
	testFnAvro := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, "SET CLUSTER SETTING kv.closed_timestamp.target_duration = '50ms'")
		t.Run("regional by row change works", func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE rbr (a INT PRIMARY KEY, b INT) WITH (schema_locked=false)`)
			defer sqlDB.Exec(t, `DROP TABLE rbr`)
			sqlDB.Exec(t, `INSERT INTO rbr VALUES (0, NULL)`)
			rbr := feed(t, f, `CREATE CHANGEFEED FOR rbr WITH format=avro`)
			defer closeFeed(t, rbr)
			sqlDB.Exec(t, `INSERT INTO rbr VALUES (1, 2)`)
			assertPayloads(t, rbr, []string{
				`rbr: {"a":{"long":0}}->{"after":{"rbr":{"a":{"long":0},"b":null}}}`,
				`rbr: {"a":{"long":1}}->{"after":{"rbr":{"a":{"long":1},"b":{"long":2}}}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE rbr SET LOCALITY REGIONAL BY ROW`)
			assertPayloads(t, rbr, []string{
				`rbr: {"a":{"long":0},"crdb_region":{"string":"us-east-1"}}->{"after":{"rbr":{"a":{"long":0},"b":null,"crdb_region":{"string":"us-east-1"}}}}`,
				`rbr: {"a":{"long":1},"crdb_region":{"string":"us-east-1"}}->{"after":{"rbr":{"a":{"long":1},"b":{"long":2},"crdb_region":{"string":"us-east-1"}}}}`,
			})
		})
		t.Run("regional by row as change works", func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE rbr (a INT PRIMARY KEY, b INT, region crdb_internal_region NOT NULL DEFAULT 'us-east-1') WITH (schema_locked = false)`)
			defer sqlDB.Exec(t, `DROP TABLE rbr`)
			sqlDB.Exec(t, `INSERT INTO rbr VALUES (0, NULL)`)
			rbr := feed(t, f, `CREATE CHANGEFEED FOR rbr WITH format=avro`)
			defer closeFeed(t, rbr)
			sqlDB.Exec(t, `INSERT INTO rbr VALUES (1, 2)`)
			assertPayloads(t, rbr, []string{
				`rbr: {"a":{"long":0}}->{"after":{"rbr":{"a":{"long":0},"b":null,"region":{"string":"us-east-1"}}}}`,
				`rbr: {"a":{"long":1}}->{"after":{"rbr":{"a":{"long":1},"b":{"long":2},"region":{"string":"us-east-1"}}}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE rbr SET LOCALITY REGIONAL BY ROW AS region`)
			assertPayloads(t, rbr, []string{
				`rbr: {"a":{"long":0},"region":{"string":"us-east-1"}}->{"after":{"rbr":{"a":{"long":0},"b":null,"region":{"string":"us-east-1"}}}}`,
				`rbr: {"a":{"long":1},"region":{"string":"us-east-1"}}->{"after":{"rbr":{"a":{"long":1},"b":{"long":2},"region":{"string":"us-east-1"}}}}`,
			})
		})
	}

	withTestServerRegion := func(args *base.TestServerArgs) {
		args.Locality.Tiers = append(args.Locality.Tiers, roachpb.Tier{
			Key:   "region",
			Value: testServerRegion,
		})
	}

	// Tenants skipped because of:
	//
	// error executing 'ALTER DATABASE d PRIMARY REGION
	// "us-east-1"': pq: get_live_cluster_regions: unimplemented:
	// operation is unsupported inside virtual clusters
	//
	// TODO(knz): This seems incorrect; see issue #109418.
	opts := []feedTestOption{
		feedTestNoTenants,
		feedTestEnterpriseSinks,
		withArgsFn(withTestServerRegion),
	}
	cdcTestNamedWithSystem(t, "format=json", testFnJSON, opts...)
	cdcTestNamed(t, "format=avro", testFnAvro, append(opts, feedTestForceSink("kafka"))...)
}

func TestChangefeedRBRAvroAddRegion(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// We need a cluster here to make sure we have multiple active
	// regions that we can add to the database.
	cluster, db, cleanup := startTestCluster(t)
	defer cleanup()

	f := makeKafkaFeedFactory(t, cluster, db)
	sqlDB := sqlutils.MakeSQLRunner(db)
	sqlDB.Exec(t, `CREATE TABLE rbr (a INT PRIMARY KEY) WITH (schema_locked = false)`)
	waitForSchemaChange(t, sqlDB, `ALTER TABLE rbr SET LOCALITY REGIONAL BY ROW`)
	sqlDB.Exec(t, `INSERT INTO rbr VALUES (0)`)
	rbr := feed(t, f, `CREATE CHANGEFEED FOR rbr WITH format=avro`)
	defer closeFeed(t, rbr)
	assertPayloads(t, rbr, []string{
		`rbr: {"a":{"long":0},"crdb_region":{"string":"us-east1"}}->{"after":{"rbr":{"a":{"long":0},"crdb_region":{"string":"us-east1"}}}}`,
	})

	// We do not expect a backfill from the ADD REGION, but we do
	// expect the new rows with the added region to be encoded
	// correctly.
	sqlDB.Exec(t, `ALTER DATABASE d ADD REGION "us-east2"`)
	sqlDB.Exec(t, `INSERT INTO rbr (crdb_region, a) VALUES ('us-east2', 1)`)
	assertPayloads(t, rbr, []string{
		`rbr: {"a":{"long":1},"crdb_region":{"string":"us-east2"}}->{"after":{"rbr":{"a":{"long":1},"crdb_region":{"string":"us-east2"}}}}`,
	})

	// An update is seen as a DELETE and and INSERT
	sqlDB.Exec(t, `UPDATE rbr SET crdb_region = 'us-east2' WHERE a = 0`)
	assertPayloads(t, rbr, []string{
		`rbr: {"a":{"long":0},"crdb_region":{"string":"us-east1"}}->{"after":null}`,
		`rbr: {"a":{"long":0},"crdb_region":{"string":"us-east2"}}->{"after":{"rbr":{"a":{"long":0},"crdb_region":{"string":"us-east2"}}}}`,
	})
}

func TestChangefeedStopOnSchemaChange(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t)
	skip.UnderShort(t)

	schemaChangeTimestampRegexp := regexp.MustCompile(`schema change occurred at ([0-9]+\.[0-9]+)`)
	timestampStrFromError := func(t *testing.T, err error) string {
		require.Regexp(t, schemaChangeTimestampRegexp, err)
		m := schemaChangeTimestampRegexp.FindStringSubmatch(err.Error())
		return m[1]
	}
	waitForSchemaChangeErrorAndCloseFeed := func(t *testing.T, f cdctest.TestFeed) (tsStr string) {
		t.Helper()
		for {
			if ev, err := f.Next(); err != nil {
				log.Infof(context.Background(), "got event %v %v", ev, err)
				tsStr = timestampStrFromError(t, err)
				_ = f.Close()
				return tsStr
			}
		}
	}
	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sysDB := sqlutils.MakeSQLRunner(s.SystemServer.SQLConn(t))
		// Shorten the intervals so this test doesn't take so long. We need to wait
		// for timestamps to get resolved.
		sysDB.Exec(t, "SET CLUSTER SETTING changefeed.experimental_poll_interval = '200ms'")
		sysDB.Exec(t, "ALTER TENANT ALL SET CLUSTER SETTING changefeed.experimental_poll_interval = '200ms'")
		sysDB.Exec(t, "SET CLUSTER SETTING kv.closed_timestamp.target_duration = '50ms'")
		sysDB.Exec(t, "SET CLUSTER SETTING kv.closed_timestamp.side_transport_interval = '50ms'")
		sysDB.Exec(t, "SET CLUSTER SETTING kv.rangefeed.closed_timestamp_refresh_interval = '50ms'")

		t.Run("add column not null", func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_column_not_null (a INT PRIMARY KEY)`)
			defer sqlDB.Exec(t, `DROP TABLE add_column_not_null`)
			sqlDB.Exec(t, `INSERT INTO add_column_not_null VALUES (0)`)
			addColumnNotNull := feed(t, f, `CREATE CHANGEFEED FOR add_column_not_null `+
				`WITH schema_change_events='column_changes', schema_change_policy='stop'`)
			sqlDB.Exec(t, `INSERT INTO add_column_not_null VALUES (1)`)
			assertPayloads(t, addColumnNotNull, []string{
				`add_column_not_null: [0]->{"after": {"a": 0}}`,
				`add_column_not_null: [1]->{"after": {"a": 1}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_column_not_null ADD COLUMN b INT NOT NULL DEFAULT 0`)
			sqlDB.Exec(t, "INSERT INTO add_column_not_null VALUES (2, 1)")
			tsStr := waitForSchemaChangeErrorAndCloseFeed(t, addColumnNotNull)
			addColumnNotNull = feed(t, f, `CREATE CHANGEFEED FOR add_column_not_null `+
				`WITH schema_change_events='column_changes', schema_change_policy='stop', cursor = '`+tsStr+`'`)
			defer closeFeed(t, addColumnNotNull)
			assertPayloads(t, addColumnNotNull, []string{
				`add_column_not_null: [2]->{"after": {"a": 2, "b": 1}}`,
			})
		})
		t.Run("add column null", func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_column_null (a INT PRIMARY KEY)`)
			defer sqlDB.Exec(t, `DROP TABLE add_column_null`)
			sqlDB.Exec(t, `INSERT INTO add_column_null VALUES (0)`)
			addColumnNull := feed(t, f, `CREATE CHANGEFEED FOR add_column_null `+
				`WITH schema_change_events='column_changes', schema_change_policy='stop'`)
			sqlDB.Exec(t, `INSERT INTO add_column_null VALUES (1)`)
			assertPayloads(t, addColumnNull, []string{
				`add_column_null: [0]->{"after": {"a": 0}}`,
				`add_column_null: [1]->{"after": {"a": 1}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_column_null ADD COLUMN b INT`)
			sqlDB.Exec(t, "INSERT INTO add_column_null VALUES (2, NULL)")
			tsStr := waitForSchemaChangeErrorAndCloseFeed(t, addColumnNull)
			addColumnNull = feed(t, f, `CREATE CHANGEFEED FOR add_column_null `+
				`WITH schema_change_events='column_changes', schema_change_policy='stop', cursor = '`+tsStr+`'`)
			defer closeFeed(t, addColumnNull)
			assertPayloads(t, addColumnNull, []string{
				`add_column_null: [2]->{"after": {"a": 2, "b": null}}`,
			})
		})
		t.Run(`add column computed`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_comp_col (a INT PRIMARY KEY)`)
			defer sqlDB.Exec(t, `DROP TABLE add_comp_col`)
			sqlDB.Exec(t, `INSERT INTO add_comp_col VALUES (0)`)
			addCompCol := feed(t, f, `CREATE CHANGEFEED FOR add_comp_col `+
				`WITH schema_change_events='column_changes', schema_change_policy='stop'`)
			sqlDB.Exec(t, `INSERT INTO add_comp_col VALUES (1)`)
			assertPayloads(t, addCompCol, []string{
				`add_comp_col: [0]->{"after": {"a": 0}}`,
				`add_comp_col: [1]->{"after": {"a": 1}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_comp_col ADD COLUMN b INT AS (a + 1) STORED`)
			sqlDB.Exec(t, "INSERT INTO add_comp_col VALUES (2)")
			tsStr := waitForSchemaChangeErrorAndCloseFeed(t, addCompCol)
			addCompCol = feed(t, f, `CREATE CHANGEFEED FOR add_comp_col `+
				`WITH schema_change_events='column_changes', schema_change_policy='stop', cursor = '`+tsStr+`'`)
			defer closeFeed(t, addCompCol)
			assertPayloads(t, addCompCol, []string{
				`add_comp_col: [2]->{"after": {"a": 2, "b": 3}}`,
			})
		})
		t.Run("drop column", func(t *testing.T) {
			// Sinkless feeds are not currently able to restart in the face of
			// any schema changes. Dropping a column in the declarative schema
			// changer means that an extra error will occur.
			if _, isSinkless := f.(*sinklessFeedFactory); isSinkless {
				return
			}
			sqlDB.Exec(t, `CREATE TABLE drop_column (a INT PRIMARY KEY, b INT)`)
			defer sqlDB.Exec(t, `DROP TABLE drop_column`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (0, NULL)`)
			dropColumn := feed(t, f, `CREATE CHANGEFEED FOR drop_column `+
				`WITH schema_change_events='column_changes', schema_change_policy='stop'`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (1, 2)`)
			assertPayloads(t, dropColumn, []string{
				`drop_column: [0]->{"after": {"a": 0, "b": null}}`,
				`drop_column: [1]->{"after": {"a": 1, "b": 2}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE drop_column DROP COLUMN b`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (2)`)
			tsStr := waitForSchemaChangeErrorAndCloseFeed(t, dropColumn)
			dropColumn = feed(t, f, `CREATE CHANGEFEED FOR drop_column `+
				`WITH schema_change_events='column_changes', schema_change_policy='stop', cursor = '`+tsStr+`'`)
			defer closeFeed(t, dropColumn)
			assertPayloads(t, dropColumn, []string{
				`drop_column: [2]->{"after": {"a": 2}}`,
			})
		})
		t.Run("add index", func(t *testing.T) {
			// This case does not exit
			sqlDB.Exec(t, `CREATE TABLE add_index (a INT PRIMARY KEY, b INT)`)
			defer sqlDB.Exec(t, `DROP TABLE add_index`)
			sqlDB.Exec(t, `INSERT INTO add_index VALUES (0, NULL)`)
			addIndex := feed(t, f, `CREATE CHANGEFEED FOR add_index `+
				`WITH schema_change_events='column_changes', schema_change_policy='stop'`)
			defer closeFeed(t, addIndex)
			sqlDB.Exec(t, `INSERT INTO add_index VALUES (1, 2)`)
			assertPayloads(t, addIndex, []string{
				`add_index: [0]->{"after": {"a": 0, "b": null}}`,
				`add_index: [1]->{"after": {"a": 1, "b": 2}}`,
			})
			sqlDB.Exec(t, `CREATE INDEX ON add_index (b)`)
			sqlDB.Exec(t, `INSERT INTO add_index VALUES (2, NULL)`)
			assertPayloads(t, addIndex, []string{
				`add_index: [2]->{"after": {"a": 2, "b": null}}`,
			})
		})
	}

	cdcTestWithSystem(t, testFn)
}

func TestChangefeedNoBackfill(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t)
	skip.UnderShort(t)
	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sysDB := sqlutils.MakeSQLRunner(s.SystemServer.SQLConn(t))

		usingLegacySchemaChanger := maybeDisableDeclarativeSchemaChangesForTest(t, sqlDB)

		// Shorten the intervals so this test doesn't take so long. We need to wait
		// for timestamps to get resolved.
		sysDB.Exec(t, "SET CLUSTER SETTING changefeed.experimental_poll_interval = '200ms'")
		sysDB.Exec(t, "ALTER TENANT ALL SET CLUSTER SETTING changefeed.experimental_poll_interval = '200ms'")
		sysDB.Exec(t, "SET CLUSTER SETTING kv.closed_timestamp.target_duration = '50ms'")
		sysDB.Exec(t, "SET CLUSTER SETTING kv.closed_timestamp.side_transport_interval = '10ms'")
		sysDB.Exec(t, "SET CLUSTER SETTING kv.rangefeed.closed_timestamp_refresh_interval = '10ms'")

		t.Run("add column not null", func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_column_not_null (a INT PRIMARY KEY)`)
			defer sqlDB.Exec(t, `DROP TABLE add_column_not_null`)
			sqlDB.Exec(t, `INSERT INTO add_column_not_null VALUES (0)`)
			addColumnNotNull := feed(t, f, `CREATE CHANGEFEED FOR add_column_not_null `+
				`WITH schema_change_policy='nobackfill'`)
			defer closeFeed(t, addColumnNotNull)
			sqlDB.Exec(t, `INSERT INTO add_column_not_null VALUES (1)`)
			assertPayloads(t, addColumnNotNull, []string{
				`add_column_not_null: [0]->{"after": {"a": 0}}`,
				`add_column_not_null: [1]->{"after": {"a": 1}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_column_not_null ADD COLUMN b INT NOT NULL DEFAULT 0`)
			sqlDB.Exec(t, "INSERT INTO add_column_not_null VALUES (2, 1)")
			assertPayloads(t, addColumnNotNull, []string{
				`add_column_not_null: [2]->{"after": {"a": 2, "b": 1}}`,
			})
		})
		t.Run("add column null", func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_column_null (a INT PRIMARY KEY)`)
			defer sqlDB.Exec(t, `DROP TABLE add_column_null`)
			sqlDB.Exec(t, `INSERT INTO add_column_null VALUES (0)`)
			addColumnNull := feed(t, f, `CREATE CHANGEFEED FOR add_column_null `+
				`WITH schema_change_policy='nobackfill'`)
			defer closeFeed(t, addColumnNull)
			sqlDB.Exec(t, `INSERT INTO add_column_null VALUES (1)`)
			assertPayloads(t, addColumnNull, []string{
				`add_column_null: [0]->{"after": {"a": 0}}`,
				`add_column_null: [1]->{"after": {"a": 1}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_column_null ADD COLUMN b INT`)
			sqlDB.Exec(t, "INSERT INTO add_column_null VALUES (2, NULL)")
			assertPayloads(t, addColumnNull, []string{
				`add_column_null: [2]->{"after": {"a": 2, "b": null}}`,
			})
		})
		t.Run(`add column computed`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE add_comp_col (a INT PRIMARY KEY)`)
			defer sqlDB.Exec(t, `DROP TABLE add_comp_col`)
			sqlDB.Exec(t, `INSERT INTO add_comp_col VALUES (0)`)
			addCompCol := feed(t, f, `CREATE CHANGEFEED FOR add_comp_col `+
				`WITH schema_change_policy='nobackfill'`)
			defer closeFeed(t, addCompCol)
			sqlDB.Exec(t, `INSERT INTO add_comp_col VALUES (1)`)
			assertPayloads(t, addCompCol, []string{
				`add_comp_col: [0]->{"after": {"a": 0}}`,
				`add_comp_col: [1]->{"after": {"a": 1}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE add_comp_col ADD COLUMN b INT AS (a + 1) STORED`)
			sqlDB.Exec(t, "INSERT INTO add_comp_col VALUES (2)")
			assertPayloads(t, addCompCol, []string{
				`add_comp_col: [2]->{"after": {"a": 2, "b": 3}}`,
			})
		})
		t.Run("drop column", func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE drop_column (a INT PRIMARY KEY, b INT)`)
			defer sqlDB.Exec(t, `DROP TABLE drop_column`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (0, NULL)`)
			dropColumn := feed(t, f, `CREATE CHANGEFEED FOR drop_column `+
				`WITH schema_change_policy='nobackfill'`)
			defer closeFeed(t, dropColumn)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (1, 2)`)
			assertPayloads(t, dropColumn, []string{
				`drop_column: [0]->{"after": {"a": 0, "b": null}}`,
				`drop_column: [1]->{"after": {"a": 1, "b": 2}}`,
			})
			sqlDB.Exec(t, `ALTER TABLE drop_column DROP COLUMN b`)
			sqlDB.Exec(t, `INSERT INTO drop_column VALUES (2)`)

			var payloads []string
			if usingLegacySchemaChanger {
				// NB: Legacy schema changes modify the physical KVs in place while
				// the changefeed is running, so you see a "backfill" even though
				// the changefeed does not perform one. If we did not specify
				// `schema_change_policy='nobackfill'`, then we would have seen
				// 0 and 1 an additional time before seeing row 2.
				payloads = []string{
					`drop_column: [0]->{"after": {"a": 0}}`,
					`drop_column: [1]->{"after": {"a": 1}}`,
					`drop_column: [2]->{"after": {"a": 2}}`,
				}
			} else {
				payloads = []string{
					`drop_column: [2]->{"after": {"a": 2}}`,
				}
			}
			assertPayloads(t, dropColumn, payloads)
		})
		t.Run("add index", func(t *testing.T) {
			// This case does not exit
			sqlDB.Exec(t, `CREATE TABLE add_index (a INT PRIMARY KEY, b INT)`)
			defer sqlDB.Exec(t, `DROP TABLE add_index`)
			sqlDB.Exec(t, `INSERT INTO add_index VALUES (0, NULL)`)
			addIndex := feed(t, f, `CREATE CHANGEFEED FOR add_index `+
				`WITH schema_change_policy='nobackfill'`)
			defer closeFeed(t, addIndex)
			sqlDB.Exec(t, `INSERT INTO add_index VALUES (1, 2)`)
			assertPayloads(t, addIndex, []string{
				`add_index: [0]->{"after": {"a": 0, "b": null}}`,
				`add_index: [1]->{"after": {"a": 1, "b": 2}}`,
			})
			sqlDB.Exec(t, `CREATE INDEX ON add_index (b)`)
			sqlDB.Exec(t, `INSERT INTO add_index VALUES (2, NULL)`)
			assertPayloads(t, addIndex, []string{
				`add_index: [2]->{"after": {"a": 2, "b": null}}`,
			})
		})
	}

	cdcTestWithSystem(t, testFn)
}

func TestChangefeedStoredComputedColumn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE cc (
		a INT, b INT AS (a + 1) STORED, c INT AS (a + 2) STORED, PRIMARY KEY (b, a)
	)`)
		sqlDB.Exec(t, `INSERT INTO cc (a) VALUES (1)`)

		cc := feed(t, f, `CREATE CHANGEFEED FOR cc`)
		defer closeFeed(t, cc)

		assertPayloads(t, cc, []string{
			`cc: [2, 1]->{"after": {"a": 1, "b": 2, "c": 3}}`,
		})

		sqlDB.Exec(t, `INSERT INTO cc (a) VALUES (10)`)
		assertPayloads(t, cc, []string{
			`cc: [11, 10]->{"after": {"a": 10, "b": 11, "c": 12}}`,
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedVirtualComputedColumn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	tests := map[string]struct {
		formatOpt               changefeedbase.FormatType
		virtualColumnVisibility changefeedbase.VirtualColumnVisibility
		changeFeedStmt          string
		payloadAfterInsert      []string
		payloadAfterUpdate      []string
	}{
		`format="json",virtual_columns="omitted"`: {
			formatOpt:               changefeedbase.OptFormatJSON,
			virtualColumnVisibility: changefeedbase.OptVirtualColumnsOmitted,
			payloadAfterInsert:      []string{`cc: [1]->{"after": {"a": 1, "b": 1}, "before": null}`},
			payloadAfterUpdate:      []string{`cc: [1]->{"after": {"a": 1, "b": 10}, "before": {"a": 1, "b": 1}}`},
		},
		`format="json",virtual_columns="null"`: {
			formatOpt:               changefeedbase.OptFormatJSON,
			virtualColumnVisibility: changefeedbase.OptVirtualColumnsNull,
			payloadAfterInsert:      []string{`cc: [1]->{"after": {"a": 1, "b": 1, "c": null}, "before": null}`},
			payloadAfterUpdate:      []string{`cc: [1]->{"after": {"a": 1, "b": 10, "c": null}, "before": {"a": 1, "b": 1, "c": null}}`},
		},
		`format="avro",virtual_columns="omitted"`: {
			formatOpt:               changefeedbase.OptFormatAvro,
			virtualColumnVisibility: changefeedbase.OptVirtualColumnsOmitted,
			payloadAfterInsert:      []string{`cc: {"a":{"long":1}}->{"after":{"cc":{"a":{"long":1},"b":{"long":1}}},"before":null}`},
			payloadAfterUpdate:      []string{`cc: {"a":{"long":1}}->{"after":{"cc":{"a":{"long":1},"b":{"long":10}}},"before":{"cc_before":{"a":{"long":1},"b":{"long":1}}}}`},
		},
		`format="avro",virtual_columns="null"`: {
			formatOpt:               changefeedbase.OptFormatAvro,
			virtualColumnVisibility: changefeedbase.OptVirtualColumnsNull,
			payloadAfterInsert:      []string{`cc: {"a":{"long":1}}->{"after":{"cc":{"a":{"long":1},"b":{"long":1},"c":null}},"before":null}`},
			payloadAfterUpdate:      []string{`cc: {"a":{"long":1}}->{"after":{"cc":{"a":{"long":1},"b":{"long":10},"c":null}},"before":{"cc_before":{"a":{"long":1},"b":{"long":1},"c":null}}}`},
		},
	}

	for _, test := range tests {
		testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
			sqlDB := sqlutils.MakeSQLRunner(s.DB)

			sqlDB.Exec(t, `CREATE TABLE cc (
					a INT primary key, b INT, c INT AS (b + 1) VIRTUAL NOT NULL
				)`)
			defer sqlDB.Exec(t, `DROP TABLE cc`)

			sqlDB.Exec(t, `INSERT INTO cc VALUES (1, 1)`)

			changeFeed := feed(t, f, fmt.Sprintf(`CREATE CHANGEFEED FOR cc WITH diff, format="%s", virtual_columns="%s"`,
				test.formatOpt, test.virtualColumnVisibility))
			defer closeFeed(t, changeFeed)

			assertPayloads(t, changeFeed, test.payloadAfterInsert)

			sqlDB.Exec(t, `UPDATE cc SET b=10 WHERE a=1`)

			assertPayloads(t, changeFeed, test.payloadAfterUpdate)
		}

		if test.formatOpt != changefeedbase.OptFormatAvro {
			cdcTest(t, testFn)
		} else {
			cdcTest(t, testFn, feedTestForceSink("kafka"))
		}
	}
}

func TestChangefeedUpdatePrimaryKey(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		// This NOT NULL column checks a regression when used with UPDATE-ing a
		// primary key column or with DELETE.
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING NOT NULL)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'bar')`)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, foo)
		assertPayloads(t, foo, []string{
			`foo: [0]->{"after": {"a": 0, "b": "bar"}}`,
		})

		sqlDB.Exec(t, `UPDATE foo SET a = 1`)
		assertPayloads(t, foo, []string{
			`foo: [0]->{"after": null}`,
			`foo: [1]->{"after": {"a": 1, "b": "bar"}}`,
		})

		sqlDB.Exec(t, `DELETE FROM foo`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": null}`,
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedTruncateOrDrop(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	assertFailuresCounter := func(t *testing.T, m *Metrics, exp int64) {
		t.Helper()
		// If this changefeed is running as a job, we anticipate that it will move
		// through the failed state and will increment the metric. Sinkless feeds
		// don't contribute to the failures counter.
		if strings.Contains(t.Name(), `sinkless`) {
			return
		}
		testutils.SucceedsSoon(t, func() error {
			if got := m.Failures.Count(); got != exp {
				return errors.Errorf("expected %d failures, got %d", exp, got)
			}
			return nil
		})
	}

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		registry := s.Server.JobRegistry().(*jobs.Registry)
		metrics := registry.MetricsStruct().Changefeed.(*Metrics)

		drainUntilErr := func(f cdctest.TestFeed) (err error) {
			var msg *cdctest.TestFeedMessage
			for msg, err = f.Next(); msg != nil; msg, err = f.Next() {
			}
			return err
		}

		sqlDB.Exec(t, `CREATE TABLE truncate (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `CREATE TABLE truncate_cascade (b INT PRIMARY KEY REFERENCES truncate (a))`)
		sqlDB.Exec(t,
			`BEGIN; INSERT INTO truncate VALUES (1); INSERT INTO truncate_cascade VALUES (1); COMMIT`)
		truncate := feed(t, f, `CREATE CHANGEFEED FOR truncate`)
		defer closeFeed(t, truncate)
		truncateCascade := feed(t, f, `CREATE CHANGEFEED FOR truncate_cascade`)
		defer closeFeed(t, truncateCascade)
		assertPayloads(t, truncate, []string{`truncate: [1]->{"after": {"a": 1}}`})
		assertPayloads(t, truncateCascade, []string{`truncate_cascade: [1]->{"after": {"b": 1}}`})
		sqlDB.Exec(t, `TRUNCATE TABLE truncate CASCADE`)
		if err := drainUntilErr(truncate); !testutils.IsError(err, `"truncate" was truncated`) {
			t.Fatalf(`expected ""truncate" was truncated" error got: %+v`, err)
		}
		if err := drainUntilErr(truncateCascade); !testutils.IsError(
			err, `"truncate_cascade" was truncated`,
		) {
			t.Fatalf(`expected ""truncate_cascade" was truncated" error got: %+v`, err)
		}
		assertFailuresCounter(t, metrics, 2)

		sqlDB.Exec(t, `CREATE TABLE drop (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO drop VALUES (1)`)
		drop := feed(t, f, `CREATE CHANGEFEED FOR drop`)
		defer closeFeed(t, drop)
		assertPayloads(t, drop, []string{`drop: [1]->{"after": {"a": 1}}`})
		sqlDB.Exec(t, `DROP TABLE drop`)
		// Dropping the table should cause the schema feed to return an error.
		// This error can either come from validateDescriptor (the first two)
		// or the lease manager (catalog.ErrDescriptorDropped).
		dropOrOfflineRE := fmt.Sprintf(
			`"drop" was dropped|CHANGEFEED cannot target offline table: drop|%s`,
			catalog.ErrDescriptorDropped,
		)
		if err := drainUntilErr(drop); !testutils.IsError(err, dropOrOfflineRE) {
			t.Errorf(`expected %q error, instead got: %+v`, dropOrOfflineRE, err)
		}
		assertFailuresCounter(t, metrics, 3)
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, withAllowChangefeedErr("expects errors"))
	// will sometimes fail, non deterministic
}

func TestChangefeedMonitoring(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sysDB := sqlutils.MakeSQLRunner(s.SystemServer.SQLConn(t))
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)

		if c := s.Server.MustGetSQLCounter(`changefeed.emitted_messages`); c != 0 {
			t.Errorf(`expected 0 got %d`, c)
		}
		if c := s.Server.MustGetSQLCounter(`changefeed.emitted_bytes`); c != 0 {
			t.Errorf(`expected 0 got %d`, c)
		}
		if c := s.Server.MustGetSQLCounter(`changefeed.flushed_bytes`); c != 0 {
			t.Errorf(`expected 0 got %d`, c)
		}
		if c := s.Server.MustGetSQLCounter(`changefeed.flushes`); c != 0 {
			t.Errorf(`expected 0 got %d`, c)
		}
		if c := s.Server.MustGetSQLCounter(`changefeed.max_behind_nanos`); c != 0 {
			t.Errorf(`expected %d got %d`, 0, c)
		}
		if c := s.Server.MustGetSQLCounter(`changefeed.buffer_entries.in`); c != 0 {
			t.Errorf(`expected 0 got %d`, c)
		}
		if c := s.Server.MustGetSQLCounter(`changefeed.buffer_entries.out`); c != 0 {
			t.Errorf(`expected 0 got %d`, c)
		}
		if c := s.Server.MustGetSQLCounter(`changefeed.schemafeed.table_metadata_nanos`); c != 0 {
			t.Errorf(`expected 0 got %d`, c)
		}
		if c := s.Server.MustGetSQLCounter(`changefeed.schemafeed.table_history_scans`); c != 0 {
			t.Errorf(`expected 0 got %d`, c)
		}

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH metrics_label='tier0'`)
		_, err := foo.Next()
		require.NoError(t, err)

		testutils.SucceedsSoon(t, func() error {
			if c := s.Server.MustGetSQLCounter(`changefeed.emitted_messages`); c != 1 {
				return errors.Errorf(`expected 1 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.emitted_bytes`); c != 22 {
				return errors.Errorf(`expected 22 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.flushed_bytes`); c != 22 {
				return errors.Errorf(`expected 22 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.flushes`); c <= 0 {
				return errors.Errorf(`expected > 0 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.running`); c != 1 {
				return errors.Errorf(`expected 1 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.max_behind_nanos`); c <= 0 {
				return errors.Errorf(`expected > 0 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.buffer_entries.in`); c <= 0 {
				return errors.Errorf(`expected > 0 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.buffer_entries.out`); c <= 0 {
				return errors.Errorf(`expected > 0 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.schemafeed.table_history_scans`); c <= 0 {
				return errors.Errorf(`expected > 0 got %d`, c)
			}
			return nil
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (2)`)

		// Check that two changefeeds add correctly.
		// Set cluster settings back so we don't interfere with schema changes.
		sysDB.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.target_duration = '1s'`)
		fooCopy := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		_, _ = fooCopy.Next()
		_, _ = fooCopy.Next()
		testutils.SucceedsSoon(t, func() error {
			// We can't assert exactly 4 or 88 in case we get (allowed) duplicates
			// from RangeFeed.
			if c := s.Server.MustGetSQLCounter(`changefeed.emitted_messages`); c < 4 {
				return errors.Errorf(`expected >= 4 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.emitted_bytes`); c < 88 {
				return errors.Errorf(`expected >= 88 got %d`, c)
			}
			return nil
		})

		// Cancel all the changefeeds and check that max_behind_nanos returns to 0
		// and the number running returns to 0.
		require.NoError(t, foo.Close())
		require.NoError(t, fooCopy.Close())
		testutils.SucceedsSoon(t, func() error {
			if c := s.Server.MustGetSQLCounter(`changefeed.max_behind_nanos`); c != 0 {
				return errors.Errorf(`expected 0 got %d`, c)
			}
			if c := s.Server.MustGetSQLCounter(`changefeed.running`); c != 0 {
				return errors.Errorf(`expected 0 got %d`, c)
			}
			return nil
		})
	}

	cdcTestWithSystem(t, testFn, feedTestForceSink("sinkless"))
}

func TestChangefeedRetryableError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)
		var failEmit int64
		knobs.BeforeEmitRow = func(_ context.Context) error {
			switch atomic.LoadInt64(&failEmit) {
			case 1:
				return changefeedbase.MarkRetryableError(fmt.Errorf("synthetic retryable error"))
			case 2:
				return changefeedbase.WithTerminalError(errors.New("synthetic terminal error"))
			default:
				return nil
			}
		}

		// Set up a new feed and verify that the sink is started up.
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, foo)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"a": 1}}`,
		})

		// Set sink to return unique retryable errors and insert a row. Verify that
		// sink is failing requests.
		atomic.StoreInt64(&failEmit, 1)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (2)`)
		registry := s.Server.JobRegistry().(*jobs.Registry)

		sli, err := registry.MetricsStruct().Changefeed.(*Metrics).getSLIMetrics(defaultSLIScope)
		require.NoError(t, err)
		retryCounter := sli.ErrorRetries
		testutils.SucceedsSoon(t, func() error {
			if retryCounter.Value() < 3 {
				return fmt.Errorf("insufficient error retries detected")
			}
			return nil
		})

		// Verify job progress contains retryable error status.
		jobID := foo.(cdctest.EnterpriseTestFeed).JobID()
		job, err := registry.LoadJob(context.Background(), jobID)
		require.NoError(t, err)
		require.Contains(t, job.Progress().StatusMessage, "synthetic retryable error")

		// Verify `SHOW JOBS` also shows this information.
		var statusMessage string
		sqlDB.QueryRow(t,
			`SELECT running_status FROM [SHOW JOBS] WHERE job_id = $1`, jobID,
		).Scan(&statusMessage)
		require.Contains(t, statusMessage, "synthetic retryable error")

		// Fix the sink and insert another row. Check that nothing funky happened.
		atomic.StoreInt64(&failEmit, 0)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (3)`)
		assertPayloads(t, foo, []string{
			`foo: [2]->{"after": {"a": 2}}`,
			`foo: [3]->{"after": {"a": 3}}`,
		})

		// Set sink to return a terminal error and insert a row. Ensure that we
		// eventually get the error message back out.
		atomic.StoreInt64(&failEmit, 2)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (4)`)
		for {
			_, err := foo.Next()
			if err == nil {
				continue
			}
			require.EqualError(t, err, `synthetic terminal error`)
			break
		}
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, withAllowChangefeedErr("expects error"))
}

func TestChangefeedJobUpdateFailsIfNotClaimed(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Set TestingKnobs to return a known session for easier
	// comparison.
	adoptionInterval := 20 * time.Minute
	sessionOverride := withKnobsFn(func(knobs *base.TestingKnobs) {
		// This is a hack to avoid the job adoption loop from
		// immediately re-adopting the job that is running. The job
		// adoption loop basically just sets the claim ID, which will
		// undo our deletion of the claim ID below.
		knobs.JobsTestingKnobs.(*jobs.TestingKnobs).IntervalOverrides.Adopt = &adoptionInterval
	})
	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		knobs := s.TestingKnobs.DistSQL.(*execinfra.TestingKnobs).Changefeed.(*TestingKnobs)
		errChan := make(chan error, 1)
		knobs.HandleDistChangefeedError = func(err error) error {
			select {
			case errChan <- err:
			default:
			}
			return err
		}

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b INT)`)
		sqlDB.Exec(t, `INSERT INTO foo (a, b) VALUES (1, 1)`)

		cf := feed(t, f, "CREATE CHANGEFEED FOR TABLE foo")
		jobID := cf.(cdctest.EnterpriseTestFeed).JobID()
		defer func() {
			// Manually update job status to avoid closeFeed waitng for the registry to cancel it
			sqlDB.Exec(t, `UPDATE system.jobs SET status = $1 WHERE id = $2`, jobs.StateFailed, jobID)
			closeFeed(t, cf)
		}()

		assertPayloads(t, cf, []string{
			`foo: [1]->{"after": {"a": 1, "b": 1}}`,
		})

		// Mimic the claim dying and being cleaned up by
		// another node.
		sqlDB.Exec(t, `UPDATE system.jobs SET claim_session_id = NULL WHERE id = $1`, jobID)

		timeout := (5 * time.Second) + changefeedbase.Quantize.Get(&s.Server.ClusterSettings().SV)

		if util.RaceEnabled {
			// Timeout should be at least 30s to allow for race conditions.
			timeout += 25 * time.Second
		}
		// Expect that the distflow fails since it can't
		// update the checkpoint.
		select {
		case err := <-errChan:
			require.Error(t, err)
			// TODO(ssd): Replace this error in the jobs system with
			// an error type we can check against.
			require.Regexp(t, "expected session .* but found NULL", err.Error())
		case <-time.After(timeout):
			t.Fatal("expected distflow to fail")
		}
	}

	// TODO: Figure out why this freezes on tenants
	cdcTest(t, testFn, sessionOverride, feedTestNoTenants, feedTestEnterpriseSinks)
}

// TestChangefeedDataTTL ensures that changefeeds fail with an error in the case
// where the feed has fallen behind the GC TTL of the table data.
func TestChangefeedDataTTL(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		// Set a very simple channel-based, wait-and-resume function as the
		// BeforeEmitRow hook.
		var shouldWait int32
		wait := make(chan struct{})
		resume := make(chan struct{})
		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)
		knobs.FeedKnobs.BeforeScanRequest = func(_ *kv.Batch) error {
			if atomic.LoadInt32(&shouldWait) == 0 {
				return nil
			}
			wait <- struct{}{}
			<-resume
			return nil
		}

		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		// Create the data table; it will only contain a
		// single row with multiple versions.
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b INT)`)

		counter := 0
		upsertedValues := make(map[int]struct{})
		upsertRow := func() {
			counter++
			sqlDB.Exec(t, `UPSERT INTO foo (a, b) VALUES (1, $1)`, counter)
			upsertedValues[counter] = struct{}{}
		}

		// Create the initial version of the row and the
		// changefeed itself. The initial version is necessary
		// to ensure that there is at least one row to
		// backfill.
		upsertRow()

		// Set emit trap to ensure the backfill will pause.
		// The backfill happens before the construction of the
		// rangefeed. Further the backfill sends rows to the
		// changeAggregator via an unbuffered channel, so
		// blocking the emit should block the scan from
		// finishing.
		atomic.StoreInt32(&shouldWait, 1)

		// The changefeed needs to be initialized in a background goroutine because
		// pgx will try to pull results from it as soon as it runs the conn.Query
		// method, but that will block until `resume` is signaled.
		changefeedInit := make(chan cdctest.TestFeed, 1)
		var dataExpiredRows cdctest.TestFeed
		defer func() {
			if dataExpiredRows != nil {
				closeFeed(t, dataExpiredRows)
			}
		}()
		go func() {
			feed, err := f.Feed("CREATE CHANGEFEED FOR TABLE foo")
			if err == nil {
				changefeedInit <- feed
			}
			close(changefeedInit)
		}()

		// Ensure our changefeed is started and waiting during the backfill.
		<-wait

		// Upsert additional versions. One of these will be
		// deleted by the GC process before the rangefeed is
		// started.
		upsertRow()
		upsertRow()
		upsertRow()

		// Force a GC of the table. This should cause both
		// versions of the table to be deleted.
		forceTableGC(t, s.SystemServer, sqlDB, "d", "foo")

		// Resume our changefeed normally.
		atomic.StoreInt32(&shouldWait, 0)
		resume <- struct{}{}
		dataExpiredRows = <-changefeedInit
		require.NotNil(t, dataExpiredRows)

		// Verify that, at some point, Next() returns a "must
		// be after replica GC threshold" error. In the common
		// case, that'll be the second call, the first will
		// should return the row from the backfill and the
		// second should be returning
		for {
			msg, err := dataExpiredRows.Next()
			if testutils.IsError(err, `must be after replica GC threshold`) {
				t.Logf("got expected GC error: %s", err)
				break
			}
			if msg != nil {
				t.Logf("ignoring message: %s", msg)
				var decodedMessage struct {
					After struct {
						A int
						B int
					}
				}
				err = gojson.Unmarshal(msg.Value, &decodedMessage)
				require.NoError(t, err)
				delete(upsertedValues, decodedMessage.After.B)
				if len(upsertedValues) == 0 {
					t.Error("TestFeed emitted all values despite GC running")
					return
				}
			}
		}
	}
	// NOTE(ssd): This test doesn't apply to enterprise
	// changefeeds since enterprise changefeeds create a protected
	// timestamp before beginning their backfill.
	// TODO(samiskin): Tenant test disabled because this test requires
	// forceTableGC which doesn't work on tenants
	cdcTestWithSystem(t, testFn, feedTestForceSink("sinkless"), feedTestNoTenants, withAllowChangefeedErr("expects batch ts gc error"))
}

// TestChangefeedOutdatedCursor ensures that create changefeeds fail with an
// error in the case where the cursor is older than the GC TTL of the table.
func TestChangefeedOutdatedCursor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE f (a INT PRIMARY KEY)`)
		outdatedTS := s.Server.Clock().Now().AsOfSystemTime()
		sqlDB.Exec(t, `INSERT INTO f VALUES (1)`)
		forceTableGC(t, s.SystemServer, sqlDB, "system", "descriptor")
		createChangefeed :=
			fmt.Sprintf(`CREATE CHANGEFEED FOR TABLE f with cursor = '%s'`, outdatedTS)
		expectedErrorSubstring :=
			fmt.Sprintf(
				"could not create changefeed: cursor %s is older than the GC threshold", outdatedTS)
		expectErrCreatingFeed(t, f, createChangefeed, expectedErrorSubstring)
	}

	cdcTestWithSystem(t, testFn, feedTestNoTenants)
}

// TestChangefeedCursorWarning ensures that we show a warning if
// any of the tables we're creating a changefeed is past
// the warning threshold.
func TestChangefeedCursorAgeWarning(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	var cursorAges = []time.Duration{
		time.Hour,
		6 * time.Hour,
	}

	testutils.RunValues(t, "cursor age", cursorAges, func(t *testing.T, cursorAge time.Duration) {
		s, stopServer := makeServer(t, withAllowChangefeedErr("expects batch ts gc error"))
		defer stopServer()
		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)
		knobs.OverrideCursorAge = func() int64 {
			return int64(cursorAge)
		}

		warning := fmt.Sprintf(
			"the provided cursor is %d hours old; older cursors can result in increased changefeed latency",
			int64(cursorAge/time.Hour))
		noWarning := "(no notice)"

		expectedWarning := func(initial_scan string) string {
			if cursorAge == time.Hour || initial_scan == "only" {
				return noWarning
			}
			return warning
		}

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE f (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO f VALUES (1)`)
		timeNow := strings.Split(s.Server.Clock().Now().AsOfSystemTime(), ".")[0]

		expectNotice(t, s.Server,
			fmt.Sprintf(
				`CREATE CHANGEFEED FOR TABLE d.f INTO 'null://' with cursor = '%s', initial_scan='only'`,
				timeNow), expectedWarning("only"))

		expectNotice(t, s.Server,
			fmt.Sprintf(
				`CREATE CHANGEFEED FOR TABLE d.f INTO 'null://' with cursor = '%s', initial_scan='yes'`,
				timeNow), expectedWarning("yes"))

		expectNotice(t, s.Server,
			fmt.Sprintf(
				`CREATE CHANGEFEED FOR TABLE d.f INTO 'null://' with cursor = '%s', initial_scan='no'`,
				timeNow), expectedWarning("no"))
	})
}

// TestChangefeedSchemaTTL ensures that changefeeds fail with an error in the case
// where the feed has fallen behind the GC TTL of the table's schema.
func TestChangefeedSchemaTTL(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		// Set a very simple channel-based, wait-and-resume function as the
		// BeforeEmitRow hook.
		var shouldWait int32
		wait := make(chan struct{})
		resume := make(chan struct{})
		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)
		knobs.BeforeEmitRow = func(_ context.Context) error {
			if atomic.LoadInt32(&shouldWait) == 0 {
				return nil
			}
			wait <- struct{}{}
			<-resume
			return nil
		}

		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		// Create the data table; it will only contain a single row with multiple
		// versions.
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		counter := 0
		upsertRow := func() {
			counter++
			sqlDB.Exec(t, `UPSERT INTO foo (a, b) VALUES (1, $1)`, fmt.Sprintf("version %d", counter))
		}

		// Create the initial version of the row and the changefeed itself. The initial
		// version is necessary to prevent CREATE CHANGEFEED itself from hanging.
		upsertRow()
		dataExpiredRows := feed(t, f, "CREATE CHANGEFEED FOR TABLE foo")
		defer closeFeed(t, dataExpiredRows)

		// Set up our emit trap and update the row, which will allow us to "pause" the
		// changefeed in order to force a GC.
		atomic.StoreInt32(&shouldWait, 1)
		upsertRow()
		<-wait

		// Upsert two additional versions. One of these will be deleted by the GC
		// process before changefeed polling is resumed.
		waitForSchemaChange(t, sqlDB, "ALTER TABLE foo ADD COLUMN c STRING")
		upsertRow()
		waitForSchemaChange(t, sqlDB, "ALTER TABLE foo ADD COLUMN d STRING")
		upsertRow()

		// Force a GC of the table. This should cause both older versions of the
		// table to be deleted, with the middle version being lost to the changefeed.
		forceTableGC(t, s.SystemServer, sqlDB, "system", "descriptor")

		// Resume our changefeed normally.
		atomic.StoreInt32(&shouldWait, 0)
		resume <- struct{}{}

		// Verify that the third call to Next() returns an error (the first is the
		// initial row, the second is the first change.
		// Note: rows, and the error message may arrive in any order, so we just loop
		// until we see an error.
		for {
			_, err := dataExpiredRows.Next()
			if err != nil {
				require.Regexp(t, `GC threshold`, err)
				break
			}
		}

	}

	// TODO(samiskin): tenant tests skipped because of forceTableGC not working
	// with a ApplicationLayerInterface
	cdcTestWithSystem(t, testFn, feedTestNoTenants, withAllowChangefeedErr("expects batch ts gc error"))
}

func TestChangefeedErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	srv, db, _ := serverutils.StartServer(t, base.TestServerArgs{
		Locality: roachpb.Locality{
			Tiers: []roachpb.Tier{{
				Key:   "region",
				Value: testServerRegion,
			}},
		},
	})
	defer srv.Stopper().Stop(ctx)

	s := srv.ApplicationLayer()

	schemaReg := cdctest.StartTestSchemaRegistry()
	defer schemaReg.Close()

	sqlDB := sqlutils.MakeSQLRunner(db)
	sqlDB.SucceedsSoonDuration = 5 * time.Second

	// Another SQLDB that has a longer "SucceedsSoonDuration", because some tests will take longer to fail due to DNS resolution retries.
	longTimeoutSQLDB := sqlutils.MakeSQLRunner(db)
	longTimeoutSQLDB.SucceedsSoonDuration = 30 * time.Second

	sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, j JSONB)`)
	sqlDB.Exec(t, `CREATE DATABASE d`)

	// Changefeeds default to rangefeed, but for now, rangefeed defaults to off.
	// Verify that this produces a useful error.
	for _, l := range []serverutils.ApplicationLayerInterface{s, srv.SystemLayer()} {
		kvserver.RangefeedEnabled.Override(ctx, &l.ClusterSettings().SV, false)
	}

	sqlDB.Exec(t, `CREATE TABLE rangefeed_off (a INT PRIMARY KEY)`)
	sqlDB.ExpectErrWithTimeout(
		t, `rangefeeds require the kv.rangefeed.enabled setting`,
		`EXPERIMENTAL CHANGEFEED FOR rangefeed_off`,
	)

	for _, l := range []serverutils.ApplicationLayerInterface{s, srv.SystemLayer()} {
		kvserver.RangefeedEnabled.Override(ctx, &l.ClusterSettings().SV, true)
	}

	// Feature flag for changefeeds is off — test that CREATE CHANGEFEED and
	// EXPERIMENTAL CHANGEFEED FOR surface error.
	featureChangefeedEnabled.Override(ctx, &s.ClusterSettings().SV, false)
	sqlDB.ExpectErrWithTimeout(t, `feature CHANGEFEED was disabled by the database administrator`,
		`CREATE CHANGEFEED FOR foo`)
	sqlDB.ExpectErrWithTimeout(t, `feature CHANGEFEED was disabled by the database administrator`,
		`EXPERIMENTAL CHANGEFEED FOR foo`)
	featureChangefeedEnabled.Override(ctx, &s.ClusterSettings().SV, true)

	sqlDB.ExpectErrWithTimeout(
		t, `unknown format: nope`,
		`EXPERIMENTAL CHANGEFEED FOR foo WITH format=nope`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `unknown envelope: nope`,
		`EXPERIMENTAL CHANGEFEED FOR foo WITH envelope=nope`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `time: invalid duration "bar"`,
		`EXPERIMENTAL CHANGEFEED FOR foo WITH resolved='bar'`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `negative durations are not accepted: resolved='-1s'`,
		`EXPERIMENTAL CHANGEFEED FOR foo WITH resolved='-1s'`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `timestamp '.*' is in the future`,
		`EXPERIMENTAL CHANGEFEED FOR foo WITH cursor=$1`, timeutil.Now().Add(time.Hour),
	)

	sqlDB.ExpectErrWithTimeout(
		t, `omit the SINK clause`,
		`CREATE CHANGEFEED FOR foo INTO ''`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `omit the SINK clause`,
		`CREATE CHANGEFEED FOR foo INTO $1`, ``,
	)

	// Watching system.jobs would create a cycle, since the resolved timestamp
	// high-water mark is saved in it.
	sqlDB.ExpectErrWithTimeout(
		t, `not supported on system tables`,
		`EXPERIMENTAL CHANGEFEED FOR system.jobs`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `table "bar" does not exist`,
		`EXPERIMENTAL CHANGEFEED FOR bar`,
	)
	sqlDB.Exec(t, `CREATE SEQUENCE seq`)
	sqlDB.ExpectErrWithTimeout(
		t, `CHANGEFEED cannot target sequences: seq`,
		`EXPERIMENTAL CHANGEFEED FOR seq`,
	)
	sqlDB.Exec(t, `CREATE VIEW vw AS SELECT a, b FROM foo`)
	sqlDB.ExpectErrWithTimeout(
		t, `CHANGEFEED cannot target views: vw`,
		`EXPERIMENTAL CHANGEFEED FOR vw`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `CHANGEFEED targets TABLE foo and TABLE foo are duplicates`,
		`EXPERIMENTAL CHANGEFEED FOR foo, foo`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `CHANGEFEED targets TABLE foo and TABLE defaultdb.foo are duplicates`,
		`EXPERIMENTAL CHANGEFEED FOR foo, defaultdb.foo`,
	)
	sqlDB.Exec(t,
		`CREATE TABLE threefams (a int, b int, c int, family f_a(a), family f_b(b), family f_c(c))`)
	sqlDB.ExpectErrWithTimeout(
		t, `CHANGEFEED targets TABLE foo FAMILY f_a and TABLE foo FAMILY f_a are duplicates`,
		`EXPERIMENTAL CHANGEFEED FOR foo family f_a, foo FAMILY f_b, foo FAMILY f_a`,
	)

	// Backup has the same bad error message #28170.
	sqlDB.ExpectErrWithTimeout(
		t, `"information_schema.tables" does not exist`,
		`EXPERIMENTAL CHANGEFEED FOR information_schema.tables`,
	)

	// TODO(dan): These two tests shouldn't need initial data in the table
	// to pass.
	sqlDB.Exec(t, `CREATE TABLE dec (a DECIMAL PRIMARY KEY)`)
	sqlDB.Exec(t, `INSERT INTO dec VALUES (1.0)`)
	sqlDB.ExpectErrWithTimeout(
		t, `.*column a: decimal with no precision`,
		`EXPERIMENTAL CHANGEFEED FOR dec WITH format=$1, confluent_schema_registry=$2`,
		changefeedbase.OptFormatAvro, schemaReg.URL(),
	)
	sqlDB.Exec(t, `CREATE TABLE "oid" (a OID PRIMARY KEY)`)
	sqlDB.Exec(t, `INSERT INTO "oid" VALUES (3::OID)`)
	sqlDB.ExpectErrWithTimeout(
		t, `.*column a: type OID not yet supported with avro`,
		`EXPERIMENTAL CHANGEFEED FOR "oid" WITH format=$1, confluent_schema_registry=$2`,
		changefeedbase.OptFormatAvro, schemaReg.URL(),
	)

	unknownParams := func(sink string, params ...string) string {
		return fmt.Sprintf(`unknown %s sink query parameters: [%s]`, sink, strings.Join(params, ", "))
	}

	// Check that sink URLs have valid scheme
	sqlDB.ExpectErrWithTimeout(
		t, `no scheme found for sink URL`,
		`CREATE CHANGEFEED FOR foo INTO 'kafka%3A%2F%2Fnope%0A'`,
	)

	// Check that confluent_schema_registry is only accepted if format is avro.
	// TODO: This should be testing it as a WITH option and check avro_schema_prefix too
	sqlDB.ExpectErrWithTimeout(
		t, unknownParams("SQL", "confluent_schema_registry", "weird"),
		`CREATE CHANGEFEED FOR foo INTO $1`, `experimental-sql://d/?confluent_schema_registry=foo&weird=bar`,
	)

	badHostErrRE := "(no such host|connection refused|network is unreachable)"
	if KafkaV2Enabled.Get(&s.ClusterSettings().SV) {
		badHostErrRE = "(unable to dial|unable to open connection to broker|lookup .* on .*: server misbehaving|connection refused)"
	}

	// Check unavailable kafka - bad dns.
	longTimeoutSQLDB.ExpectErrWithTimeout(
		t, badHostErrRE,
		`CREATE CHANGEFEED FOR foo INTO 'kafka://nope:9999'`,
	)

	// Check unavailable kafka - not running.
	sqlDB.ExpectErrWithTimeout(
		t, badHostErrRE,
		`CREATE CHANGEFEED FOR foo INTO 'kafka://localhost:9999'`,
	)

	// Test that a well-formed URI gets as far as unavailable kafka error.
	longTimeoutSQLDB.ExpectErrWithTimeout(
		t, badHostErrRE,
		`CREATE CHANGEFEED FOR foo INTO 'kafka://nope:9999/?tls_enabled=true&insecure_tls_skip_verify=true&topic_name=foo'`,
	)

	// kafka_topic_prefix was referenced by an old version of the RFC, it's
	// "topic_prefix" now.
	sqlDB.ExpectErrWithTimeout(
		t, unknownParams(`kafka`, `kafka_topic_prefix`),
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?kafka_topic_prefix=foo`,
	)

	// topic_name is only honored for kafka sinks
	sqlDB.ExpectErrWithTimeout(
		t, unknownParams("SQL", "topic_name"),
		`CREATE CHANGEFEED FOR foo INTO $1`, `experimental-sql://d/?topic_name=foo`,
	)

	// schema_topic will be implemented but isn't yet.
	sqlDB.ExpectErrWithTimeout(
		t, `schema_topic is not yet supported`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?schema_topic=foo`,
	)

	// Sanity check kafka tls parameters.
	sqlDB.ExpectErrWithTimeout(
		t, `param tls_enabled must be a bool`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?tls_enabled=foo`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `param insecure_tls_skip_verify must be a bool`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?tls_enabled=true&insecure_tls_skip_verify=foo`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `param ca_cert must be base 64 encoded`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?ca_cert=!`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `ca_cert requires tls_enabled=true`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?&ca_cert=Zm9v`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `param client_cert must be base 64 encoded`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?client_cert=!`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `param client_key must be base 64 encoded`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?client_key=!`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `client_cert requires tls_enabled=true`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?client_cert=Zm9v`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `client_cert requires client_key to be set`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?tls_enabled=true&client_cert=Zm9v`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `client_key requires client_cert to be set`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?tls_enabled=true&client_key=Zm9v`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `invalid client certificate`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?tls_enabled=true&client_cert=Zm9v&client_key=Zm9v`,
	)

	// Sanity check kafka sasl parameters.
	sqlDB.ExpectErrWithTimeout(
		t, `param sasl_enabled must be a bool`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_enabled=maybe`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `param sasl_handshake must be a bool`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_enabled=true&sasl_user=x&sasl_password=y&sasl_handshake=maybe`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sasl_enabled must be enabled to configure SASL handshake behavior`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_handshake=false`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sasl_user must be provided when SASL is enabled`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_enabled=true`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sasl_password must be provided when SASL is enabled`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_enabled=true&sasl_user=a`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sasl_user must be provided when SASL is enabled using mechanism SCRAM-SHA-256`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_enabled=true&sasl_mechanism=SCRAM-SHA-256`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sasl_client_id must be provided when SASL is enabled using mechanism OAUTHBEARER`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_enabled=true&sasl_mechanism=OAUTHBEARER`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sasl_enabled must be enabled if sasl_user is provided`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_user=a`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sasl_enabled must be enabled if sasl_password is provided`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_password=a`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sasl_client_id is only a valid parameter for sasl_mechanism=OAUTHBEARER`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_enabled=true&sasl_client_id=a`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sasl_enabled must be enabled to configure SASL mechanism`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_mechanism=SCRAM-SHA-256`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `param sasl_mechanism must be one of AWS_MSK_IAM, OAUTHBEARER, PLAIN, SCRAM-SHA-256, or SCRAM-SHA-512`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `kafka://nope/?sasl_enabled=true&sasl_mechanism=unsuppported`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, badHostErrRE,
		`CREATE CHANGEFEED FOR foo INTO 'kafka://nope:9999/' WITH kafka_sink_config='{"Flush": {"Messages": 100, "Frequency": "1s"}}'`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `this sink is incompatible with option webhook_client_timeout`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_client_timeout='1s'`,
		`kafka://nope:9999/`,
	)
	// The avro format doesn't support key_in_value or topic_in_value yet.
	sqlDB.ExpectErrWithTimeout(
		t, `key_in_value is not supported with format=avro`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH key_in_value, format='experimental_avro'`,
		`kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `topic_in_value is not supported with format=avro`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH topic_in_value, format='experimental_avro'`,
		`kafka://nope`,
	)

	// Unordered flag required for some options, disallowed for others.
	sqlDB.ExpectErrWithTimeout(t, `resolved timestamps cannot be guaranteed to be correct in unordered mode`, `CREATE CHANGEFEED FOR foo WITH resolved, unordered`)
	sqlDB.ExpectErrWithTimeout(t, `Use of gcpubsub without specifying a region requires the WITH unordered option.`, `CREATE CHANGEFEED FOR foo INTO "gcpubsub://foo"`)
	sqlDB.ExpectErrWithTimeout(t, `key_column requires the unordered option`, `CREATE CHANGEFEED FOR foo WITH key_column='b'`)

	// The topics option should not be exposed to users since it is used
	// internally to display topics in the show changefeed jobs query
	sqlDB.ExpectErrWithTimeout(
		t, `invalid option "topics"`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH topics='foo,bar'`,
		`kafka://nope`,
	)

	// The cloudStorageSink is particular about the options it will work with.
	sqlDB.ExpectErrWithTimeout(
		t, `this sink is incompatible with option confluent_schema_registry`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH format='avro', confluent_schema_registry=$2`,
		`experimental-nodelocal://1/bar`, schemaReg.URL(),
	)
	sqlDB.ExpectErrWithTimeout(
		t, `this sink is incompatible with envelope=key_only`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH envelope='key_only'`,
		`experimental-nodelocal://1/bar`,
	)

	// WITH key_in_value requires envelope=wrapped
	sqlDB.ExpectErrWithTimeout(
		t, `key_in_value is only usable with envelope=wrapped`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH key_in_value, envelope='key_only'`, `kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `key_in_value is only usable with envelope=wrapped`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH key_in_value, envelope='row'`, `kafka://nope`,
	)

	// WITH topic_in_value requires envelope=wrapped
	sqlDB.ExpectErrWithTimeout(
		t, `topic_in_value is only usable with envelope=wrapped`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH topic_in_value, envelope='key_only'`, `kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `topic_in_value is only usable with envelope=wrapped`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH topic_in_value, envelope='row'`, `kafka://nope`,
	)

	// WITH initial_scan and no_initial_scan disallowed
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan and no_initial_scan`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH initial_scan, no_initial_scan`, `kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan and no_initial_scan`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH no_initial_scan, initial_scan`, `kafka://nope`,
	)

	// WITH only_initial_scan and no_initial_scan disallowed
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and no_initial_scan`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH initial_scan_only, no_initial_scan`, `kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and no_initial_scan`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH no_initial_scan, initial_scan_only`, `kafka://nope`,
	)

	// WITH initial_scan_only and initial_scan disallowed
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan and initial_scan_only`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH initial_scan_only, initial_scan`, `kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan and initial_scan_only`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH initial_scan, initial_scan_only`, `kafka://nope`,
	)

	// WITH only_initial_scan and end_time disallowed
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and end_time`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH initial_scan_only, end_time = '1'`, `kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and end_time`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH end_time = '1', initial_scan_only`, `kafka://nope`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and end_time`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH end_time = '1', initial_scan = 'only'`, `kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and end_time`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH initial_scan = 'only', end_time = '1'`, `kafka://nope`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and resolved`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH resolved, initial_scan = 'only'`, `kafka://nope`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and diff`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH diff, initial_scan = 'only'`, `kafka://nope`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and mvcc_timestamp`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH mvcc_timestamp, initial_scan = 'only'`, `kafka://nope`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan='only' and updated`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH updated, initial_scan = 'only'`, `kafka://nope`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `unknown initial_scan: foo`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH initial_scan = 'foo'`, `kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan and no_initial_scan`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH initial_scan = 'yes', no_initial_scan`, `kafka://nope`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `cannot specify both initial_scan and initial_scan_only`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH initial_scan = 'no', initial_scan_only`, `kafka://nope`,
	)

	sqlDB.ExpectErrWithTimeout(
		t, `format=csv is only usable with initial_scan_only`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH format = csv`, `kafka://nope`,
	)

	var tsCurrent string
	sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp()`).Scan(&tsCurrent)

	sqlDB.ExpectErrWithTimeout(
		t,
		fmt.Sprintf(`specified end time 1.0000000000 cannot be less than statement time %s`, tsCurrent),
		`CREATE CHANGEFEED FOR foo INTO $1 WITH cursor = $2, end_time = '1.0000000000'`, `kafka://nope`, tsCurrent,
	)

	// Sanity check schema registry tls parameters.
	sqlDB.ExpectErrWithTimeout(
		t, `param ca_cert must be base 64 encoded`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH format='experimental_avro', confluent_schema_registry=$2`,
		`kafka://nope`, `https://schemareg-nope/?ca_cert=!`,
	)
	longTimeoutSQLDB.ExpectErrWithTimeout(
		t, `failed to parse certificate data`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH format='experimental_avro', confluent_schema_registry=$2`,
		`kafka://nope`, `https://schemareg-nope/?ca_cert=Zm9v`,
	)

	// Sanity check webhook sink options.
	sqlDB.ExpectErrWithTimeout(
		t, `param insecure_tls_skip_verify must be a bool`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `webhook-https://fake-host?insecure_tls_skip_verify=foo`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `param ca_cert must be base 64 encoded`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `webhook-https://fake-host?ca_cert=?`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `failed to parse certificate data`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `webhook-https://fake-host?ca_cert=Zm9v`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `sink requires webhook-https`,
		`CREATE CHANGEFEED FOR foo INTO $1`, `webhook-http://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `this sink is incompatible with option confluent_schema_registry`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH format='avro', confluent_schema_registry=$2`,
		`webhook-https://fake-host`, schemaReg.URL(),
	)
	sqlDB.ExpectErrWithTimeout(
		t, `problem parsing option webhook_client_timeout: time: invalid duration "not_an_integer"`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_client_timeout='not_an_integer'`, `webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `option webhook_client_timeout must be a duration greater than 0`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_client_timeout='0s'`, `webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `negative durations are not accepted: webhook_client_timeout='-500s'`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_client_timeout='-500s'`, `webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `problem parsing option webhook_client_timeout: time: missing unit in duration`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_client_timeout='0.5'`, `webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `this sink is incompatible with envelope=key_only`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH envelope='key_only'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `this sink is incompatible with envelope=row`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH envelope='row'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `invalid sink config, all values must be non-negative`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_sink_config='{"Flush": {"Messages": -100, "Frequency": "1s"}}'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `invalid sink config, all values must be non-negative`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_sink_config='{"Flush": {"Messages": 100, "Frequency": "-1s"}}'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `invalid sink config, Flush.Frequency is not set, messages may never be sent`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_sink_config='{"Flush": {"Messages": 100}}'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `error unmarshalling json: time: invalid duration "Zm9v"`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_sink_config='{"Flush": {"Frequency": "Zm9v"}}'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `error unmarshalling json: invalid character`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_sink_config='not json'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `unknown compression: invalid, valid values are 'gzip' and 'zstd'`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH compression='invalid'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `Retry.Max must be either a positive int or 'inf' for infinite retries.`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_sink_config='{"Retry": {"Max": "not valid"}}'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `Retry.Max must be a positive integer. use 'inf' for infinite retries.`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_sink_config='{"Retry": {"Max": 0}}'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `Retry.Max must be a positive integer. use 'inf' for infinite retries.`,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH webhook_sink_config='{"Retry": {"Max": -1}}'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, ``,
		`CREATE CHANGEFEED FOR foo INTO $1 WITH updated, webhook_sink_config='{"Retry":{"Max":"inf"}}'`,
		`webhook-https://fake-host`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `client_cert requires client_key to be set`,
		`CREATE CHANGEFEED FOR foo INTO $1`,
		`webhook-https://fake-host?client_cert=Zm9v`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `client_key requires client_cert to be set`,
		`CREATE CHANGEFEED FOR foo INTO $1`,
		`webhook-https://fake-host?client_key=Zm9v`,
	)

	// Sanity check on_error option
	sqlDB.ExpectErrWithTimeout(
		t, `option "on_error" requires a value`,
		`CREATE CHANGEFEED FOR foo into $1 WITH on_error`,
		`kafka://nope`)
	sqlDB.ExpectErrWithTimeout(
		t, `unknown on_error: not_valid, valid values are 'pause' and 'fail'`,
		`CREATE CHANGEFEED FOR foo into $1 WITH on_error='not_valid'`,
		`kafka://nope`)

	// Sanity check for options compatibility validation.
	sqlDB.ExpectErrWithTimeout(
		t, `this sink is incompatible with option compression`,
		`CREATE CHANGEFEED FOR foo into $1 WITH compression='gzip'`,
		`kafka://nope`)

	sqlDB.ExpectErrWithTimeout(
		t, `required column idk not present on table foo`,
		`CREATE CHANGEFEED FOR foo into $1 WITH headers_json_column_name='idk'`,
		`kafka://nope`)

	sqlDB.ExpectErrWithTimeout(
		t, `column b of type string does not match required type json`,
		`CREATE CHANGEFEED FOR foo into $1 WITH headers_json_column_name='b'`,
		`kafka://nope`)

	sqlDB.ExpectErrWithTimeout(
		t, `this sink is incompatible with option headers_json_column_name`,
		`CREATE CHANGEFEED FOR foo into $1 WITH headers_json_column_name='j'`,
		`nodelocal://.`)

	sqlDB.ExpectErrWithTimeout(
		t, `headers_json_column_name is only usable with format=json/avro`,
		`CREATE CHANGEFEED FOR foo into $1 WITH headers_json_column_name='j', format=csv, initial_scan='only'`,
		`kafka://nope`)

	sqlDB.ExpectErrWithTimeout(
		t, `envelope=enriched is incompatible with SELECT statement`,
		`CREATE CHANGEFEED INTO 'null://' WITH envelope=enriched AS SELECT * from foo`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `envelope=enriched is only usable with format=json/avro`,
		`CREATE CHANGEFEED FOR foo INTO 'null://' WITH envelope=enriched, format=csv, initial_scan='only'`,
	)
	sqlDB.ExpectErrWithTimeout(
		// I also would have accepted "this sink is incompatible with envelope=enriched".
		t, `envelope=enriched is only usable with format=json/avro`,
		`CREATE CHANGEFEED FOR foo INTO 'nodelocal://.' WITH envelope=enriched, format=parquet`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `this sink is incompatible with envelope=enriched`,
		`CREATE CHANGEFEED FOR foo INTO 'pulsar://.' WITH envelope=enriched`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `enriched_properties is only usable with envelope=enriched`,
		`CREATE CHANGEFEED FOR foo INTO 'null://' WITH enriched_properties='schema'`,
	)
	sqlDB.ExpectErrWithTimeout(
		t, `unknown enriched_properties: potato, valid values are: source, schema`,
		`CREATE CHANGEFEED FOR foo INTO 'null://' WITH enriched_properties='schema,potato'`,
	)

	t.Run("sinkless enriched non-json", func(t *testing.T) {
		skip.WithIssue(t, 130949, "sinkless feed validations are subpar")
		sqlDB.ExpectErrWithTimeout(
			t, `some error`,
			`CREATE CHANGEFEED FOR foo WITH envelope=enriched, format=avro, confluent_schema_registry='http://localhost:8888'`,
		)
	})

	t.Run("enriched alters", func(t *testing.T) {
		res := sqlDB.QueryStr(t, `CREATE CHANGEFEED FOR FOO INTO 'null://' WITH envelope=enriched`)
		jobIDStr := res[0][0]
		jobID, err := strconv.Atoi(jobIDStr)
		require.NoError(t, err)
		sqlDB.Exec(t, `PAUSE JOB $1`, jobID)
		waitForJobState(sqlDB, t, catpb.JobID(jobID), jobs.StatePaused)
		sqlDB.ExpectErrWithTimeout(
			t, `envelope=enriched is only usable with format=json/avro`,
			`ALTER CHANGEFEED $1 SET format=parquet`, jobIDStr,
		)
	})

}

func TestChangefeedDescription(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Intentionally don't use the TestFeedFactory because we want to
	// control the placeholders.
	s, stopServer := makeServerWithOptions(t, makeOptions(t, withAllowChangefeedErr("create strange changefeeds that don't actually run")))
	defer stopServer()

	sqlDB := sqlutils.MakeSQLRunner(s.DB)
	// Create enum to ensure enum values displayed correctly in the summary.
	sqlDB.Exec(t, `CREATE TYPE status AS ENUM ('open', 'closed', 'inactive')`)
	sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, status status)`)
	sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)

	sink, cleanup := pgurlutils.PGUrl(t, s.Server.SQLAddr(), t.Name(), url.User(username.RootUser))
	defer cleanup()
	sink.Scheme = changefeedbase.SinkSchemeExperimentalSQL
	sink.Path = `d`

	redactedSink := strings.Replace(sink.String(), username.RootUser, `redacted`, 1)
	for _, tc := range []struct {
		create string
		descr  string
	}{
		{
			create: "CREATE CHANGEFEED FOR foo INTO $1 WITH updated, envelope = $2",
			descr:  `CREATE CHANGEFEED FOR TABLE foo INTO '` + redactedSink + `' WITH OPTIONS (envelope = 'wrapped', updated)`,
		},
		{
			create: "CREATE CHANGEFEED FOR public.foo INTO $1 WITH updated, envelope = $2",
			descr:  `CREATE CHANGEFEED FOR TABLE public.foo INTO '` + redactedSink + `' WITH OPTIONS (envelope = 'wrapped', updated)`,
		},
		{
			create: "CREATE CHANGEFEED FOR d.public.foo INTO $1 WITH updated, envelope = $2",
			descr:  `CREATE CHANGEFEED FOR TABLE d.public.foo INTO '` + redactedSink + `' WITH OPTIONS (envelope = 'wrapped', updated)`,
		},
		{
			// TODO(#85143): remove schema_change_policy='stop' from this test.
			create: "CREATE CHANGEFEED INTO $1 WITH updated, envelope = $2, schema_change_policy='stop' AS SELECT a FROM foo WHERE a % 2 = 0",
			descr:  `CREATE CHANGEFEED INTO '` + redactedSink + `' WITH OPTIONS (envelope = 'wrapped', schema_change_policy = 'stop', updated) AS SELECT a FROM foo WHERE (a % 2) = 0`,
		},
		{
			// TODO(#85143): remove schema_change_policy='stop' from this test.
			create: "CREATE CHANGEFEED INTO $1 WITH updated, envelope = $2, schema_change_policy='stop' AS SELECT a FROM public.foo AS bar WHERE a % 2 = 0",
			descr:  `CREATE CHANGEFEED INTO '` + redactedSink + `' WITH OPTIONS (envelope = 'wrapped', schema_change_policy = 'stop', updated) AS SELECT a FROM public.foo AS bar WHERE (a % 2) = 0`,
		},
		{
			// TODO(#85143): remove schema_change_policy='stop' from this test.
			create: "CREATE CHANGEFEED INTO $1 WITH updated, envelope = $2, schema_change_policy='stop' AS SELECT a FROM foo WHERE status IN ('open', 'closed')",
			descr:  `CREATE CHANGEFEED INTO '` + redactedSink + `' WITH OPTIONS (envelope = 'wrapped', schema_change_policy = 'stop', updated) AS SELECT a FROM foo WHERE status IN ('open', 'closed')`,
		},
	} {
		t.Run(tc.create, func(t *testing.T) {
			var jobID jobspb.JobID
			sqlDB.QueryRow(t, tc.create, sink.String(), `wrapped`).Scan(&jobID)

			var description string
			sqlDB.QueryRow(t,
				`SELECT description FROM [SHOW JOB $1]`, jobID,
			).Scan(&description)

			require.Equal(t, tc.descr, description)
		})
	}
}

func TestChangefeedKafkaV1ConnectionError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		KafkaV2Enabled.Override(context.Background(), &s.Server.ClusterSettings().SV, false)
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo(id int primary key, s string)`)
		sqlDB.Exec(t, `INSERT INTO foo(id, s) VALUES (0, 'hello'), (1, null)`)
		_, err := f.Feed(`CREATE CHANGEFEED FOR foo`)
		require.ErrorContains(t, err, "client has run out of available brokers")
	}
	cdcTest(t, testFn, feedTestForceSink("kafka"), feedTestForceKafkaV1ConnectionCheck)
}

func TestChangefeedPanicRecovery(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	// Panics can mess with the test setup so run these each in their own test.

	defer cdceval.TestingDisableFunctionsBlacklist()()

	prep := func(t *testing.T, sqlDB *sqlutils.SQLRunner) {
		sqlDB.Exec(t, `CREATE TABLE foo(id int primary key, s string)`)
		sqlDB.Exec(t, `INSERT INTO foo(id, s) VALUES (0, 'hello'), (1, null)`)
	}

	waitForFeedErr := func(t *testing.T, feed cdctest.TestFeed, timeout time.Duration) error {
		start := timeutil.Now()
		for {
			if time.Since(start) >= timeout {
				t.Fatalf("feed did not return error before timeout of %s", timeout)
			}
			_, err := feed.Next()
			if err != nil {
				return err
			}
		}
	}

	cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		prep(t, sqlDB)
		// Check that disallowed expressions have a good error message.
		// Also regression test for https://github.com/cockroachdb/cockroach/issues/90416
		sqlDB.ExpectErrWithTimeout(t, "sub-query expressions not supported by CDC",
			`CREATE CHANGEFEED WITH schema_change_policy='stop' AS SELECT 1 FROM foo WHERE EXISTS (SELECT true)`)
	})

	// Check that all panics while evaluating the WHERE clause in an expression are recovered from.
	// NB: REPAIRCLUSTER is required to use crdb_internal.force_panic.
	cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		prep(t, sqlDB)
		foo := feed(t, f,
			`CREATE CHANGEFEED WITH schema_change_policy='stop' AS SELECT 1 FROM foo WHERE crdb_internal.force_panic('wat') IS NULL`)
		defer closeFeed(t, foo)
		err := waitForFeedErr(t, foo, 2*time.Minute)
		require.ErrorContains(t, err, "error evaluating CDC expression", "expected panic recovery while evaluating WHERE clause")
	}, feedTestAdditionalSystemPrivs("REPAIRCLUSTER"), withAllowChangefeedErr("expects error"))

	// Check that all panics while evaluating the SELECT clause in an expression are recovered from.
	// NB: REPAIRCLUSTER is required to use crdb_internal.force_panic.
	cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		prep(t, sqlDB)
		foo := feed(t, f,
			`CREATE CHANGEFEED WITH schema_change_policy='stop' AS SELECT crdb_internal.force_panic('wat') FROM foo`)
		defer closeFeed(t, foo)
		err := waitForFeedErr(t, foo, 2*time.Minute)
		require.ErrorContains(t, err, "error evaluating CDC expression", "expected panic recovery while evaluating SELECT clause")
	}, feedTestAdditionalSystemPrivs("REPAIRCLUSTER"), withAllowChangefeedErr("expects error"))
}

func TestChangefeedPauseUnpause(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b'), (4, 'c'), (7, 'd'), (8, 'e')`)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH resolved`)
		defer closeFeed(t, foo)

		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"a": 1, "b": "a"}}`,
			`foo: [2]->{"after": {"a": 2, "b": "b"}}`,
			`foo: [4]->{"after": {"a": 4, "b": "c"}}`,
			`foo: [7]->{"after": {"a": 7, "b": "d"}}`,
			`foo: [8]->{"after": {"a": 8, "b": "e"}}`,
		})

		// Wait for the high-water mark on the job to be updated after the initial
		// scan, to make sure we don't get the initial scan data again.
		m, err := foo.Next()
		if err != nil {
			t.Fatal(err)
		} else if m.Key != nil {
			t.Fatalf(`expected a resolved timestamp got %s: %s->%s`, m.Topic, m.Key, m.Value)
		}

		feedJob := foo.(cdctest.EnterpriseTestFeed)
		sqlDB.Exec(t, `PAUSE JOB $1`, feedJob.JobID())
		// PAUSE JOB only requests the job to be paused. Block until it's paused.
		waitForJobState(sqlDB, t, feedJob.JobID(), jobs.StatePaused)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (16, 'f')`)
		sqlDB.Exec(t, `RESUME JOB $1`, feedJob.JobID())
		assertPayloads(t, foo, []string{
			`foo: [16]->{"after": {"a": 16, "b": "f"}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks)
}

func TestChangefeedPauseUnpauseCursorAndInitialScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	skip.UnderRaceWithIssue(t, 67565)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b'), (4, 'c'), (7, 'd'), (8, 'e')`)
		var tsStr string
		sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp() from foo`).Scan(&tsStr)
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo `+
			`WITH initial_scan, resolved='10ms', cursor='`+tsStr+`'`)
		defer closeFeed(t, foo)

		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"a": 1, "b": "a"}}`,
			`foo: [2]->{"after": {"a": 2, "b": "b"}}`,
			`foo: [4]->{"after": {"a": 4, "b": "c"}}`,
			`foo: [7]->{"after": {"a": 7, "b": "d"}}`,
			`foo: [8]->{"after": {"a": 8, "b": "e"}}`,
		})

		// Wait for the high-water mark on the job to be updated after the initial
		// scan, to make sure we don't get the initial scan data again.
		expectResolvedTimestamp(t, foo)
		expectResolvedTimestamp(t, foo)

		feedJob := foo.(cdctest.EnterpriseTestFeed)
		require.NoError(t, feedJob.Pause())

		foo.(seenTracker).reset()
		sqlDB.Exec(t, `INSERT INTO foo VALUES (16, 'f')`)
		require.NoError(t, feedJob.Resume())
		assertPayloads(t, foo, []string{
			`foo: [16]->{"after": {"a": 16, "b": "f"}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks)
}

func TestCDCPrev(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)
		// TODO(#85143): remove schema_change_policy='stop' from this test.
		foo := feed(t, f, `CREATE CHANGEFEED WITH envelope='row', schema_change_policy='stop' AS SELECT (cdc_prev).b AS old FROM foo`)
		defer closeFeed(t, foo)

		// cdc_prev values are null during initial scan
		assertPayloads(t, foo, []string{
			`foo: [0]->{"old": null}`,
		})

		// cdc_prev values are null for an insert event
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'original')`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"old": null}`,
		})

		// cdc_prev returns the previous value on an update
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (1, 'updated')`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"old": "original"}`,
		})
	}

	// envelope=wrapped is required for some sinks, but
	// envelope=wrapped output with cdc_prev looks silly.
	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestManyChangefeedsOneTable(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'init')`)

		foo1 := feed(t, f, `CREATE CHANGEFEED FOR foo WITH diff`)
		defer closeFeed(t, foo1)
		foo2 := feed(t, f, `CREATE CHANGEFEED FOR foo`) // without diff
		defer closeFeed(t, foo2)
		foo3 := feed(t, f, `CREATE CHANGEFEED FOR foo WITH diff`)
		defer closeFeed(t, foo3)

		// Make sure all the changefeeds are going.
		assertPayloads(t, foo1, []string{`foo: [0]->{"after": {"a": 0, "b": "init"}, "before": null}`})
		assertPayloads(t, foo2, []string{`foo: [0]->{"after": {"a": 0, "b": "init"}}`})
		assertPayloads(t, foo3, []string{`foo: [0]->{"after": {"a": 0, "b": "init"}, "before": null}`})

		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'v0')`)
		assertPayloads(t, foo1, []string{
			`foo: [0]->{"after": {"a": 0, "b": "v0"}, "before": {"a": 0, "b": "init"}}`,
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'v1')`)
		assertPayloads(t, foo1, []string{
			`foo: [1]->{"after": {"a": 1, "b": "v1"}, "before": null}`,
		})
		assertPayloads(t, foo2, []string{
			`foo: [0]->{"after": {"a": 0, "b": "v0"}}`,
			`foo: [1]->{"after": {"a": 1, "b": "v1"}}`,
		})

		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'v2')`)
		assertPayloads(t, foo1, []string{
			`foo: [0]->{"after": {"a": 0, "b": "v2"}, "before": {"a": 0, "b": "v0"}}`,
		})
		assertPayloads(t, foo2, []string{
			`foo: [0]->{"after": {"a": 0, "b": "v2"}}`,
		})
		assertPayloads(t, foo3, []string{
			`foo: [0]->{"after": {"a": 0, "b": "v0"}, "before": {"a": 0, "b": "init"}}`,
			`foo: [0]->{"after": {"a": 0, "b": "v2"}, "before": {"a": 0, "b": "v0"}}`,
			`foo: [1]->{"after": {"a": 1, "b": "v1"}, "before": null}`,
		})
	}

	cdcTest(t, testFn)
}

func TestUnspecifiedPrimaryKey(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT)`)
		var id0 int
		sqlDB.QueryRow(t, `INSERT INTO foo VALUES (0) RETURNING rowid`).Scan(&id0)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, foo)

		var id1 int
		sqlDB.QueryRow(t, `INSERT INTO foo VALUES (1) RETURNING rowid`).Scan(&id1)

		assertPayloads(t, foo, []string{
			fmt.Sprintf(`foo: [%d]->{"after": {"a": 0, "rowid": %d}}`, id0, id0),
			fmt.Sprintf(`foo: [%d]->{"after": {"a": 1, "rowid": %d}}`, id1, id1),
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedTelemetry(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO bar VALUES (1)`)

		// Reset the counts.
		_ = telemetry.GetFeatureCounts(telemetry.Raw, telemetry.ResetCounts)

		// Start some feeds (and read from them to make sure they've started.
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, foo)
		fooBar := feed(t, f, `CREATE CHANGEFEED FOR foo, bar WITH format=json`)
		defer closeFeed(t, fooBar)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"a": 1}}`,
		})
		assertPayloads(t, fooBar, []string{
			`bar: [1]->{"after": {"a": 1}}`,
			`foo: [1]->{"after": {"a": 1}}`,
		})

		var expectedSink string
		if strings.Contains(t.Name(), `sinkless`) || strings.Contains(t.Name(), `poller`) {
			expectedSink = `sinkless`
		} else {
			expectedSink = `experimental-sql`
		}

		counts := telemetry.GetFeatureCounts(telemetry.Raw, telemetry.ResetCounts)
		require.Equal(t, int32(2), counts[`changefeed.create.sink.`+expectedSink])
		require.Equal(t, int32(2), counts[`changefeed.create.format.json`])
		require.Equal(t, int32(1), counts[`changefeed.create.num_tables.1`])
		require.Equal(t, int32(1), counts[`changefeed.create.num_tables.2`])
	}

	cdcTest(t, testFn, feedTestForceSink("sinkless"))
	cdcTest(t, testFn, feedTestForceSink("enterprise"))
}

func TestChangefeedContinuousTelemetry(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		// Hack: since setting a zero value disabled, set a negative value to ensure we always log.
		interval := -10 * time.Millisecond
		continuousTelemetryInterval.Override(context.Background(), &s.Server.ClusterSettings().SV, interval)

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (id INT PRIMARY KEY)`)
		// NB: In order for this test to work for sinkless feeds, we must
		// have at least one row before creating the feed.
		sqlDB.Exec(t, `INSERT INTO foo VALUES (-1)`)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, foo)
		var jobID jobspb.JobID
		if foo, ok := foo.(cdctest.EnterpriseTestFeed); ok {
			jobID = foo.JobID()
		}

		for i := 0; i < 5; i++ {
			beforeCreate := timeutil.Now()
			sqlDB.Exec(t, fmt.Sprintf(`INSERT INTO foo VALUES (%d) RETURNING cluster_logical_timestamp()`, i))
			verifyLogsWithEmittedBytesAndMessages(t, jobID, beforeCreate.UnixNano(), interval.Nanoseconds(), false)
		}
	}

	cdcTest(t, testFn)
}

type testTelemetryLogger struct {
	telemetryLogger
	id                      int32
	afterIncEmittedCounters func(numMessages int, numBytes int)
}

var _ telemetryLogger = (*testTelemetryLogger)(nil)

func (t *testTelemetryLogger) incEmittedCounters(numMessages int, numBytes int) {
	t.telemetryLogger.incEmittedCounters(numMessages, numBytes)
	t.afterIncEmittedCounters(numMessages, numBytes)
}

func (t *testTelemetryLogger) maybeFlushLogs() {
	if t.id == 1 {
		t.telemetryLogger.maybeFlushLogs()
	}
}

func (t *testTelemetryLogger) close() {
	if t.id == 1 {
		t.telemetryLogger.close()
	}
}

func TestChangefeedContinuousTelemetryOnTermination(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		interval := 24 * time.Hour
		continuousTelemetryInterval.Override(context.Background(), &s.Server.ClusterSettings().SV, interval)
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (id INT PRIMARY KEY)`)
		// NB: In order for this test to work for sinkless feeds, we must
		// have at least one row before creating the feed.
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)

		var seen atomic.Bool
		waitForIncEmittedCounters := func() error {
			if !seen.Load() {
				return errors.Newf("emitted counters have not been incremented yet")
			}
			return nil
		}
		var numPeriodicTelemetryLogger atomic.Int32
		// Synchronization to prevent a race between the changefeed closing
		// and the telemetry logger getting emitted counts after messages
		// have been emitted to the sink.
		s.TestingKnobs.DistSQL.(*execinfra.TestingKnobs).Changefeed.(*TestingKnobs).
			WrapTelemetryLogger = func(logger telemetryLogger) telemetryLogger {
			return &testTelemetryLogger{
				telemetryLogger: logger,
				afterIncEmittedCounters: func(numMessages int, _ int) {
					if numMessages > 0 {
						seen.Store(true)
					}
				},
				id: numPeriodicTelemetryLogger.Add(1),
			}
		}

		// Insert a row and wait for logs to be created.
		beforeFirstLog := timeutil.Now()
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		var jobID jobspb.JobID
		if foo, ok := foo.(cdctest.EnterpriseTestFeed); ok {
			jobID = foo.JobID()
		}
		testutils.SucceedsSoon(t, waitForIncEmittedCounters)
		verifyLogsWithEmittedBytesAndMessages(t, jobID, beforeFirstLog.UnixNano(), interval.Nanoseconds(), false /* closing */)

		// Insert more rows. No logs should be created for these since we recently
		// published them above and the interval is 24h.
		afterFirstLog := timeutil.Now()
		seen.Store(false)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (2)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (3)`)
		assertPayloads(t, foo, []string{
			`foo: [1]->{"after": {"id": 1}}`,
			`foo: [2]->{"after": {"id": 2}}`,
			`foo: [3]->{"after": {"id": 3}}`,
		})
		testutils.SucceedsSoon(t, waitForIncEmittedCounters)

		// Close the changefeed and ensure logs were created after closing.
		require.NoError(t, foo.Close())

		if numPeriodicTelemetryLogger.Load() > 1 {
			t.Log("transient error")
		}

		verifyLogsWithEmittedBytesAndMessages(
			t, jobID, afterFirstLog.UnixNano(), interval.Nanoseconds(), true, /* closing */
		)
	}

	cdcTest(t, testFn)
}

func TestChangefeedContinuousTelemetryDifferentJobs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		// Hack: since setting a zero value disabled, set a negative value to ensure we always log.
		interval := -100 * time.Millisecond
		continuousTelemetryInterval.Override(context.Background(), &s.Server.ClusterSettings().SV, interval)
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (id INT PRIMARY KEY)`)
		sqlDB.Exec(t, `CREATE TABLE foo2 (id INT PRIMARY KEY)`)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		foo2 := feed(t, f, `CREATE CHANGEFEED FOR foo2`)
		job1 := foo.(cdctest.EnterpriseTestFeed).JobID()
		job2 := foo2.(cdctest.EnterpriseTestFeed).JobID()

		beforeInsert := timeutil.Now()
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)
		sqlDB.Exec(t, `INSERT INTO foo2 VALUES (1)`)
		verifyLogsWithEmittedBytesAndMessages(t, job1, beforeInsert.UnixNano(), interval.Nanoseconds(), false)
		verifyLogsWithEmittedBytesAndMessages(t, job2, beforeInsert.UnixNano(), interval.Nanoseconds(), false)
		require.NoError(t, foo.Close())

		beforeInsert = timeutil.Now()
		sqlDB.Exec(t, `INSERT INTO foo2 VALUES (2)`)
		verifyLogsWithEmittedBytesAndMessages(t, job2, beforeInsert.UnixNano(), interval.Nanoseconds(), false)
		require.NoError(t, foo2.Close())
	}

	cdcTest(t, testFn, feedTestOmitSinks("sinkless"))
}

func TestChangefeedHandlesDrainingNodes(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t, "Takes too long with race enabled")

	var shouldDrain int32
	knobs := base.TestingKnobs{
		DistSQL: &execinfra.TestingKnobs{
			DrainFast:  true,
			Changefeed: &TestingKnobs{},
			Flowinfra: &flowinfra.TestingKnobs{
				FlowRegistryDraining: func() bool {
					if atomic.LoadInt32(&shouldDrain) > 0 {
						atomic.StoreInt32(&shouldDrain, 0)
						return true
					}
					return false
				},
			},
		},
		JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
	}

	sinkDir, cleanupFn := testutils.TempDir(t)
	defer cleanupFn()

	tc := serverutils.StartCluster(t, 4, base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			// Test uses SPLIT AT, which isn't currently supported for
			// secondary tenants. Tracked with #76378.
			DefaultTestTenant: base.TODOTestTenantDisabled,
			UseDatabase:       "test",
			Knobs:             knobs,
			ExternalIODir:     sinkDir,
		}})
	defer tc.Stopper().Stop(context.Background())

	db := tc.ServerConn(1)
	sqlDB := sqlutils.MakeSQLRunner(db)
	serverutils.SetClusterSetting(t, tc, "kv.rangefeed.enabled", true)
	serverutils.SetClusterSetting(t, tc, "kv.closed_timestamp.target_duration", time.Second)
	serverutils.SetClusterSetting(t, tc, "changefeed.experimental_poll_interval", 10*time.Millisecond)

	sqlutils.CreateTable(
		t, db, "foo",
		"k INT PRIMARY KEY, v INT",
		10,
		sqlutils.ToRowFn(sqlutils.RowIdxFn, sqlutils.RowModuloFn(2)),
	)

	// Introduce 4 splits to get 5 ranges.  We need multiple ranges in order to run distributed
	// flow.
	sqlDB.Exec(t, "ALTER TABLE test.foo SPLIT AT (SELECT i*2 FROM generate_series(1, 4) AS g(i))")
	sqlDB.Exec(t, "ALTER TABLE test.foo SCATTER")

	// Create a factory which executes the CREATE CHANGEFEED statement on server 0.
	// This statement should fail, but the job itself ought to be created.
	// After some time, that job should be adopted by another node, and executed successfully.
	//
	// We use feedTestUseRootUserConnection to prevent the
	// feed factory from trying to create a test user. Because the registry is draining, creating the test user
	// will fail and the test will fail prematurely.
	f, closeSink := makeFeedFactory(t, randomSinkType(t, feedTestEnterpriseSinks), tc.Server(1), tc.ServerConn(0),
		feedTestUseRootUserConnection)
	defer closeSink()

	atomic.StoreInt32(&shouldDrain, 1)
	feed := feed(t, f, "CREATE CHANGEFEED FOR foo")
	defer closeFeed(t, feed)

	jobID := feed.(cdctest.EnterpriseTestFeed).JobID()
	registry := tc.Server(1).JobRegistry().(*jobs.Registry)
	loadProgress := func() jobspb.Progress {
		job, err := registry.LoadJob(context.Background(), jobID)
		require.NoError(t, err)
		return job.Progress()
	}

	// Wait until highwater advances.
	testutils.SucceedsSoon(t, func() error {
		progress := loadProgress()
		if hw := progress.GetHighWater(); hw == nil || hw.IsEmpty() {
			return errors.New("waiting for highwater")
		}
		return nil
	})
}

// Verifies changefeed updates checkpoint when cluster undergoes rolling
// restart.
func TestChangefeedHandlesRollingRestart(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer testingUseFastRetry()()

	skip.UnderRace(t, "Takes too long with race enabled")

	const numNodes = 4

	opts := makeOptions(t)
	opts.forceRootUserConnection = true
	defer addCloudStorageOptions(t, &opts)()

	var checkpointHW atomic.Value
	checkpointHW.Store(hlc.Timestamp{})
	var nodeDrainChannels [numNodes]atomic.Value // of chan struct

	proceed := make(chan struct{}, 1)
	errCh := make(chan error, 1)

	makeTestServerArgs := func(n int) base.TestServerArgs {
		nodeDrainChannels[n].Store(make(chan struct{}))

		return base.TestServerArgs{
			UseDatabase: "test",
			Knobs: base.TestingKnobs{
				DistSQL: &execinfra.TestingKnobs{
					DrainFast: true,
					Changefeed: &TestingKnobs{
						// Filter out draining nodes; normally we rely on dist sql planner
						// to do that for us.
						FilterDrainingNodes: func(
							partitions []sql.SpanPartition, draining []roachpb.NodeID,
						) ([]sql.SpanPartition, error) {
							toSkip := map[roachpb.NodeID]struct{}{}
							for _, n := range draining {
								toSkip[n] = struct{}{}
							}
							var filtered []sql.SpanPartition
							var filteredSpans []roachpb.Span
							for _, p := range partitions {
								if _, s := toSkip[roachpb.NodeID(p.SQLInstanceID)]; s {
									filteredSpans = append(filteredSpans, p.Spans...)
								} else {
									filtered = append(filtered, p)
								}
							}
							if len(filtered) == 0 {
								return nil, errors.AssertionFailedf("expected non-empty filtered span partitions")
							}
							if len(filteredSpans) == 0 {
								return partitions, nil
							}
							filtered[0].Spans = append(filtered[0].Spans, filteredSpans...)
							return filtered, nil
						},

						// Disable all checkpoints.  This test verifies that even when
						// checkpoints are behind, changefeed can handle rolling restarts by
						// utilizing the most up-to-date checkpoint information transmitted by
						// the aggregators to the change frontier processor.
						ShouldCheckpointToJobRecord: func(hw hlc.Timestamp) bool {
							checkpointHW.Store(hw)
							return false
						},

						OnDrain: func() <-chan struct{} {
							return nodeDrainChannels[n].Load().(chan struct{})
						},

						BeforeDistChangefeed: func() {
							ctx, cancel := context.WithTimeout(context.Background(), time.Second*60)
							defer cancel()
							select {
							case <-proceed:
							case <-ctx.Done():
								t.Fatal("did not get signal to proceed")
							}
						},
						// Handle transient changefeed error.  We expect to see node drain error.
						// When we do, notify drainNotification, and reset node drain channel.
						HandleDistChangefeedError: func(err error) error {
							errCh <- err
							return err
						},
					},
				},
				JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
			},
			ExternalIODir: opts.externalIODir,
		}
	}

	tc := serverutils.StartCluster(t, numNodes, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		ServerArgsPerNode: func() map[int]base.TestServerArgs {
			perNode := make(map[int]base.TestServerArgs)
			for i := 0; i < numNodes; i++ {
				perNode[i] = makeTestServerArgs(i)
			}
			return perNode
		}(),
		ServerArgs: base.TestServerArgs{
			// Test uses SPLIT AT, which isn't currently supported for
			// secondary tenants. Tracked with #76378.
			DefaultTestTenant: base.TODOTestTenantDisabled,
		},
	})
	defer tc.Stopper().Stop(context.Background())

	db := tc.ServerConn(1)
	sqlDB := sqlutils.MakeSQLRunner(db)
	serverutils.SetClusterSetting(t, tc, "changefeed.shutdown_checkpoint.enabled", true)
	serverutils.SetClusterSetting(t, tc, "kv.rangefeed.enabled", true)
	serverutils.SetClusterSetting(t, tc, "kv.closed_timestamp.target_duration", 10*time.Millisecond)
	serverutils.SetClusterSetting(t, tc, "changefeed.experimental_poll_interval", 10*time.Millisecond)
	serverutils.SetClusterSetting(t, tc, "changefeed.aggregator.heartbeat", 10*time.Millisecond)
	// Randomizing replica assignment can cause timeouts or other
	// failures due to assumptions in the testing knobs about balanced
	// assignments.
	serverutils.SetClusterSetting(t, tc, "changefeed.random_replica_selection.enabled", false)

	sqlutils.CreateTable(
		t, db, "foo",
		"k INT PRIMARY KEY, v INT",
		400,
		sqlutils.ToRowFn(sqlutils.RowIdxFn, sqlutils.RowModuloFn(2)),
	)

	tableDesc := desctestutils.TestingGetPublicTableDescriptor(
		tc.Server(0).DB(), keys.SystemSQLCodec, "test", "foo")
	tc.SplitTable(t, tableDesc, []serverutils.SplitPoint{
		{TargetNodeIdx: 1, Vals: []interface{}{100}},
		{TargetNodeIdx: 2, Vals: []interface{}{200}},
		{TargetNodeIdx: 3, Vals: []interface{}{300}},
	})

	// Create a factory which executes the CREATE CHANGEFEED statement on server 1.
	// Feed logic (helpers) running on node 4.

	f, closeSink := makeFeedFactoryWithOptions(t, "cloudstorage", tc.Server(3), tc.ServerConn(0), opts)
	defer closeSink()

	proceed <- struct{}{} // Allow changefeed to start.
	feed := feed(t, f, "CREATE CHANGEFEED FOR foo WITH initial_scan='no', min_checkpoint_frequency='100ms'")
	defer closeFeed(t, feed)

	jf := feed.(cdctest.EnterpriseTestFeed)

	// waitCheckpointAttempt waits until an attempt to checkpoint is made.
	waitCheckpoint := func(minHW hlc.Timestamp) {
		t.Helper()
		testutils.SucceedsSoon(t, func() error {
			if minHW.Less(checkpointHW.Load().(hlc.Timestamp)) {
				return nil
			}
			return errors.New("still waiting for checkpoint")
		})
	}

	// Shutdown each node, one at a time.
	// Insert few values on each iteration.
	// Even though checkpointing is disabled via testing knobs,
	// the drain logic should preserve up-to-date restart information.
	for i := 0; i < numNodes; i++ {
		beforeInsert := tc.Server(3).Clock().Now()
		sqlDB.Exec(t, "UPDATE test.foo SET v=$1 WHERE k IN (10, 110, 220, 330)", 42+i)
		assertPayloads(t, feed, []string{
			fmt.Sprintf(`foo: [10]->{"after": {"k": 10, "v": %d}}`, 42+i),
			fmt.Sprintf(`foo: [110]->{"after": {"k": 110, "v": %d}}`, 42+i),
			fmt.Sprintf(`foo: [220]->{"after": {"k": 220, "v": %d}}`, 42+i),
			fmt.Sprintf(`foo: [330]->{"after": {"k": 330, "v": %d}}`, 42+i),
		})

		// Wait for a checkpoint attempt.  The checkpoint will not be committed
		// to the jobs table (due to testing knobs), but when we trigger drain
		// below, we expect correct restart information to be checkpointed anyway.
		waitCheckpoint(beforeInsert)

		// Send drain notification.
		close(nodeDrainChannels[i].Load().(chan struct{}))

		// Changefeed should encounter node draining error.
		var err error
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		select {
		case err = <-errCh:
		case <-ctx.Done():
			t.Fatal("could not get draining error on channel")
		}
		cancel()
		require.True(t, errors.Is(err, changefeedbase.ErrNodeDraining))

		// Reset drain channel.
		nodeDrainChannels[i].Store(make(chan struct{}))

		// Even though checkpointing was disabled, when we drain, an attempt is
		// made to persist up-to-date checkpoint.
		require.NoError(t, jf.WaitForHighWaterMark(beforeInsert))

		// Let the retry proceed.
		ctx, cancel = context.WithTimeout(context.Background(), time.Second*60)
		select {
		case proceed <- struct{}{}:
		case <-ctx.Done():
			t.Fatal("could not send signal to proceed")
		}
		cancel()
	}
}

// TestChangefeedTimelyResolvedTimestampUpdatePostRollingRestart verifies that
// a changefeed over a large number of quiesced ranges is able to quickly
// advance its resolved timestamp after a rolling restart. At the lowest level,
// the test ensures that lease acquisitions required to advance the closed
// timestamp of the constituent changefeed ranges is fast.
func TestChangefeedTimelyResolvedTimestampUpdatePostRollingRestart(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Add verbose logging to help debug future failures.
	require.NoError(t, log.SetVModule("changefeed_processors=1,replica_rangefeed=2,"+
		"replica_range_lease=3,raft=3"))

	// This test requires many range splits, which can be slow under certain test
	// conditions. Skip potentially slow tests.
	skip.UnderDeadlock(t)
	skip.UnderRace(t)

	opts := makeOptions(t)
	defer addCloudStorageOptions(t, &opts)()
	opts.forceRootUserConnection = true
	defer changefeedbase.TestingSetDefaultMinCheckpointFrequency(testSinkFlushFrequency)()
	defer testingUseFastRetry()()
	const numNodes = 3

	stickyVFSRegistry := fs.NewStickyRegistry()
	listenerReg := listenerutil.NewListenerRegistry()
	defer listenerReg.Close()

	perServerKnobs := make(map[int]base.TestServerArgs, numNodes)
	for i := 0; i < numNodes; i++ {
		perServerKnobs[i] = base.TestServerArgs{
			Knobs: base.TestingKnobs{
				DistSQL: &execinfra.TestingKnobs{
					Changefeed: &TestingKnobs{},
				},
				JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
				Server: &server.TestingKnobs{
					StickyVFSRegistry: stickyVFSRegistry,
				},
			},
			ExternalIODir: opts.externalIODir,
			UseDatabase:   "d",
		}
	}

	tc := serverutils.StartCluster(t, numNodes,
		base.TestClusterArgs{
			ServerArgsPerNode: perServerKnobs,
			ServerArgs: base.TestServerArgs{
				// Test uses SPLIT AT, which isn't currently supported for
				// secondary tenants. Tracked with #76378.
				DefaultTestTenant: base.TODOTestTenantDisabled,
			},
			ReusableListenerReg: listenerReg,
		})
	defer tc.Stopper().Stop(context.Background())

	db := tc.ServerConn(1)
	sqlDB := sqlutils.MakeSQLRunner(db)
	serverutils.SetClusterSetting(t, tc, "kv.rangefeed.enabled", true)

	// Create a table with 1000 ranges.
	sqlDB.ExecMultiple(t,
		`CREATE DATABASE d;`,
		`CREATE TABLE d.foo (k INT PRIMARY KEY);`,
		`INSERT INTO d.foo (k) SELECT * FROM generate_series(1, 1000);`,
		`ALTER TABLE d.foo SPLIT AT (SELECT * FROM generate_series(1, 1000));`,
	)

	// Wait for ranges to quiesce.
	testutils.SucceedsSoon(t, func() error {
		for i := range tc.NumServers() {
			store, err := tc.Server(i).GetStores().(*kvserver.Stores).GetStore(tc.Server(i).GetFirstStoreID())
			require.NoError(t, err)
			numQuiescent := store.Metrics().QuiescentCount.Value()
			numQualifyForQuiesence := store.Metrics().LeaseEpochCount.Value()
			if numQuiescent < numQualifyForQuiesence {
				return errors.Newf(
					"waiting for ranges to quiesce on node %d; quiescent: %d; should quiesce: %d",
					tc.Server(i).NodeID(), numQuiescent, numQualifyForQuiesence,
				)
			}
		}
		return nil
	})

	// Capture the pre-restart timestamp. We'll use this as the start time for the
	// changefeed later.
	var tsLogical string
	sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp()`).Scan(&tsLogical)

	// Perform the rolling restart.
	require.NoError(t, tc.Restart())

	// For validation, the test requires an enterprise feed.
	feedTestEnterpriseSinks(&opts)
	sinkType := randomSinkTypeWithOptions(opts)
	f, closeSink := makeFeedFactoryWithOptions(t, sinkType, tc, tc.ServerConn(0), opts)
	defer closeSink()
	// The end time is captured post restart. The changefeed spans from before the
	// restart to after.
	endTime := tc.Server(0).Clock().Now().AddDuration(5 * time.Second)
	testFeed := feed(t, f, `CREATE CHANGEFEED FOR d.foo WITH cursor=$1, end_time=$2`,
		tsLogical, eval.TimestampToDecimalDatum(endTime).String())
	defer closeFeed(t, testFeed)

	defer DiscardMessages(testFeed)()

	// Ensure the changefeed is able to complete in a reasonable amount of time.
	require.NoError(t, testFeed.(cdctest.EnterpriseTestFeed).WaitDurationForState(5*time.Minute, func(s jobs.State) bool {
		return s == jobs.StateSucceeded
	}))
}

func TestChangefeedPropagatesTerminalError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	opts := makeOptions(t)
	defer addCloudStorageOptions(t, &opts)()
	defer changefeedbase.TestingSetDefaultMinCheckpointFrequency(testSinkFlushFrequency)()
	defer testingUseFastRetry()()
	const numNodes = 3

	perServerKnobs := make(map[int]base.TestServerArgs, numNodes)
	for i := 0; i < numNodes; i++ {
		perServerKnobs[i] = base.TestServerArgs{
			Knobs: base.TestingKnobs{
				DistSQL: &execinfra.TestingKnobs{
					DrainFast:  true,
					Changefeed: &TestingKnobs{},
				},
				JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
			},
			ExternalIODir: opts.externalIODir,
			UseDatabase:   "d",
		}
	}

	tc := serverutils.StartCluster(t, numNodes,
		base.TestClusterArgs{
			ServerArgsPerNode: perServerKnobs,
			ReplicationMode:   base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				// Test uses SPLIT AT, which isn't currently supported for
				// secondary tenants. Tracked with #76378.
				DefaultTestTenant: base.TODOTestTenantDisabled,
			},
		})
	defer tc.Stopper().Stop(context.Background())

	{
		db := tc.ServerConn(1)
		sqlDB := sqlutils.MakeSQLRunner(db)
		serverutils.SetClusterSetting(t, tc, "kv.rangefeed.enabled", true)

		sqlDB.ExecMultiple(t,
			`CREATE DATABASE d;`,
			`CREATE TABLE foo (k INT PRIMARY KEY);`,
			`INSERT INTO foo (k) SELECT * FROM generate_series(1, 1000);`,
			`ALTER TABLE foo SPLIT AT (SELECT * FROM generate_series(1, 1000, 50));`,
		)
		for i := 1; i <= 1000; i += 50 {
			sqlDB.ExecSucceedsSoon(t, "ALTER TABLE foo EXPERIMENTAL_RELOCATE VALUES (ARRAY[$1], $2)", 1+(i%numNodes), i)
		}
	}
	// changefeed coordinator will run on this node.
	const coordinatorID = 0

	testFn := func(t *testing.T, nodesToFail []int, opts feedTestOptions) {
		for _, n := range nodesToFail {
			// Configure changefeed to emit fatal error on the specified nodes.
			distSQLKnobs := perServerKnobs[n].Knobs.DistSQL.(*execinfra.TestingKnobs)
			var numEmitted int32
			nodeToFail := n
			distSQLKnobs.Changefeed.(*TestingKnobs).BeforeEmitRow = func(ctx context.Context) error {
				// Emit few rows before returning an error.
				if atomic.AddInt32(&numEmitted, 1) > 10 {
					// Mark error as terminal, but make it a bit more
					// interesting by wrapping it few times.
					err := errors.Wrap(
						changefeedbase.WithTerminalError(
							pgerror.Wrapf(
								errors.Newf("synthetic fatal error from node %d", nodeToFail),
								pgcode.Io, "something happened with IO")),
						"while doing something")
					log.Errorf(ctx, "BeforeEmitRow returning error %s", err)
					return err
				}
				return nil
			}
		}

		defer func() {
			// Reset all changefeed knobs.
			for i := 0; i < numNodes; i++ {
				perServerKnobs[i].Knobs.DistSQL.(*execinfra.TestingKnobs).Changefeed = &TestingKnobs{}
			}
		}()

		sinkType := randomSinkTypeWithOptions(opts)
		f, closeSink := makeFeedFactoryWithOptions(t, sinkType, tc, tc.ServerConn(coordinatorID), opts)
		defer closeSink()
		feed := feed(t, f, "CREATE CHANGEFEED FOR foo")
		defer closeFeed(t, feed)

		// We don't know if we picked enterprise or core feed; regardless, consuming
		// from feed should eventually return an error.
		var feedErr error
		for feedErr == nil {
			_, feedErr = feed.Next()
		}
		log.Errorf(context.Background(), "feedErr=%s", feedErr)
		require.Regexp(t, "synthetic fatal error", feedErr)

		// enterprise feeds should also have the job marked failed.
		if jobFeed, ok := feed.(cdctest.EnterpriseTestFeed); ok {
			require.NoError(t, jobFeed.WaitForState(func(s jobs.State) bool { return s == jobs.StateFailed }))
		}
	}

	for _, tc := range []struct {
		name        string
		nodesToFail []int
		opts        feedTestOptions
	}{
		{
			name:        "coordinator",
			nodesToFail: []int{coordinatorID},
			opts:        opts,
		},
		{
			name:        "aggregator",
			nodesToFail: []int{2},
			opts:        opts.omitSinks("sinkless"), // Sinkless run on coordinator only.
		},
		{
			name:        "many aggregators",
			nodesToFail: []int{0, 2},
			opts:        opts.omitSinks("sinkless"), // Sinkless run on coordinator only.
		},
	} {
		t.Run(tc.name, func(t *testing.T) { testFn(t, tc.nodesToFail, tc.opts) })
	}
}

// Primary key changes are supported by changefeeds starting in 21.1. This tests
// that basic behavior works.
func TestChangefeedPrimaryKeyChangeWorks(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t)
	skip.UnderShort(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY USING HASH, b STRING NOT NULL)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)

		const baseStmt = `CREATE CHANGEFEED FOR foo WITH resolved = '100ms'`
		foo := feed(t, f, baseStmt)
		defer closeFeed(t, foo)

		// 'initial' is skipped because only the latest value ('updated') is
		// emitted by the initial scan.
		assertPayloads(t, foo, []string{
			`foo: [2, 0]->{"after": {"a": 0, "b": "updated"}}`,
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b')`)
		assertPayloads(t, foo, []string{
			`foo: [11, 1]->{"after": {"a": 1, "b": "a"}}`,
			`foo: [6, 2]->{"after": {"a": 2, "b": "b"}}`,
		})

		sqlDB.Exec(t, `ALTER TABLE foo ALTER PRIMARY KEY USING COLUMNS (b) USING HASH`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (3, 'c'), (4, 'd')`)
		assertPayloads(t, foo, []string{
			`foo: [6, "c"]->{"after": {"a": 3, "b": "c"}}`,
			`foo: [15, "d"]->{"after": {"a": 4, "b": "d"}}`,
		})

		// ALTER PRIMARY KEY should work and we should see the changed
		// primary key in subsequent writes.
		sqlDB.Exec(t, `
BEGIN;
ALTER TABLE foo ALTER PRIMARY KEY USING COLUMNS (a);
INSERT INTO foo VALUES (5, 'e');
UPDATE foo SET a = 6 WHERE b = 'a';
COMMIT;
INSERT INTO foo VALUES (1, 'f');
`)
		// Note that the primary key change is asynchronous and that only the
		// subsequent write will be displayed using the new primary key.
		assertPayloads(t, foo, []string{
			`foo: [6, "a"]->{"after": {"a": 6, "b": "a"}}`,
			`foo: [14, "e"]->{"after": {"a": 5, "b": "e"}}`,
			`foo: [1]->{"after": {"a": 1, "b": "f"}}`,
		})
	}

	cdcTest(t, testFn)
}

// Primary key changes are supported by changefeeds starting in 21.1. This test
// specifically focuses on backfill behavior when a single transaction changes
// multiple tables including a primary key change to one and a column change
// requiring a backfill to another.
//
// Note that at time of writing, this change will not end up occurring in the
// same transaction and thus at the same moment but in later code changes, it
// will.
func TestChangefeedPrimaryKeyChangeWorksWithMultipleTables(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t)
	skip.UnderShort(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING NOT NULL)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY, b STRING NOT NULL)`)
		sqlDB.Exec(t, `INSERT INTO bar VALUES (1, 'a')`)

		const baseStmt = `CREATE CHANGEFEED FOR foo, bar WITH resolved = '100ms'`
		cf := feed(t, f, baseStmt)
		defer closeFeed(t, cf)

		// maybeHandleRestart deals with the fact that sinkless changefeeds don't
		// gracefully handle primary index changes but rather force the client to
		// deal with restarting the changefeed as of the last resolved timestamp.
		//
		// This ends up being pretty sane; sinkless changefeeds already require this
		// behavior in the face of other transient failures so clients already need
		// to implement this logic.
		maybeHandleRestart := func(t *testing.T) (cleanup func()) {
			return func() {}
		}

		// 'initial' is skipped because only the latest value ('updated') is
		// emitted by the initial scan.
		assertPayloads(t, cf, []string{
			`foo: [0]->{"after": {"a": 0, "b": "updated"}}`,
			`bar: [1]->{"after": {"a": 1, "b": "a"}}`,
		})

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a'), (2, 'b')`)
		sqlDB.Exec(t, `INSERT INTO bar VALUES (2, 'b'), (3, 'c')`)
		assertPayloads(t, cf, []string{
			`foo: [1]->{"after": {"a": 1, "b": "a"}}`,
			`foo: [2]->{"after": {"a": 2, "b": "b"}}`,
			`bar: [2]->{"after": {"a": 2, "b": "b"}}`,
			`bar: [3]->{"after": {"a": 3, "b": "c"}}`,
		})

		sqlDB.Exec(t, `
BEGIN;
ALTER TABLE foo ALTER PRIMARY KEY USING COLUMNS (b);
INSERT INTO bar VALUES (4, 'd'), (5, 'e');
INSERT INTO foo VALUES (3, 'c');
COMMIT;
INSERT INTO foo VALUES (4, 'd');
INSERT INTO bar VALUES (6, 'f');
`)

		assertPayloads(t, cf, []string{
			`bar: [4]->{"after": {"a": 4, "b": "d"}}`,
			`bar: [5]->{"after": {"a": 5, "b": "e"}}`,
			`foo: [3]->{"after": {"a": 3, "b": "c"}}`,
		})
		defer maybeHandleRestart(t)()
		assertPayloads(t, cf, []string{
			`foo: ["d"]->{"after": {"a": 4, "b": "d"}}`,
			`bar: [6]->{"after": {"a": 6, "b": "f"}}`,
		})
	}

	cdcTest(t, testFn)
}

// TestChangefeedCheckpointSchemaChange tests to make sure that writes that
// occur in the same transaction that performs an immediately visible schema
// change, like drop column, observe the schema change. Also, this tests that
// resuming from that cursor from the same timestamp as the schema change
// only includes later updates (thus validating the cursor semantics as they
// pertain to schema changes). It also does that test using an initial
// backfill, which makes the cursor, more or less, inclusive rather than
// exclusive.
func TestChangefeedCheckpointSchemaChange(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t)
	skip.UnderShort(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		// Uses transactions below, which don't support declarative schema changer.
		sqlDB.Exec(t, "SET create_table_with_schema_locked=false")
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING NOT NULL)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY, b STRING NOT NULL)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO bar VALUES (0, 'initial')`)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo, bar WITH resolved = '100ms', updated`,
			optOutOfMetamorphicEnrichedEnvelope{reason: "this test uses readNextMessages directly, which the metamorphic enriched envelope does not support"})

		// Sketch of the test is as follows:
		//
		//  1) Write some rows into two tables, foo and bar.
		//  2) In a transaction, write to and update both tables,
		//     and drop a column on foo.
		//  3) Ensure that the messages are the 3 writes to foo, 2
		//     writes to bar and then the 3 values of foo being
		//     rewritten. Also note that none of the foo values are
		//     under the old schema.
		//  4) Extract the timestamp from the initial messages.
		//  5) Run a cursor-based changefeed from that timestamp with
		//     no initial_scan. See only the 3 touch writes.
		//  6) Run a cursor-based changefeed from that timestamp with
		//     with initial_scan. See all 8 writes at the same timestamps.
		//
		assertPayloadsStripTs(t, foo, []string{
			`foo: [0]->{"after": {"a": 0, "b": "initial"}}`,
			`bar: [0]->{"after": {"a": 0, "b": "initial"}}`,
		})

		_, err := s.DB.Exec("SET autocommit_before_ddl = false")
		require.NoError(t, err)
		require.NoError(t, crdb.ExecuteTx(context.Background(), s.DB, nil, func(tx *gosql.Tx) error {
			for _, stmt := range []string{
				`CREATE TABLE baz ()`,
				`INSERT INTO foo VALUES (2, 'initial')`,
				`INSERT INTO foo VALUES (1, 'initial')`,
				`UPSERT INTO foo VALUES (0, 'updated')`,
				`ALTER TABLE foo DROP COLUMN b`,
				`UPSERT INTO bar VALUES (0, 'updated')`,
				`UPSERT INTO bar VALUES (1, 'initial')`,
			} {
				if _, err := tx.Exec(stmt); err != nil {
					return err
				}
			}
			return nil
		}))
		_, err = s.DB.Exec("RESET autocommit_before_ddl")
		require.NoError(t, err)

		expected := []string{
			`bar: [0]->{"after": {"a": 0, "b": "updated"}}`,
			`bar: [1]->{"after": {"a": 1, "b": "initial"}}`,
			`foo: [0]->{"after": {"a": 0}}`,
			`foo: [1]->{"after": {"a": 1}}`,
			`foo: [2]->{"after": {"a": 2}}`,
			// Touch writes due to column backfill.
			`foo: [0]->{"after": {"a": 0}}`,
			`foo: [1]->{"after": {"a": 1}}`,
			`foo: [2]->{"after": {"a": 2}}`,
		}
		msgs, err := readNextMessages(context.Background(), foo, len(expected))
		require.NoError(t, err)

		var msgsFormatted []string
		for _, m := range msgs {
			msgsFormatted = append(msgsFormatted, fmt.Sprintf(`%s: %s->%s`, m.Topic, m.Key, m.Value))
		}

		// Sort the messages by their timestamp.
		re := regexp.MustCompile(`.*(, "updated": "(\d+\.\d+)")}.*`)
		getHLC := func(i int) string { return re.FindStringSubmatch(msgsFormatted[i])[2] }
		trimHlC := func(s string) string {
			indexes := re.FindStringSubmatchIndex(s)
			return s[:indexes[2]] + s[indexes[3]:]
		}
		sort.Slice(msgsFormatted, func(i, j int) bool {
			a, b := getHLC(i), getHLC(j)
			if a == b {
				return msgsFormatted[i] < msgsFormatted[j]
			}
			return a < b
		})
		schemaChangeTS := getHLC(0)
		stripped := make([]string, len(msgsFormatted))
		for i, m := range msgsFormatted {
			stripped[i] = trimHlC(m)
		}
		require.Equal(t, expected, stripped)
		// Make sure there are no more messages.
		{
			next, err := foo.Next()
			require.NoError(t, err)
			require.NotNil(t, next.Resolved)
		}
		closeFeed(t, foo)

		t.Run("cursor, no backfill", func(t *testing.T) {
			// Resume at exactly the timestamp of the schema change, observe only
			// events after it.
			foo = feed(t, f,
				"CREATE CHANGEFEED FOR foo, bar WITH"+
					" resolved = '100ms', updated, cursor = $1",
				schemaChangeTS)
			defer closeFeed(t, foo)
			// Observe only the touch writes.
			assertPayloads(t, foo, msgsFormatted[5:])
			// Make sure there are no more messages.
			{
				next, err := foo.Next()
				require.NoError(t, err)
				require.NotNil(t, next.Resolved)
			}
		})

		t.Run("cursor, with backfill", func(t *testing.T) {
			// Resume at exactly the timestamp of the schema change, observe the
			// writes at that timestamp exactly, but with the new schema change.
			foo = feed(t, f,
				"CREATE CHANGEFEED FOR foo, bar WITH"+
					" resolved = '100ms', updated, cursor = $1, initial_scan",
				schemaChangeTS)
			defer closeFeed(t, foo)
			assertPayloads(t, foo, msgsFormatted)
			// Make sure there are no more messages.
			{
				next, err := foo.Next()
				require.NoError(t, err)
				require.NotNil(t, next.Resolved)
			}
		})
	}

	cdcTest(t, testFn)
}

func TestChangefeedBackfillCheckpoint(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t)
	skip.UnderShort(t)

	rnd, _ := randutil.NewTestRand()
	var maxCheckpointSize int64

	drainUntilTimestamp := func(f cdctest.TestFeed, ts hlc.Timestamp) (err error) {
		var msg *cdctest.TestFeedMessage
		for msg, err = f.Next(); msg != nil; msg, err = f.Next() {
			if msg.Resolved != nil {
				resolvedTs := extractResolvedTimestamp(t, msg)
				if ts.LessEq(resolvedTs) {
					break
				}
			}
		}
		return err
	}

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		valRange := []int{1, 1000}
		sqlDB.Exec(t, `CREATE TABLE foo(a INT PRIMARY KEY)`)
		sqlDB.Exec(t, fmt.Sprintf(`INSERT INTO foo (a) SELECT * FROM generate_series(%d, %d)`, valRange[0], valRange[1]))

		fooDesc := desctestutils.TestingGetPublicTableDescriptor(
			s.SystemServer.DB(), s.Codec, "d", "foo")
		tableSpan := fooDesc.PrimaryIndexSpan(s.Codec)

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		// Ensure Scan Requests are always small enough that we receive multiple
		// resolved events during a backfill
		knobs.FeedKnobs.BeforeScanRequest = func(b *kv.Batch) error {
			b.Header.MaxSpanRequestKeys = 1 + rnd.Int63n(100)
			return nil
		}

		// Emit resolved events for majority of spans.  Be extra paranoid and ensure that
		// we have at least 1 span for which we don't emit resolved timestamp (to force checkpointing).
		haveGaps := false
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			if r.Span.Equal(tableSpan) {
				// Do not emit resolved events for the entire table span.
				// We "simulate" large table by splitting single table span into many parts, so
				// we want to resolve those sub-spans instead of the entire table span.
				// However, we have to emit something -- otherwise the entire changefeed
				// machine would not work.
				r.Span.EndKey = tableSpan.Key.Next()
				return false, nil
			}
			if haveGaps {
				return rnd.Intn(10) > 7, nil
			}
			haveGaps = true
			return true, nil
		}

		// Checkpoint progress frequently, and set the checkpoint size limit.
		changefeedbase.SpanCheckpointInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 1)
		changefeedbase.SpanCheckpointMaxBytes.Override(
			context.Background(), &s.Server.ClusterSettings().SV, maxCheckpointSize)

		registry := s.Server.JobRegistry().(*jobs.Registry)
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH resolved='100ms'`)
		// Some test feeds (kafka) are not buffered, so we have to consume messages.
		var shouldDrain int32 = 1
		g := ctxgroup.WithContext(context.Background())
		g.Go(func() error {
			for {
				if shouldDrain == 0 {
					return nil
				}
				m, err := foo.Next()
				if err != nil {
					return err
				}

				if m.Resolved != nil {
					ts := extractResolvedTimestamp(t, m)
					if ts.IsEmpty() {
						return errors.New("unexpected epoch resolved event")
					}
				}
			}
		})

		defer func() {
			closeFeed(t, foo)
		}()

		jobFeed := foo.(cdctest.EnterpriseTestFeed)
		loadProgress := func() jobspb.Progress {
			jobID := jobFeed.JobID()
			job, err := registry.LoadJob(context.Background(), jobID)
			require.NoError(t, err)
			return job.Progress()
		}

		// Wait for non-nil checkpoint.
		testutils.SucceedsSoon(t, func() error {
			progress := loadProgress()
			if loadCheckpoint(t, progress) != nil {
				return nil
			}
			return errors.New("waiting for checkpoint")
		})

		// Pause the job and read and verify the latest checkpoint information.
		require.NoError(t, jobFeed.Pause())
		progress := loadProgress()
		require.NotNil(t, progress.GetChangefeed())
		h := progress.GetHighWater()
		noHighWater := h == nil || h.IsEmpty()
		require.True(t, noHighWater)

		spanLevelCheckpoint := loadCheckpoint(t, progress)
		require.NotNil(t, spanLevelCheckpoint)
		checkpointSpanGroup := makeSpanGroupFromCheckpoint(t, spanLevelCheckpoint)

		// Collect spans we attempt to resolve after when we resume.
		var resolved []roachpb.Span
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			if !r.Span.Equal(tableSpan) {
				resolved = append(resolved, r.Span)
			}
			return false, nil
		}

		var actualFrontierStr atomic.Value
		knobs.AfterCoordinatorFrontierRestore = func(frontier *resolvedspan.CoordinatorFrontier) {
			require.NotNil(t, frontier)
			actualFrontierStr.Store(frontier.String())
		}

		// Resume job.
		require.NoError(t, jobFeed.Resume())

		// Verify that the resumed job has restored the progress from the checkpoint
		// to the change frontier.
		expectedFrontier, err := span.MakeFrontier(tableSpan)
		if err != nil {
			t.Fatal(err)
		}
		assert.NoError(t, checkpoint.Restore(expectedFrontier, spanLevelCheckpoint))
		expectedFrontierStr := expectedFrontier.String()
		testutils.SucceedsSoon(t, func() error {
			if s := actualFrontierStr.Load(); s != nil {
				require.Equal(t, expectedFrontierStr, s)
				return nil
			}
			return errors.New("waiting for frontier to be restored")
		})

		// Wait for the high water mark to be non-zero.
		testutils.SucceedsSoon(t, func() error {
			prog := loadProgress()
			if p := prog.GetHighWater(); p != nil && !p.IsEmpty() {
				return nil
			}
			return errors.New("waiting for highwater")
		})

		// At this point, highwater mark should be set, and previous checkpoint should be gone.
		progress = loadProgress()
		require.NotNil(t, progress.GetChangefeed())
		require.Nil(t, loadCheckpoint(t, progress))

		// Verify that none of the resolved spans after resume were checkpointed.
		for _, sp := range resolved {
			require.Falsef(t, checkpointSpanGroup.Contains(sp.Key), "span should not have been resolved: %s", sp)
		}

		// Consume all potentially buffered kv events
		atomic.StoreInt32(&shouldDrain, 0)
		if err := g.Wait(); err != nil {
			require.NotRegexp(t, "unexpected epoch resolved event", err)
		}
		err = drainUntilTimestamp(foo, *progress.GetHighWater())
		require.NoError(t, err)

		// Verify that the checkpoint does not affect future scans
		sqlDB.Exec(t, `ALTER TABLE foo ADD COLUMN b STRING DEFAULT 'd'`)
		var expected []string
		for i := valRange[0]; i <= valRange[1]; i++ {
			expected = append(expected, fmt.Sprintf(
				`foo: [%d]->{"after": {"a": %d, "b": "d"}}`, i, i,
			))
		}
		assertPayloads(t, foo, expected)
	}

	// TODO(ssd): Tenant testing disabled because of use of DB()
	for _, sz := range []int64{100 << 20, 100} {
		maxCheckpointSize = sz
		cdcTestNamedWithSystem(t, fmt.Sprintf("limit=%s", humanize.Bytes(uint64(sz))), testFn, feedTestEnterpriseSinks)
	}
}

// TestCoreChangefeedBackfillScanCheckpoint tests that a core changefeed
// successfully completes the initial scan of a table when transient errors occur.
// This test only succeeds if checkpoints are taken.
func TestCoreChangefeedBackfillScanCheckpoint(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t)
	skip.UnderShort(t)

	rnd, _ := randutil.NewPseudoRand()

	rowCount := 10000

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo(a INT PRIMARY KEY)`)
		sqlDB.Exec(t, fmt.Sprintf(`INSERT INTO foo (a) SELECT * FROM generate_series(%d, %d)`, 0, rowCount))

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		// Ensure Scan Requests are always small enough that we receive multiple
		// resolved events during a backfill. Also ensure that checkpoint frequency
		// and size are large enough to induce several checkpoints when
		// writing `rowCount` rows.
		knobs.FeedKnobs.BeforeScanRequest = func(b *kv.Batch) error {
			b.Header.MaxSpanRequestKeys = 1 + rnd.Int63n(25)
			return nil
		}
		changefeedbase.SpanCheckpointInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 1)
		changefeedbase.SpanCheckpointMaxBytes.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 100<<20)

		emittedCount := 0
		errorCount := 0
		knobs.RaiseRetryableError = func() error {
			emittedCount++
			if emittedCount%200 == 0 {
				errorCount++
				return errors.New("test transient error")
			}
			return nil
		}

		foo := feed(t, f, `CREATE CHANGEFEED FOR TABLE foo`)
		defer closeFeed(t, foo)

		payloads := make([]string, rowCount+1)
		for i := 0; i < rowCount+1; i++ {
			payloads[i] = fmt.Sprintf(`foo: [%d]->{"after": {"a": %d}}`, i, i)
		}
		assertPayloads(t, foo, payloads)
		require.GreaterOrEqual(t, errorCount, 1)
	}

	cdcTest(t, testFn, feedTestForceSink("sinkless"))
}

func TestCheckpointFrequency(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const frontierAdvanced = true
	const frontierDidNotAdvance = false

	// Test the logic around throttling of job progress updates.
	// It's pretty difficult to set up a fast end-to-end test since we need to simulate slow
	// job table update.  Instead, we just test canCheckpointHighWatermark directly.
	ts := timeutil.NewManualTime(timeutil.Now())
	js := newJobState(nil, /* job */
		cluster.MakeTestingClusterSettings(),
		MakeMetrics(time.Second, cidr.NewTestLookup()).(*Metrics), ts,
	)

	ctx := context.Background()

	require.False(t, js.canCheckpointHighWatermark(frontierDidNotAdvance))
	require.True(t, js.canCheckpointHighWatermark(frontierAdvanced))

	// Pretend our mean time to update progress is 1 minute, and we just updated progress.
	require.EqualValues(t, 0, js.checkpointDuration)
	js.checkpointCompleted(ctx, 12*time.Second)
	require.Less(t, int64(0), js.checkpointDuration.Nanoseconds())

	// Even though frontier advanced, we shouldn't checkpoint.
	require.False(t, js.canCheckpointHighWatermark(frontierAdvanced))
	require.True(t, js.progressUpdatesSkipped)

	// Once enough time elapsed, we allow progress update, even if frontier did not advance.
	ts.Advance(js.checkpointDuration)
	require.True(t, js.canCheckpointHighWatermark(frontierDidNotAdvance))

	// If we also specify minimum amount of time between updates, we would skip updates
	// until enough time has elapsed.
	minAdvance := 10 * time.Minute
	changefeedbase.ResolvedTimestampMinUpdateInterval.Override(ctx, &js.settings.SV, minAdvance)

	require.False(t, js.canCheckpointHighWatermark(frontierAdvanced))
	ts.Advance(minAdvance)
	require.True(t, js.canCheckpointHighWatermark(frontierAdvanced))

	// When we mark checkpoint completion, job state updated to reflect that.
	completionTime := timeutil.Now().Add(time.Hour)
	ts.AdvanceTo(completionTime)
	js.checkpointCompleted(ctx, 42*time.Second)
	require.Equal(t, completionTime, js.lastProgressUpdate)
	require.False(t, js.progressUpdatesSkipped)
}

func TestFlushJitter(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Test the logic around applying jitter to the flush logic.
	// The more involved test that would try to capture flush times would likely
	// be pretty flaky due to the fact that flush times do not happen at exactly
	// min_flush_frequency period, and thus it would be hard to tell if the
	// difference is due to jitter or not.  Just verify nextFlushWithJitter function
	// works as expected with controlled time source.

	ts := timeutil.NewManualTime(timeutil.Now())
	const numIters = 100

	for _, tc := range []struct {
		flushFrequency        time.Duration
		jitter                float64
		expectedFlushDuration time.Duration
		expectedErr           bool
	}{
		// Negative jitter.
		{
			flushFrequency:        -1,
			jitter:                -0.1,
			expectedFlushDuration: 0,
			expectedErr:           true,
		},
		{
			flushFrequency:        0,
			jitter:                -0.1,
			expectedFlushDuration: 0,
			expectedErr:           true,
		},
		{
			flushFrequency:        10 * time.Millisecond,
			jitter:                -0.1,
			expectedFlushDuration: 0,
			expectedErr:           true,
		},
		{
			flushFrequency:        100 * time.Millisecond,
			jitter:                -0.1,
			expectedFlushDuration: 0,
			expectedErr:           true,
		},
		// Disable Jitter.
		{
			flushFrequency:        -1,
			jitter:                0,
			expectedFlushDuration: 0,
			expectedErr:           true,
		},
		{
			flushFrequency:        0,
			jitter:                0,
			expectedFlushDuration: 0,
			expectedErr:           false,
		},
		{
			flushFrequency:        10 * time.Millisecond,
			jitter:                0,
			expectedFlushDuration: 10 * time.Millisecond,
			expectedErr:           false,
		},
		{
			flushFrequency:        100 * time.Millisecond,
			jitter:                0,
			expectedFlushDuration: 100 * time.Millisecond,
			expectedErr:           false,
		},
		// Enable Jitter.
		{
			flushFrequency:        -1,
			jitter:                0.1,
			expectedFlushDuration: 0,
			expectedErr:           true,
		},
		{
			flushFrequency:        0,
			jitter:                0.1,
			expectedFlushDuration: 0,
			expectedErr:           false,
		},
		{
			flushFrequency:        10 * time.Millisecond,
			jitter:                0.1,
			expectedFlushDuration: 10 * time.Millisecond,
			expectedErr:           false,
		},
		{
			flushFrequency:        100 * time.Millisecond,
			jitter:                0.1,
			expectedFlushDuration: 100 * time.Millisecond,
			expectedErr:           false,
		},
		// Expect actual jitter to be 0 since flushFrequency * jitter < 1.
		{
			flushFrequency:        1,
			jitter:                0.1,
			expectedFlushDuration: 1,
			expectedErr:           false,
		},
		// Expect actual jitter to be 0 since flushFrequency * jitter < 1.
		{
			flushFrequency:        10,
			jitter:                0.01,
			expectedFlushDuration: 10,
			expectedErr:           false,
		},
	} {
		t.Run(fmt.Sprintf("flushfrequency=%sjitter=%f", tc.flushFrequency, tc.jitter), func(t *testing.T) {
			for i := 0; i < numIters; i++ {
				next, err := nextFlushWithJitter(ts, tc.flushFrequency, tc.jitter)
				if tc.expectedErr {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				if tc.jitter > 0 {
					minBound := tc.expectedFlushDuration
					maxBound := tc.expectedFlushDuration + time.Duration(float64(tc.expectedFlushDuration)*tc.jitter)
					actualDuration := next.Sub(ts.Now())
					require.LessOrEqual(t, minBound, actualDuration)
					require.LessOrEqual(t, actualDuration, maxBound)
				} else {
					require.Equal(t, tc.expectedFlushDuration, next.Sub(ts.Now()))
				}
				ts.AdvanceTo(next)
			}
		})
	}
}

func TestChangefeedOrderingWithErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH updated`)
		webhookFoo := foo.(*webhookFeed)
		// retry, then fail, then restart changefeed and successfully send messages
		webhookFoo.mockSink.SetStatusCodes(append(repeatStatusCode(
			http.StatusInternalServerError,
			defaultRetryConfig().MaxRetries+1),
			[]int{http.StatusOK, http.StatusOK, http.StatusOK}...))
		defer closeFeed(t, foo)

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (1, 'b')`)
		sqlDB.Exec(t, `DELETE FROM foo WHERE a = 1`)
		assertPayloadsPerKeyOrderedStripTs(t, foo, []string{
			`foo: [1]->{"after": {"a": 1, "b": "a"}}`,
			`foo: [1]->{"after": {"a": 1, "b": "b"}}`,
			`foo: [1]->{"after": null}`,
		})

		webhookFoo.mockSink.SetStatusCodes([]int{http.StatusInternalServerError})
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (1, 'c')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (1, 'd')`)
		feedJob := foo.(cdctest.EnterpriseTestFeed)

		// check that running status correctly updates with retryable error
		testutils.SucceedsSoon(t, func() error {
			status, err := feedJob.FetchStatusMessage()
			if err != nil {
				return err
			}
			require.Regexp(t, "500 Internal Server Error", status)
			return nil
		})

		webhookFoo.mockSink.SetStatusCodes([]int{http.StatusOK})
		// retryable error should disappear after request becomes successful
		assertPayloadsPerKeyOrderedStripTs(t, foo, []string{
			`foo: [1]->{"after": {"a": 1, "b": "c"}}`,
			`foo: [1]->{"after": {"a": 1, "b": "d"}}`,
		})
	}

	// only used for webhook sink for now since it's the only testfeed where
	// we can control the ordering of errors
	cdcTest(t, testFn, feedTestForceSink("webhook"), feedTestNoExternalConnection, withAllowChangefeedErr("expects error"))
}

func TestChangefeedOnErrorOption(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		t.Run(`pause on error`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

			knobs := s.TestingKnobs.
				DistSQL.(*execinfra.TestingKnobs).
				Changefeed.(*TestingKnobs)
			knobs.BeforeEmitRow = func(_ context.Context) error {
				return changefeedbase.WithTerminalError(errors.New("should fail with custom error"))
			}

			foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH on_error='pause'`)
			sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'a')`)

			feedJob := foo.(cdctest.EnterpriseTestFeed)

			// check for paused status on failure
			require.NoError(t, feedJob.WaitForState(func(s jobs.State) bool { return s == jobs.StatePaused }))

			// Verify job progress contains paused on error status.
			jobID := foo.(cdctest.EnterpriseTestFeed).JobID()
			registry := s.Server.JobRegistry().(*jobs.Registry)
			job, err := registry.LoadJob(context.Background(), jobID)
			require.NoError(t, err)
			require.Contains(t, job.Progress().StatusMessage, "job failed (should fail with custom error) but is being paused because of on_error=pause")
			knobs.BeforeEmitRow = nil

			require.NoError(t, feedJob.Resume())
			// changefeed should continue to work after it has been resumed
			assertPayloads(t, foo, []string{
				`foo: [1]->{"after": {"a": 1, "b": "a"}}`,
			})

			closeFeed(t, foo)
			// cancellation should still go through if option is in place
			// to avoid race condition, check only that the job is progressing to be
			// canceled (we don't know what stage it will be in)
			require.NoError(t, feedJob.WaitForState(func(s jobs.State) bool {
				return s == jobs.StateCancelRequested ||
					s == jobs.StateReverting ||
					s == jobs.StateCanceled
			}))
		})

		t.Run(`fail on error`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY, b STRING)`)

			knobs := s.TestingKnobs.
				DistSQL.(*execinfra.TestingKnobs).
				Changefeed.(*TestingKnobs)
			knobs.BeforeEmitRow = func(_ context.Context) error {
				return changefeedbase.WithTerminalError(errors.New("should fail with custom error"))
			}

			foo := feed(t, f, `CREATE CHANGEFEED FOR bar WITH on_error = 'fail'`)
			sqlDB.Exec(t, `INSERT INTO bar VALUES (1, 'a')`)
			defer closeFeed(t, foo)

			feedJob := foo.(cdctest.EnterpriseTestFeed)

			require.NoError(t, feedJob.WaitForState(func(s jobs.State) bool { return s == jobs.StateFailed }))
			require.EqualError(t, feedJob.FetchTerminalJobErr(), "should fail with custom error")
		})

		t.Run(`default`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE quux (a INT PRIMARY KEY, b STRING)`)

			knobs := s.TestingKnobs.
				DistSQL.(*execinfra.TestingKnobs).
				Changefeed.(*TestingKnobs)
			knobs.BeforeEmitRow = func(_ context.Context) error {
				return changefeedbase.WithTerminalError(errors.New("should fail with custom error"))
			}

			foo := feed(t, f, `CREATE CHANGEFEED FOR quux`)
			sqlDB.Exec(t, `INSERT INTO quux VALUES (1, 'a')`)
			defer closeFeed(t, foo)

			feedJob := foo.(cdctest.EnterpriseTestFeed)

			// if no option is provided, fail should be the default behavior
			require.NoError(t, feedJob.WaitForState(func(s jobs.State) bool { return s == jobs.StateFailed }))
			require.EqualError(t, feedJob.FetchTerminalJobErr(), "should fail with custom error")
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, withAllowChangefeedErr("expects error"))
}

func TestDistSenderRangeFeedPopulatesVirtualTable(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, cleanup := makeServer(t)
	defer cleanup()

	sqlDB := sqlutils.MakeSQLRunner(s.DB)
	sqlDB.Exec(t, `CREATE TABLE tbl (a INT, b STRING);`)
	sqlDB.Exec(t, `INSERT INTO tbl VALUES (1, 'one'), (2, 'two'), (3, 'three');`)
	sqlDB.Exec(t, `CREATE CHANGEFEED FOR tbl INTO 'null://';`)

	var tableID int
	sqlDB.QueryRow(t, "SELECT table_id FROM crdb_internal.tables WHERE name='tbl'").Scan(&tableID)
	tableKey := s.Codec.TablePrefix(uint32(tableID))

	numRangesQuery := fmt.Sprintf(
		"SELECT count(*) FROM crdb_internal.active_range_feeds WHERE range_start LIKE '%s/%%'",
		tableKey)
	sqlDB.CheckQueryResultsRetry(t, numRangesQuery, [][]string{{"1"}})
}

func TestChangefeedCaseInsensitiveOpts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Sanity check for case insensitive options
	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		// Set up a type and table.
		sqlDB.Exec(t, `CREATE TABLE insensitive (x INT PRIMARY KEY, y string)`)
		sqlDB.Exec(t, `INSERT INTO insensitive VALUES (0, 'hello')`)

		t.Run(`format=JSON`, func(t *testing.T) {
			cf := feed(t, f, `CREATE CHANGEFEED FOR TABLE insensitive WITH format=JSON`)
			defer closeFeed(t, cf)
			assertPayloads(t, cf, []string{`insensitive: [0]->{"after": {"x": 0, "y": "hello"}}`})
		})

		t.Run(`envelope=ROW`, func(t *testing.T) {
			cf := feed(t, f, `CREATE CHANGEFEED FOR insensitive WITH envelope='ROW'`)
			defer closeFeed(t, cf)
			assertPayloads(t, cf, []string{`insensitive: [0]->{"x": 0, "y": "hello"}`})
		})

		t.Run(`schema_change_events=COLUMN_CHANGES, schema_change_policy=STOP`, func(t *testing.T) {
			cf := feed(t, f, `CREATE CHANGEFEED FOR insensitive `+
				`WITH schema_change_events=COLUMN_CHANGES, schema_change_policy=STOP`)
			defer closeFeed(t, cf)
			assertPayloads(t, cf, []string{`insensitive: [0]->{"after": {"x": 0, "y": "hello"}}`})
		})

		t.Run(`on_error=FAIL`, func(t *testing.T) {
			cf := feed(t, f, `CREATE CHANGEFEED FOR insensitive WITH on_error=FAIL`)
			defer closeFeed(t, cf)
			assertPayloads(t, cf, []string{`insensitive: [0]->{"after": {"x": 0, "y": "hello"}}`})
		})
	}

	// Some sinks are incompatible with envelope
	cdcTest(t, testFn, feedTestRestrictSinks("sinkless", "enterprise", "kafka"))
}

func TestChangefeedEndTime(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)
		endTimeReached := make(chan struct{})
		knobs.FeedKnobs.EndTimeReached = func() bool {
			select {
			case <-endTimeReached:
				return true
			default:
				return false
			}
		}

		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, "CREATE TABLE foo (a INT PRIMARY KEY)")
		sqlDB.Exec(t, "INSERT INTO foo VALUES (1), (2), (3)")

		fakeEndTime := s.Server.Clock().Now().Add(int64(time.Hour), 0).AsOfSystemTime()
		feed := feed(t, f, "CREATE CHANGEFEED FOR foo WITH end_time = $1", fakeEndTime)
		defer closeFeed(t, feed)

		assertPayloads(t, feed, []string{
			`foo: [1]->{"after": {"a": 1}}`,
			`foo: [2]->{"after": {"a": 2}}`,
			`foo: [3]->{"after": {"a": 3}}`,
		})

		close(endTimeReached)

		testFeed := feed.(cdctest.EnterpriseTestFeed)
		require.NoError(t, testFeed.WaitForState(func(s jobs.State) bool {
			return s == jobs.StateSucceeded
		}))
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks)
}

func TestChangefeedEndTimeWithCursor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, "CREATE TABLE foo (a INT PRIMARY KEY)")

		var tsCursor string
		sqlDB.QueryRow(t, "SELECT (cluster_logical_timestamp())").Scan(&tsCursor)

		// Insert 1k rows -- using separate statements to get different MVCC timestamps.
		for i := 0; i < 1024; i++ {
			sqlDB.Exec(t, "INSERT INTO foo VALUES ($1)", i)
		}

		// Split table into multiple ranges to make things more interesting.
		sqlDB.Exec(t, "ALTER TABLE foo SPLIT AT VALUES (100), (200), (400), (800)")

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)
		fooSpan := func() roachpb.Span {
			fooDesc := desctestutils.TestingGetPublicTableDescriptor(
				s.Server.DB(), s.Codec, "d", "foo")
			return fooDesc.PrimaryIndexSpan(s.Codec)
		}()

		// Capture resolved events emitted during changefeed.  We expect
		// every range to emit resolved event with end_time timestamp.
		frontier, err := span.MakeFrontier(fooSpan)
		require.NoError(t, err)
		knobs.FilterSpanWithMutation = func(rs *jobspb.ResolvedSpan) (bool, error) {
			_, err := frontier.Forward(rs.Span, rs.Timestamp)
			return false, err
		}

		// endTime must be after creation time (5 seconds should be enough
		// to reach create changefeed statement and process it).
		endTime := s.Server.Clock().Now().AddDuration(5 * time.Second)
		feed := feed(t, f, "CREATE CHANGEFEED FOR foo WITH cursor = $1, end_time = $2, no_initial_scan",
			tsCursor, eval.TimestampToDecimalDatum(endTime).String())
		defer closeFeed(t, feed)

		// Don't care much about the values emitted (tested elsewhere) -- all
		// we want to make sure is that the feed terminates.  However, we do need
		// to consume those values since some of the test sink implementations (kafka)
		// will block.
		defer DiscardMessages(feed)()

		testFeed := feed.(cdctest.EnterpriseTestFeed)
		require.NoError(t, testFeed.WaitForState(func(s jobs.State) bool {
			return s == jobs.StateSucceeded
		}))

		// After changefeed completes, verify we have seen all ranges emit resolved
		// event with end_time timestamp.  That is: verify frontier.Frontier() is at end_time.
		expectedFrontier := endTime.Prev()
		testutils.SucceedsWithin(t, func() error {
			if expectedFrontier == frontier.Frontier() {
				return nil
			}
			return errors.Newf("still waiting for frontier to reach %s, current %s",
				expectedFrontier, frontier.Frontier())
		}, 5*time.Second)
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks)
}

func TestChangefeedOnlyInitialScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	initialScanOnlyTests := map[string]string{
		`initial scan only`:     `CREATE CHANGEFEED FOR foo WITH initial_scan_only`,
		`initial backfill only`: `CREATE CHANGEFEED FOR foo WITH initial_scan = 'only'`,
	}

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		for testName, changefeedStmt := range initialScanOnlyTests {
			t.Run(testName, func(t *testing.T) {
				sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
				sqlDB.Exec(t, `INSERT INTO foo (a) SELECT * FROM generate_series(1, 5000);`)
				defer func() {
					sqlDB.Exec(t, `DROP TABLE foo`)
				}()

				feed := feed(t, f, changefeedStmt)
				defer closeFeed(t, feed)

				// Insert few more rows after the feed started -- we should not see those emitted.
				sqlDB.Exec(t, "INSERT INTO foo VALUES (5005), (5007), (5009)")

				var expectedMessages []string
				for i := 1; i <= 5000; i++ {
					expectedMessages = append(expectedMessages, fmt.Sprintf(
						`foo: [%d]->{"after": {"a": %d}}`, i, i,
					))
				}

				assertPayloads(t, feed, expectedMessages)

				// It would be nice to assert that after we've seen expectedMessages,
				// that none of the unexpected messages show up before job termination.
				// However, if any of those unexpected messages were emitted, then, we
				// would expect this test to flake (hopefully, with an error message
				// that makes it clear that the unexpected event happen).
				jobFeed := feed.(cdctest.EnterpriseTestFeed)
				require.NoError(t, jobFeed.WaitForState(func(s jobs.State) bool {
					return s == jobs.StateSucceeded
				}))
			})
		}
	}

	// "enterprise" and "webhook" sink implementations are too slow
	// for a test that reads 5k messages.
	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestOmitSinks("enterprise", "webhook"))
}

func TestChangefeedOnlyInitialScanCSV(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	tests := map[string]struct {
		changefeedStmt  string
		expectedPayload []string
	}{
		`initial scan only with csv`: {
			changefeedStmt: `CREATE CHANGEFEED FOR foo WITH initial_scan_only, format = csv`,
			expectedPayload: []string{
				`1,Alice`,
				`2,Bob`,
				`3,Carol`,
			},
		},
		`initial backfill only with csv`: {
			changefeedStmt: `CREATE CHANGEFEED FOR foo WITH initial_scan = 'only', format = csv`,
			expectedPayload: []string{
				`1,Alice`,
				`2,Bob`,
				`3,Carol`,
			},
		},
		`initial backfill only with csv multiple tables`: {
			changefeedStmt: `CREATE CHANGEFEED FOR foo, bar WITH initial_scan = 'only', format = csv`,
			expectedPayload: []string{
				`1,a`,
				`2,b`,
				`3,c`,
				`1,Alice`,
				`2,Bob`,
				`3,Carol`,
			},
		},
	}

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		for testName, testData := range tests {
			t.Run(testName, func(t *testing.T) {
				sqlDB.Exec(t, "CREATE TABLE foo (id INT PRIMARY KEY, name STRING)")
				sqlDB.Exec(t, "CREATE TABLE bar (id INT PRIMARY KEY, name STRING)")

				sqlDB.Exec(t, "INSERT INTO foo VALUES (1, 'Alice'), (2, 'Bob'), (3, 'Carol')")
				sqlDB.Exec(t, "INSERT INTO bar VALUES (1, 'a'), (2, 'b'), (3, 'c')")

				sqlDB.CheckQueryResultsRetry(t, `SELECT count(*) FROM foo,bar`, [][]string{{`9`}})

				feed := feed(t, f, testData.changefeedStmt)

				sqlDB.Exec(t, "INSERT INTO foo VALUES (4, 'Doug'), (5, 'Elaine'), (6, 'Fred')")
				sqlDB.Exec(t, "INSERT INTO bar VALUES (4, 'd'), (5, 'e'), (6, 'f')")

				var actualMessages []string
				g := ctxgroup.WithContext(context.Background())
				g.Go(func() error {
					for {
						m, err := feed.Next()
						if err != nil {
							return err
						}
						if len(m.Resolved) > 0 {
							continue
						}
						actualMessages = append(actualMessages, string(m.Value))
					}
				})
				defer func(expectedPayload []string) {
					closeFeed(t, feed)
					sqlDB.Exec(t, `DROP TABLE foo`)
					sqlDB.Exec(t, `DROP TABLE bar`)
					_ = g.Wait()
					require.Equal(t, len(expectedPayload), len(actualMessages))
					sort.Strings(expectedPayload)
					sort.Strings(actualMessages)
					for i := range expectedPayload {
						require.Equal(t, expectedPayload[i], actualMessages[i])
					}
				}(testData.expectedPayload)

				jobFeed := feed.(cdctest.EnterpriseTestFeed)
				require.NoError(t, jobFeed.WaitForState(func(s jobs.State) bool {
					return s == jobs.StateSucceeded
				}))
			})
		}
	}

	// TODO(#119289): re-enable pulsar
	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestOmitSinks("pulsar"))
}

func TestChangefeedOnlyInitialScanCSVSinkless(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	initialScanOnlyCSVTests := map[string]string{
		`initial scan only with csv`:     `CREATE CHANGEFEED FOR foo WITH initial_scan_only, format = csv`,
		`initial backfill only with csv`: `CREATE CHANGEFEED FOR foo WITH initial_scan = 'only', format = csv`,
	}

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		for testName, changefeedStmt := range initialScanOnlyCSVTests {
			t.Run(testName, func(t *testing.T) {
				sqlDB.Exec(t, "CREATE TABLE foo (id INT PRIMARY KEY, name STRING)")
				sqlDB.Exec(t, "INSERT INTO foo VALUES (1, 'Alice'), (2, 'Bob'), (3, 'Carol')")

				sqlDB.CheckQueryResultsRetry(t, `SELECT count(*) FROM foo`, [][]string{{`3`}})

				feed := feed(t, f, changefeedStmt)

				sqlDB.Exec(t, "INSERT INTO foo VALUES (4, 'Doug'), (5, 'Elaine'), (6, 'Fred')")

				expectedMessages := []string{
					`1,Alice`,
					`2,Bob`,
					`3,Carol`,
				}
				var actualMessages []string

				defer func() {
					closeFeed(t, feed)
					sqlDB.Exec(t, `DROP TABLE foo`)
					require.Equal(t, len(expectedMessages), len(actualMessages))
					sort.Strings(expectedMessages)
					sort.Strings(actualMessages)
					for i := range expectedMessages {
						require.Equal(t, expectedMessages[i], actualMessages[i])
					}
				}()

				for {
					m, err := feed.Next()
					if err != nil || m == nil {
						break
					}
					actualMessages = append(actualMessages, string(m.Value))
				}
			})
		}
	}

	cdcTest(t, testFn, feedTestForceSink("sinkless"))
}

func TestChangefeedPredicates(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(alias string) cdcTestFn {
		return func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
			sqlDB := sqlutils.MakeSQLRunner(s.DB)
			sqlDB.Exec(t, `CREATE TYPE status AS ENUM ('open', 'closed', 'inactive')`)
			sqlDB.Exec(t, `
CREATE TABLE foo (
  a INT,
  b STRING,
  c STRING,
  d STRING AS (concat(b, c)) VIRTUAL,
  e status DEFAULT 'inactive',
  PRIMARY KEY (a, b)
)`)

			// TODO(#85143): remove schema_change_policy='stop' from this test.
			sqlDB.Exec(t, `
INSERT INTO foo (a, b) VALUES (0, 'zero'), (1, 'one');
INSERT INTO foo (a, b, e) VALUES (2, 'two', 'closed');
`)
			topic, fromClause := "foo", "foo"
			if alias != "" {
				topic, fromClause = "foo", "foo AS "+alias
			}
			feed := feed(t, f, `
CREATE CHANGEFEED
WITH schema_change_policy='stop'
AS SELECT * FROM `+fromClause+`
WHERE e IN ('open', 'closed') AND event_op() != 'delete'`)
			defer closeFeed(t, feed)

			assertPayloads(t, feed, []string{
				topic + `: [2, "two"]->{"a": 2, "b": "two", "c": null, "e": "closed"}`,
			})

			sqlDB.Exec(t, `
UPDATE foo SET e = 'open', c = 'really open' WHERE a=0;  -- should be emitted
DELETE FROM foo WHERE a=2; -- should be skipped
INSERT INTO foo (a, b, e) VALUES (3, 'tres', 'closed'); -- should be emitted
`)

			assertPayloads(t, feed, []string{
				topic + `: [0, "zero"]->{"a": 0, "b": "zero", "c": "really open", "e": "open"}`,
				topic + `: [3, "tres"]->{"a": 3, "b": "tres", "c": null, "e": "closed"}`,
			})
		}
	}

	testutils.RunTrueAndFalse(t, "alias", func(t *testing.T, useAlias bool) {
		alias := ""
		if useAlias {
			alias = "bar"
		}
		cdcTest(t, testFn(alias))
	})
}

// Some predicates and projections can be verified when creating changefeed.
// The types of errors that can be detected early on is restricted to simple checks
// (such as type checking, non-existent columns, etc).  More complex errors detected
// during execution.
// Verify that's the case.
func TestChangefeedInvalidPredicate(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	_, db, stopServer := startTestFullServer(t, makeOptions(t, feedTestNoTenants))
	defer stopServer()
	sqlDB := sqlutils.MakeSQLRunner(db)

	sqlDB.Exec(t, `CREATE TYPE status AS ENUM ('open', 'closed', 'inactive')`)
	sqlDB.Exec(t, `
CREATE TABLE foo (
  a INT,
  b STRING,
  c STRING,
  d STRING AS (concat(b, c)) VIRTUAL,
  e status DEFAULT 'inactive',
  PRIMARY KEY (a, b)
)`)

	for _, tc := range []struct {
		name   string
		create string
		err    string
	}{
		{
			name:   "no such column",
			create: `CREATE CHANGEFEED INTO 'null://' AS SELECT no_such_column FROM foo`,
			err:    `column "no_such_column" does not exist`,
		},
		{
			name:   "wrong type",
			create: `CREATE CHANGEFEED INTO 'null://' AS SELECT * FROM foo WHERE a = 'wrong type'`,
			err:    `could not parse "wrong type" as type int`,
		},
		{
			name:   "invalid enum value",
			create: `CREATE CHANGEFEED INTO 'null://' AS SELECT * FROM foo WHERE e = 'bad'`,
			err:    `invalid input value for enum status: "bad"`,
		},
		{
			name:   "contradiction: a > 1 && a < 1",
			create: `CREATE CHANGEFEED INTO 'null://'  AS SELECT * FROM foo WHERE a > 1 AND a < 1`,
			err:    `does not match any rows`,
		},
		{
			name:   "contradiction: a IS null",
			create: `CREATE CHANGEFEED INTO 'null://' AS SELECT * FROM foo WHERE a IS NULL`,
			err:    `does not match any rows`,
		},
		{
			name:   "wrong table name",
			create: `CREATE CHANGEFEED INTO 'null://' AS SELECT * FROM foo AS bar WHERE foo.a > 0`,
			err:    `no data source matches prefix: foo in this context`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB.ExpectErrWithTimeout(t, tc.err, tc.create)
		})
	}
}

func TestChangefeedPredicateWithSchemaChange(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t, "takes too long under race")

	setupSQL := []string{
		`CREATE TYPE status AS ENUM ('open', 'closed', 'inactive')`,
		`CREATE SCHEMA alt`,
		`CREATE TYPE alt.status AS ENUM ('alt_open', 'alt_closed')`,
		`CREATE TABLE foo (
  a INT,
  b STRING,
  c STRING,
  e status DEFAULT 'inactive',
  PRIMARY KEY (a, b)
)`,
		`INSERT INTO foo (a, b) VALUES (1, 'one')`,
		`INSERT INTO foo (a, b, c, e) VALUES (2, 'two', 'c string', 'open')`,
	}
	initialPayload := []string{
		`foo: [1, "one"]->{"a": 1, "b": "one", "c": null, "e": "inactive"}`,
		`foo: [2, "two"]->{"a": 2, "b": "two", "c": "c string", "e": "open"}`,
	}

	type testCase struct {
		name                string
		disableSchemaLocked bool
		createFeedStmt      string   // Create changefeed statement.
		initialPayload      []string // Expected payload after create.
		alterStmt           string   // Alter statement to execute.
		afterAlterStmt      string   // Execute after alter statement.
		expectErr           string   // Alter may result in changefeed terminating with error.
		payload             []string // Expect the following payload after executing afterAlterStmt.
	}

	testFn := func(tc testCase) cdcTestFn {
		return func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
			sqlDB := sqlutils.MakeSQLRunner(s.DB)

			if tc.disableSchemaLocked {
				sqlDB.Exec(t, "SET create_table_with_schema_locked=false")
			}
			sqlDB.ExecMultiple(t, setupSQL...)
			foo := feed(t, f, tc.createFeedStmt)
			feedJob := foo.(cdctest.EnterpriseTestFeed)
			defer closeFeed(t, foo)

			assertPayloads(t, foo, tc.initialPayload)

			sqlDB.Exec(t, tc.alterStmt)

			// Execute afterAlterStmt immediately following alterStmt. Sometimes, we
			// need to e.g. insert new rows in order to observe changefeed error.
			if tc.afterAlterStmt != "" {
				sqlDB.Exec(t, tc.afterAlterStmt)
			}

			if tc.expectErr != "" {
				require.NoError(t, feedJob.WaitForState(
					func(s jobs.State) bool { return s == jobs.StateFailed }))
				require.Regexp(t, tc.expectErr, feedJob.FetchTerminalJobErr())
			} else {
				assertPayloads(t, foo, tc.payload)
			}
		}
	}

	for _, tc := range []testCase{
		{
			name:           "add column no default",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT * FROM foo",
			initialPayload: initialPayload,
			alterStmt:      "ALTER TABLE foo ADD COLUMN new STRING",
			afterAlterStmt: "INSERT INTO foo (a, b) VALUES (3, 'tres')",
			payload: []string{
				`foo: [3, "tres"]->{"a": 3, "b": "tres", "c": null, "e": "inactive", "new": null}`,
			},
		},
		{
			name:           "add column",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT * FROM foo",
			initialPayload: initialPayload,
			alterStmt:      "ALTER TABLE foo ADD COLUMN new STRING DEFAULT 'new'",
			payload: []string{
				`foo: [1, "one"]->{"a": 1, "b": "one", "c": null, "e": "inactive", "new": "new"}`,
				`foo: [2, "two"]->{"a": 2, "b": "two", "c": "c string", "e": "open", "new": "new"}`,
			},
		},
		{
			// This test adds a column with 'alt.status' type. The table already has a
			// column "e" with "public.status" type. Verify that we correctly resolve
			// enums with the same enum name.
			name:           "add alt.status",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT * FROM foo",
			initialPayload: initialPayload,
			alterStmt:      "ALTER TABLE foo ADD COLUMN alt alt.status DEFAULT 'alt_closed'",
			afterAlterStmt: "INSERT INTO foo (a, b, alt) VALUES (3, 'tres', 'alt_open')",
			payload: []string{
				`foo: [1, "one"]->{"a": 1, "alt": "alt_closed", "b": "one", "c": null, "e": "inactive"}`,
				`foo: [2, "two"]->{"a": 2, "alt": "alt_closed", "b": "two", "c": "c string", "e": "open"}`,
				`foo: [3, "tres"]->{"a": 3, "alt": "alt_open", "b": "tres", "c": null, "e": "inactive"}`,
			},
		},
		{
			name:           "drop column",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT * FROM foo",
			initialPayload: initialPayload,
			alterStmt:      "ALTER TABLE foo DROP COLUMN c",
			afterAlterStmt: "INSERT INTO foo (a, b) VALUES (3, 'tres')",
			payload: []string{
				`foo: [1, "one"]->{"a": 1, "b": "one", "e": "inactive"}`,
				`foo: [2, "two"]->{"a": 2, "b": "two", "e": "open"}`,
				`foo: [3, "tres"]->{"a": 3, "b": "tres", "e": "inactive"}`,
			},
		},
		{
			name:           "drop referenced column projection",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT a, b, c, e FROM foo",
			initialPayload: initialPayload,
			alterStmt:      "ALTER TABLE foo DROP COLUMN c",
			expectErr:      `column "c" does not exist`,
		},
		{
			name:           "drop referenced column filter",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT * FROM foo WHERE c IS NOT NULL",
			initialPayload: []string{
				`foo: [2, "two"]->{"a": 2, "b": "two", "c": "c string", "e": "open"}`,
			},
			alterStmt: "ALTER TABLE foo DROP COLUMN c",
			expectErr: `column "c" does not exist`,
		},
		{
			name:           "rename referenced column projection",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT a, b, c, e FROM foo",
			initialPayload: initialPayload,
			alterStmt:      "ALTER TABLE foo RENAME COLUMN c TO c_new",
			afterAlterStmt: "INSERT INTO foo (a, b) VALUES (3, 'tres')",
			expectErr:      `column "c" does not exist`,
		},
		{
			name:           "rename referenced column filter",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT * FROM foo WHERE c IS NOT NULL",
			initialPayload: []string{
				`foo: [2, "two"]->{"a": 2, "b": "two", "c": "c string", "e": "open"}`,
			},
			alterStmt:      "ALTER TABLE foo RENAME COLUMN c TO c_new",
			afterAlterStmt: "INSERT INTO foo (a, b) VALUES (3, 'tres')",
			expectErr:      `column "c" does not exist`,
		},
		{
			name:           "alter enum",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT * FROM foo",
			initialPayload: initialPayload,
			alterStmt:      "ALTER TYPE status ADD VALUE 'pending'",
			afterAlterStmt: "INSERT INTO foo (a, b, e) VALUES (3, 'tres', 'pending')",
			payload: []string{
				`foo: [3, "tres"]->{"a": 3, "b": "tres", "c": null, "e": "pending"}`,
			},
		},
		{
			name:           "alter enum value fails",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT * FROM foo WHERE e = 'open'",
			initialPayload: []string{
				`foo: [2, "two"]->{"a": 2, "b": "two", "c": "c string", "e": "open"}`,
			},
			alterStmt:      "ALTER TYPE status RENAME VALUE 'open' TO 'active'",
			afterAlterStmt: "INSERT INTO foo (a, b, e) VALUES (3, 'tres', 'active')",
			expectErr:      `invalid input value for enum status: "open"`,
		},
		{
			name:           "alter enum use correct enum version",
			createFeedStmt: "CREATE CHANGEFEED AS SELECT e, (cdc_prev).e AS prev_e FROM foo",
			initialPayload: []string{
				`foo: [1, "one"]->{"e": "inactive", "prev_e": null}`,
				`foo: [2, "two"]->{"e": "open", "prev_e": null}`,
			},
			alterStmt:      "ALTER TYPE status ADD VALUE 'done'",
			afterAlterStmt: "UPDATE foo SET e = 'done', c = 'c value' WHERE a = 1",
			payload: []string{
				`foo: [1, "one"]->{"e": "done", "prev_e": "inactive"}`,
			},
		},
		{
			// Alter and rename a column. The changefeed expression does not
			// explicitly involve the column in question (c) -- so, schema change works
			// fine. Note: we get 2 backfill events -- one for each logical change
			// (rename column, then add column).
			name:                "add and rename column",
			disableSchemaLocked: true, // legacy schema change
			createFeedStmt:      "CREATE CHANGEFEED AS SELECT *, (cdc_prev).e as old_e FROM foo",
			initialPayload: []string{
				`foo: [1, "one"]->{"a": 1, "b": "one", "c": null, "e": "inactive", "old_e": null}`,
				`foo: [2, "two"]->{"a": 2, "b": "two", "c": "c string", "e": "open", "old_e": null}`,
			},
			alterStmt: "ALTER TABLE foo RENAME COLUMN c to c_old, ADD COLUMN c int DEFAULT 42",
			payload: []string{
				`foo: [1, "one"]->{"a": 1, "b": "one", "c": 42, "c_old": null, "e": "inactive", "old_e": "inactive"}`,
				`foo: [1, "one"]->{"a": 1, "b": "one", "c_old": null, "e": "inactive", "old_e": "inactive"}`,
				`foo: [2, "two"]->{"a": 2, "b": "two", "c": 42, "c_old": "c string", "e": "open", "old_e": "open"}`,
				`foo: [2, "two"]->{"a": 2, "b": "two", "c_old": "c string", "e": "open", "old_e": "open"}`,
			},
		},
		{
			// Alter and rename a column. The changefeed expression does
			// explicitly involve the column in question (c) -- so we expect
			// to get an error because as soon as the first rename goes through, column
			// no longer exists.
			name:                "add and rename column error",
			disableSchemaLocked: true, // legacy schema change
			createFeedStmt:      "CREATE CHANGEFEED AS SELECT c, (cdc_prev).c AS prev_c FROM foo",
			initialPayload: []string{
				`foo: [1, "one"]->{"c": null, "prev_c": null}`,
				`foo: [2, "two"]->{"c": "c string", "prev_c": null}`,
			},
			alterStmt: "ALTER TABLE foo RENAME COLUMN c to c_old, ADD COLUMN c int DEFAULT 42",
			expectErr: `column "c" does not exist`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testOpts := []feedTestOption{feedTestEnterpriseSinks}
			if tc.expectErr != "" {
				testOpts = append(testOpts, withAllowChangefeedErr(tc.expectErr))
			}
			cdcTest(t, testFn(tc), testOpts...)
		})
	}
}

func startMonitorWithBudget(budget int64) *mon.BytesMonitor {
	mm := mon.NewMonitor(mon.Options{
		Name:      mon.MakeName("test-mm"),
		Limit:     budget,
		Increment: 128, /* small allocation increment */
		Settings:  cluster.MakeTestingClusterSettings(),
	})
	mm.Start(context.Background(), nil, mon.NewStandaloneBudget(budget))
	return mm
}

type testSink struct{}

// getConcreteType implements the Sink interfaces.
func (s testSink) getConcreteType() sinkType {
	return sinkTypeNull
}

type memoryHoggingSink struct {
	testSink
	allEmitted chan struct{}
	mu         struct {
		syncutil.Mutex
		expectedRows int
		seenRows     map[string]struct{}
		numFlushes   int
		alloc        kvevent.Alloc
	}
}

var _ Sink = (*memoryHoggingSink)(nil)

func (s *memoryHoggingSink) expectRows(n int) chan struct{} {
	if n <= 0 {
		panic("n<=0")
	}
	s.allEmitted = make(chan struct{})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mu.expectedRows = n
	s.mu.numFlushes = 0
	s.mu.seenRows = make(map[string]struct{})
	s.mu.alloc.Release(context.Background()) // Release leftover alloc
	return s.allEmitted
}

func (s *memoryHoggingSink) Dial() error {
	return nil
}

func (s *memoryHoggingSink) EmitRow(
	ctx context.Context,
	topic TopicDescriptor,
	key, value []byte,
	updated, mvcc hlc.Timestamp,
	alloc kvevent.Alloc,
	headers rowHeaders,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mu.seenRows[string(key)] = struct{}{}
	s.mu.alloc.Merge(&alloc)
	if s.mu.expectedRows == len(s.mu.seenRows) && s.allEmitted != nil {
		close(s.allEmitted)
		s.allEmitted = nil
	}
	return nil
}

func (s *memoryHoggingSink) EmitResolvedTimestamp(
	ctx context.Context, encoder Encoder, resolved hlc.Timestamp,
) error {
	panic("should not be called")
}

func (s *memoryHoggingSink) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mu.numFlushes++
	s.mu.alloc.Release(ctx)
	return nil
}

func (s *memoryHoggingSink) numFlushes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mu.numFlushes
}
func (s *memoryHoggingSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mu.alloc.Release(context.Background())
	return nil
}

type countEmittedRowsSink struct {
	memoryHoggingSink
	numRows int64 // Accessed atomically; not using atomic.Int64 to make backports possible.
}

func (s *countEmittedRowsSink) EmitRow(
	ctx context.Context,
	topic TopicDescriptor,
	key, value []byte,
	updated, mvcc hlc.Timestamp,
	alloc kvevent.Alloc,
	_headers rowHeaders,
) error {
	alloc.Release(ctx)
	atomic.AddInt64(&s.numRows, 1)
	return nil
}

var _ Sink = (*countEmittedRowsSink)(nil)

func TestChangefeedFlushesSinkToReleaseMemory(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, stopServer := makeServer(t)
	defer stopServer()

	sqlDB := sqlutils.MakeSQLRunner(s.DB)
	knobs := s.TestingKnobs.
		DistSQL.(*execinfra.TestingKnobs).
		Changefeed.(*TestingKnobs)

	// Arrange for a small memory budget.
	knobs.MemMonitor = startMonitorWithBudget(4096)

	// Ignore resolved events delivered to this changefeed.  This has
	// an effect of never advancing the frontier, and thus never flushing
	// the sink due to frontier advancement.  The only time we flush the sink
	// is if the memory pressure causes flush request to be delivered.
	knobs.FilterSpanWithMutation = func(_ *jobspb.ResolvedSpan) (bool, error) {
		return true, nil
	}

	// Arrange for custom sink to be used -- a sink that does not
	// release its resources.
	sink := &memoryHoggingSink{}
	knobs.WrapSink = func(_ Sink, _ jobspb.JobID) Sink {
		return sink
	}

	// Create table, and insert 123 rows in it -- this fills up
	// our tiny memory buffer (~26 rows do)
	sqlDB.Exec(t, `CREATE TABLE foo(key INT PRIMARY KEY DEFAULT unique_rowid(), val INT)`)
	sqlDB.Exec(t, `INSERT INTO foo (val) SELECT * FROM generate_series(1, 123)`)

	// Expect 123 rows from backfill.
	allEmitted := sink.expectRows(123)

	sqlDB.Exec(t, `CREATE CHANGEFEED FOR foo INTO 'http://host/does/not/matter'`)

	<-allEmitted
	require.Greater(t, sink.numFlushes(), 0)

	// Insert another set of rows.  This now uses rangefeeds.
	allEmitted = sink.expectRows(123)
	sqlDB.Exec(t, `INSERT INTO foo (val) SELECT * FROM generate_series(1, 123)`)
	<-allEmitted
	require.Greater(t, sink.numFlushes(), 0)
}

// Test verifies that KV feed does not leak event memory allocation
// when it reaches end_time or scan boundary.
func TestKVFeedDoesNotLeakMemoryWhenSkippingEvents(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, stopServer := makeServer(t)
	defer stopServer()

	sqlDB := sqlutils.MakeSQLRunner(s.DB)
	knobs := s.TestingKnobs.
		DistSQL.(*execinfra.TestingKnobs).
		Changefeed.(*TestingKnobs)

	// Arrange for a small memory budget.
	knobs.MemMonitor = startMonitorWithBudget(4096)

	// Arrange for custom sink to be used -- a sink that counts emitted rows.
	sink := &countEmittedRowsSink{}
	knobs.WrapSink = func(_ Sink, _ jobspb.JobID) Sink {
		return sink
	}
	sqlDB.Exec(t, `CREATE TABLE foo(key INT PRIMARY KEY DEFAULT unique_rowid(), val INT)`)

	startTime := s.Server.Clock().Now().AsOfSystemTime()

	// Insert 123 rows -- this fills up our tiny memory buffer (~26 rows do)
	// Collect statement timestamp -- this will become our end time.
	var insertTimeStr string
	sqlDB.QueryRow(t,
		`INSERT INTO foo (val) SELECT * FROM generate_series(1, 123) RETURNING cluster_logical_timestamp();`,
	).Scan(&insertTimeStr)
	endTime := parseTimeToHLC(t, insertTimeStr).AsOfSystemTime()

	// Start the changefeed, with end_time set to be equal to the insert time.
	// KVFeed should ignore all events.
	var jobID jobspb.JobID
	sqlDB.QueryRow(t, `CREATE CHANGEFEED FOR foo INTO 'null:' WITH cursor = $1, end_time = $2`,
		startTime, endTime).Scan(&jobID)

	// If everything is fine (events are ignored, but their memory allocation is released),
	// the changefeed should terminate.  If not, we'll time out waiting for job.
	waitForJobState(sqlDB, t, jobID, jobs.StateSucceeded)

	// No rows should have been emitted (all should have been filtered out due to end_time).
	require.EqualValues(t, 0, atomic.LoadInt64(&sink.numRows))
}

func TestChangefeedMultiPodTenantPlanning(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	skip.UnderRace(t, "may time out due to multiple servers")

	// Record the number of aggregators in planning
	aggregatorCount := 0

	// Create 2 connections of the same tenant on a cluster to have 2 pods
	tc, _, cleanupDB := startTestCluster(t)
	defer cleanupDB()

	tenantKnobs := base.TestingKnobs{
		DistSQL: &execinfra.TestingKnobs{Changefeed: &TestingKnobs{
			OnDistflowSpec: func(aggregatorSpecs []*execinfrapb.ChangeAggregatorSpec, _ *execinfrapb.ChangeFrontierSpec) {
				aggregatorCount = len(aggregatorSpecs)
			},
		}},
		JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
		Server:           &server.TestingKnobs{},
	}
	tenant1Args := base.TestTenantArgs{
		TenantID:     serverutils.TestTenantID(),
		TestingKnobs: tenantKnobs,
		UseDatabase:  `d`,
	}
	tenant1Server, tenant1DB := serverutils.StartTenant(t, tc.Server(0), tenant1Args)
	tenantRunner := sqlutils.MakeSQLRunner(tenant1DB)
	tenantRunner.ExecMultiple(t, strings.Split(tenantSetupStatements, ";")...)
	sql1 := sqlutils.MakeSQLRunner(tenant1DB)
	defer tenant1DB.Close()

	tenant2Args := tenant1Args
	tenant2Args.DisableCreateTenant = true
	_, db2 := serverutils.StartTenant(t, tc.Server(1), tenant2Args)
	defer db2.Close()

	// Ensure both pods can be assigned work
	waitForTenantPodsActive(t, tenant1Server, 2)

	feedFactory, cleanupSink := makeFeedFactory(t, randomSinkType(t, feedTestEnterpriseSinks), tenant1Server, tenant1DB)
	defer cleanupSink()

	// Run a changefeed across two tables to guarantee multiple spans that can be spread across the aggregators
	sql1.Exec(t, "CREATE TABLE foo (a INT PRIMARY KEY)")
	sql1.Exec(t, "INSERT INTO foo VALUES (1), (2)")
	sql1.Exec(t, "CREATE TABLE bar (b INT PRIMARY KEY)")
	sql1.Exec(t, "INSERT INTO bar VALUES (1), (2)")

	foo := feed(t, feedFactory, "CREATE CHANGEFEED FOR foo, bar")
	defer closeFeed(t, foo)

	assertPayloads(t, foo, []string{
		`foo: [1]->{"after": {"a": 1}}`,
		`foo: [2]->{"after": {"a": 2}}`,
		`bar: [1]->{"after": {"b": 1}}`,
		`bar: [2]->{"after": {"b": 2}}`,
	})

	require.Equal(t, 2, aggregatorCount)
}

func TestCreateChangefeedTelemetryLogs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, stopServer := makeServer(t)
	defer stopServer()

	sqlDB := sqlutils.MakeSQLRunner(s.DB)
	sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
	sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
	sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY, b STRING)`)
	sqlDB.Exec(t, `INSERT INTO bar VALUES (0, 'initial')`)

	t.Run(`core_sink_type`, func(t *testing.T) {
		coreFeedFactory, cleanup := makeFeedFactory(t, "sinkless", s.Server, s.DB)
		defer cleanup()

		beforeCreateSinkless := timeutil.Now()
		coreFeed := feed(t, coreFeedFactory, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, coreFeed)

		createLogs := checkCreateChangefeedLogs(t, beforeCreateSinkless.UnixNano())
		require.Equal(t, 1, len(createLogs))
		require.Equal(t, "core", createLogs[0].SinkType)
	})

	t.Run(`gcpubsub_sink_type_with_options`, func(t *testing.T) {
		pubsubFeedFactory := makePubsubFeedFactory(s.Server, s.DB)
		beforeCreatePubsub := timeutil.Now()
		pubsubFeed := feed(t, pubsubFeedFactory, `CREATE CHANGEFEED FOR foo, bar WITH resolved="10s", no_initial_scan`)
		defer closeFeed(t, pubsubFeed)

		createLogs := checkCreateChangefeedLogs(t, beforeCreatePubsub.UnixNano())
		require.Equal(t, 1, len(createLogs))
		require.Equal(t, `gcpubsub`, createLogs[0].SinkType)
		require.Equal(t, int32(2), createLogs[0].NumTables)
		require.Equal(t, `10s`, createLogs[0].Resolved)
		require.Equal(t, `no`, createLogs[0].InitialScan)
		require.Equal(t, false, createLogs[0].Transformation)
	})

	t.Run(`with_transformation`, func(t *testing.T) {
		pubsubFeedFactory := makePubsubFeedFactory(s.Server, s.DB)
		beforeCreateWithTransformation := timeutil.Now()
		pubsubFeed := feed(t, pubsubFeedFactory, `CREATE CHANGEFEED AS SELECT b FROM foo`)
		defer closeFeed(t, pubsubFeed)

		createLogs := checkCreateChangefeedLogs(t, beforeCreateWithTransformation.UnixNano())
		require.Equal(t, 1, len(createLogs))
		require.Equal(t, true, createLogs[0].Transformation)
	})
}

func TestAlterChangefeedTelemetryLogs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cdcTest(t, func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY, b STRING)`)

		beforeCreate := timeutil.Now()
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo, bar`)
		defer closeFeed(t, testFeed)
		feed := testFeed.(cdctest.EnterpriseTestFeed)

		// Alter changefeed to drop bar as a target and set resolved.
		require.NoError(t, feed.Pause())
		sqlDB.Exec(t, `ALTER CHANGEFEED $1 DROP bar SET resolved`, feed.JobID())
		require.NoError(t, feed.Resume())

		var logs []eventpb.AlterChangefeed
		testutils.SucceedsSoon(t, func() error {
			logs = checkAlterChangefeedLogs(t, beforeCreate.UnixNano())
			if len(logs) < 1 {
				return errors.New("no logs found")
			}
			return nil
		})

		require.Len(t, logs, 1)
		l := logs[0]
		require.EqualValues(t, feed.JobID(), l.JobId)
		require.Equal(t, `alter_changefeed`, l.EventType)
		require.Contains(t, l.PreviousDescription, `bar`)
		require.NotContains(t, l.Description, `bar`)
		require.Equal(t, "yes", l.Resolved)
	}, feedTestEnterpriseSinks)
}

// Note that closeFeed needs to be called in order for the logs to be detected
func TestChangefeedFailedTelemetryLogs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	waitForLogs := func(t *testing.T, startTime time.Time) []eventpb.ChangefeedFailed {
		var logs []eventpb.ChangefeedFailed
		testutils.SucceedsSoon(t, func() error {
			logs = checkChangefeedFailedLogs(t, startTime.UnixNano())
			if len(logs) < 1 {
				return fmt.Errorf("no logs found")
			}
			return nil
		})
		return logs
	}

	t.Run(`connection_closed`, func(t *testing.T) {
		s, stopServer := makeServer(t, withAllowChangefeedErr("expects error"))
		defer stopServer()

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		sqlDB.Exec(t, `UPSERT INTO foo VALUES (0, 'updated')`)

		coreFactory, sinkCleanup := makeFeedFactory(t, "sinkless", s.Server, s.DB)
		coreFeed := feed(t, coreFactory, `CREATE CHANGEFEED FOR foo`)
		assertPayloads(t, coreFeed, []string{
			`foo: [0]->{"after": {"a": 0, "b": "updated"}}`,
		})
		beforeCoreSinkClose := timeutil.Now()

		sinkCleanup()
		closeFeed(t, coreFeed)

		failLogs := waitForLogs(t, beforeCoreSinkClose)
		require.Equal(t, 1, len(failLogs))
		require.Equal(t, failLogs[0].FailureType, changefeedbase.ConnectionClosed)
	})

	cdcTestNamed(t, "user_input", func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		beforeCreate := timeutil.Now()
		_, err := f.Feed(`CREATE CHANGEFEED FOR foo, invalid_table`)
		require.Error(t, err)

		failLogs := waitForLogs(t, beforeCreate)
		require.Equal(t, 1, len(failLogs))
		require.Equal(t, failLogs[0].FailureType, changefeedbase.UserInput)
	}, feedTestEnterpriseSinks, withAllowChangefeedErr("expects error"))

	cdcTestNamed(t, "unknown_error", func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)
		knobs.BeforeEmitRow = func(_ context.Context) error {
			return changefeedbase.WithTerminalError(errors.New("should fail"))
		}

		beforeCreate := timeutil.Now()
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH on_error=FAIL`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'next')`)
		feedJob := foo.(cdctest.EnterpriseTestFeed)
		require.NoError(t, feedJob.WaitForState(func(s jobs.State) bool { return s == jobs.StateFailed }))

		closeFeed(t, foo)
		failLogs := waitForLogs(t, beforeCreate)
		require.Equal(t, 1, len(failLogs))
		require.Equal(t, failLogs[0].FailureType, changefeedbase.UnknownError)
		require.Contains(t, []string{`gcpubsub`, `external`}, failLogs[0].SinkType)
		require.Equal(t, failLogs[0].NumTables, int32(1))
	}, feedTestForceSink("pubsub"), withAllowChangefeedErr("expects error"))
}

func TestChangefeedCanceledTelemetryLogs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	waitForLogs := func(t *testing.T, startTime time.Time) []eventpb.ChangefeedCanceled {
		var logs []eventpb.ChangefeedCanceled
		testutils.SucceedsSoon(t, func() error {
			logs = checkChangefeedCanceledLogs(t, startTime.UnixNano())
			if len(logs) < 1 {
				return fmt.Errorf("no logs found")
			}
			return nil
		})
		return logs
	}

	cdcTestNamed(t, "canceled enterprise changefeeds", func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		beforeCreate := timeutil.Now()
		feed, err := f.Feed(`CREATE CHANGEFEED FOR foo`)
		require.NoError(t, err)
		enterpriseFeed := feed.(cdctest.EnterpriseTestFeed)

		sqlDB.Exec(t, `CANCEL JOB $1`, enterpriseFeed.JobID())

		canceledLogs := waitForLogs(t, beforeCreate)
		require.Equal(t, 1, len(canceledLogs))
		require.Equal(t, enterpriseFeed.JobID().String(), strconv.FormatInt(canceledLogs[0].JobId, 10))
		require.Equal(t, "changefeed_canceled", canceledLogs[0].EventType)
		require.NoError(t, feed.Close())
	}, feedTestEnterpriseSinks)
}

func TestChangefeedTestTimesOut(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		nada := feed(t, f, "CREATE CHANGEFEED FOR foo WITH resolved='100ms'")
		defer func() {
			// close could return an error due to the race in withTimeout function
			// which cancels the job.
			_ = nada.Close()
		}()

		expectResolvedTimestamp(t, nada) // Make sure feed is running.

		const expectTimeout = 500 * time.Millisecond
		var observedError error
		require.NoError(t,
			testutils.SucceedsWithinError(func() error {
				observedError = withTimeout(
					nada, expectTimeout,
					func(ctx context.Context) error {
						return assertPayloadsBaseErr(
							ctx, nada, []string{`nada: [2]->{"after": {}}`}, false, false, nil, changefeedbase.OptEnvelopeWrapped)
					})
				return nil
			}, 20*expectTimeout))

		require.Error(t, observedError)
	}

	cdcTest(t, testFn)
}

// Regression for #85008.
func TestSchemachangeDoesNotBreakSinklessFeed(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE mytable (id INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO mytable VALUES (0)`)

		// Open up the changefeed.
		cf := feed(t, f, `CREATE CHANGEFEED FOR TABLE mytable`)
		defer closeFeed(t, cf)
		assertPayloads(t, cf, []string{
			`mytable: [0]->{"after": {"id": 0}}`,
		})

		sqlDB.Exec(t, `ALTER TABLE mytable ADD COLUMN val INT DEFAULT 0`)
		assertPayloads(t, cf, []string{
			`mytable: [0]->{"after": {"id": 0, "val": 0}}`,
		})
		sqlDB.Exec(t, `INSERT INTO mytable VALUES (1,1)`)
		assertPayloads(t, cf, []string{
			`mytable: [1]->{"after": {"id": 1, "val": 1}}`,
		})
	}

	runWithAndWithoutRegression141453(t, testFn, func(t *testing.T, testFn cdcTestFn) {
		cdcTest(t, testFn, feedTestForceSink("sinkless"))
	})
}

func TestChangefeedKafkaMessageTooLarge(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		if KafkaV2Enabled.Get(&s.Server.ClusterSettings().SV) {
			// This is already covered for the v2 sink in another test: TestKafkaSinkClientV2_Resize
			return
		}

		changefeedbase.BatchReductionRetryEnabled.Override(
			context.Background(), &s.Server.ClusterSettings().SV, true)

		knobs := mustBeKafkaFeedFactory(f).knobs
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (2)`)

		t.Run(`succeed eventually if batches are rejected by the server for being too large`, func(t *testing.T) {
			// MaxMessages of 0 means unlimited
			foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH kafka_sink_config='{"Flush": {"MaxMessages": 0}}'`)
			defer closeFeed(t, foo)
			assertPayloads(t, foo, []string{
				`foo: [1]->{"after": {"a": 1}}`,
				`foo: [2]->{"after": {"a": 2}}`,
			})

			// Messages should be sent by a smaller and smaller MaxMessages config
			// only until ErrMessageSizeTooLarge is no longer returned.
			knobs.kafkaInterceptor = func(m *sarama.ProducerMessage, client kafkaClient) error {
				maxMessages := client.Config().Producer.Flush.MaxMessages
				if maxMessages == 0 || maxMessages >= 250 {
					return sarama.ErrMessageSizeTooLarge
				}
				require.Greater(t, maxMessages, 100)
				return nil
			}

			sqlDB.Exec(t, `INSERT INTO foo VALUES (3)`)
			sqlDB.Exec(t, `INSERT INTO foo VALUES (4)`)
			assertPayloads(t, foo, []string{
				`foo: [3]->{"after": {"a": 3}}`,
				`foo: [4]->{"after": {"a": 4}}`,
			})
			sqlDB.Exec(t, `INSERT INTO foo VALUES (5)`)
			sqlDB.Exec(t, `INSERT INTO foo VALUES (6)`)
			assertPayloads(t, foo, []string{
				`foo: [5]->{"after": {"a": 5}}`,
				`foo: [6]->{"after": {"a": 6}}`,
			})
		})

		t.Run(`succeed against a large backfill`, func(t *testing.T) {
			sqlDB.Exec(t, `CREATE TABLE large (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `INSERT INTO large (a) SELECT * FROM generate_series(1, 2000);`)

			foo := feed(t, f, `CREATE CHANGEFEED FOR large WITH kafka_sink_config='{"Flush": {"MaxMessages": 1000}}'`)
			defer closeFeed(t, foo)

			rnd, _ := randutil.NewPseudoRand()

			maxFailures := int32(200)
			var numFailures atomic.Int32
			knobs.kafkaInterceptor = func(m *sarama.ProducerMessage, client kafkaClient) error {
				if client.Config().Producer.Flush.MaxMessages > 1 && numFailures.Add(1) < maxFailures && rnd.Int()%10 == 0 {
					return sarama.ErrMessageSizeTooLarge
				}
				return nil
			}

			var expected []string
			for i := 1; i <= 2000; i++ {
				expected = append(expected, fmt.Sprintf(
					`large: [%d]->{"after": {"a": %d}}`, i, i,
				))
			}
			assertPayloads(t, foo, expected)
		})

		// Validate that different failure scenarios result in a full changefeed retry
		sqlDB.Exec(t, `CREATE TABLE errors (a INT PRIMARY KEY);`)
		sqlDB.Exec(t, `INSERT INTO errors (a) SELECT * FROM generate_series(1, 1000);`)
		for _, failTest := range []struct {
			failInterceptor func(m *sarama.ProducerMessage, client kafkaClient) error
			errMsg          string
		}{
			{
				func(m *sarama.ProducerMessage, client kafkaClient) error {
					return sarama.ErrMessageSizeTooLarge
				},
				"kafka server: Message was too large, server rejected it to avoid allocation error",
			},
			{
				func(m *sarama.ProducerMessage, client kafkaClient) error {
					return errors.Errorf("unrelated error")
				},
				"unrelated error",
			},
			{
				func(m *sarama.ProducerMessage, client kafkaClient) error {
					maxMessages := client.Config().Producer.Flush.MaxMessages
					if maxMessages == 0 || maxMessages > 250 {
						return sarama.ErrMessageSizeTooLarge
					}
					return errors.Errorf("unrelated error mid-retry")
				},
				"unrelated error mid-retry",
			},
			{
				func() func(m *sarama.ProducerMessage, client kafkaClient) error {
					// Trigger an internal retry for the first message but have successive
					// messages throw a non-retryable error. This can happen in practice
					// when the second message is on a different topic to the first.
					startedBuffering := false
					return func(m *sarama.ProducerMessage, client kafkaClient) error {
						if !startedBuffering {
							startedBuffering = true
							return sarama.ErrMessageSizeTooLarge
						}
						return errors.Errorf("unrelated error mid-buffering")
					}
				}(),
				"unrelated error mid-buffering",
			},
		} {
			t.Run(fmt.Sprintf(`eventually surface error for retry: %s`, failTest.errMsg), func(t *testing.T) {
				knobs.kafkaInterceptor = failTest.failInterceptor
				foo := feed(t, f, `CREATE CHANGEFEED FOR errors WITH kafka_sink_config='{"Flush": {"MaxMessages": 0}}'`)
				defer closeFeed(t, foo)

				feedJob := foo.(cdctest.EnterpriseTestFeed)

				// check that running status correctly updates with retryable error
				testutils.SucceedsSoon(t, func() error {
					status, err := feedJob.FetchStatusMessage()
					if err != nil {
						return err
					}

					if !strings.Contains(status, failTest.errMsg) {
						return errors.Errorf("expected error to contain '%s', got: %v", failTest.errMsg, status)
					}
					return nil
				})
			})
		}
	}

	cdcTest(t, testFn, feedTestForceSink(`kafka`), withAllowChangefeedErr("expects kafka error"))
}

// Regression for #85902.
func TestRedactedSchemaRegistry(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE test_table (id INT PRIMARY KEY, i int, j int)`)

		userInfoToRedact := "7JHKUXMWYD374NV:secret-key"
		registryURI := fmt.Sprintf("https://%s@psrc-x77pq.us-central1.gcp.confluent.cloud:443", userInfoToRedact)

		changefeedDesc := fmt.Sprintf(`CREATE CHANGEFEED FOR TABLE test_table WITH updated,
					confluent_schema_registry =
					"%s";`, registryURI)
		registryURIWithRedaction := strings.Replace(registryURI, userInfoToRedact, "redacted", 1)
		cf := feed(t, f, changefeedDesc)
		defer closeFeed(t, cf)

		var description string
		sqlDB.QueryRow(t, "SELECT description from [SHOW CHANGEFEED JOBS]").Scan(&description)

		assert.Contains(t, description, registryURIWithRedaction)
	}

	// kafka supports the confluent_schema_registry option.
	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestChangefeedMetricsScopeNotice(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, stopServer := makeServer(t)
	defer stopServer()
	sqlDB := sqlutils.MakeSQLRunner(s.DB)
	sqlDB.Exec(t, "CREATE table foo (i int)")
	sqlDB.Exec(t, `SET CLUSTER SETTING server.child_metrics.enabled = false`)

	sqlCreate := "CREATE CHANGEFEED FOR d.foo INTO 'null://' WITH metrics_label='scope'"
	expectNotice(t, s.Server, sqlCreate, `server.child_metrics.enabled is set to false, metrics will only be published to the 'scope' label when it is set to true`)

	var jobID string
	sqlDB.QueryRow(t, `SELECT job_id FROM [SHOW JOBS] where job_type='CHANGEFEED'`).Scan(&jobID)
	sqlDB.Exec(t, "PAUSE JOB $1", jobID)
	sqlDB.CheckQueryResultsRetry(
		t,
		fmt.Sprintf(`SELECT count(*) FROM [SHOW JOBS] WHERE job_type='CHANGEFEED' AND status='%s'`, jobs.StatePaused),
		[][]string{{"1"}},
	)

	sqlAlter := fmt.Sprintf("ALTER CHANGEFEED %s SET metrics_label='other'", jobID)
	expectNotice(t, s.Server, sqlAlter, `server.child_metrics.enabled is set to false, metrics will only be published to the 'other' label when it is set to true`)
}

// TestPubsubValidationErrors tests error messages during pubsub sink URI validations.
func TestPubsubValidationErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s, stopServer := makeServer(t)
	defer stopServer()

	sqlDB := sqlutils.MakeSQLRunner(s.DB)
	sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)
	enableEnterprise := utilccl.TestingDisableEnterprise()
	enableEnterprise()

	for _, tc := range []struct {
		name          string
		uri           string
		expectedError string
	}{
		{
			name:          "project name",
			expectedError: "missing project name",
			uri:           "gcpubsub://?region={region}",
		},
		{
			name:          "region",
			expectedError: "region query parameter not found",
			uri:           "gcpubsub://myproject",
		},
		{
			name:          "credentials for default auth specified",
			expectedError: "missing credentials parameter",
			uri:           "gcpubsub://myproject?region={region}&AUTH=specified",
		},
		{
			name:          "base64",
			expectedError: "illegal base64 data",
			uri:           "gcpubsub://myproject?region={region}&CREDENTIALS={credentials}",
		},
		{
			name:          "invalid json",
			expectedError: "creating credentials from json: invalid character",
			uri: fmt.Sprintf("gcpubsub://myproject?region={region}&CREDENTIALS=%s",
				base64.StdEncoding.EncodeToString([]byte("invalid json"))),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB.ExpectErrWithTimeout(t, tc.expectedError, fmt.Sprintf("CREATE CHANGEFEED FOR foo INTO '%s'", tc.uri))
		})
	}
}

func TestChangefeedExecLocality(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	dir, dirCleanupFn := testutils.TempDir(t)
	defer dirCleanupFn()

	str := strconv.Itoa

	const nodes = 4
	args := base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TODOTestTenantDisabled, // need nodelocal and splits.
		},
		ServerArgsPerNode: map[int]base.TestServerArgs{},
	}
	for i := 0; i < nodes; i++ {
		args.ServerArgsPerNode[i] = base.TestServerArgs{
			ExternalIODir: path.Join(dir, str(i)),
			Locality: roachpb.Locality{
				Tiers: []roachpb.Tier{{Key: "x", Value: str(i / 2)}, {Key: "y", Value: str(i % 2)}}},
		}
	}

	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, nodes, args)
	defer tc.Stopper().Stop(ctx)
	tc.ToggleReplicateQueues(false)

	n2 := sqlutils.MakeSQLRunner(tc.Conns[1])

	// Setup a table with at least one range on each node to be sure we will see a
	// file from that node if it isn't excluded by filter. Relocate can fail with
	// errors like `change replicas... descriptor changed` thus the SucceedsSoon.
	n2.ExecMultiple(t,
		"SET CLUSTER SETTING kv.rangefeed.enabled = true",
		"CREATE TABLE x (id INT PRIMARY KEY)",
		"INSERT INTO x SELECT generate_series(1, 40)",
		"ALTER TABLE x SPLIT AT SELECT id FROM x WHERE id % 5 = 0",
	)
	for _, i := range []string{
		`ALTER TABLE x EXPERIMENTAL_RELOCATE VALUES (ARRAY[1, 2, 3], 0)`,
		`ALTER TABLE x EXPERIMENTAL_RELOCATE VALUES (ARRAY[1, 3, 4], 5)`,
		`ALTER TABLE x EXPERIMENTAL_RELOCATE VALUES (ARRAY[2, 1, 3], 10)`,
		`ALTER TABLE x EXPERIMENTAL_RELOCATE VALUES (ARRAY[2, 1, 4], 15)`,
		`ALTER TABLE x EXPERIMENTAL_RELOCATE VALUES (ARRAY[3, 4, 2], 20)`,
		`ALTER TABLE x EXPERIMENTAL_RELOCATE VALUES (ARRAY[3, 4, 1], 25)`,
		`ALTER TABLE x EXPERIMENTAL_RELOCATE VALUES (ARRAY[4, 2, 1], 30)`,
		`ALTER TABLE x EXPERIMENTAL_RELOCATE VALUES (ARRAY[4, 2, 3], 35)`,
	} {
		n2.ExecSucceedsSoon(t, i)
	}

	test := func(t *testing.T, name, filter string, expect []bool) {
		t.Run(name, func(t *testing.T) {
			// Run and wait for the changefeed.
			var job int
			n2.QueryRow(t, "CREATE CHANGEFEED FOR x INTO $1 WITH initial_scan='only', execution_locality=$2",
				"nodelocal://0/"+name, filter).Scan(&job)
			n2.Exec(t, "SHOW JOB WHEN COMPLETE $1", job)
			// Now check each dir against expectation.
			filesSomewhere := false
			for i := range expect {
				where := path.Join(dir, str(i), name)
				x, err := os.ReadDir(where)
				filesHere := err == nil && len(x) > 0
				if !expect[i] {
					require.False(t, filesHere, where)
				}
				filesSomewhere = filesSomewhere || filesHere
			}
			require.True(t, filesSomewhere)
		})
	}

	test(t, "all", "", []bool{true, true, true, true})
	test(t, "x", "x=0", []bool{true, true, false, false})
	test(t, "y", "y=1", []bool{false, true, false, true})
}

func TestChangefeedTopicNames(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		rand, _ := randutil.NewTestRand()
		cfg := randident.DefaultNameGeneratorConfig()
		cfg.Noise = true
		cfg.Finalize()
		ng := randident.NewNameGenerator(&cfg, rand, "table")

		names, _ := ng.GenerateMultiple(context.Background(), 100, make(map[string]struct{}))

		var escapedNames []string
		for _, name := range names {
			escapedNames = append(escapedNames, strings.ReplaceAll(name, `"`, `""`))
		}

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		for _, name := range escapedNames {
			sqlDB.Exec(t, fmt.Sprintf(`CREATE TABLE "%s" (a INT PRIMARY KEY);`, name))
			sqlDB.Exec(t, fmt.Sprintf(`INSERT INTO "%s" VALUES (1);`, name))
		}

		var quotedNames []string
		for _, name := range escapedNames {
			quotedNames = append(quotedNames, "\""+name+"\"")
		}
		createStmt := fmt.Sprintf(`CREATE CHANGEFEED FOR %s`, strings.Join(quotedNames, ", "))
		foo := feed(t, f, createStmt)
		defer closeFeed(t, foo)

		var expected []string
		for _, name := range names {
			expected = append(expected, fmt.Sprintf(`%s: [1]->{"after": {"a": 1}}`, name))
		}
		assertPayloads(t, foo, expected)
	}

	cdcTest(t, testFn, feedTestForceSink("pubsub"))
}

// Regression test for #108450. When a changefeed hits a retryable error
// and retries, it should start with the most up-to-date highwater (ie. the
// highwater in the job record). If there is an error reading the highwater
// from the job record, there should be retries until we are able to get the
// highwater.
func TestHighwaterDoesNotRegressOnRetry(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		defer changefeedbase.TestingSetDefaultMinCheckpointFrequency(10 * time.Millisecond)()
		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		// NB: We call this in a testing knob which runs in a separate goroutine, so we prefer
		// not to use `require.NoError` because that may panic.
		loadProgressErr := func(jobID jobspb.JobID, jobRegistry *jobs.Registry) (jobspb.Progress, error) {
			job, err := jobRegistry.LoadJob(context.Background(), jobID)
			if err != nil {
				return jobspb.Progress{}, err
			}
			return job.Progress(), nil
		}

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		foo := feed(t, f, `CREATE CHANGEFEED FOR foo WITH resolved = '10ms'`)
		defer closeFeed(t, foo)

		// Rough estimate of the statement time. The test only asserts that
		// things happen after the statement time. Asserting things happen after
		// this is good enough.
		initialHighwater := s.Server.Clock().Now()

		jobFeed := foo.(cdctest.EnterpriseTestFeed)
		jobRegistry := s.Server.JobRegistry().(*jobs.Registry)

		// Pause the changefeed to configure testing knobs which need the job ID.
		require.NoError(t, jobFeed.Pause())

		// A flag we toggle on to put the changefeed in a retrying state.
		var changefeedIsRetrying atomic.Bool
		knobs.RaiseRetryableError = func() error {
			if changefeedIsRetrying.Load() {
				return errors.New("test retryable error")
			}
			return nil
		}

		// NB: We use the errCh to return errors in testing knobs because they run in separate goroutines.
		// Avoid using `require` because it can panic and the goroutines may `recover()` the panic.
		doneCh := make(chan struct{}, 1)
		errCh := make(chan error, 1)
		sendErrWithCtx := func(ctx context.Context, err error) {
			t.Errorf("sending error: %s", err)
			select {
			case <-ctx.Done():
				return
			case errCh <- err:
				return
			}
		}

		knobs.StartDistChangefeedInitialHighwater = func(ctx context.Context, retryHighwater hlc.Timestamp) {
			if changefeedIsRetrying.Load() {
				progress, err := loadProgressErr(jobFeed.JobID(), jobRegistry)
				if err != nil {
					sendErrWithCtx(ctx, err)
					return
				}
				progressHighwater := progress.GetHighWater()
				// Sanity check that the highwater is not nil, meaning that a
				// highwater timestamp was written to the job record.
				if progressHighwater == nil {
					sendErrWithCtx(ctx, errors.AssertionFailedf("job highwater is nil"))
					return
				}
				// Assert that the retry highwater is equal to the one in the job
				// record.
				if !progressHighwater.Equal(retryHighwater) {
					sendErrWithCtx(ctx, errors.AssertionFailedf("highwater %s does not match job highwater %s",
						retryHighwater, progressHighwater))
					return
				}
				// Terminate the test.
				t.Log("signalling for test completion")
				select {
				case <-ctx.Done():
					return
				case doneCh <- struct{}{}:
					return
				}
			}
		}

		loadJobErrCount := 2
		knobs.LoadJobErr = func() error {
			if loadJobErrCount > 0 {
				loadJobErrCount -= 1
				return errors.New("test error")
			}
			return nil
		}

		require.NoError(t, jobFeed.Resume())

		// Step 1: Wait for the highwater to advance. This guarantees that there is some highwater
		//         in the changefeed job record to use when retrying.
		testutils.SucceedsSoon(t, func() error {
			progress, err := loadProgressErr(jobFeed.JobID(), jobRegistry)
			if err != nil {
				return err
			}
			progressHighwater := progress.GetHighWater()
			if progressHighwater != nil && initialHighwater.Less(*progressHighwater) {
				changefeedIsRetrying.Store(true)
				return nil
			}
			return errors.Newf("waiting for highwater %s to advance ahead of initial highwater %s",
				progressHighwater, initialHighwater)
		})

		// Check that the following happens soon.
		//
		// Step 2: Since `changefeedIsRetrying` is true, the changefeed will now attempt retries in
		//         via `knobs.RaiseRetryableError`.
		// Step 3: `knobs.LoadJobErr` will result an in error when reading the job record a couple of times, causing
		//          more retries.
		// Step 4: Eventually, a dist changefeed is started at a certain highwater timestamp.
		//         `knobs.StartDistChangefeedInitialHighwater`. should see this stimetsamp and assert that it's the one
		//         from the job record.
		select {
		case <-time.After(30 * time.Second):
			t.Fatal("test timed out")
		case err := <-errCh:
			t.Fatal(err)
		case <-doneCh:
		}
	}
	cdcTest(t, testFn, feedTestEnterpriseSinks, withAllowChangefeedErr("injects error"))
}

// TestChangefeedPubsubResolvedMessages tests that the pubsub sink emits
// resolved messages to each topic.
func TestChangefeedPubsubResolvedMessages(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {

		db := sqlutils.MakeSQLRunner(s.DB)
		db.Exec(t, "CREATE TABLE one (i int)")
		db.Exec(t, "CREATE TABLE two (i int)")
		db.Exec(t, "CREATE TABLE three (i int)")

		foo, err := f.Feed("CREATE CHANGEFEED FOR TABLE one, TABLE two, TABLE three with resolved = '10ms'")
		require.NoError(t, err)

		seenTopics := make(map[string]struct{})
		expectedTopics := map[string]struct{}{
			"projects/testfeed/topics/one":   {},
			"projects/testfeed/topics/two":   {},
			"projects/testfeed/topics/three": {},
		}

		// There may be retries, so we could get the same resolved message for a topic more than once.
		testutils.SucceedsSoon(t, func() error {
			for i := 0; i < 3; i++ {
				// We should only see resolved messages since there is no data in the table.
				msg, err := foo.Next()
				require.NoError(t, err)
				seenTopics[msg.Topic] = struct{}{}
			}
			if !reflect.DeepEqual(seenTopics, expectedTopics) {
				return errors.Newf("failed to see expected resolved messages on each topic. seen: %v, expected: %v",
					seenTopics, expectedTopics)
			}
			return nil
		})

		require.NoError(t, foo.Close())
	}

	cdcTest(t, testFn, feedTestForceSink("pubsub"))
}

// TestCloudstorageBufferedBytesMetric tests the metric which tracks the number
// of buffered bytes in the cloudstorage sink.
func TestCloudstorageBufferedBytesMetric(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	rng, _ := randutil.NewTestRand()

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		registry := s.Server.JobRegistry().(*jobs.Registry)
		metrics := registry.MetricsStruct().Changefeed.(*Metrics)
		defaultSLI, err := metrics.getSLIMetrics(defaultSLIScope)
		require.NoError(t, err)

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		var shouldEmit atomic.Bool
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			return !shouldEmit.Load(), nil
		}
		db := sqlutils.MakeSQLRunner(s.DB)
		db.Exec(t, `
		  CREATE TABLE foo (key INT PRIMARY KEY);
		  INSERT INTO foo (key) SELECT * FROM generate_series(1, 1000);
  		`)

		require.Equal(t, int64(0), defaultSLI.CloudstorageBufferedBytes.Value())

		format := "json"
		if rng.Float32() < 0.5 {
			format = "parquet"
		}
		foo, err := f.Feed(fmt.Sprintf("CREATE CHANGEFEED FOR TABLE foo WITH format='%s'", format))
		require.NoError(t, err)

		// Because checkpoints are disabled, we should have some bytes build up
		// in the sink.
		targetBytes := int64(40000)
		if format == "parquet" {
			// Parquet is a much more efficient format, so the buffered files will
			// be much smaller.
			targetBytes = 2000
		}
		testutils.SucceedsSoon(t, func() error {
			numBytes := defaultSLI.CloudstorageBufferedBytes.Value()
			if defaultSLI.CloudstorageBufferedBytes.Value() < targetBytes {
				return errors.Newf("expected at least %d buffered bytes but found %d", targetBytes, numBytes)
			}
			return nil
		})

		// Allow checkpoints to pass through and flush the sink. We should see
		// zero bytes buffered after that.
		shouldEmit.Store(true)
		testutils.SucceedsSoon(t, func() error {
			numBytes := defaultSLI.CloudstorageBufferedBytes.Value()
			if defaultSLI.CloudstorageBufferedBytes.Value() != 0 {
				return errors.Newf("expected at least %d buffered bytes but found %d", 0, numBytes)
			}
			return nil
		})

		require.NoError(t, foo.Close())
	}

	cdcTest(t, testFn, feedTestForceSink("cloudstorage"))
}

// TestBatchSizeMetric the emitted batch size histogram metric.
func TestBatchSizeMetric(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		registry := s.Server.JobRegistry().(*jobs.Registry)
		batchSizeHist := registry.MetricsStruct().Changefeed.(*Metrics).AggMetrics.EmittedBatchSizes

		db := sqlutils.MakeSQLRunner(s.DB)
		db.Exec(t, `
		  CREATE TABLE foo (key INT PRIMARY KEY);
		  INSERT INTO foo (key) VALUES (1), (2), (3);
		`)

		numSamples, sum := batchSizeHist.WindowedSnapshot().Total()
		require.Equal(t, int64(0), numSamples)
		require.Equal(t, 0.0, sum)

		foo, err := f.Feed("CREATE CHANGEFEED FOR TABLE foo")
		require.NoError(t, err)

		testutils.SucceedsSoon(t, func() error {
			numSamples, sum = batchSizeHist.WindowedSnapshot().Total()
			if numSamples <= 0 && sum <= 0.0 {
				return errors.Newf("waiting for metric %d %d", numSamples, sum)
			}
			return nil
		})
		require.NoError(t, foo.Close())
	}
	cdcTest(t, testFn)
}

// TestParallelIOMetrics tests parallel io metrics.
func TestParallelIOMetrics(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// This test relies on messing with timings to see pending rows build up,
	//  so skip it when the system is loaded.
	skip.UnderDuress(t)

	// Add delay so queuing occurs, which results in the below metrics being
	// nonzero.
	defer testingEnableQueuingDelay()()

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		registry := s.Server.JobRegistry().(*jobs.Registry)
		metrics := registry.MetricsStruct().Changefeed.(*Metrics).AggMetrics

		db := sqlutils.MakeSQLRunner(s.DB)
		db.Exec(t, `SET CLUSTER SETTING changefeed.sink_io_workers = 1`)
		db.Exec(t, `
		  CREATE TABLE foo (a INT PRIMARY KEY);
		`)

		// Keep writing data to the same key to ensure contention.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		g := ctxgroup.WithContext(ctx)
		done := make(chan struct{})
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-done:
					return nil
				default:
					_, err := s.DB.Exec(`UPSERT INTO foo (a)  SELECT * FROM generate_series(1, 10)`)
					if err != nil {
						return err
					}
				}
			}
		})
		// Set the frequency to 1s. The default frequency at the time of writing is
		foo, err := f.Feed("CREATE CHANGEFEED FOR TABLE foo WITH pubsub_sink_config=" +
			"'{\"Flush\": {\"Frequency\": \"100ms\"}}'")
		require.NoError(t, err)

		testutils.SucceedsSoon(t, func() error {
			numSamples, sum := metrics.ParallelIOPendingQueueNanos.WindowedSnapshot().Total()
			if numSamples <= 0 && sum <= 0.0 {
				return errors.Newf("waiting for queue nanos: %d %f", numSamples, sum)
			}
			return nil
		})
		testutils.SucceedsSoon(t, func() error {
			pendingKeys := metrics.ParallelIOPendingRows.Value()
			if pendingKeys <= 0 {
				return errors.Newf("waiting for pending keys: %d", pendingKeys)
			}
			return nil
		})
		testutils.SucceedsSoon(t, func() error {
			for i := 0; i < 50; i++ {
				inFlightKeys := metrics.ParallelIOInFlightKeys.Value()
				if inFlightKeys > 0 {
					return nil
				}
			}
			return errors.New("waiting for in-flight keys")
		})
		testutils.SucceedsSoon(t, func() error {
			numSamples, sum := metrics.ParallelIOResultQueueNanos.WindowedSnapshot().Total()
			if numSamples <= 0 && sum <= 0.0 {
				return errors.Newf("waiting for result queue nanos: %d %f", numSamples, sum)
			}
			return nil
		})
		close(done)
		require.NoError(t, g.Wait())
		require.NoError(t, foo.Close())
	}
	cdcTest(t, testFn, feedTestForceSink("pubsub"))
}

// TestSinkBackpressureMetric tests that the sink backpressure metric is recorded
// when quota limits are hit.
func TestSinkBackpressureMetric(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		registry := s.Server.JobRegistry().(*jobs.Registry)
		metrics := registry.MetricsStruct().Changefeed.(*Metrics).AggMetrics

		db := sqlutils.MakeSQLRunner(s.DB)
		db.Exec(t, `SET CLUSTER SETTING changefeed.sink_io_workers = 1`)
		db.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		g := ctxgroup.WithContext(ctx)
		done := make(chan struct{})
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-done:
					return nil
				default:
					_, err := s.DB.Exec(`UPSERT INTO foo (a)  SELECT * FROM generate_series(1, 10)`)
					if err != nil {
						return err
					}
				}
			}
		})

		foo, err := f.Feed(`CREATE CHANGEFEED FOR TABLE foo WITH pubsub_sink_config='{"Flush": {"Frequency": "100ms"}}'`)
		require.NoError(t, err)

		testutils.SucceedsSoon(t, func() error {
			numSamples, sum := metrics.SinkBackpressureNanos.WindowedSnapshot().Total()
			if numSamples <= 0 && sum <= 0.0 {
				return errors.Newf("waiting for backpressure nanos: %d %f", numSamples, sum)
			}
			return nil
		})

		close(done)
		require.NoError(t, g.Wait())
		require.NoError(t, foo.Close())
	}
	cdcTest(t, testFn, feedTestForceSink("pubsub"))
}

type changefeedLogSpy struct {
	syncutil.Mutex
	logs []string
}

// Intercept implements log.Interceptor.
func (s *changefeedLogSpy) Intercept(entry []byte) {
	s.Lock()
	defer s.Unlock()
	var j map[string]any
	if err := gojson.Unmarshal(entry, &j); err != nil {
		panic(err)
	}
	if !strings.Contains(j["file"].(string), "ccl/changefeedccl/") {
		return
	}

	s.logs = append(s.logs, j["message"].(string))
}

var _ log.Interceptor = (*changefeedLogSpy)(nil)

func TestChangefeedHeadersJSONVals(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("event_processing=3"))

	// Make it easier to test the logs.
	jsonHeaderWrongValTypeLogLim = log.Every(0)
	jsonHeaderWrongTypeLogLim = log.Every(0)

	cases := []struct {
		name           string
		headersJSONStr string
		expected       cdctest.Headers
		warn           string
	}{
		{
			name:           "empty",
			headersJSONStr: `'{}'`,
			expected:       cdctest.Headers{},
		},
		{
			name:           "flat primitives - happy path",
			headersJSONStr: `'{"a": "b", "c": "d", "e": 42, "f": false}'`,
			expected: cdctest.Headers{
				{K: "a", V: []byte("b")},
				{K: "c", V: []byte("d")},
				{K: "e", V: []byte("42")},
				{K: "f", V: []byte("false")},
			},
		},
		{
			name:           "some bad some good",
			headersJSONStr: `'{"a": "b", "c": 1, "d": true, "e": null, "f": [1, 2, 3], "g": {"h": "i"}}'`,
			expected: cdctest.Headers{
				{K: "a", V: []byte("b")},
				{K: "c", V: []byte("1")},
				{K: "d", V: []byte("true")},
				// e will be skipped since its value is null. f and g will be skipped since they're non-primitive types.
			},
			warn: "must be a JSON object with primitive values",
		},
		{
			name:           "not an object",
			headersJSONStr: `'[1,2,3]'`,
			warn:           "must be a JSON object",
		},
		// Both types of nulls are ok.
		{
			name:           "sql null",
			headersJSONStr: `null`,
		},
		{
			name:           "json null",
			headersJSONStr: `'null'`,
		},
	}

	for _, format := range []string{"json", "avro"} {
		t.Run(format, func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
						spy := &changefeedLogSpy{}
						cleanup := log.InterceptWith(context.Background(), spy)
						defer cleanup()

						sqlDB := sqlutils.MakeSQLRunner(s.DB)
						sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, headerz JSONB)`)
						// Using fmt.Sprintf because it's tricky to specify sql null vs json null with params.
						sqlDB.Exec(t, fmt.Sprintf(`INSERT INTO foo VALUES (1, %s::jsonb)`, c.headersJSONStr))

						foo := feed(t, f, fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH headers_json_column_name=headerz, format=%s`, format))
						defer closeFeed(t, foo)

						headersStr := c.expected.String()
						if c.warn != "" {
							defer func() {
								spy.Lock()
								defer spy.Unlock()

								for _, log := range spy.logs {
									if strings.Contains(log, c.warn) {
										return
									}
								}
								t.Errorf("expected warning %q not found in logs: %v", c.warn, spy.logs)
							}()
						}
						key := "[1]"
						val := `{"after": {"a": 1}}`
						if format == "avro" {
							key = `{"a":{"long":1}}`
							val = `{"after":{"foo":{"a":{"long":1}}}}`
						}
						assertPayloads(t, foo, []string{fmt.Sprintf(`foo: %s%s->%s`, key, headersStr, val)})
					}
					cdcTest(t, testFn, feedTestForceSink("kafka"))
				})
			}
		})
	}
}

// TestPubsubAttributes tests that the "attributes" field in the
// `pubsub_sink_config` behaves as expected.
func TestPubsubAttributes(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		db := sqlutils.MakeSQLRunner(s.DB)

		// asserts the next message has these attributes and is sent to each of the supplied topics.
		expectAttributes := func(feed cdctest.TestFeed, attributes map[string]string, allowedTopics ...string) {
			// Keep popping messages until we see all the expected topics.
			seenTopics := make(map[string]struct{})
			for len(seenTopics) < len(allowedTopics) {
				msg, err := feed.(*pubsubFeed).Next()
				require.NoError(t, err)

				raw := msg.RawMessage.(*mockPubsubMessage)

				require.Contains(t, allowedTopics, msg.Topic)
				if attributes == nil {
					require.Nil(t, raw.attributes)
				} else {
					require.True(t, reflect.DeepEqual(attributes, raw.attributes),
						"%#v=%#v", attributes, raw.attributes)
				}
				seenTopics[msg.Topic] = struct{}{}
				t.Logf("message %s: %s -> %s, %v", msg.Key, msg.Value, msg.Topic, raw.attributes)
			}
		}

		t.Run("separate tables", func(t *testing.T) {
			db.Exec(t, "CREATE TABLE one (i int)")
			db.Exec(t, "CREATE TABLE two (i int)")

			foo, err := f.Feed(`CREATE CHANGEFEED FOR TABLE one, TABLE two ` +
				`INTO 'gcpubsub://testfeed?with_table_name_attribute=true' `)
			require.NoError(t, err)

			db.Exec(t, "INSERT INTO one VALUES (1)")
			expectAttributes(foo, map[string]string{"TABLE_NAME": "one"}, "one")

			db.Exec(t, "INSERT INTO two VALUES (1)")
			expectAttributes(foo, map[string]string{"TABLE_NAME": "two"}, "two")

			require.NoError(t, foo.Close())
		})

		t.Run("same table different families", func(t *testing.T) {
			db.Exec(t, "CREATE TABLE withFams (i int, j int, k int, FAMILY ifam(i), FAMILY jfam(j))")
			db.Exec(t, "CREATE TABLE withoutFams (i int)")

			foo, err := f.Feed(`CREATE CHANGEFEED FOR TABLE withFams FAMILY ifam, TABLE withFams FAMILY jfam, ` +
				`TABLE withoutFams INTO 'gcpubsub://testfeed?with_table_name_attribute=true'`)
			require.NoError(t, err)

			// We get two messages because the changefeed is targeting two familes.
			// Each message should reference the same table.
			db.Exec(t, "INSERT INTO withFams VALUES (1, 2, 3)")
			expectAttributes(foo, map[string]string{"TABLE_NAME": "withfams"}, "withfams.jfam", "withfams.ifam")

			db.Exec(t, "INSERT INTO withoutFams VALUES (1)")
			expectAttributes(foo, map[string]string{"TABLE_NAME": "withoutfams"}, "withoutfams")

			require.NoError(t, foo.Close())
		})

		t.Run("different tables with one topic", func(t *testing.T) {
			db.Exec(t, "CREATE TABLE a (i int)")
			db.Exec(t, "CREATE TABLE b (i int)")
			db.Exec(t, "CREATE TABLE c (i int)")
			foo, err := f.Feed(`CREATE CHANGEFEED FOR TABLE a, TABLE b, TABLE c ` +
				`INTO 'gcpubsub://testfeed?topic_name=mytopicname&with_table_name_attribute=true'`)
			require.NoError(t, err)

			// Ensure each message goes in a different batch with its own
			// attributes. Ie. ensure batching is not per-topic only, but also
			// per-table when we enable the table name attribute.
			db.Exec(t, "INSERT INTO a VALUES (1)")
			expectAttributes(foo, map[string]string{"TABLE_NAME": "a"}, "mytopicname")
			db.Exec(t, "INSERT INTO b VALUES (1)")
			expectAttributes(foo, map[string]string{"TABLE_NAME": "b"}, "mytopicname")
			db.Exec(t, "INSERT INTO c VALUES (1)")
			expectAttributes(foo, map[string]string{"TABLE_NAME": "c"}, "mytopicname")

			require.NoError(t, foo.Close())
		})

		t.Run("no attributes", func(t *testing.T) {
			db.Exec(t, "CREATE TABLE non (i int)")
			foo, err := f.Feed(`CREATE CHANGEFEED FOR TABLE non`)
			require.NoError(t, err)

			db.Exec(t, "INSERT INTO non VALUES (1)")
			expectAttributes(foo, nil, "non")

			require.NoError(t, foo.Close())
		})
	}

	cdcTest(t, testFn, feedTestForceSink("pubsub"))
}

// TestChangefeedAvroDecimalColumnWithDiff is a regression test for
// https://github.com/cockroachdb/cockroach/issues/118647.
func TestChangefeedAvroDecimalColumnWithDiff(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE test1 (c1 INT PRIMARY KEY, c2 INT, c3 DECIMAL(19, 0))`)
		sqlDB.Exec(t, `INSERT INTO test1 VALUES (1, 2, 3);`)

		schemaReg := cdctest.StartTestSchemaRegistry()
		defer schemaReg.Close()
		str := fmt.Sprintf(`CREATE CHANGEFEED FOR TABLE test1 WITH OPTIONS (avro_schema_prefix = 'crdb_cdc_', diff, confluent_schema_registry ="%s", format = 'avro', on_error = 'pause', updated);`, schemaReg.URL())
		testFeed := feed(t, f, str)
		defer closeFeed(t, testFeed)

		_, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)
	}

	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestChangefeedProtectedTimestampUpdate(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	verifyFunc := func() {}
	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		defer verifyFunc()
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		// Checkpoint and trigger potential protected timestamp updates frequently.
		// Make the protected timestamp lag long enough that it shouldn't be
		// immediately updated after a restart.
		changefeedbase.SpanCheckpointInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 10*time.Millisecond)
		changefeedbase.ProtectTimestampInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 10*time.Millisecond)
		changefeedbase.ProtectTimestampLag.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 10*time.Hour)

		sqlDB.Exec(t, `CREATE TABLE foo (id INT)`)

		registry := s.Server.JobRegistry().(*jobs.Registry)
		metrics := registry.MetricsStruct().Changefeed.(*Metrics)
		createPtsCount, _ := metrics.AggMetrics.Timers.PTSCreate.WindowedSnapshot().Total()
		managePtsCount, _ := metrics.AggMetrics.Timers.PTSManage.WindowedSnapshot().Total()
		managePTSErrorCount, _ := metrics.AggMetrics.Timers.PTSManageError.WindowedSnapshot().Total()
		require.Equal(t, int64(0), createPtsCount)
		require.Equal(t, int64(0), managePtsCount)
		require.Equal(t, int64(0), managePTSErrorCount)

		createStmt := `CREATE CHANGEFEED FOR foo WITH resolved='10ms', no_initial_scan`
		testFeed := feed(t, f, createStmt)
		defer closeFeed(t, testFeed)

		createPtsCount, _ = metrics.AggMetrics.Timers.PTSCreate.WindowedSnapshot().Total()
		managePtsCount, _ = metrics.AggMetrics.Timers.PTSManage.WindowedSnapshot().Total()
		require.Equal(t, int64(1), createPtsCount)
		require.Equal(t, int64(0), managePtsCount)

		eFeed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		// Wait for the changefeed to checkpoint and update PTS at least once.
		var lastHWM hlc.Timestamp
		checkHWM := func() error {
			hwm, err := eFeed.HighWaterMark()
			if err == nil && !hwm.IsEmpty() && lastHWM.Less(hwm) {
				lastHWM = hwm
				return nil
			}
			return errors.New("waiting for high watermark to advance")
		}
		testutils.SucceedsSoon(t, checkHWM)

		// Get the PTS of this feed.
		p, err := eFeed.Progress()
		require.NoError(t, err)

		ptsQry := fmt.Sprintf(`SELECT ts FROM system.protected_ts_records WHERE id = '%s'`, p.ProtectedTimestampRecord)
		var ts, ts2 string
		sqlDB.QueryRow(t, ptsQry).Scan(&ts)
		require.NoError(t, err)

		// Force the changefeed to restart.
		require.NoError(t, eFeed.Pause())
		require.NoError(t, eFeed.Resume())

		// Wait for a new checkpoint.
		testutils.SucceedsSoon(t, checkHWM)

		// Check that the PTS was not updated after the resume.
		sqlDB.QueryRow(t, ptsQry).Scan(&ts2)
		require.NoError(t, err)
		require.Equal(t, ts, ts2)

		// Lower the PTS lag and check that it has been updated.
		changefeedbase.ProtectTimestampLag.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 10*time.Millisecond)

		// Ensure that the resolved timestamp advances at least once
		// since the PTS lag override.
		testutils.SucceedsSoon(t, checkHWM)
		testutils.SucceedsSoon(t, checkHWM)

		sqlDB.QueryRow(t, ptsQry).Scan(&ts2)
		require.NoError(t, err)
		require.Less(t, ts, ts2)

		managePtsCount, _ = metrics.AggMetrics.Timers.PTSManage.WindowedSnapshot().Total()
		managePTSErrorCount, _ = metrics.AggMetrics.Timers.PTSManageError.WindowedSnapshot().Total()
		require.GreaterOrEqual(t, managePtsCount, int64(2))
		require.Equal(t, int64(0), managePTSErrorCount)
	}

	withTxnRetries := withArgsFn(func(args *base.TestServerArgs) {
		requestFilter, vf := testutils.TestingRequestFilterRetryTxnWithPrefix(t, changefeedJobProgressTxnName, 1)
		args.Knobs.Store = &kvserver.StoreTestingKnobs{
			TestingRequestFilter: requestFilter,
		}
		verifyFunc = vf
	})

	cdcTest(t, testFn, feedTestForceSink("kafka"), withTxnRetries)
}

func TestChangefeedProtectedTimestampUpdateError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		// Checkpoint and trigger potential protected timestamp updates frequently.
		// Make the protected timestamp lag long enough that it shouldn't be
		// immediately updated after a restart.
		changefeedbase.SpanCheckpointInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 10*time.Millisecond)
		changefeedbase.ProtectTimestampInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 10*time.Millisecond)
		changefeedbase.ProtectTimestampLag.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 10*time.Hour)

		sqlDB.Exec(t, `CREATE TABLE foo (id INT)`)

		registry := s.Server.JobRegistry().(*jobs.Registry)
		metrics := registry.MetricsStruct().Changefeed.(*Metrics)
		createPtsCount, _ := metrics.AggMetrics.Timers.PTSCreate.WindowedSnapshot().Total()
		managePtsCount, _ := metrics.AggMetrics.Timers.PTSManage.WindowedSnapshot().Total()
		managePTSErrorCount, _ := metrics.AggMetrics.Timers.PTSManageError.WindowedSnapshot().Total()
		require.Equal(t, int64(0), createPtsCount)
		require.Equal(t, int64(0), managePtsCount)
		require.Equal(t, int64(0), managePTSErrorCount)

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		knobs.ManagePTSError = func() error {
			return errors.New("test error")
		}

		createStmt := `CREATE CHANGEFEED FOR foo WITH resolved='10ms', no_initial_scan`
		testFeed := feed(t, f, createStmt)
		defer closeFeed(t, testFeed)

		createPtsCount, _ = metrics.AggMetrics.Timers.PTSCreate.WindowedSnapshot().Total()
		require.Equal(t, int64(1), createPtsCount)
		managePTSErrorCount, _ = metrics.AggMetrics.Timers.PTSManageError.WindowedSnapshot().Total()
		require.Equal(t, int64(0), managePTSErrorCount)

		// Lower the PTS lag to trigger a PTS update.
		changefeedbase.ProtectTimestampLag.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 10*time.Millisecond)

		testutils.SucceedsSoon(t, func() error {
			managePTSErrorCount, _ = metrics.AggMetrics.Timers.PTSManageError.WindowedSnapshot().Total()
			if managePTSErrorCount > 0 {
				fmt.Println("manage protected timestamps test: manage pts error count", managePTSErrorCount)
				return nil
			}
			return errors.New("waiting for manage pts error")
		})
	}
	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestCDCQuerySelectSingleRow(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	errCh := make(chan error, 1)
	knobsFn := func(knobs *base.TestingKnobs) {
		if knobs.DistSQL == nil {
			knobs.DistSQL = &execinfra.TestingKnobs{}
		}
		if knobs.DistSQL.(*execinfra.TestingKnobs).Changefeed == nil {
			knobs.DistSQL.(*execinfra.TestingKnobs).Changefeed = &TestingKnobs{}
		}
		cfKnobs := knobs.DistSQL.(*execinfra.TestingKnobs).Changefeed.(*TestingKnobs)
		cfKnobs.HandleDistChangefeedError = func(err error) error {
			// Only capture the first error -- that's enough for the test.
			select {
			case errCh <- err:
			default:
			}
			return err
		}
	}

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		db := sqlutils.MakeSQLRunner(s.DB)
		db.Exec(t, `CREATE TABLE foo (key INT PRIMARY KEY);`)
		db.Exec(t, `INSERT INTO foo VALUES (1), (2), (3);`)

		// initial_scan='only' is not required, but it makes testing this easier.
		foo := feed(t, f, `CREATE CHANGEFEED WITH initial_scan='only' AS SELECT * FROM foo WHERE key = 1`)
		defer closeFeed(t, foo)

		done := make(chan struct{})
		go func() {
			defer close(done)
			assertPayloads(t, foo, []string{`foo: [1]->{"key": 1}`})
		}()

		select {
		case err := <-errCh:
			// Ignore any error after the above assertion completed, because
			// it's likely just due to feed shutdown.
			select {
			case <-done:
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("timed out")
		case <-done:
			return
		}
	}
	cdcTest(t, testFn, withKnobsFn(knobsFn))
}

func assertReasonableMVCCTimestamp(t *testing.T, ts string) {
	epochNanos := parseTimeToHLC(t, ts).WallTime
	now := timeutil.Now()
	require.GreaterOrEqual(t, epochNanos, now.Add(-1*time.Hour).UnixNano())
}

func assertEqualTSNSHLCWalltime(t *testing.T, tsns int64, tshlc string) {
	tsHLCWallTimeNano := parseTimeToHLC(t, tshlc).WallTime
	require.EqualValues(t, tsns, tsHLCWallTimeNano)
}

// TestChangefeedAsSelectForEmptyTable verifies that a changefeed
// yields a proper user error on an empty table and in the same
// allows hidden columns to be selected.
func TestChangefeedAsSelectForEmptyTable(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE empty()`)
		sqlDB.Exec(t, `INSERT INTO empty DEFAULT VALUES`)
		// Should fail when no columns are selected.
		// Use expectErrCreatingFeed which handles sinkless feeds correctly by
		// attempting to read from the feed if no error occurs at creation time
		expectErrCreatingFeed(t, f, `CREATE CHANGEFEED AS SELECT * FROM empty`, `SELECT yields no columns`)

		// Should succeed when a rowid column is explicitly selected.
		feed, err := f.Feed(`CREATE CHANGEFEED AS SELECT rowid FROM empty`)
		require.NoError(t, err)
		defer closeFeed(t, feed)
	}

	cdcTest(t, testFn)
}

func TestChangefeedMVCCTimestampWithQueries(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (key INT PRIMARY KEY);`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1);`)

		feed, err := f.Feed(`CREATE CHANGEFEED WITH mvcc_timestamp, format=json, envelope=bare AS SELECT * FROM foo`)
		require.NoError(t, err)
		defer closeFeed(t, feed)

		msgs, err := readNextMessages(ctx, feed, 1)
		require.NoError(t, err)

		var m map[string]any
		require.NoError(t, gojson.Unmarshal(msgs[0].Value, &m))
		ts := m["__crdb__"].(map[string]any)["mvcc_timestamp"].(string)
		assertReasonableMVCCTimestamp(t, ts)
	}

	cdcTest(t, testFn)
}

func TestCloudstorageParallelCompression(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// This test only provides value under race, as it's explicitly testing for
	// data races between feeds.
	skip.UnlessUnderRace(t)

	const numFeedsEach = 10

	testutils.RunValues(t, "compression", []string{"zstd", "gzip"}, func(t *testing.T, compression string) {
		opts := makeOptions(t)
		opts.externalIODir = t.TempDir()
		s, cleanup := makeServerWithOptions(t, opts)
		defer cleanup()

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT);`)
		sqlDB.Exec(t, `INSERT INTO foo (a) SELECT * FROM generate_series(1, 5000);`)

		t.Logf("inserted into table")

		var jobIDs []int
		for i := range numFeedsEach {
			var jobID int
			sqlDB.QueryRow(t, fmt.Sprintf(`CREATE CHANGEFEED FOR foo INTO 'nodelocal://1/%d-testout' WITH compression='%s', initial_scan='only', format='parquet';`, i, compression)).Scan(&jobID)
			jobIDs = append(jobIDs, jobID)
		}

		t.Logf("created changefeeds")

		const duration = 3 * time.Minute
		const checkStatusInterval = 10 * time.Second

		for start := timeutil.Now(); timeutil.Since(start) < duration; {
			// Check the statuses of the jobs.
			for _, jobID := range jobIDs {
				var status string
				sqlDB.QueryRow(t, `SELECT status FROM [SHOW JOBS] WHERE job_id = $1`, jobID).Scan(&status)
				if status != "succeeded" && status != "running" {
					t.Fatalf("job %d entered unknown state: %s", jobID, status)
				}
			}
			time.Sleep(checkStatusInterval)
		}
	})
}

func TestChangefeedExtraHeaders(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		// Headers are not supported in the v1 kafka sink.
		sqlDB.Exec(t, `SET CLUSTER SETTING changefeed.new_kafka_sink.enabled = true`)

		sqlDB.Exec(t, `CREATE TABLE foo (key INT PRIMARY KEY);`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1);`)

		cases := []struct {
			name        string
			headersArg  string
			wantHeaders cdctest.Headers
			expectErr   bool
		}{
			{
				name:        "single header",
				headersArg:  `{"X-Someheader": "somevalue"}`,
				wantHeaders: cdctest.Headers{{K: "X-Someheader", V: []byte("somevalue")}},
			},
			{
				name:       "multiple headers",
				headersArg: `{"X-Someheader": "somevalue", "X-Someotherheader": "someothervalue"}`,
				wantHeaders: cdctest.Headers{
					{K: "X-Someheader", V: []byte("somevalue")},
					{K: "X-Someotherheader", V: []byte("someothervalue")},
				},
			},
			{
				name:       "inappropriate json",
				headersArg: `4`,
				expectErr:  true,
			},
			{
				name:       "also inappropriate json",
				headersArg: `["X-Someheader", "somevalue"]`,
				expectErr:  true,
			},
			{
				name:       "invalid json",
				headersArg: `xxxx`,
				expectErr:  true,
			},
		}

		for _, c := range cases {
			feed, err := f.Feed(fmt.Sprintf(`CREATE CHANGEFEED FOR foo WITH extra_headers='%s'`, c.headersArg))
			if c.expectErr {
				require.Error(t, err)
				continue
			} else {
				require.NoError(t, err)
			}

			assertPayloads(t, feed, []string{
				fmt.Sprintf(`foo: [1]%s->{"after": {"key": 1}}`, c.wantHeaders.String()),
			})
			closeFeed(t, feed)
		}
	}

	cdcTest(t, testFn, feedTestRestrictSinks("kafka", "webhook"))
}
func TestChangefeedAdditionalHeadersDoesntWorkWithV1KafkaSink(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `SET CLUSTER SETTING changefeed.new_kafka_sink.enabled = false`)

		sqlDB.Exec(t, `CREATE TABLE foo (key INT PRIMARY KEY);`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1);`)

		_, err := f.Feed(`CREATE CHANGEFEED FOR foo WITH extra_headers='{"X-Someheader": "somevalue"}'`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "headers are not supported for the v1 kafka sink")
	}

	cdcTest(t, testFn, feedTestForceSink("kafka"))
}

func TestDatabaseLevelChangefeed(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE DATABASE foo`)
		sqlDB.Exec(t, `CREATE TABLE foo.bar(id int primary key, s string)`)
		sqlDB.Exec(t, `INSERT INTO foo.bar(id, s) VALUES (0, 'hello'), (1, null)`)
		expectErrCreatingFeed(t, f, `CREATE CHANGEFEED for DATABASE foo`, "database-level changefeed is not implemented")
	}
	cdcTest(t, testFn)
}

func TestChangefeedProtobuf(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	type testCase struct {
		envelope     string
		withDiff     bool
		expectedRows []string
	}

	tests := []testCase{
		{
			envelope: "bare",
			withDiff: false,
			expectedRows: []string{
				`pricing: {"id":1}->{"values":{"discount":15.75,"id":1,"name":"Chair","options":["Brown","Black"],"tax":"2.500"},"__crdb__":{"key":{"id":1},"topic":"pricing"}}`,
				`pricing: {"id":2}->{"values":{"discount":20,"id":2,"name":"Table","options":["Brown","Black"],"tax":"1.23456789"},"__crdb__":{"key":{"id":2},"topic":"pricing"}}`,
				`pricing: {"id":2}->{"values":{"discount":25.5,"id":2,"name":"Table","options":["Brown","Black"],"tax":"1.23456789"},"__crdb__":{"key":{"id":2},"topic":"pricing"}}`,
				`pricing: {"id":1}->{"values":{"discount":10,"id":1,"name":"Armchair","options":["Red"],"tax":"1.000"},"__crdb__":{"key":{"id":1},"topic":"pricing"}}`,
				`pricing: {"id":3}->{"values":{"discount":50,"id":3,"name":"Sofa","options":["Gray"],"tax":"4.250"},"__crdb__":{"key":{"id":3},"topic":"pricing"}}`,
				`pricing: {"id":2}->{"values":{"discount":null,"id":2,"name":null,"options":null,"tax":null},"__crdb__":{"key":{"id":2},"topic":"pricing"}}`,
			},
		},
		{
			envelope: "wrapped",
			withDiff: true,
			expectedRows: []string{
				`pricing: {"id":1}->{"after":{"values":{"discount":15.75,"id":1,"name":"Chair","options":["Brown","Black"],"tax":"2.500"}},"before":{},"key":{"id":1},"topic":"pricing"}`,
				`pricing: {"id":2}->{"after":{"values":{"discount":20,"id":2,"name":"Table","options":["Brown","Black"],"tax":"1.23456789"}},"before":{},"key":{"id":2},"topic":"pricing"}`,
				`pricing: {"id":2}->{"after":{"values":{"discount":25.5,"id":2,"name":"Table","options":["Brown","Black"],"tax":"1.23456789"}},"before":{"values":{"discount":20,"id":2,"name":"Table","options":["Brown","Black"],"tax":"1.23456789"}},"key":{"id":2},"topic":"pricing"}`,
				`pricing: {"id":1}->{"after":{"values":{"discount":10,"id":1,"name":"Armchair","options":["Red"],"tax":"1.000"}},"before":{"values":{"discount":15.75,"id":1,"name":"Chair","options":["Brown","Black"],"tax":"2.500"}},"key":{"id":1},"topic":"pricing"}`,
				`pricing: {"id":3}->{"after":{"values":{"discount":50,"id":3,"name":"Sofa","options":["Gray"],"tax":"4.250"}},"before":{},"key":{"id":3},"topic":"pricing"}`,
				`pricing: {"id":2}->{"after":{},"before":{"values":{"discount":25.5,"id":2,"name":"Table","options":["Brown","Black"],"tax":"1.23456789"}},"key":{"id":2},"topic":"pricing"}`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("envelope=%s", tc.envelope), func(t *testing.T) {
			testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
				sqlDB := sqlutils.MakeSQLRunner(s.DB)

				sqlDB.Exec(t, `
					CREATE TABLE pricing (
						id INT PRIMARY KEY,
						name STRING,
						discount FLOAT,
						tax DECIMAL,
						options STRING[]
					)`)
				sqlDB.Exec(t, `
					INSERT INTO pricing VALUES
						(1, 'Chair', 15.75, 2.500, ARRAY['Brown', 'Black']), 
						(2, 'Table', 20.00, 1.23456789, ARRAY['Brown', 'Black'])`)

				var opts []string
				opts = append(opts, fmt.Sprintf("envelope='%s'", tc.envelope))
				opts = append(opts, "format='protobuf'", "key_in_value", "topic_in_value")
				if tc.withDiff {
					opts = append(opts, "diff")
				}

				feed := feed(t, f, fmt.Sprintf("CREATE CHANGEFEED FOR pricing WITH %s", strings.Join(opts, ", ")))
				defer closeFeed(t, feed)

				sqlDB.Exec(t, `UPDATE pricing SET discount = 25.50 WHERE id = 2`)
				sqlDB.Exec(t, `UPSERT INTO pricing (id, name, discount, tax, options) VALUES (1, 'Armchair', 10.00, 1.000, ARRAY['Red'])`)
				sqlDB.Exec(t, `INSERT INTO pricing VALUES (3, 'Sofa', 50.00, 4.250, ARRAY['Gray'])`)
				sqlDB.Exec(t, `DELETE FROM pricing WHERE id = 2`)

				assertPayloads(t, feed, tc.expectedRows)
			}
			cdcTest(t, testFn, feedTestForceSink("kafka"))
		})
	}
}
