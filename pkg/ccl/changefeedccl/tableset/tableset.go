// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package tableset implements a table set watcher. TODO comment
package tableset

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdcevent"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/rangefeed"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/systemschema"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/span"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
)

// will come from pb in reality
type Filter struct {
	DB            string
	Schema        string
	IncludeTables []string
	ExcludeTables []string
}

type Table struct {
	DB     string
	Schema string
	Name   string
	ID     descpb.ID
}

func (t Table) String() string {
	return fmt.Sprintf("%s.%s.%s", t.DB, t.Schema, t.Name)
}

type TableSet struct {
	Set  map[Table]struct{}
	AsOf hlc.Timestamp
}

type Watcher struct {
	filter Filter

	id      string
	mon     *mon.BytesMonitor
	execCfg *sql.ExecutorConfig
}

func NewWatcher(filter Filter, execCfg *sql.ExecutorConfig, mon *mon.BytesMonitor, id string) *Watcher {
	return &Watcher{filter: filter, execCfg: execCfg, mon: mon, id: id}
}

func (w *Watcher) Start(ctx context.Context, initialTS hlc.Timestamp) error {
	ctx = logtags.AddTag(ctx, "tableset.watcher.filter", w.filter)
	ctx, sp := tracing.ChildSpan(ctx, "changefeed.tableset.watcher.start")
	defer sp.Finish()

	acc := w.mon.MakeBoundAccount()

	errCh := make(chan error, 1)

	setErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case errCh <- err:
		default:
		}
	}

	var cfTargets changefeedbase.Targets
	cfTargets.Add(changefeedbase.Target{
		TableID:           systemschema.NamespaceTable.TableDescriptor.GetID(),
		StatementTimeName: changefeedbase.StatementTimeName(systemschema.NamespaceTable.TableDescriptor.TableDesc().Name),
		Type:              jobspb.ChangefeedTargetSpecification_COLUMN_FAMILY,
		FamilyName:        "fam_4_id",
	})
	dec, err := cdcevent.NewEventDecoder(ctx, w.execCfg, cfTargets, false, false)
	if err != nil {
		return err
	}

	// called from initial scans and maybe other places (catchups?)
	onValues := func(ctx context.Context, values []kv.KeyValue) {
		for _, kv := range values {
			if !kv.Value.IsPresent() {
				continue // ?
			}
			pbkv := roachpb.KeyValue{Key: kv.Key, Value: *kv.Value}
			row, err := dec.DecodeKV(ctx, pbkv, cdcevent.CurrentRow, kv.Value.Timestamp, false)
			if err != nil {
				setErr(err)
				return
			}
			fmt.Printf("row: %+v\n", row)
		}
	}
	// called with ordinary rangefeed values
	onValue := func(ctx context.Context, kv *kvpb.RangeFeedValue) {
		if !kv.Value.IsPresent() {
			return
		}
		pbkv, pbkvPrev := roachpb.KeyValue{Key: kv.Key, Value: kv.Value}, roachpb.KeyValue{Key: kv.Key, Value: kv.PrevValue}
		row, err := dec.DecodeKV(ctx, pbkv, cdcevent.CurrentRow, kv.Value.Timestamp, false)
		if err != nil {
			setErr(err)
			return
		}
		rowPrev, err := dec.DecodeKV(ctx, pbkvPrev, cdcevent.PrevRow, kv.Value.Timestamp, false)
		if err != nil {
			setErr(err)
			return
		}
		fmt.Printf("row: %+v\n", row)
		fmt.Printf("rowPrev: %+v\n", rowPrev)
	}

	// Common rangefeed options.
	opts := []rangefeed.Option{
		rangefeed.WithPProfLabel("job", fmt.Sprintf("id=%s", w.id)),
		rangefeed.WithMemoryMonitor(w.mon),
		rangefeed.WithOnCheckpoint(func(ctx context.Context, checkpoint *kvpb.RangeFeedCheckpoint) {}),
		rangefeed.WithOnInternalError(func(ctx context.Context, err error) { setErr(err) }),
		rangefeed.WithFrontierQuantized(1 * time.Second),
		rangefeed.WithOnValues(onValues),
		rangefeed.WithDiff(true),
		rangefeed.WithConsumerID(int64(42)),
		rangefeed.WithInvoker(func(fn func() error) error { return fn() }),
		rangefeed.WithFiltering(false),
	}

	// find family to watch
	var indexID descpb.IndexID
	for _, family := range systemschema.NamespaceTable.TableDescriptor.TableDesc().Families {
		if family.Name == "fam_4_id" {
			indexID = descpb.IndexID(family.ID)
			break
		}
	}
	watchSpans := roachpb.Spans{systemschema.NamespaceTable.TableDescriptor.IndexSpan(w.execCfg.Codec, indexID)}

	frontier, err := span.MakeFrontier(watchSpans...)
	if err != nil {
		return err
	}
	frontier = span.MakeConcurrentFrontier(frontier)
	defer frontier.Release()

	// TODO: is this our buffer size? do we need this?
	if err := acc.Grow(ctx, 1024); err != nil {
		return errors.Wrapf(err, "failed to allocated %d bytes from monitor", 1024)
	}

	// Start rangefeed.
	rf := w.execCfg.RangeFeedFactory.New(
		fmt.Sprintf("tableset.watcher.id=%s", w.id), initialTS, onValue, opts...,
	)
	defer rf.Close()

	fmt.Printf("starting rangefeed\n")

	if err := rf.StartFromFrontier(ctx, frontier); err != nil {
		return err
	}

	fmt.Printf("rangefeed started\n")

	// wait for shutdown due to error or context cancellation
	select {
	case err := <-errCh:
		fmt.Printf("shutting down due to error: %v\n", err)
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Watcher) OnChange(cb func(tableSet TableSet)) {}

func (w *Watcher) ValidAsOf(tableSet TableSet, ts hlc.Timestamp) bool {
	return false
}

func (w *Watcher) Close() {}
