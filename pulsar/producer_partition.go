// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package pulsar

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/apache/pulsar-client-go/pulsar/internal/compression"

	"github.com/gogo/protobuf/proto"

	log "github.com/sirupsen/logrus"

	"github.com/apache/pulsar-client-go/pulsar/internal"
	pb "github.com/apache/pulsar-client-go/pulsar/internal/pulsar_proto"
)

const (
	// producer states
	producerInit int32 = iota
	producerReady
	producerClosing
	producerClosed
)

var (
	errFailAddBatch    = errors.New("message send failed")
	errMessageTooLarge = errors.New("message size exceeds MaxMessageSize")

	buffersPool sync.Pool
)

var (
	messagesPublished = promauto.NewCounter(prometheus.CounterOpts{
		Name: "pulsar_client_messages_published",
		Help: "Counter of messages published by the client",
	})

	bytesPublished = promauto.NewCounter(prometheus.CounterOpts{
		Name: "pulsar_client_bytes_published",
		Help: "Counter of messages published by the client",
	})

	messagesPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pulsar_client_producer_pending_messages",
		Help: "Counter of messages pending to be published by the client",
	})

	bytesPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pulsar_client_producer_pending_bytes",
		Help: "Counter of bytes pending to be published by the client",
	})

	publishErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "pulsar_client_producer_errors",
		Help: "Counter of publish errors",
	})

	publishLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "pulsar_client_producer_latency_seconds",
		Help:    "Publish latency experienced by the client",
		Buckets: []float64{.0005, .001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	})
)

type partitionProducer struct {
	state  int32
	client *client
	topic  string
	log    *log.Entry
	cnx    internal.Connection

	options             *ProducerOptions
	producerName        string
	producerID          uint64
	batchBuilder        *internal.BatchBuilder
	sequenceIDGenerator *uint64 // NOTE: generate seq id for batch

	// Channel where app is posting messages to be published
	// NOTE: pending message components
	eventsChan       chan interface{} // NOTE: 1. buffered channel to store msgs
	batchFlushTicker *time.Ticker     // NOTE: 2. batch flush interval

	publishSemaphore internal.Semaphore     // NOTE: 3. flow control
	pendingQueue     internal.BlockingQueue // NOTE: 4. storage batch msg until receive receipt
	lastSequenceID   int64

	partitionIdx int32
}

// NOTE: connect to broker, open write goroutine
func newPartitionProducer(client *client, topic string, options *ProducerOptions, partitionIdx int) (
	*partitionProducer, error) {
	var batchingMaxPublishDelay time.Duration
	if options.BatchingMaxPublishDelay != 0 {
		batchingMaxPublishDelay = options.BatchingMaxPublishDelay
	} else {
		batchingMaxPublishDelay = defaultBatchingMaxPublishDelay
	}

	var maxPendingMessages int
	if options.MaxPendingMessages == 0 {
		maxPendingMessages = 1000
	} else {
		maxPendingMessages = options.MaxPendingMessages
	}

	p := &partitionProducer{
		state:            producerInit,
		log:              log.WithField("topic", topic),
		client:           client,
		topic:            topic,
		options:          options,
		producerID:       client.rpcClient.NewProducerID(),
		eventsChan:       make(chan interface{}, maxPendingMessages),
		batchFlushTicker: time.NewTicker(batchingMaxPublishDelay),
		publishSemaphore: internal.NewSemaphore(int32(maxPendingMessages)),
		pendingQueue:     internal.NewBlockingQueue(maxPendingMessages),
		lastSequenceID:   -1,
		partitionIdx:     int32(partitionIdx),
	}

	if options.Name != "" {
		p.producerName = options.Name
	}

	err := p.grabCnx()
	if err != nil {
		log.WithError(err).Errorf("Failed to create producer")
		return nil, err
	}

	p.log = p.log.WithField("producer_name", p.producerName).
		WithField("producerID", p.producerID)
	p.log.WithField("cnx", p.cnx.ID()).Info("Created producer")
	atomic.StoreInt32(&p.state, producerReady)

	go p.runEventsLoop()

	return p, nil
}

// NOTE: lookup and connect to target broker, send CMD PRODUCER to register myself
// NOTE: register myself to cnx, to call ReceivedSendReceipt when produce succeed
// NOTE: sync last seq id and flush last pending batch data
func (p *partitionProducer) grabCnx() error {
	lr, err := p.client.lookupService.Lookup(p.topic)
	if err != nil {
		p.log.WithError(err).Warn("Failed to lookup topic")
		return err
	}

	p.log.Debug("Lookup result: ", lr)
	id := p.client.rpcClient.NewRequestID()
	cmdProducer := &pb.CommandProducer{
		RequestId:  proto.Uint64(id),
		Topic:      proto.String(p.topic),
		Encrypted:  nil,
		ProducerId: proto.Uint64(p.producerID),
		Schema:     nil,
	}

	if p.producerName != "" {
		cmdProducer.ProducerName = proto.String(p.producerName)
	}

	if len(p.options.Properties) > 0 {
		cmdProducer.Metadata = toKeyValues(p.options.Properties)
	}
	res, err := p.client.rpcClient.Request(lr.LogicalAddr, lr.PhysicalAddr, id, pb.BaseCommand_PRODUCER, cmdProducer)
	if err != nil {
		p.log.WithError(err).Error("Failed to create producer")
		return err
	}

	p.producerName = res.Response.ProducerSuccess.GetProducerName()
	if p.batchBuilder == nil {
		p.batchBuilder, err = internal.NewBatchBuilder(p.options.BatchingMaxMessages, p.options.BatchingMaxSize,
			p.producerName, p.producerID, pb.CompressionType(p.options.CompressionType),
			compression.Level(p.options.CompressionLevel),
			p)
		if err != nil {
			return err
		}
	}

	if p.sequenceIDGenerator == nil {
		// NOTE: sync last seq id with broker
		nextSequenceID := uint64(res.Response.ProducerSuccess.GetLastSequenceId() + 1)
		p.sequenceIDGenerator = &nextSequenceID
	}
	p.cnx = res.Cnx
	// NOTE: register itself to cnx,
	p.cnx.RegisterListener(p.producerID, p)
	p.log.WithField("cnx", res.Cnx.ID()).Debug("Connected producer")

	pendingItems := p.pendingQueue.ReadableSlice()
	if len(pendingItems) > 0 {
		p.log.Infof("Resending %d pending batches", len(pendingItems))
		for _, pi := range pendingItems {
			// NOTE: directly write pending batch data which already wrapped to cmd and proto marshalled
			p.cnx.WriteData(pi.(*pendingItem).batchData)
		}
	}
	return nil
}

type connectionClosed struct{}

func (p *partitionProducer) GetBuffer() internal.Buffer {
	b, ok := buffersPool.Get().(internal.Buffer)
	if ok {
		b.Clear()
	}
	return b
}

// NOTE: broker notify producer the connection will broken
func (p *partitionProducer) ConnectionClosed() {
	// Trigger reconnection in the produce goroutine
	p.log.WithField("cnx", p.cnx.ID()).Warn("Connection was closed")
	p.eventsChan <- &connectionClosed{}
}

func (p *partitionProducer) reconnectToBroker() {
	backoff := internal.Backoff{}
	for {
		if atomic.LoadInt32(&p.state) != producerReady {
			// Producer is already closing
			return
		}

		d := backoff.Next()
		p.log.Info("Reconnecting to broker in ", d)
		time.Sleep(d)

		err := p.grabCnx()
		if err == nil {
			// Successfully reconnected
			p.log.WithField("cnx", p.cnx.ID()).Info("Reconnected producer to broker")
			return
		}
	}
}

func (p *partitionProducer) runEventsLoop() {
	for {
		select {
		case i := <-p.eventsChan:
			switch v := i.(type) {
			case *sendRequest: // NOTE: append msg to batch then may flush
				p.internalSend(v)
			case *connectionClosed: // NOTE: broker notify conn will broken, try reconnect
				p.reconnectToBroker()
			case *flushRequest: // NOTE: called Flush() manually
				p.internalFlush(v)
			case *closeProducer: // NOTE: called Close() manually, notify broker conn will broken
				p.internalClose(v)
				return
			}

		case <-p.batchFlushTicker.C: // NOTE: interval flush batch
			p.internalFlushCurrentBatch()
		}
	}
}

func (p *partitionProducer) Topic() string {
	return p.topic
}

func (p *partitionProducer) Name() string {
	return p.producerName
}

func (p *partitionProducer) internalSend(request *sendRequest) {
	p.log.Debug("Received send request: ", *request)

	msg := request.msg

	// if msg is too large
	if len(msg.Payload) > int(p.cnx.GetMaxMessageSize()) {
		p.publishSemaphore.Release()
		// NOTE: callback with error msg
		request.callback(nil, request.msg, errMessageTooLarge)
		p.log.WithField("size", len(msg.Payload)).
			WithField("properties", msg.Properties).
			WithError(errMessageTooLarge).Error()
		publishErrors.Inc()
		return
	}

	deliverAt := msg.DeliverAt
	if msg.DeliverAfter.Nanoseconds() > 0 {
		deliverAt = time.Now().Add(msg.DeliverAfter)
	}

	// NOTE: add to batch conditions
	// NOTE: 1. enable batch
	// NOTE: 2. not a delay msg
	sendAsBatch := !p.options.DisableBatching &&
		msg.ReplicationClusters == nil &&
		deliverAt.UnixNano() < 0

	smm := &pb.SingleMessageMetadata{
		PayloadSize: proto.Int(len(msg.Payload)),
	}

	if msg.EventTime.UnixNano() != 0 {
		smm.EventTime = proto.Uint64(internal.TimestampMillis(msg.EventTime))
	}

	if msg.Key != "" {
		smm.PartitionKey = proto.String(msg.Key)
	}

	if msg.Properties != nil {
		smm.Properties = internal.ConvertFromStringMap(msg.Properties)
	}

	var sequenceID uint64
	if msg.SequenceID != nil {
		sequenceID = uint64(*msg.SequenceID)
	} else {
		sequenceID = internal.GetAndAdd(p.sequenceIDGenerator, 1)
	}

	added := p.batchBuilder.Add(smm, sequenceID, msg.Payload, request,
		msg.ReplicationClusters, deliverAt)
	if !added {
		// The current batch is full.. flush it and retry
		// NOTE: 1. if batching fulled, flush it
		p.internalFlushCurrentBatch()

		// after flushing try again to add the current payload
		if ok := p.batchBuilder.Add(smm, sequenceID, msg.Payload, request,
			msg.ReplicationClusters, deliverAt); !ok {
			p.publishSemaphore.Release()
			request.callback(nil, request.msg, errFailAddBatch)
			p.log.WithField("size", len(msg.Payload)).
				WithField("sequenceID", sequenceID).
				WithField("properties", msg.Properties).
				Error("unable to add message to batch")
			return
		}
	}

	// NOTE: 2. if disabled batch, internal still using batch. Iit just batch ONE msg a time
	// NOTE: 3. if msg need delay, flush it with other batched msgs. It batch MANY msgs and trigger flush
	if !sendAsBatch || request.flushImmediately {
		p.internalFlushCurrentBatch()
	}
}

type pendingItem struct {
	sync.Mutex
	batchData    internal.Buffer
	sequenceID   uint64
	sendRequests []interface{}
	completed    bool
}

// NOTE: wrap batch and write to cnx, append to pendingQueue
func (p *partitionProducer) internalFlushCurrentBatch() {
	batchData, sequenceID, callbacks := p.batchBuilder.Flush()
	if batchData == nil {
		return
	}

	p.pendingQueue.Put(&pendingItem{
		batchData:    batchData,
		sequenceID:   sequenceID,
		sendRequests: callbacks,
	})
	p.cnx.WriteData(batchData)
}

func (p *partitionProducer) internalFlush(fr *flushRequest) {
	p.internalFlushCurrentBatch()

	pi, ok := p.pendingQueue.PeekLast().(*pendingItem)
	if !ok {
		// NOTE: pendingQueue is empty, called Flush(), but interval flushed already
		fr.waitGroup.Done()
		return
	}

	// lock the pending request while adding requests
	// since the ReceivedSendReceipt func iterates over this list
	pi.Lock()
	defer pi.Unlock()

	if pi.completed {
		// The last item in the queue has been completed while we were
		// looking at it. It's safe at this point to assume that every
		// message enqueued before Flush() was called are now persisted
		fr.waitGroup.Done()
		return
	}

	sendReq := &sendRequest{
		msg: nil,
		callback: func(id MessageID, message *ProducerMessage, e error) {
			fr.err = e
			fr.waitGroup.Done()
		},
	}

	// waiting for Flush() done:
	// NOTE: 1. appended a empty sendRequest to LAST pendingItem.sendRequests, so need lock itself at first
	// NOTE: 2. attached a wg to last callback
	pi.sendRequests = append(pi.sendRequests, sendReq)
}

func (p *partitionProducer) Send(ctx context.Context, msg *ProducerMessage) (MessageID, error) {
	wg := sync.WaitGroup{}
	wg.Add(1)

	var err error
	var msgID MessageID

	p.internalSendAsync(ctx, msg, func(ID MessageID, message *ProducerMessage, e error) {
		err = e
		msgID = ID
		wg.Done()
	}, true)

	wg.Wait()
	return msgID, err
}

func (p *partitionProducer) SendAsync(ctx context.Context, msg *ProducerMessage,
	callback func(MessageID, *ProducerMessage, error)) {
	p.internalSendAsync(ctx, msg, callback, false)
}

func (p *partitionProducer) internalSendAsync(ctx context.Context, msg *ProducerMessage,
	callback func(MessageID, *ProducerMessage, error), flushImmediately bool) {

	sr := &sendRequest{
		ctx:              ctx,
		msg:              msg,
		callback:         callback,
		flushImmediately: flushImmediately,
		publishTime:      time.Now(),
	}
	p.options.Interceptors.BeforeSend(p, msg)

	messagesPending.Inc()
	bytesPending.Add(float64(len(sr.msg.Payload)))

	p.publishSemaphore.Acquire()
	p.eventsChan <- sr
}

func (p *partitionProducer) ReceivedSendReceipt(response *pb.CommandSendReceipt) {
	// NOTE: get first enqueue batch msg
	pi, ok := p.pendingQueue.Peek().(*pendingItem)

	// NOTE: 1. received a non-exist receipt
	if !ok {
		// if we receive a receipt although the pending queue is empty, the state of the broker and the producer differs.
		// At that point, it is better to close the connection to the broker to reconnect to a broker hopping it solves
		// the state discrepancy.
		p.log.Warnf("Received ack for %v although the pending queue is empty, closing connection", response.GetMessageId())
		p.cnx.Close()
		return
	}

	// NOTE: 2. received non-peek receipt
	if pi.sequenceID != response.GetSequenceId() {
		// if we receive a receipt that is not the one expected, the state of the broker and the producer differs.
		// At that point, it is better to close the connection to the broker to reconnect to a broker hopping it solves
		// the state discrepancy.
		p.log.Warnf("Received ack for %v on sequenceId %v - expected: %v, closing connection", response.GetMessageId(),
			response.GetSequenceId(), pi.sequenceID)
		p.cnx.Close()
		return
	}

	// The ack was indeed for the expected item in the queue, we can remove it and trigger the callback
	p.pendingQueue.Poll()

	now := time.Now().UnixNano()

	// lock the pending item while sending the requests
	// NOTE: if called Flush() manually, lock will blocked here until pi.completed become true and exit
	pi.Lock()
	defer pi.Unlock()
	for idx, i := range pi.sendRequests {
		sr := i.(*sendRequest)
		if sr.msg != nil {
			atomic.StoreInt64(&p.lastSequenceID, int64(pi.sequenceID))
			p.publishSemaphore.Release()

			publishLatency.Observe(float64(now-sr.publishTime.UnixNano()) / 1.0e9)
			messagesPublished.Inc()
			messagesPending.Dec()
			payloadSize := float64(len(sr.msg.Payload))
			bytesPublished.Add(payloadSize)
			bytesPending.Sub(payloadSize)
		}

		if sr.callback != nil || len(p.options.Interceptors) > 0 {
			msgID := newMessageID(
				int64(response.MessageId.GetLedgerId()),
				int64(response.MessageId.GetEntryId()),
				int32(idx),
				p.partitionIdx,
			)

			if sr.callback != nil {
				// NOTE: exec callback for every pending msg
				sr.callback(msgID, sr.msg, nil)
			}

			p.options.Interceptors.OnSendAcknowledgement(p, sr.msg, msgID)
		}
	}

	// Mark this pending item as done
	pi.completed = true
	// Return buffer to the pool since we're now done using it
	buffersPool.Put(pi.batchData)
}

func (p *partitionProducer) internalClose(req *closeProducer) {
	defer req.waitGroup.Done()
	if !atomic.CompareAndSwapInt32(&p.state, producerReady, producerClosing) {
		return
	}

	p.log.Info("Closing producer")

	id := p.client.rpcClient.NewRequestID()
	_, err := p.client.rpcClient.RequestOnCnx(p.cnx, id, pb.BaseCommand_CLOSE_PRODUCER, &pb.CommandCloseProducer{
		ProducerId: &p.producerID,
		RequestId:  &id,
	})

	if err != nil {
		p.log.WithError(err).Warn("Failed to close producer")
	} else {
		p.log.Info("Closed producer")
	}

	if err = p.batchBuilder.Close(); err != nil {
		p.log.WithError(err).Warn("Failed to close batch builder")
	}

	atomic.StoreInt32(&p.state, producerClosed)
	p.cnx.UnregisterListener(p.producerID)
	p.batchFlushTicker.Stop()
}

func (p *partitionProducer) LastSequenceID() int64 {
	return atomic.LoadInt64(&p.lastSequenceID)
}

func (p *partitionProducer) Flush() error {
	wg := sync.WaitGroup{}
	wg.Add(1)

	cp := &flushRequest{&wg, nil}
	p.eventsChan <- cp

	wg.Wait()
	return cp.err
}

func (p *partitionProducer) Close() {
	if atomic.LoadInt32(&p.state) != producerReady {
		// Producer is closing
		return
	}

	wg := sync.WaitGroup{}
	wg.Add(1)

	cp := &closeProducer{&wg}
	p.eventsChan <- cp

	wg.Wait()
}

type sendRequest struct {
	ctx              context.Context
	msg              *ProducerMessage
	callback         func(MessageID, *ProducerMessage, error)
	publishTime      time.Time
	flushImmediately bool
}

type closeProducer struct {
	waitGroup *sync.WaitGroup
}

type flushRequest struct {
	waitGroup *sync.WaitGroup
	err       error
}
