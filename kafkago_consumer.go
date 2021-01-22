package kafkaclient

import (
	"context"
	"crypto/tls"
	"sync"
	"time"

	logger "github.com/san-services/apilogger"
	"github.com/segmentio/kafka-go"
)

type kafkagoConsumer struct {
	gen              *kafka.Generation
	group            *kafka.ConsumerGroup
	topicNames       []string
	brokers          []string
	initialized      chan bool
	consumerWait     *sync.WaitGroup
	topicConfig      map[string]TopicConfig
	procDependencies ProcessorDependencies
	failMessages     chan failedMessage
}

func newKafkagoConsumer(groupID string, brokers []string,
	topicNames []string, topicConf map[string]TopicConfig,
	pd ProcessorDependencies, tls *tls.Config) (c kafkagoConsumer, e error) {

	lg := logger.New(nil, "")

	d := &kafka.Dialer{
		Timeout:   10 * time.Second,
		DualStack: true,
		TLS:       tls}

	group, e := kafka.NewConsumerGroup(kafka.ConsumerGroupConfig{
		ID:          groupID,
		Brokers:     brokers,
		Topics:      topicNames,
		StartOffset: kafka.LastOffset,
		Dialer:      d})

	if e != nil {
		lg.Error(logger.LogCatKafkaConsumerInit, e)
		return
	}

	c = kafkagoConsumer{
		topicConfig:      topicConf,
		topicNames:       topicNames,
		brokers:          brokers,
		group:            group,
		consumerWait:     new(sync.WaitGroup),
		procDependencies: pd}

	c.initialized = make(chan bool, 1)
	c.failMessages = make(chan failedMessage)

	select {
	case <-c.initialized:
	default:
		close(c.initialized)
	}
	return
}

func (c *kafkagoConsumer) startConsume() {
	lg := logger.New(nil, "")
	var e error

	for {
		// get the next (latest) generation of a consumer group - every time
		// a member (service instance in our case) enters or exits the group,
		// a new generation occurs, which results in a new *kafka.Generation instance
		c.gen, e = c.group.Next(context.TODO())
		if e != nil {
			lg.Error(logger.LogCatKafkaConsume, e)
			time.Sleep(5 * time.Second)
			continue
		}

		c.consumerWait.Add(len(c.topicNames))
		for _, t := range c.topicNames {
			go c.consumeTopic(t)
		}

		c.consumerWait.Wait()
	}
}

func (c *kafkagoConsumer) consumeTopic(topic string) {
	lg := logger.New(nil, "")
	defer c.consumerWait.Done()

	conf := c.topicConfig[topic]

	// get this consumer's partition assignments by topic
	assignments := c.gen.Assignments[topic]

	// loop through each partition assigned, find the offset, and
	// start the work or processing that should be done (gen.start func param)
	// for each message
	for _, assignment := range assignments {
		partition, offset := assignment.ID, assignment.Offset

		c.gen.Start(func(ctx context.Context) {
			// create reader for this topic/partition.
			reader := c.reader(topic, partition)
			defer reader.Close()

			// tell the reader where to read from -
			// the last committed offset for this partition
			reader.SetOffset(offset)

			// read messages
			for {
				msg, e := reader.ReadMessage(ctx)
				if e != nil {
					lg.Error(logger.LogCatKafkaConsume, errMsgRead(e))
					return
				}
				offset = msg.Offset

				m := newKafkaGoMessage(msg, conf.messageCodec)
				e = conf.MessageProcessor(ctx, c.procDependencies, m)

				if e != nil {
					// failed message processing
					if conf.FailedProcessingTopic != "" {
						select {
						case c.failMessages <- newFailedMessage(m, conf.FailedProcessingTopic, e):
							lg.Info(logger.LogCatKafkaConsume, infoEvent("failed message sent to fail handler",
								msg.Topic, int32(msg.Partition), msg.Offset))
						default:
							lg.Error(logger.LogCatKafkaConsume,
								errEvent("failed message not sent to fail handler",
									msg.Topic, int32(msg.Partition), msg.Offset))
						}
					}

					e = reader.SetOffset(offset)
					if e != nil {
						lg.Error(logger.LogCatKafkaCommitOffset, errSetOffset(e))
					}

					c.commitOffset(topic, partition, offset-1)
				}

				// successful message processing
				c.commitOffset(topic, partition, offset)
			}
		})
	}
}

func (c *kafkagoConsumer) commitOffset(topic string, partition int, offset int64) {
	lg := logger.New(nil, "")

	e := c.gen.CommitOffsets(
		map[string]map[int]int64{topic: {partition: offset}})

	if e != nil {
		lg.Error(logger.LogCatKafkaCommitOffset, errCommit(e))

		// retry
		e := c.gen.CommitOffsets(
			map[string]map[int]int64{topic: {partition: offset}})

		if e != nil {
			lg.Error(logger.LogCatKafkaCommitOffset, errCommit(e))
		}
	}
}

func (c *kafkagoConsumer) reader(topic string, partition int) *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:   c.brokers,
		Topic:     topic,
		Partition: partition})
}
