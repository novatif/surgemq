// Copyright (c) 2014 The SurgeMQ Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package session

import (
	"sync"

	"container/list"
	"errors"
	"github.com/juju/loggo"
	"github.com/troian/surgemq/message"
	persistenceTypes "github.com/troian/surgemq/persistence/types"
	"github.com/troian/surgemq/systree"
	"github.com/troian/surgemq/topics"
	"github.com/troian/surgemq/types"
	"io"
	"sync/atomic"
)

type managerCallbacks struct {
	onClose      func(id string, s message.TopicsQoS)
	onDisconnect func(id string, messages *persistenceTypes.SessionMessages, suspend bool)
	onPublish    func(id string, msg *message.PublishMessage)
}

// Config is system wide configuration parameters for every session
type config struct {
	// Topics manager for all the client subscriptions
	topicsMgr *topics.Manager

	// The number of seconds to wait for the CONNACK message before disconnecting.
	// If not set then default to 2 seconds.
	connectTimeout int

	// The number of seconds to wait for any ACK messages before failing.
	// If not set then default to 20 seconds.
	ackTimeout int

	// The number of times to retry sending a packet if ACK is not received.
	// If no set then default to 3 retries.
	timeoutRetries int

	metric struct {
		packets systree.PacketsMetric
		session systree.SessionStat
	}

	subscriptions message.TopicsQoS

	callbacks managerCallbacks

	id string
}

type publisher struct {
	// signal publisher goroutine to exit on channel close
	quit chan struct{}
	// make sure writer started before exiting from Start()
	started sync.WaitGroup
	// make sure writer has finished before any finalization
	stopped sync.WaitGroup

	messages *list.List
	lock     sync.Mutex
	cond     *sync.Cond
}

// Type session
type Type struct {
	config config

	ack struct {
		pubIn  *ackQueue
		pubOut *ackQueue
	}

	// message to publish if connect is closed unexpectedly
	will *message.PublishMessage

	conn *connection

	subscriber types.Subscriber

	// Serialize access to this session
	mu sync.Mutex

	wgSessionStarted sync.WaitGroup
	wgSessionStopped sync.WaitGroup

	publisher publisher

	retained struct {
		lock sync.Mutex
		list []*message.PublishMessage
	}

	// Whether this is service is closed or not.
	running int64
	closed  int64

	clean bool

	packetID uint64
}

var appLog loggo.Logger

func init() {
	appLog = loggo.GetLogger("mq.session")
	appLog.SetLogLevel(loggo.INFO)
}

func newSession(config config) (*Type, error) {
	s := Type{
		config: config,
		publisher: publisher{
			messages: list.New(),
		},
	}

	s.publisher.cond = sync.NewCond(&s.publisher.lock)

	s.ack.pubIn = newAckQueue(s.onAckIn)
	s.ack.pubOut = newAckQueue(s.onAckOut)
	s.subscriber.Publish = s.onSubscribedPublish

	// restore subscriptions if any
	for t, q := range s.config.subscriptions {
		if _, err := s.config.topicsMgr.Subscribe(t, q, &s.subscriber); err != nil {
			appLog.Errorf("Couldn't subscribe [%s] to [%s/%d]: %s", s.config.id, t, q, err.Error())
		}
	}

	return &s, nil
}

// restore messages if any
func (s *Type) restore(messages *persistenceTypes.SessionMessages) {
	if messages != nil {
		s.publisher.lock.Lock()
		for _, m := range messages.Out.Messages {
			s.publisher.messages.PushBack(m)
		}

		for _, m := range messages.In.Messages {
			s.ack.pubIn.put(m)
		}
		s.publisher.lock.Unlock()
		s.publisher.cond.Signal()
	}
}

// Start inform session there is a new connection with matching clientID
// thus provide necessary info to spin
func (s *Type) start(msg *message.ConnectMessage, conn io.Closer) (err error) {
	defer func() {
		if err != nil {
			close(s.publisher.quit)
			s.conn = nil
			atomic.StoreInt64(&s.running, 0)
		}
		s.wgSessionStarted.Done()
	}()

	// In case we call start for stored session onClose might not finished
	// it's work thus lets wait until it completed
	s.wgSessionStopped.Wait()

	if !atomic.CompareAndSwapInt64(&s.running, 0, 1) {
		return errors.New("Already running")
	}

	s.wgSessionStarted.Add(1)
	s.wgSessionStopped.Add(1)

	if msg.WillFlag() {
		s.will = message.NewPublishMessage()
		s.will.SetQoS(msg.WillQos())     // nolint: errcheck
		s.will.SetTopic(msg.WillTopic()) // nolint: errcheck
		s.will.SetPayload(msg.WillMessage())
		s.will.SetRetain(msg.WillRetain())
	}

	s.clean = msg.CleanSession()
	s.publisher.quit = make(chan struct{})

	s.conn, err = newConnection(
		connConfig{
			id:        s.config.id,
			conn:      conn,
			keepAlive: int(msg.KeepAlive()),
			on: onProcess{
				publish:     s.onPublish,
				ack:         s.onAck,
				subscribe:   s.onSubscribe,
				unSubscribe: s.onUnSubscribe,
				close:       s.onClose,
			},
			packetsMetric: s.config.metric.packets,
		})
	if err != nil {
		return err
	}

	s.conn.start()

	s.publisher.stopped.Add(1)
	s.publisher.started.Add(1)
	go s.publishWorker()
	s.publisher.started.Wait()

	return nil
}

// stop session. Function assumed to be invoked once server about to shutdown
func (s *Type) stop() {
	if !atomic.CompareAndSwapInt64(&s.closed, 0, 1) {
		s.wgSessionStarted.Wait()
		return
	}

	// If Stop has been issued by the server handler it looks like
	// application about to shutdown, thus we try close network connection.
	// If close successful connection manager invokes onClose method which cleans up writer.
	// If close error just check writer goroutine has finished it's job
	var err error
	if s.conn != nil {
		if err = s.conn.config.conn.Close(); err != nil {
			appLog.Errorf("Couldn't close connection [%s]: %s", s.config.id, err.Error())
		}
	}
	s.wgSessionStopped.Wait()

	if !s.clean {
		s.config.callbacks.onClose(s.config.id, s.config.subscriptions)
	}
}

// onSubscribedPublish is the method that gets added to the topic subscribers list by the
// processSubscribe() method. When the server finishes the ack cycle for a
// PUBLISH message, it will call the subscriber, which is this method.
//
// For the server, when this method is called, it means there's a message that
// should be published to the client on the other end of this connection. So we
// will call publish() to send the message.
func (s *Type) onSubscribedPublish(msg *message.PublishMessage) error {
	m := message.NewPublishMessage()
	m.SetQoS(msg.QoS())     // nolint: errcheck
	m.SetTopic(msg.Topic()) // nolint: errcheck
	m.SetPayload(msg.Payload())

	// [MQTT-3.3.1-9]
	m.SetRetain(false)

	// [MQTT-3.3.1-3]
	m.SetDup(false)

	// If this is Fire and Forget firstly check is client online
	if msg.QoS() == message.QosAtMostOnce {
		// By checking s.publisher.quit channel we can effectively detect is client is connected or not
		select {
		case <-s.publisher.quit:
			s.publisher.lock.Lock()
			s.config.callbacks.onPublish(s.config.id, m)
			s.publisher.lock.Unlock()
			return nil
		default:
		}
	}

	s.publisher.lock.Lock()
	s.publisher.messages.PushBack(m)
	s.publisher.lock.Unlock()
	s.publisher.cond.Signal()

	return nil
}

// AddTopic add topic
func (s *Type) addTopic(topic string, qos message.QosType) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.config.subscriptions[topic] = qos

	return nil
}

// RemoveTopic remove
func (s *Type) removeTopic(topic string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.config.subscriptions, topic)

	return nil
}

func (s *Type) newPacketID() uint16 {
	return uint16(atomic.AddUint64(&s.packetID, 1) & 0xFFFF)
}

// forward PUBLISH message to topics manager which takes care about subscribers
func (s *Type) publishToTopic(msg *message.PublishMessage) error {
	// [MQTT-3.3.1.3]
	if msg.Retain() {
		if err := s.config.topicsMgr.Retain(msg); err != nil {
			appLog.Errorf("Error retaining message [%s]: %v", s.config.id, err)
		}

		// [MQTT-3.3.1-7]
		if msg.QoS() == message.QosAtMostOnce {
			m := message.NewPublishMessage()
			m.SetQoS(msg.QoS())     // nolint: errcheck
			m.SetTopic(msg.Topic()) // nolint: errcheck

			s.retained.lock.Lock()
			s.retained.list = append(s.retained.list, m)
			s.retained.lock.Unlock()
		}
	}

	msg.SetRetain(false)

	if err := s.config.topicsMgr.Publish(msg); err != nil {
		appLog.Errorf(" Error retrieving subscribers list [%s]: %v", s.config.id, err)
	}

	return nil
}

// onAckIn ack process for incoming messages
func (s *Type) onAckIn(msg message.Provider, status error) {
	switch m := msg.(type) {
	case *message.PublishMessage:
		s.publishToTopic(m) // nolint: errcheck
	}
}

// onAckComplete process messages that required ack cycle
// onAckTimeout if publish message has not been acknowledged withing specified ackTimeout
// server should mark it as a dup and send again
func (s *Type) onAckOut(msg message.Provider, status error) {
	//if status == errAckTimedOut {
	//	appLog.Errorf("[%s] message not acknoweledged: %s", s.id, status.Error())
	//
	//	switch m := msg.(type) {
	//	case *message.PublishMessage:
	//		// if ack for QoS 1 or 2 has been timed out put this message back to the publish queue
	//		if m.QoS() == message.QosAtLeastOnce {
	//			m.SetDup(true)
	//		}
	//		s.publisher.lock.Lock()
	//		s.publisher.messages.PushBack(msg)
	//		s.publisher.lock.Unlock()
	//		s.publisher.cond.Signal()
	//	case *message.PubRelMessage:
	//	}
	//}
}

// publishWorker publish messages coming from subscribed topics
func (s *Type) publishWorker() {
	defer func() {
		s.publisher.lock.Lock()
		var next *list.Element
		for elem := s.publisher.messages.Front(); elem != nil; elem = next {
			next = elem.Next()
			switch m := elem.Value.(type) {
			case *message.PublishMessage:
				if m.QoS() == message.QosAtMostOnce {
					s.publisher.messages.Remove(elem)
				}
			}
		}
		s.publisher.lock.Unlock()
		s.publisher.stopped.Done()

		if r := recover(); r != nil {
			appLog.Errorf("Recover from panic: %v", r)
		}
	}()

	s.publisher.started.Done()

	for {
		if s.publisher.isDone() {
			return
		}

		s.publisher.cond.L.Lock()
		for s.publisher.messages.Len() == 0 {
			s.publisher.cond.Wait()
			if s.publisher.isDone() {
				s.publisher.cond.L.Unlock()
				return
			}
		}

		var msg message.Provider

		elem := s.publisher.messages.Front()
		msg = elem.Value.(message.Provider)
		s.publisher.messages.Remove(elem)
		s.publisher.cond.L.Unlock()

		if msg != nil {
			switch m := msg.(type) {
			case *message.PubRelMessage:
				s.ack.pubOut.put(msg)
			case *message.PublishMessage:
				switch m.QoS() {
				case message.QosAtLeastOnce:
					fallthrough
				case message.QosExactlyOnce:
					if m.PacketID() == 0 {
						m.SetPacketID(s.newPacketID())
					}
					s.ack.pubOut.put(msg)
				}
			}

			if _, err := s.conn.writeMessage(msg); err != nil {
				switch m := msg.(type) {
				case *message.PubRelMessage:
					s.ack.pubOut.ack(msg) // nolint: errcheck
				case *message.PublishMessage:
					switch m.QoS() {
					case message.QosAtLeastOnce:
						fallthrough
					case message.QosExactlyOnce:
						m.SetDup(true)
						s.ack.pubOut.ack(msg) // nolint: errcheck
					}
				}

				// Couldn't deliver message to client thus requeue it back
				s.publisher.cond.L.Lock()
				s.publisher.messages.PushBack(msg)
				s.publisher.cond.L.Unlock()
				return
			}
		}
	}
}

func (p *publisher) isDone() bool {
	select {
	case <-p.quit:
		return true
	default:
	}

	return false
}