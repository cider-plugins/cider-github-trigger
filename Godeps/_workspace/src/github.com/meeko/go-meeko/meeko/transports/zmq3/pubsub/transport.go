// Copyright (c) 2013 The go-meeko AUTHORS
//
// Use of this source code is governed by The MIT License
// that can be found in the LICENSE file.

package pubsub

import (
	// Stdlib
	"bytes"
	"encoding/binary"

	// Meeko
	"github.com/meeko/go-meeko/meeko/services"
	"github.com/meeko/go-meeko/meeko/services/pubsub"
	"github.com/meeko/go-meeko/meeko/transports/zmq3/loop"
	"github.com/meeko/go-meeko/meeko/utils/codecs"

	// Other
	log "github.com/cihub/seelog"
	"github.com/dmotylev/nutrition"
	zmq "github.com/pebbe/zmq3"
)

type TransportFactory struct {
	RouterEndpoint string
	DealerSndhwm   int
	DealerRcvhwm   int
	PubEndpoint    string
	SubRcvhwm      int
}

func NewTransportFactory() *TransportFactory {
	// Keep ZeroMQ defaults by default.
	return &TransportFactory{
		DealerSndhwm: 1000,
		DealerRcvhwm: 1000,
		SubRcvhwm:    1000,
	}
}

func (factory *TransportFactory) ReadConfigFromEnv(prefix string) error {
	return nutrition.Env(prefix).Feed(factory)
}

func (factory *TransportFactory) MustReadConfigFromEnv(prefix string) *TransportFactory {
	if err := factory.ReadConfigFromEnv(prefix); err != nil {
		panic(err)
	}
	return factory
}

func (factory *TransportFactory) IsFullyConfigured() error {
	if factory.RouterEndpoint == "" {
		return &services.ErrMissingConfig{"ROUTER endpoint", "ZeroMQ 3.x PubSub transport"}
	}
	if factory.PubEndpoint == "" {
		return &services.ErrMissingConfig{"PUB endpoint", "ZeroMQ 3.x PubSub transport"}
	}
	return nil
}

func (factory *TransportFactory) MustBeFullyConfigured() *TransportFactory {
	if err := factory.IsFullyConfigured(); err != nil {
		panic(err)
	}
	return factory
}

type Transport struct {
	// Internal channels
	cmdCh      chan *command
	routerCh   chan [][]byte
	abortCh    chan error
	closeCh    chan chan error
	closeAckCh chan struct{}

	// Output interface for the Service using this Transport.
	eventCh chan pubsub.Event
	tableCh chan pubsub.EventSeqTable
	errorCh chan error

	// Error to return from Wait.
	err error
}

func (factory *TransportFactory) NewTransport(identity string) (*Transport, error) {
	// Make sure the config is complete.
	if err := factory.IsFullyConfigured(); err != nil {
		return nil, err
	}

	// DEALER socket
	dealer, err := zmq.NewSocket(zmq.DEALER)
	if err != nil {
		return nil, err
	}

	if err := dealer.SetIdentity(identity); err != nil {
		dealer.Close()
		return nil, err
	}

	if err := dealer.SetSndhwm(factory.DealerSndhwm); err != nil {
		dealer.Close()
		return nil, err
	}

	if err := dealer.SetRcvhwm(factory.DealerRcvhwm); err != nil {
		dealer.Close()
		return nil, err
	}

	if err := dealer.Connect(factory.RouterEndpoint); err != nil {
		dealer.Close()
		return nil, err
	}

	// SUB socket
	sub, err := zmq.NewSocket(zmq.SUB)
	if err != nil {
		dealer.Close()
		return nil, err
	}

	if err := sub.SetRcvhwm(factory.SubRcvhwm); err != nil {
		dealer.Close()
		sub.Close()
		return nil, err
	}

	if err := sub.Connect(factory.PubEndpoint); err != nil {
		dealer.Close()
		sub.Close()
		return nil, err
	}

	// Transport
	t := &Transport{
		cmdCh:      make(chan *command, 1),
		routerCh:   make(chan [][]byte),
		abortCh:    make(chan error, 1),
		closeCh:    make(chan chan error),
		closeAckCh: make(chan struct{}),
		eventCh:    make(chan pubsub.Event),
		tableCh:    make(chan pubsub.EventSeqTable),
		errorCh:    make(chan error),
	}

	go t.loop(dealer, sub)
	return t, nil
}

// pubsub.Transport interface ----------------------------------------------------

const (
	cmdPublish int = iota
	cmdSubscribe
	cmdUnsubscribe
)

type publishArgs struct {
	eventKind   string
	eventObject interface{}
}

func (t *Transport) Publish(eventKind string, eventObject interface{}) error {
	log.Debug("zmq3<PubSub>: Publish called")
	return t.exec(cmdPublish, &publishArgs{eventKind, eventObject})
}

func (t *Transport) Subscribe(eventKindPrefix string) error {
	log.Debugf("zmq3<PubSub>: Subscribe called for %v", eventKindPrefix)
	return t.exec(cmdSubscribe, &eventKindPrefix)
}

func (t *Transport) Unsubscribe(eventKindPrefix string) error {
	log.Debugf("zmq3<PubSub>: Unsubscribe called for %v", eventKindPrefix)
	return t.exec(cmdUnsubscribe, &eventKindPrefix)
}

func (t *Transport) EventChan() <-chan pubsub.Event {
	return t.eventCh
}

func (t *Transport) EventSeqTableChan() <-chan pubsub.EventSeqTable {
	return t.tableCh
}

func (t *Transport) ErrorChan() <-chan error {
	return t.errorCh
}

func (t *Transport) Close() (err error) {
	errCh := make(chan error, 1)
	select {
	case t.closeCh <- errCh:
		err = <-errCh
	case <-t.Closed():
		err = &services.ErrTerminated{"ZeroMQ 3.x PubSub transport"}
	}
	return
}

func (t *Transport) Closed() <-chan struct{} {
	return t.closeAckCh
}

func (t *Transport) Wait() error {
	<-t.Closed()
	return t.err
}

// Internal command loop -------------------------------------------------------

type command struct {
	typ   int
	args  interface{}
	errCh chan error
}

func (cmd *command) Type() int {
	return cmd.typ
}

func (t *Transport) exec(cmdType int, cmdArgs interface{}) (err error) {
	errCh := make(chan error, 1)
	select {
	case t.cmdCh <- &command{cmdType, cmdArgs, errCh}:
		err = <-errCh
	case <-t.Closed():
		err = &services.ErrTerminated{"ZeroMQ 3.x PubSub transport"}
	}
	return
}

const (
	messageTypeEvent byte = iota
	messageTypeEventSeqTable
)

const maxMessageType = messageTypeEvent

var (
	frameEmpty  = []byte{}
	frameHeader = []byte("CDR#PUBSUB@01")

	frameEventType         = []byte{messageTypeEvent}
	frameEventSeqTableType = []byte{messageTypeEventSeqTable}
)

func (t *Transport) loop(dealer *zmq.Socket, sub *zmq.Socket) {
	items := loop.PollItems{
		{
			dealer,
			func(msg [][]byte) {
				// Make sure that the message valid.
				// Drop it if that is not the case.
				//
				// FRAME 0:        message header
				// FRAME 1:        message type
				// FRAME 2-(2k+2): event sequence numbers
				switch {
				case len(msg) < 2:
					log.Warn("zmq3<PubSub>: Message too short")
					return
				case !bytes.Equal(msg[0], frameHeader):
					log.Warn("zmq3<PubSub>: Invalid message header")
					return
				case !bytes.Equal(msg[1], frameEventSeqTableType):
					log.Warn("zmq3<PubSub>: Invalid message type")
					return
				case len(msg)%2 != 0:
					log.Warn("zmq3<PubSub>: Invalid message length")
					return
				}

				log.Debug("zmq3<PubSub>: SEQTABLE message received")

				table := make(map[string]pubsub.EventSeqNum, len(msg)-2)
				for i := 2; i < len(msg); i += 2 {
					// msg[i]   - event kind
					// msg[i+1] - event sequence number, uint32, BE
					var seq pubsub.EventSeqNum
					if err := binary.Read(bytes.NewReader(msg[i+1]), binary.BigEndian, &seq); err != nil {
						return
					}
					table[string(msg[i])] = seq
				}

				t.tableCh <- pubsub.EventSeqTable(table)
			},
		},
		{
			sub,
			func(msg [][]byte) {
				// Make sure that the message is a valid event.
				// Drop it if that is not the case.
				//
				// FRAME 0: event type (string)
				// FRAME 1: publisher (string)
				// FRAME 2: message header (string)
				// FRAME 3: message type (byte)
				// FRAME 4: event sequence number (uint32, BE)
				// FRAME 5: event object (bytes)
				switch {
				case len(msg) != 6:
					log.Warn("zmq3<PubSub>: Message dropped: event message too short")
					return
				case len(msg[0]) == 0:
					log.Warn("zmq3<PubSub>: Message dropped: event kind not set")
					return
				case len(msg[1]) == 0:
					log.Warn("zmq3<PubSub>: Message dropped: event publisher not set")
					return
				case !bytes.Equal(msg[2], frameHeader):
					log.Warn("zmq3<PubSub>: Message dropped: invalid message header")
					return
				case !bytes.Equal(msg[3], frameEventType):
					log.Warn("zmq3<PubSub>: Message dropped: invalid message type")
					return
				case len(msg[4]) != 4: // XXX: Hardcoded len, no good.
					log.Warn("zmq3<PubSub>: Message dropped: invalid event sequence number")
					return
				}

				log.Debug("zmq3<PubSub>: EVENT message received")

				// Forward the event to the next layer.
				t.eventCh <- newEvent(msg)
			},
		},
	}

	handlers := loop.CommandHandlers{
		cmdPublish: func(c loop.Cmd) {
			cmd := c.(*command)
			args := cmd.args.(*publishArgs)
			log.Debug("zmq3<PubSub>: Executing Publish")

			// This emits the only recoverable error so we don't call t.abort(err).
			var buf bytes.Buffer
			err := codecs.MessagePack.Encode(&buf, args.eventObject)
			if err != nil {
				cmd.errCh <- err
				return
			}
			// Publish the event by sending a message to the broker.
			if _, err = dealer.SendMessage([][]byte{
				[]byte(args.eventKind),
				frameHeader,
				frameEventType,
				frameEmpty,
				buf.Bytes(),
			}); err != nil {
				cmd.errCh <- err
				t.abort(err)
				return
			}
			cmd.errCh <- nil
		},
		cmdSubscribe: func(c loop.Cmd) {
			cmd := c.(*command)
			prefix := cmd.args.(*string)
			log.Debugf("zmq3<PubSub>: Executing Subscribe for %v", *prefix)

			if err := sub.SetSubscribe(*prefix); err != nil {
				cmd.errCh <- err
				t.abort(err)
				return
			}
			// Request Event Sequence Table for the specified kind prefix.
			if _, err := dealer.SendMessage([][]byte{
				[]byte(*prefix),
				frameHeader,
				frameEventSeqTableType,
			}); err != nil {
				cmd.errCh <- err
				t.abort(err)
				return
			}
			cmd.errCh <- nil
		},
		cmdUnsubscribe: func(c loop.Cmd) {
			cmd := c.(*command)
			prefix := cmd.args.(*string)
			log.Debugf("zmq3<PubSub>: Executing Unsubscribe for %v", *prefix)

			if err := sub.SetUnsubscribe(*prefix); err != nil {
				cmd.errCh <- err
				t.abort(err)
				return
			}
			cmd.errCh <- nil
		},
	}

	messageLoop, err := loop.New(items, handlers)
	if err != nil {
		// If we don't manage to create the internal message loop,
		// mimic t.abort() and return.
		t.err = err
		t.errorCh <- err
		close(t.errorCh)
		close(t.closeAckCh)
		return
	}

	for {
		select {
		case cmd := <-t.cmdCh:
			if err := messageLoop.PushCommand(cmd); err != nil {
				cmd.errCh <- err
				t.abort(err)
			}

		case errCh := <-t.closeCh:
			errCh <- nil
			t.abort(nil)

		case err := <-t.abortCh:
			if err != nil {
				// Set the error to be returned from Wait.
				t.err = err
				// Send the error to errChan so that the error is treated as unrecoverable.
				t.errorCh <- err
			}
			// Terminate the internal message loop.
			if err := messageLoop.Terminate(); err != nil {
				// This is an unrecoverable error.
				t.errorCh <- err
				// If no internal error was set, return the error from Terminate.
				if t.err == nil {
					t.err = err
				}
			}
			close(t.errorCh)
			close(t.closeAckCh)
			return
		}
	}
}

func (t *Transport) abort(err error) {
	// Make sure we don't send to t.abortCh twice.
	select {
	case <-t.Closed():
	default:
		t.abortCh <- err
	}
}
