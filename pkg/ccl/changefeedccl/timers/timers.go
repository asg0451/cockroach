package timers

import (
	"time"

	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/metric/aggmetric"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/prometheus/client_golang/prometheus"
)

// TODO: should we add a label for sink type? or other things maybe?

var TimersEnabled = settings.RegisterBoolSetting(
	settings.ApplicationLevel, "changefeed.timers.enabled",
	"enables debugging timers for changefeeds with the `changefeed.stage.latency` metric. you probable also want to set `server.child_metrics.enabled` also to debug a specific feed",
	false,
	settings.WithPublic)

type Scope string
type Stage string

type Timers struct {
	Latency    *aggmetric.AggHistogram
	settings   *settings.Values
	components struct {
		syncutil.RWMutex
		m map[Scope]map[Stage]*labeledTimer
	}
	ts timeutil.TimeSource
}

func (*Timers) MetricStruct() {}

func New(histogramWindow time.Duration, settings *settings.Values) *Timers {
	t := &Timers{
		Latency: aggmetric.NewHistogram(metric.HistogramOptions{
			Metadata: metric.Metadata{Name: "changefeed.stage.latency", Help: "Latency of changefeed stages", Unit: metric.Unit_NANOSECONDS, Measurement: "Latency"},
			Duration: histogramWindow,
			Buckets:  prometheus.ExponentialBucketsRange(float64(1*time.Microsecond), float64(1*time.Hour), 60),
			Mode:     metric.HistogramModePrometheus,
		}, "scope", "stage"),
		settings: settings,
		ts:       timeutil.DefaultTimeSource{},
	}
	t.components.m = make(map[Scope]map[Stage]*labeledTimer)
	return t
}

func (t *Timers) Scoped(scope Scope) *ScopedTimers {
	return &ScopedTimers{scope: scope, Timers: t}
}

type ScopedTimers struct {
	scope Scope
	*Timers
}

func (st *ScopedTimers) For(label Stage) *labeledTimer {
	if !TimersEnabled.Get(st.settings) {
		return nil
	}

	// Check for initialization under read lock.
	inited := func() bool {
		st.components.RLock()
		defer st.components.RUnlock()

		if _, ok := st.components.m[st.scope]; !ok {
			return false
		}
		if _, ok := st.components.m[st.scope][label]; !ok {
			return false
		}
		return true
	}()

	if !inited {
		st.initStage(label)
	}

	st.components.RLock()
	defer st.components.RUnlock()

	return st.components.m[st.scope][label]
}

func (st *ScopedTimers) initStage(stage Stage) {
	st.components.Lock()
	defer st.components.Unlock()

	if _, ok := st.components.m[st.scope]; !ok {
		st.components.m[st.scope] = make(map[Stage]*labeledTimer)
	}
	if _, ok := st.components.m[st.scope][stage]; !ok {
		st.components.m[st.scope][stage] = &labeledTimer{ts: st.ts, h: st.Latency.AddChild(string(st.scope), string(stage))}
	}
}

type labeledTimer struct {
	h  *aggmetric.Histogram
	ts timeutil.TimeSource
}

func (lt *labeledTimer) StartTimer() (stop func()) {
	if lt == nil {
		return nop
	}
	start := lt.ts.Now()
	return func() {
		lt.h.RecordValue(timeutil.Since(start).Nanoseconds())
	}
}

func (lt *labeledTimer) Time(cb func()) {
	if lt == nil {
		cb()
		return
	}
	start := lt.ts.Now()
	cb()
	lt.h.RecordValue(timeutil.Since(start).Nanoseconds())
}

var nop = func() {}
