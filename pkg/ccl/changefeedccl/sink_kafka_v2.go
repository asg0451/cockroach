package changefeedccl

import (
	"context"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/util/admission"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/segmentio/kafka-go"
)

const oneByOne = "onebyone"
const noBufAndClose = "nobuf"
const newLib = "newlib"
const whichHack = newLib

func newKafkaSinkClient(
	ctx context.Context,
	kafkaCfg *sarama.Config,
	batchCfg sinkBatchConfig,
	bootstrapAddrs string,
	topic *TopicNamer,
	settings *cluster.Settings,
	knobs kafkaSinkKnobs,
) (*kafkaSinkClient, error) {
	client, err := newKafkaClient(kafkaCfg, bootstrapAddrs, knobs)
	if err != nil {
		return nil, err
	}

	writer := &kafka.Writer{
		Addr:            kafka.TCP(bootstrapAddrs),
		Balancer:        &kafka.Hash{}, // TODO: would need to change to retain parity
		MaxAttempts:     kafkaCfg.Producer.Retry.Max,
		WriteBackoffMin: 10 * time.Millisecond,
		BatchSize:       min(kafkaCfg.Producer.Flush.MaxMessages, kafkaCfg.Producer.Flush.Messages),
		BatchBytes:      int64(kafkaCfg.Producer.Flush.Bytes),
		BatchTimeout:    kafkaCfg.Producer.Flush.Frequency,
		RequiredAcks:    kafka.RequiredAcks(kafkaCfg.Producer.RequiredAcks),
		Async:           false,
		Completion:      func(messages []kafka.Message, err error) {}, // ?
		Compression:     kafka.Compression(kafkaCfg.Producer.Compression),
		Logger: kafka.LoggerFunc(func(msg string, args ...any) {
			log.Infof(ctx, "kafka: %+v", []any{msg, args})
		}),
		ErrorLogger:            nil,
		Transport:              nil,
		AllowAutoTopicCreation: true,
	}
	log.Infof(ctx, `kafka writer: %+v`, writer)

	var producer sarama.SyncProducer
	producer, err = sarama.NewSyncProducerFromClient(client)
	// if knobs.OverrideAsyncProducerFromClient != nil {
	// 	producer, err = knobs.OverrideAsyncProducerFromClient(client)
	// } else {
	// 	producer, err = sarama.NewAsyncProducerFromClient(client)
	// }
	if err != nil {
		return nil, err
	}

	return &kafkaSinkClient{
		client:         client,
		producer:       producer,
		knobs:          knobs,
		topics:         topic,
		batchCfg:       batchCfg,
		canTryResizing: changefeedbase.BatchReductionRetryEnabled.Get(&settings.SV),
		writer:         writer,
	}, nil
}

func newKafkaClient(
	config *sarama.Config,
	bootstrapAddrs string,
	knobs kafkaSinkKnobs,
) (sarama.Client, error) {
	// Initialize client and producer
	if knobs.OverrideClientInit != nil {
		client, err := knobs.OverrideClientInit(config)
		return client.(sarama.Client), err // TODO: unhack
	}

	client, err := sarama.NewClient(strings.Split(bootstrapAddrs, `,`), config)
	if err != nil {
		return nil, pgerror.Wrapf(err, pgcode.CannotConnectNow,
			`connecting to kafka: %s`, bootstrapAddrs)
	}

	return client, err
}

// TODO: rename, with v2 in there somewhere
// single threaded ONLY
type kafkaSinkClient struct {
	format   changefeedbase.FormatType
	topics   *TopicNamer
	batchCfg sinkBatchConfig
	client   sarama.Client
	producer sarama.SyncProducer

	writer *kafka.Writer

	knobs          kafkaSinkKnobs
	canTryResizing bool

	lastMetadataRefresh time.Time

	debuggingId int64
}

// Close implements SinkClient.
func (k *kafkaSinkClient) Close() error {
	if err := k.producer.Close(); err != nil {
		return err
	}
	return k.client.Close()
}

// Flush implements SinkClient. Does not retry -- retries will be handled by ParallelIO.
func (k *kafkaSinkClient) Flush(ctx context.Context, payload SinkPayload) (retErr error) {
	msgs := payload.([]kafka.Message)
	defer log.Infof(ctx, `flushed %d messages to kafka (id=%d, err=%v)`, len(msgs), k.debuggingId, retErr)

	log.Infof(ctx, `sending %d messages to kafka`, len(msgs))
	// debugDir := path.Join(debugRoot, strconv.Itoa(int(k.debuggingId)))
	// dfn := path.Join(debugDir, time.Now().Format(`2006-01-02T15:04:05.000000000`))
	// fh, err := os.OpenFile(dfn, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	// if err != nil {
	// 	return err
	// }
	// defer fh.Close()
	// out := json.NewEncoder(fh)
	// for _, m := range msgs {
	// 	mm := map[string]any{
	// 		`topic`:     m.Topic,
	// 		`partition`: m.Partition,
	// 		`key`:       string(m.Key.(sarama.ByteEncoder)),
	// 		`value`:     string(m.Value.(sarama.ByteEncoder)),
	// 		`offset`:    m.Offset,
	// 	}
	// 	if err := out.Encode(mm); err != nil {
	// 		return err
	// 	}
	// }
	// log.Infof(ctx, `KAFKADEBUG: %d wrote %d messages to %s`, k.debuggingId, len(msgs), fh.Name())

	// defer func() {
	// 	fh2, err := os.OpenFile(dfn+".after", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// 	out := json.NewEncoder(fh2)
	// 	for _, m := range msgs {
	// 		mm := map[string]any{
	// 			`topic`:     m.Topic,
	// 			`partition`: m.Partition,
	// 			`key`:       string(m.Key.(sarama.ByteEncoder)),
	// 			`value`:     string(m.Value.(sarama.ByteEncoder)),
	// 			`offset`:    m.Offset,
	// 		}
	// 		if err := out.Encode(mm); err != nil {
	// 			panic(err)
	// 		}
	// 	}
	// 	log.Infof(ctx, `KAFKADEBUG.after: %d wrote %d messages to %s`, k.debuggingId, len(msgs), fh2.Name())
	// 	_ = fh2.Close()
	// }()

	// TODO: make this better. possibly moving the resizing up into the batch worker would help a bit
	var flushMsgs func(msgs []kafka.Message) error
	flushMsgs = func(msgs []kafka.Message) error {
		handleErr := func(err error) error {
			log.Infof(ctx, `kafka error in %d: %s`, k.debuggingId, err.Error())
			if k.shouldTryResizing(err, msgs) {
				a, b := msgs[0:len(msgs)/2], msgs[len(msgs)/2:]
				// recurse
				return errors.Join(flushMsgs(a), flushMsgs(b))
			}

			return err
		}

		// failed again:
		// 23:46:14 test_impl.go:414: test failure #1: full stack retained in failure_1.log: (cdc.go:3802).validateMessage: topic consumer for district encountered validator error(s):
		// topic district partition 0: saw new row timestamp 1717112771631751551.0000000000 after 1717112772130398025.0000000000 was seen (key [0, 3])
		// ADIR=~/tmp/artifacts-backups/kafka-chaos-back-to-sync
		// ~/tmp/ksd
		// 		$ find ~/tmp/ksd/ -type f -name '*.after' | xargs grep -F '[0, 3]' | grep district | grep -e 1717112771631751551 -e 1717112772130398025

		// node1/784/2024-05-30T23:46:22.129544646.after:{"key":"[0, 3]","offset":64542,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node1/784/2024-05-30T23:46:22.129544646.after:{"key":"[0, 3]","offset":64543,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// node1/682/2024-05-30T23:46:20.288790251.after:{"key":"[0, 3]","offset":57711,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node1/682/2024-05-30T23:46:20.288790251.after:{"key":"[0, 3]","offset":57713,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// node1/546/2024-05-30T23:46:17.665719644.after:{"key":"[0, 3]","offset":48603,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node1/546/2024-05-30T23:46:17.665719644.after:{"key":"[0, 3]","offset":48609,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// node1/653/2024-05-30T23:46:19.714983596.after:{"key":"[0, 3]","offset":55433,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node1/653/2024-05-30T23:46:19.714983596.after:{"key":"[0, 3]","offset":55434,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// node3/393/2024-05-30T23:46:22.727410479.after:{"key":"[0, 3]","offset":66822,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node3/393/2024-05-30T23:46:22.727410479.after:{"key":"[0, 3]","offset":66823,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// node3/359/2024-05-30T23:46:21.495681113.after:{"key":"[0, 3]","offset":62268,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node3/359/2024-05-30T23:46:21.495681113.after:{"key":"[0, 3]","offset":62271,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// node3/291/2024-05-30T23:46:19.107264359.after:{"key":"[0, 3]","offset":53160,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node3/291/2024-05-30T23:46:19.107264359.after:{"key":"[0, 3]","offset":53161,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// node2/342/2024-05-30T23:46:20.965629501.after:{"key":"[0, 3]","offset":59988,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node2/342/2024-05-30T23:46:20.965629501.after:{"key":"[0, 3]","offset":59989,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// node2/241/2024-05-30T23:46:14.823468502.after:{"key":"[0, 3]","offset":46325,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node2/241/2024-05-30T23:46:14.823468502.after:{"key":"[0, 3]","offset":46328,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// another offset zero here. looking at the logs we see "fail to deliver 2 messages", and looking at this file we see the first 2 messages are offset 0
		// {"key":"[0, 3]","offset":0,"partition":0,"topic":"district","value":.., \"updated\": \"1717112722268996683.0000000000\"}"}
		// {"key":"[0, 3]","offset":0,"partition":0,"topic":"district","value":.., \"updated\": \"1717112771631751551.0000000000\"}"} // this one i guess
		// so how did it happen??

		// node2/241/2024-05-30T23:46:12.303519065.after:{"key":"[0, 3]","offset":0,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node2/241/2024-05-30T23:46:12.303519065.after:{"key":"[0, 3]","offset":46032,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// node2/274/2024-05-30T23:46:18.407261212.after:{"key":"[0, 3]","offset":50882,"partition":0,"topic":"district","value":...:\"updated\": \"1717112771631751551.0000000000\"}"}
		// node2/274/2024-05-30T23:46:18.407261212.after:{"key":"[0, 3]","offset":50884,"partition":0,"topic":"district","value":...:\"updated\": \"1717112772130398025.0000000000\"}"}

		// // this still hits the issue
		// if err := k.producer.SendMessages(msgs); err != nil {
		// 	return handleErr(err)
		// }

		// this works (new lib)
		// if err := k.writer.WriteMessages(ctx, msgs...); err != nil {
		// 	return handleErr(err)
		// }

		switch whichHack {
		case oneByOne:
			// this works if we also set maxmessages = 1
			panic("undo the change")
			// for _, m := range msgs {
			// 	if _, _, err := k.producer.SendMessage(m); err != nil {
			// 		return handleErr(err)
			// 	}
			// }
		case noBufAndClose: // TODO: if this is competitive, validate that it's actually correct. nope its not working lol
			panic("undo the change")
			// TODO: i dont think Close() actually flushes. it just waits for flush...
			// producer, err := sarama.NewSyncProducerFromClient(k.client)
			// if err != nil {
			// 	return err
			// }
			// if err := producer.SendMessages(msgs); err != nil {
			// 	return handleErr(err)
			// }
			// if err := producer.Close(); err != nil {
			// 	return err
			// }
		case newLib:
			// TODO: leverage k.writer.Stats()
			if err := k.writer.WriteMessages(ctx, msgs...); err != nil {
				return handleErr(err)
			}
		default:
			panic("unknown hack")
		}
		// trk := tracker{pendingIDs: make(map[int]struct{})}
		// for _, m := range msgs {
		// 	m.Metadata = map[string]any{`id`: trk.next()}
		// }

		// // send input, while watching for errors & close
		// for sent := 0; sent < len(msgs); {
		// 	m := msgs[sent]
		// 	select {
		// 	case <-ctx.Done():
		// 		return ctx.Err()
		// 	case k.producer.Input() <- m:
		// 		sent++
		// 	case ms := <-k.producer.Successes():
		// 		// TODO: i saw a panic here: panic: id 1 not found in pendingIDs. not sure how it happened tho
		// 		trk.remove(ms.Metadata.(map[string]any)[`id`].(int))
		// 		// TODO: re add metrics support
		// 	case err := <-k.producer.Errors():
		// 		return handleErr(err)
		// 	}
		// }

		// // make sure all messages are confirmed or errored
		// for !trk.empty() {
		// 	select {
		// 	case <-ctx.Done():
		// 		return ctx.Err()
		// 	case err := <-k.producer.Errors():
		// 		return handleErr(err)
		// 	case ms := <-k.producer.Successes():
		// 		// TODO: re add metrics support
		// 		trk.remove(ms.Metadata.(map[string]any)[`id`].(int))
		// 	}
		// }

		return nil
	}
	return flushMsgs(msgs)
}

// FlushResolvedPayload implements SinkClient.
func (k *kafkaSinkClient) FlushResolvedPayload(
	ctx context.Context,
	body []byte,
	forEachTopic func(func(topic string) error) error,
	retryOpts retry.Options,
) error {
	const metadataRefreshMinDuration = time.Minute
	if timeutil.Since(k.lastMetadataRefresh) > metadataRefreshMinDuration {
		if err := k.client.RefreshMetadata(k.topics.DisplayNamesSlice()...); err != nil {
			return err
		}
		k.lastMetadataRefresh = timeutil.Now()
	}

	return forEachTopic(func(topic string) error {
		partitions, err := k.client.Partitions(topic)
		if err != nil {
			return err
		}
		for _, partition := range partitions {
			msgs := []kafka.Message{{
				Topic:     topic,
				Partition: int(partition),
				Key:       nil,
				Value:     sarama.ByteEncoder(body),
			}}
			if err := k.Flush(ctx, msgs); err != nil {
				return err
			}
		}
		return nil
	})
}

// MakeBatchBuffer implements SinkClient.
func (k *kafkaSinkClient) MakeBatchBuffer(topic string) BatchBuffer {
	return &kafkaBuffer{topic: topic, batchCfg: k.batchCfg}
}

func (k *kafkaSinkClient) shouldTryResizing(err error, msgs []kafka.Message) bool {
	if !k.canTryResizing || err == nil || len(msgs) < 2 {
		return false
	}
	var kError sarama.KError
	return errors.As(err, &kError) && kError == sarama.ErrMessageSizeTooLarge
}

var _ SinkClient = (*kafkaSinkClient)(nil)
var _ SinkPayload = ([]kafka.Message)(nil) // this doesnt actually assert anything fyi

type keyPlusPayload struct {
	key     []byte
	payload []byte
}

type kafkaBuffer struct {
	topic     string
	messages  []keyPlusPayload
	byteCount int

	batchCfg sinkBatchConfig
}

// Append implements BatchBuffer.
func (b *kafkaBuffer) Append(key []byte, value []byte, _ attributes) {
	b.messages = append(b.messages, keyPlusPayload{key: key, payload: value})
	b.byteCount += len(value)
}

// Close implements BatchBuffer. Convert the buffer into a SinkPayload for sending to kafka.
func (b *kafkaBuffer) Close() (SinkPayload, error) {
	msgs := make([]kafka.Message, 0, len(b.messages))
	for _, m := range b.messages {
		msgs = append(msgs, kafka.Message{
			Topic: b.topic,
			Key:   m.key,
			Value: m.payload,
		})
	}
	return msgs, nil
}

// ShouldFlush implements BatchBuffer.
func (b *kafkaBuffer) ShouldFlush() bool {
	return shouldFlushBatch(b.byteCount, len(b.messages), b.batchCfg)
}

var _ BatchBuffer = (*kafkaBuffer)(nil)

var lastSinkId atomic.Int64

const debugRoot = `/mnt/data2/kafka_sink_debug/`

func makeKafkaSinkV2(ctx context.Context,
	u sinkURL,
	targets changefeedbase.Targets,
	jsonConfig changefeedbase.SinkSpecificJSONConfig,
	parallelism int,
	pacerFactory func() *admission.Pacer,
	timeSource timeutil.TimeSource,
	settings *cluster.Settings,
	mb metricsRecorderBuilder,
) (Sink, error) {
	batchCfg, retryOpts, err := getSinkConfigFromJson(jsonConfig, sinkJSONConfig{
		// TODO[rachael]: Change to kafka defaults
		// ..but the defaults for these are all zero -- flush immediately.
		Flush: sinkBatchConfig{
			Frequency: jsonDuration(10 * time.Millisecond),
			Messages:  100,
			Bytes:     1e6,
		},
	})
	if err != nil {
		return nil, err
	}

	kafkaTopicPrefix := u.consumeParam(changefeedbase.SinkParamTopicPrefix)
	kafkaTopicName := u.consumeParam(changefeedbase.SinkParamTopicName)
	if schemaTopic := u.consumeParam(changefeedbase.SinkParamSchemaTopic); schemaTopic != `` {
		return nil, errors.Errorf(`%s is not yet supported`, changefeedbase.SinkParamSchemaTopic)
	}

	m := mb(requiresResourceAccounting)
	kafkaCfg, err := buildKafkaConfig(ctx, u, jsonConfig, m.getKafkaThrottlingMetrics(settings))
	if err != nil {
		return nil, err
	}
	kafkaCfg.Producer.Retry.Max = 0 // retry is handled by the batching sink / parallelIO
	// need to set this to ensure message ordering without turning on idempotency
	// this is bugged though so doesnt work right. looks like this might still be the case even with retries off
	// if we end up with 2 batches, A and B, A can fail and B can still go through.
	kafkaCfg.Net.MaxOpenRequests = 1
	// kafkaCfg.Producer.Flush.Bytes = int(sarama.MaxRequestSize)
	// kafkaCfg.Producer.Flush.Messages = 0

	// trying one by one. see Flush()
	switch whichHack {
	case oneByOne:
		kafkaCfg.Producer.Flush.MaxMessages = 1
	case noBufAndClose:
		kafkaCfg.Producer.Flush.MaxMessages = batchCfg.Messages * 100
		kafkaCfg.Producer.Flush.Messages = batchCfg.Messages * 100
		kafkaCfg.Producer.Flush.Bytes = batchCfg.Bytes * 100
		kafkaCfg.Producer.Flush.Frequency = 0
	case newLib:
	}

	// but with this set and still using SendMessages(), we expect to see the issue more frequently. since num sarama batches per messageBatch will be > 1
	// kafkaCfg.Producer.Flush.MaxMessages = 1
	// didnt see it more frequently but still saw it :shrug:
	// 13:50:12 test_impl.go:414: test failure #1: full stack retained in failure_1.log: (cdc.go:3805).validateMessage: topic consumer for district encountered validator error(s):
	// topic district partition 0: saw new row timestamp 1717422587821883071.0000000000 after 1717422607448984688.0000000000 was seen (key [15, 10])

	// $ find ~/tmp/ksd/ -type f -name '*.after' | xargs grep -F '[15, 10]' | grep district | grep -e 1717422587821883071.0000000000 -e 1717422607448984688.0000000000
	// node1/856/2024-06-03T13:50:14.334392912.after:{"key":"[15, 10]","offset":86357,"partition":0,"topic":"district","value":"{, \"updated\": \"1717422587821883071.0000000000\"}"}
	// node1/856/2024-06-03T13:50:14.334392912.after:{"key":"[15, 10]","offset":86359,"partition":0,"topic":"district","value":"{, \"updated\": \"1717422607448984688.0000000000\"}"}

	// node1/886/2024-06-03T13:50:15.029747069.after:{"key":"[15, 10]","offset":88436,"partition":0,"topic":"district","value":"{, \"updated\": \"1717422587821883071.0000000000\"}"}
	// node1/886/2024-06-03T13:50:15.029747069.after:{"key":"[15, 10]","offset":88445,"partition":0,"topic":"district","value":"{, \"updated\": \"1717422607448984688.0000000000\"}"}

	// right here we see A B, A failed, B went through, then A B emitted again as a retry.
	// can also see that this is a batch that failed because the same sink tried it twice. as opposed to the other instances where it succeeded but was caught in the crossfire of other errors
	// node1/787/2024-06-03T13:50:09.615816229.after:{"key":"[15, 10]","offset":0,    "partition":0,"topic":"district","value":"{, \"updated\": \"1717422587821883071.0000000000\"}"}
	// node1/787/2024-06-03T13:50:09.615816229.after:{"key":"[15, 10]","offset":81228,"partition":0,"topic":"district","value":"{, \"updated\": \"1717422607448984688.0000000000\"}"}
	// node1/787/2024-06-03T13:50:12.221470166.after:{"key":"[15, 10]","offset":81722,"partition":0,"topic":"district","value":"{, \"updated\": \"1717422587821883071.0000000000\"}"}
	// node1/787/2024-06-03T13:50:12.221470166.after:{"key":"[15, 10]","offset":81724,"partition":0,"topic":"district","value":"{, \"updated\": \"1717422607448984688.0000000000\"}"}

	// node3/397/2024-06-03T13:50:13.539163516.after:{"key":"[15, 10]","offset":84098,"partition":0,"topic":"district","value":"{, \"updated\": \"1717422587821883071.0000000000\"}"}
	// node3/397/2024-06-03T13:50:13.539163516.after:{"key":"[15, 10]","offset":84106,"partition":0,"topic":"district","value":"{, \"updated\": \"1717422607448984688.0000000000\"}"}

	// next try:
	// kafkaCfg.Producer.Flush.Frequency = time.Microsecond

	topicNamer, err := MakeTopicNamer(
		targets,
		WithPrefix(kafkaTopicPrefix), WithSingleName(kafkaTopicName), WithSanitizeFn(SQLNameToKafkaName))

	if err != nil {
		return nil, err
	}

	if unknownParams := u.remainingQueryParams(); len(unknownParams) > 0 {
		return nil, errors.Errorf(
			`unknown kafka sink query parameters: %s`, strings.Join(unknownParams, ", "))
	}

	clientFactory := func(ctx context.Context) (any, error) {
		log.Infof(ctx, `creating kafka sink client`)
		topicNamer2, err := MakeTopicNamer(
			targets,
			WithPrefix(kafkaTopicPrefix), WithSingleName(kafkaTopicName), WithSanitizeFn(SQLNameToKafkaName))

		if err != nil {
			return nil, err
		}

		// TODO: how to handle knobs
		client, err := newKafkaSinkClient(ctx, kafkaCfg, batchCfg, u.Host, topicNamer2, settings, kafkaSinkKnobs{})
		if err != nil {
			return nil, err
		}
		client.debuggingId = lastSinkId.Add(1)
		debugDir := path.Join(debugRoot, strconv.Itoa(int(client.debuggingId)))
		if err := os.MkdirAll(debugDir, 0755); err != nil {
			return nil, err
		}

		return client, nil
	}

	return makeBatchingSink(ctx, sinkTypeKafka, nil, clientFactory, time.Second, retryOpts,
		parallelism, topicNamer, pacerFactory, timeSource, mb(true), settings), nil
}

// type tracker struct {
// 	nextID     int
// 	pendingIDs map[int]struct{}
// }

// func (t *tracker) next() int {
// 	t.nextID++
// 	t.pendingIDs[t.nextID] = struct{}{}
// 	return t.nextID
// }

// func (t *tracker) remove(id int) {
// 	if _, ok := t.pendingIDs[id]; !ok {
// 		panic(errors.Errorf(`id %d not found in pendingIDs`, id))
// 	}
// 	delete(t.pendingIDs, id)
// }

// func (t *tracker) empty() bool {
// 	return len(t.pendingIDs) == 0
// }
