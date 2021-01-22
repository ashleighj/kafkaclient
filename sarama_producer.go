package kafkaclient

import (
	"context"
	"fmt"
	"reflect"

	"github.com/Shopify/sarama"
	logger "github.com/san-services/apilogger"
)

type saramaProducer struct {
	producer    interface{}
	topicConf   map[string]TopicConfig
	initialized bool
}

func newSaramaProducer(
	prodType producerType,
	brokers []string,
	topicConf map[string]TopicConfig,
	saramaConf *sarama.Config,
	schemaReg schemaRegistry) (p saramaProducer, e error) {

	lg := logger.New(nil, "")

	switch prodType {
	case ProducerTypeSync:
		p.producer, e = sarama.NewSyncProducer(brokers, saramaConf)
	case ProducerTypeAsync:
		p.producer, e = sarama.NewAsyncProducer(brokers, saramaConf)
	default:
		e = errInvalidProducer
	}

	if e != nil {
		lg.Error(logger.LogCatKafkaProducerInit, e)
		return
	}

	go p.handleAsyncResponses()

	p.topicConf = topicConf
	p.initialized = true
	return
}

func (p *saramaProducer) produceMessage(
	ctx context.Context, topic string, key string, msg interface{}) (e error) {

	lg := logger.New(ctx, "")

	se, e := p.getSaramaEncoder(ctx, p.topicConf[topic], msg)
	if e != nil {
		lg.Error(logger.LogCatKafkaProduce, e)
		return
	}

	m := &sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.StringEncoder(key),
		Value: se}

	switch p.producer.(type) {
	case sarama.SyncProducer:
		_, _, e = (p.producer.(sarama.SyncProducer)).SendMessage(m)
	case sarama.AsyncProducer:
		(p.producer.(sarama.AsyncProducer)).Input() <- m
	default:
		e = errInvalidProducer
	}

	if e != nil {
		lg.Error(logger.LogCatKafkaProduce, e)
	}

	return
}

func (p *saramaProducer) handleAsyncResponses() {
	lg := logger.New(nil, "")

	for {
		select {
		case message := <-(p.producer.(sarama.AsyncProducer)).Successes():
			lg.Infof(logger.LogCatKafkaProduce,
				infoEvent(infoProduceSuccess, message.Topic, message.Partition, message.Offset))
		case e := <-(p.producer.(sarama.AsyncProducer)).Errors():
			lg.Error(logger.LogCatKafkaProduce, errProduceFail(e.Msg.Topic), e)
		}
	}
}

func (p *saramaProducer) getSaramaEncoder(ctx context.Context,
	topicConf TopicConfig, msg interface{}) (s sarama.Encoder, e error) {

	lg := logger.New(ctx, "")

	codec := topicConf.messageCodec
	if codec == nil {
		lg.Error(logger.LogCatKafkaProduce, errCodecNil(topicConf.Name))
		return
	}

	switch msg.(type) {
	case string:
		s = sarama.StringEncoder(msg.(string))
		break
	case []byte:
		s = newSaramaByteEncoder(ctx, topicConf.Name, msg.([]byte), codec)
		break
	case int32, int64, float32, float64:
		s = sarama.StringEncoder(fmt.Sprint(msg))
	default:
		if reflect.ValueOf(msg).Kind() == reflect.Struct {
			s, e = newSaramaStructEncoder(ctx, topicConf.Name, msg, codec)
			break
		}
		e = errMessageType
	}

	if e != nil {
		lg.Error(logger.LogCatKafkaEncode, e)
	}

	return
}
