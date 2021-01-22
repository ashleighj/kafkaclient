package kafkaclient

import (
	"context"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	logger "github.com/san-services/apilogger"
)

// must implement sarama.ConsumerGroupHandler
type saramaConsumer struct {
	groupID          string
	group            sarama.ConsumerGroup
	config           *sarama.Config
	topicConf        map[string]TopicConfig
	topicNames       []string
	brokers          []string
	procDependencies ProcessorDependencies
	ready            chan bool
	failMessages     chan failedMessage
	initialized      chan bool
	cancel           context.CancelFunc
	ctx              context.Context
}

func newSaramaConsumer(
	saramaConf *sarama.Config, groupID string,
	topicConf map[string]TopicConfig, topicNames []string, 
	brokers []string, pd ProcessorDependencies) (c saramaConsumer, e error) {

	lg := logger.New(nil, "")

	consumerCtx, cancel := context.WithCancel(context.Background())

	c = saramaConsumer{
		groupID:          groupID,
		config:           saramaConf,
		topicConf:        topicConf,
		topicNames:       topicNames,
		brokers:          brokers,
		procDependencies: pd,
		ready:            make(chan bool),
		cancel:           cancel,
		ctx:              consumerCtx}

	c.group, e = sarama.NewConsumerGroup(brokers, groupID, saramaConf)
	if e != nil {
		lg.Error(logger.LogCatKafkaConsumerInit, e)
		return
	}

	c.initialized = make(chan bool, 1)
	c.failMessages = make(chan failedMessage)

	select {
	case <-c.initialized:
	default:
		close(c.initialized)
	}
	return
}

func (c *saramaConsumer) startConsume() {
	lg := logger.New(nil, "")

	wg := &sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()
		for {
			// `Consume` should be called inside an infinite loop,
			// when a server-side rebalance happens, the consumer
			// session will need to be recreated to get the new claims
			if e := (c.group).Consume(c.ctx, c.topicNames, c); e != nil {
				lg.Error(logger.LogCatKafkaConsume, errConsumer(e))
				time.Sleep(5 * time.Second)
			}

			// check if context was cancelled, signaling
			// that the consumer should stop
			if c.ctx.Err() != nil {
				return
			}

			c.ready = make(chan bool)
		}
	}()

	<-c.ready // await consumer set up
	lg.Info(logger.LogCatKafkaConsume, infoConsumerReady)

	select {
	case <-c.ctx.Done():
		lg.Info(logger.LogCatKafkaConsume,
			infoConsumerTerm("context cancelled"))
	}

	c.cancel()
	wg.Wait()

	if e := c.close(); e != nil {
		lg.Fatal(logger.LogCatKafkaConsumerClose, errConsumerClose(e))
	}

	return
}

// ConsumeClaim read ConsumerGroupClaim's Messages() in a loop.
// Method in sarama.ConsumerGroupHandler interface.
//
// Handles consuming and processing or delegating prosessing of topic messages.
// This method is called within a goroutine:
// https://github.com/Shopify/sarama/blob/master/consumer_group.go#L27-L29
func (c *saramaConsumer) ConsumeClaim(
	session sarama.ConsumerGroupSession,
	claim sarama.ConsumerGroupClaim) (e error) {

	lg := logger.New(c.ctx, "")

	for {
		select {
		case msg := <-claim.Messages():
			conf := c.topicConf[msg.Topic]

			if conf.Name == "" || conf.messageCodec == nil {
				e = errTopicConfMissing
				lg.Error(logger.LogCatKafkaConsume, e)
				continue
			}

			m := newSaramaMessage(msg, conf.messageCodec)
			lg.Infof(logger.LogCatKafkaConsume,
				infoEvent("message claimed", msg.Topic, msg.Partition, msg.Offset))

			e = conf.MessageProcessor(session.Context(), c.procDependencies, m)
			if e != nil {
				lg.Error(logger.LogCatKafkaProcessMessage, e)

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
				continue
			}

			session.MarkMessage(msg, "")
		case <-session.Context().Done():
			return nil
		}
	}
}

// Setup is run at the beginning of a new consumer session, before ConsumeClaim.
// Method in sarama.ConsumerGroupHandler interface.
func (c *saramaConsumer) Setup(sarama.ConsumerGroupSession) (e error) {
	// mark the consumer as ready
	close(c.ready)
	return nil
}

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have exited.
// Method in sarama.ConsumerGroupHandler interface.
func (c *saramaConsumer) Cleanup(sarama.ConsumerGroupSession) (e error) {
	return nil
}

// Close stops the ConsumerGroup and detaches any running sessions
func (c *saramaConsumer) close() (e error) {
	lg := logger.New(c.ctx, "")

	e = c.group.Close()
	if e != nil {
		lg.Error(logger.LogCatKafkaConsumerClose, errConsumerClose(e))
	}

	return
}
