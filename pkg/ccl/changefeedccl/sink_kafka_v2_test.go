package changefeedccl

import (
	"context"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

// this tests the inner sink client v2, not including the batching sink wrapper
// copied mostly from TestKafkaSink
func TestKafkaSinkClientV2(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	p := newAsyncProducerMock(1)
	sinkClient, cleanup := makeTestKafkaSinkV2(
		t, noTopicPrefix, defaultTopicName, p, "t")
	defer cleanup()

	// No inflight
	require.NoError(t, sinkClient.Flush(ctx, nil))

	type row struct {
		key, value string
	}
	mkPayload := func(rows []row) SinkPayload {
		buf := sinkClient.MakeBatchBuffer("t")
		for _, r := range rows {
			buf.Append([]byte(r.key), []byte(r.value), attributes{})
		}
		payload, err := buf.Close()
		require.NoError(t, err)
		return payload
	}

	// Timeout
	payload := mkPayload([]row{{`1`, `1`}})
	m1 := <-p.inputCh
	for i := 0; i < 2; i++ {
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		require.True(t, errors.Is(context.DeadlineExceeded, sinkClient.Flush(timeoutCtx, payload)))
	}
	go func() { p.successesCh <- m1 }()
	require.NoError(t, sinkClient.Flush(ctx, payload))

	// // Mixed success and error.
	// var pool testAllocPool
	// require.NoError(t, sinkClient.EmitRow(ctx,
	// 	topic(`t`), []byte(`2`), nil, zeroTS, zeroTS, pool.alloc()))
	// m2 := <-p.inputCh
	// require.NoError(t, sinkClient.EmitRow(
	// 	ctx, topic(`t`), []byte(`3`), nil, zeroTS, zeroTS, pool.alloc()))

	// m3 := <-p.inputCh
	// require.NoError(t, sinkClient.EmitRow(
	// 	ctx, topic(`t`), []byte(`4`), nil, zeroTS, zeroTS, pool.alloc()))

	// m4 := <-p.inputCh
	// go func() { p.successesCh <- m2 }()
	// go func() {
	// 	p.errorsCh <- &sarama.ProducerError{
	// 		Msg: m3,
	// 		Err: errors.New("m3"),
	// 	}
	// }()
	// go func() { p.successesCh <- m4 }()
	// require.Regexp(t, "m3", sinkClient.Flush(ctx))

	// // Check simple success again after error
	// require.NoError(t, sinkClient.EmitRow(
	// 	ctx, topic(`t`), []byte(`5`), nil, zeroTS, zeroTS, pool.alloc()))

	// m5 := <-p.inputCh
	// go func() { p.successesCh <- m5 }()
	// require.NoError(t, sinkClient.Flush(ctx))
	// // At the end, all of the resources has been released
	// require.EqualValues(t, 0, pool.used())
}

func makeTestKafkaSinkV2(
	t testing.TB,
	topicPrefix string,
	topicNameOverride string,
	p sarama.AsyncProducer,
	targetNames ...string,
) (s *kafkaSinkClient, cleanup func()) {
	targets := makeChangefeedTargets(targetNames...)
	topics, err := MakeTopicNamer(targets,
		WithPrefix(topicPrefix), WithSingleName(topicNameOverride), WithSanitizeFn(SQLNameToKafkaName))
	require.NoError(t, err)

	bcfg := sinkBatchConfig{}
	s, err = newKafkaSinkClient(context.TODO(), nil, bcfg, "no addrs", topics, nil, kafkaSinkKnobs{
		OverrideAsyncProducerFromClient: func(client kafkaClient) (sarama.AsyncProducer, error) {
			return p, nil
		},
		OverrideClientInit: func(config *sarama.Config) (kafkaClient, error) {
			client := &fakeKafkaClient{config}
			return client, nil
		},
	})
	require.NoError(t, err)

	return s, func() {
		require.NoError(t, s.Close())
	}
}
