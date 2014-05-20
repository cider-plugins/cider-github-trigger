// Copyright (c) 2013 The go-meeko AUTHORS
//
// Use of this source code is governed by The MIT License
// that can be found in the LICENSE file.

package rpc

import (
	// Stdlib
	"bytes"
	"encoding/binary"
	"errors"
	"sync"

	// Meeko
	"github.com/meeko/go-meeko/meeko/services"
	"github.com/meeko/go-meeko/meeko/services/rpc"
	"github.com/meeko/go-meeko/meeko/transports/zmq3/loop"
	"github.com/meeko/go-meeko/meeko/utils/codecs"

	// Other
	log "github.com/cihub/seelog"
	"github.com/dmotylev/nutrition"
	zmq "github.com/pebbe/zmq3"
)

// Command receive channel must be buffered to break a circle or unbuffered
// channels that would deadlock. Since all the loops are using select, and thus
// randomly picking messages, the channel must be large enough to fight the bad
// luck of the event loop sending to the channel subsequently without receiving
// from other channels.
const CommandChannelBufferSize = 1000

type TransportFactory struct {
	Endpoint string
	Sndhwm   int
	Rcvhwm   int
}

func NewTransportFactory() *TransportFactory {
	// Keep ZeroMQ defaults by default.
	return &TransportFactory{
		Sndhwm: 1000,
		Rcvhwm: 1000,
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
	if factory.Endpoint == "" {
		return &services.ErrMissingConfig{"ROUTER endpoint", "ZeroMQ 3.x RPC transport"}
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
	// Requests that are being handled
	incomingRequests map[string]*remoteRequest
	requestsMu       *sync.Mutex

	// Internal control channels
	cmdCh    chan rpc.Command
	closedCh chan struct{}

	// Error to be returned from Wait
	err error

	// Output interface for the Service using this Transport
	requestCh   chan rpc.RemoteRequest
	progressCh  chan rpc.RequestID
	streamingCh chan rpc.StreamFrame
	replyCh     chan rpc.RemoteCallReply
	errorCh     chan error
}

func (factory *TransportFactory) NewTransport(identity string) (rpc.Transport, error) {
	// Make sure the config is complete.
	factory.MustBeFullyConfigured()

	// Set up the 0MQ socket.
	dealer, err := zmq.NewSocket(zmq.DEALER)
	if err != nil {
		return nil, err
	}

	if err := dealer.SetIdentity(identity); err != nil {
		dealer.Close()
		return nil, err
	}

	if err := dealer.SetSndhwm(factory.Sndhwm); err != nil {
		dealer.Close()
		return nil, err
	}

	if err := dealer.SetRcvhwm(factory.Rcvhwm); err != nil {
		dealer.Close()
		return nil, err
	}

	if err := dealer.Connect(factory.Endpoint); err != nil {
		dealer.Close()
		return nil, err
	}

	// Construct Transport.
	t := &Transport{
		incomingRequests: make(map[string]*remoteRequest),
		requestsMu:       new(sync.Mutex),
		cmdCh:            make(chan rpc.Command, CommandChannelBufferSize),
		closedCh:         make(chan struct{}),
		requestCh:        make(chan rpc.RemoteRequest),
		progressCh:       make(chan rpc.RequestID),
		streamingCh:      make(chan rpc.StreamFrame),
		replyCh:          make(chan rpc.RemoteCallReply),
		errorCh:          make(chan error),
	}

	go t.loop(dealer)
	return t, nil
}

// rpc.Transport interface -----------------------------------------------------

func (t *Transport) RegisterMethod(cmd rpc.RegisterCmd) {
	log.Debugf("zmq3<RPC>: registering %q", cmd.Method())
	t.exec(cmd)
}

func (t *Transport) UnregisterMethod(cmd rpc.UnregisterCmd) {
	log.Debugf("zmq3<RPC>: unregistering %q", cmd.Method())
	t.exec(cmd)
}

func (t *Transport) RequestChan() <-chan rpc.RemoteRequest {
	return t.requestCh
}

func (t *Transport) Call(cmd rpc.CallCmd) {
	log.Debugf("zmq3<RPC>: calling %q", cmd.Method())
	t.exec(cmd)
}

func (t *Transport) Interrupt(cmd rpc.InterruptCmd) {
	log.Debugf("zmq3<RPC>: interrupting %v", cmd.TargetRequestId())
	t.exec(cmd)
}

func (t *Transport) ProgressChan() <-chan rpc.RequestID {
	return t.progressCh
}

func (t *Transport) StreamFrameChan() <-chan rpc.StreamFrame {
	return t.streamingCh
}

func (t *Transport) ReplyChan() <-chan rpc.RemoteCallReply {
	return t.replyCh
}

func (t *Transport) ErrorChan() <-chan error {
	return t.errorCh
}

type closeCmd struct {
	err   error
	errCh chan error
}

func (cmd *closeCmd) Type() int {
	return rpc.CmdClose
}

func (cmd *closeCmd) ErrorChan() chan<- error {
	return cmd.errCh
}

func (t *Transport) Close() error {
	errCh := make(chan error, 1)
	t.exec(&closeCmd{nil, errCh})
	if err := <-errCh; err != nil {
		return err
	}

	t.Wait()
	return nil
}

func (t *Transport) Closed() <-chan struct{} {
	return t.closedCh
}

func (t *Transport) Wait() error {
	<-t.Closed()
	return t.err
}

// Internal command loop -------------------------------------------------------

func (t *Transport) exec(cmd rpc.Command) {
	select {
	case t.cmdCh <- cmd:
	case <-t.Closed():
		cmd.ErrorChan() <- ErrTerminated
	}
}

func (t *Transport) abort(err error) {
	go t.exec(&closeCmd{err, make(chan error, 1)})
}

const Header = "CDR#RPC@01"

const (
	MessageTypeRegister byte = iota
	MessageTypeUnregister
	MessageTypeRequest
	MessageTypeInterrupt
	MessageTypeProgress
	MessageTypeStreamFrame
	MessageTypeReply
	MessageTypePing
	MessageTypePong
	MessageTypeKthxbye
)

var (
	frameEmpty  = []byte{}
	frameHeader = []byte(Header)

	frameRegisterMT    = []byte{MessageTypeRegister}
	frameUnregisterMT  = []byte{MessageTypeUnregister}
	frameRequestMT     = []byte{MessageTypeRequest}
	frameInterruptMT   = []byte{MessageTypeInterrupt}
	frameProgressMT    = []byte{MessageTypeProgress}
	frameStreamFrameMT = []byte{MessageTypeStreamFrame}
	frameReplyMT       = []byte{MessageTypeReply}
	framePongMT        = []byte{MessageTypePong}
	frameKthxbyeMT     = []byte{MessageTypeKthxbye}
)

var pongMessage = [][]byte{
	frameEmpty,
	frameHeader,
	framePongMT,
}

var kthxbyeMessage = [][]byte{
	frameEmpty,
	frameHeader,
	frameKthxbyeMT,
}

func (t *Transport) loop(dealer *zmq.Socket) {
	items := loop.PollItems{
		{
			dealer,
			func(msg [][]byte) {
				// Check the message header frames.
				//
				// FRAME 0: empty or sender (string)
				// FRAME 1: message header (string)
				// FRAME 2: message type (byte)
				switch {
				case len(msg) < 3:
					log.Warn("zmq3<RPC>: Message too short")
					return
				case !bytes.Equal(msg[1], frameHeader):
					log.Warn("zmq3<RPC>: Invalid message header")
					return
				case len(msg[2]) != 1:
					log.Warn("zmq3<RPC>: Invalid message type")
					return
				}

				// Process the message depending on the message type.
				switch msg[2][0] {
				case MessageTypeRequest:
					// FRAME 0: sender
					// FRAME 3: request ID (uint16; BE)
					// FRAME 4: method (string)
					// FRAME 5: method arguments (object; encoded with MessagePack)
					// FRAME 6: stdout stream tag (empty or uint16; BE)
					// FRAME 7: stderr stream tag (empty of uint16; BE)
					switch {
					case len(msg) != 8:
						log.Warn("zmq3<RPC>: REQUEST: invalid message length")
						return
					case len(msg[0]) == 0:
						log.Warn("zmq3<RPC>: REQUEST: empty sender frame received")
						return
					case len(msg[3]) != 2:
						log.Warn("zmq3<RPC>: REQUEST: invalid request ID frame received")
						return
					case len(msg[4]) == 0:
						log.Warn("zmq3<RPC>: REQUEST: empty method frame")
						return
					case len(msg[6]) != 0 && len(msg[6]) != 2:
						log.Warn("zmq3<RPC>: REQUEST: invalid stdout tag frame received")
						return
					case len(msg[7]) != 0 && len(msg[7]) != 2:
						log.Warn("zmq3<RPC>: REQUEST: invalid stdout tag frame received")
						return
					}

					req, err := t.newRequest(msg)
					if err != nil {
						log.Warnf("zmq3<RPC>: REQUEST: %v", err)
						return
					}
					t.requestCh <- req

				case MessageTypeInterrupt:
					// FRAME 0: sender (string)
					// FRAME 3: request ID (uint16; BE)
					switch {
					case len(msg) != 4:
						log.Warn("zmq3<RPC>: INTERRUPT: invalid message length")
						return
					case len(msg[0]) == 0:
						log.Warn("zmq3<RPC>: INTERRUPT: empty sender frame received")
						return
					case len(msg[3]) != 2:
						log.Warn("zmq3<RPC>: INTERRUPT: invalid request ID frame received")
						return
					}

					key := string(append(msg[0], msg[3]...))
					t.requestsMu.Lock()
					request, ok := t.incomingRequests[key]
					if !ok {
						log.Warnf("zmq3<RPC>: INTERRUPT: unknown request ID received: %q", key)
						t.requestsMu.Unlock()
						return
					}

					request.interrupt()
					delete(t.incomingRequests, key)
					t.requestsMu.Unlock()

				case MessageTypeProgress:
					// FRAME 0: empty
					// FRAME 3: request ID (uint16; BE)
					switch {
					case len(msg) != 4:
						log.Warn("zmq3<RPC>: PROGRESS: invalid message length")
						return
					case len(msg[0]) != 0:
						log.Warn("zmq3<RPC>: PROGRESS: empty sender frame expected")
						return
					case len(msg[3]) != 2:
						log.Warn("zmq3<RPC>: PROGRESS: invalid request ID frame received")
						return
					}

					var id rpc.RequestID
					binary.Read(bytes.NewReader(msg[3]), binary.BigEndian, &id)
					t.progressCh <- id

				case MessageTypeStreamFrame:
					// FRAME 0: empty
					// FRAME 3: stream tag (uint16; BE)
					// FRAME 4: frame payload (bytes)
					switch {
					case len(msg) != 5:
						log.Warn("zmq3<RPC>: STREAMFRAME: invalid message length")
						return
					case len(msg[0]) != 0:
						log.Warn("zmq3<RPC>: STREAMFRAME: empty sender frame expected")
						return
					case len(msg[3]) != 2:
						log.Warn("zmq3<RPC>: STREAMFRAME: invalid stream tag frame received")
						return
					case len(msg[4]) == 0:
						log.Warn("zmq3<RPC>: STREAMFRAME: empty frame received")
						return
					}

					t.streamingCh <- newStreamFrame(msg)

				case MessageTypeReply:
					// FRAME 0: empty
					// FRAME 3: request ID (uint16; BE)
					// FRAME 4: return code (byte)
					// FRAME 5: return value (object; encoded with MessagePack)
					switch {
					case len(msg) != 6:
						log.Warn("zmq3<RPC>: REPLY: invalid message length")
						return
					case len(msg[0]) != 0:
						log.Warn("zmq3<RPC>: REPLY: empty sender frame expected")
						return
					case len(msg[3]) != 2:
						log.Warn("zmq3<RPC>: REPLY: invalid request ID frame received")
						return
					case len(msg[4]) != 1:
						log.Warn("zmq3<RPC>: REPLY: invalid return code frame received")
						return
					}

					t.replyCh <- newReply(msg)

				case MessageTypePing:
					// FRAME 0: empty
					// no additional payload
					if _, err := dealer.SendMessage(pongMessage); err != nil {
						t.abort(err)
					}

				default:
					log.Warn("zmq3<RPC>: Unknown message type received")
				}
			},
		},
	}

	handlers := loop.CommandHandlers{
		rpc.CmdRegister: func(c loop.Cmd) {
			cmd := c.(rpc.RegisterCmd)
			log.Debugf("zmq3<RPC>: sending REGISTER for method %q", cmd.Method())

			// Register the method by sending a message to the broker.
			if _, err := dealer.SendMessage([][]byte{
				frameEmpty,
				frameHeader,
				frameRegisterMT,
				[]byte(cmd.Method()),
			}); err != nil {
				cmd.ErrorChan() <- err
				t.abort(err)
				return
			}
			cmd.ErrorChan() <- nil
		},
		rpc.CmdUnregister: func(c loop.Cmd) {
			cmd := c.(rpc.UnregisterCmd)
			log.Debugf("zmq3<RPC>: seinding UNREGISTER for method %q", cmd.Method())

			// Unregister the method by sending a message to the broker.
			if _, err := dealer.SendMessage([][]byte{
				frameEmpty,
				frameHeader,
				frameUnregisterMT,
				[]byte(cmd.Method()),
			}); err != nil {
				cmd.ErrorChan() <- err
				t.abort(err)
				return
			}
			cmd.ErrorChan() <- nil
		},
		rpc.CmdCall: func(c loop.Cmd) {
			cmd := c.(rpc.CallCmd)
			log.Debugf("zmq3<RPC>: sending REQUEST for method %q", cmd.Method())

			// Marshal request ID.
			var idBuffer bytes.Buffer
			binary.Write(&idBuffer, binary.BigEndian, cmd.RequestId())

			// Marshal arguments.
			var argsBuffer bytes.Buffer
			if err := codecs.MessagePack.Encode(&argsBuffer, cmd.Args()); err != nil {
				cmd.ErrorChan() <- err
				return
			}

			// Marshal stdout tag.
			var stdoutTagBuffer bytes.Buffer
			if tag := cmd.StdoutTag(); tag != nil {
				binary.Write(&stdoutTagBuffer, binary.BigEndian, *tag)
			}

			// Marshal stderr tag.
			var stderrTagBuffer bytes.Buffer
			if tag := cmd.StderrTag(); tag != nil {
				binary.Write(&stderrTagBuffer, binary.BigEndian, *tag)
			}

			// Send the request to the broker.
			if _, err := dealer.SendMessage([][]byte{
				frameEmpty,
				frameHeader,
				frameRequestMT,
				idBuffer.Bytes(),
				[]byte(cmd.Method()),
				argsBuffer.Bytes(),
				stdoutTagBuffer.Bytes(),
				stderrTagBuffer.Bytes(),
			}); err != nil {
				cmd.ErrorChan() <- err
				t.abort(err)
				return
			}
			cmd.ErrorChan() <- nil
		},
		rpc.CmdInterrupt: func(c loop.Cmd) {
			cmd := c.(rpc.InterruptCmd)
			log.Debugf("zmq3<RPC>: sending INTERRUPT for %v", cmd.TargetRequestId())

			// Marshal request ID.
			var idBuffer bytes.Buffer
			binary.Write(&idBuffer, binary.BigEndian, cmd.TargetRequestId())

			// Send the interrupt to the broker.
			if _, err := dealer.SendMessage([][]byte{
				frameEmpty,
				frameHeader,
				frameInterruptMT,
				idBuffer.Bytes(),
			}); err != nil {
				cmd.ErrorChan() <- err
				t.abort(err)
				return
			}
			cmd.ErrorChan() <- nil
		},
		rpc.CmdSignalProgress: func(c loop.Cmd) {
			cmd := c.(*signalProgressCmd)
			log.Debug("zmq3<RPC>: sending PROGRESS")

			// Send the progress signal to the broker.
			if _, err := dealer.SendMessage(cmd.msg); err != nil {
				cmd.errCh <- err
				t.abort(err)
				return
			}
			cmd.errCh <- nil
		},
		rpc.CmdSendStreamFrame: func(c loop.Cmd) {
			cmd := c.(*sendStreamFrameCmd)
			log.Debug("zmq3<RPC>: sending STREAM_FRAME")

			// Send the stream frame to the broker.
			if _, err := dealer.SendMessage(cmd.msg); err != nil {
				cmd.errCh <- err
				t.abort(err)
				return
			}
			cmd.errCh <- nil
		},
		rpc.CmdReply: func(c loop.Cmd) {
			cmd := c.(*replyCmd)
			log.Debug("zmq3<RPC>: sending REPLY")

			// Send the reply to the broker.
			if _, err := dealer.SendMessage(cmd.msg); err != nil {
				cmd.errCh <- err
				t.abort(err)
				return
			}
			cmd.errCh <- nil
		},
		rpc.CmdClose: func(c loop.Cmd) {
			cmd := c.(rpc.Command)
			log.Debug("zmq3<RPC>: sending KTHXBYE")

			// Send KTHXBYE to the broker.
			_, err := dealer.SendMessage(kthxbyeMessage)
			cmd.ErrorChan() <- err
		},
	}

	messageLoop, err := loop.New(items, handlers)
	if err != nil {
		// Terminate immediately if we don't manage to create
		// the internal message loop.
		t.err = err
		t.errorCh <- err
		close(t.errorCh)
		close(t.closedCh)
		return
	}

	for {
		cmd := <-t.cmdCh

		if cmd.Type() == rpc.CmdClose {
			if err := cmd.(*closeCmd).err; err != nil {
				// Set the error to be returned from Wait.
				t.err = err
				// Send the error to errChan so that the error is treated as unrecoverable.
				t.errorCh <- err
			}

			// Notify the broker. Sending of KTHXBYE is working in a best-efford way.
			// Error on sending is ignored, and if it fails, the heartbeat will timeout
			// on the broker and everything will be fine.
			if err := messageLoop.PushCommand(cmd); err != nil {
				if t.err == nil {
					t.err = err
				}
				cmd.ErrorChan() <- err
				continue
			}

			// Terminate the internal message loop now that KTHXBYE has been sent.
			if err := messageLoop.Terminate(); err != nil {
				if t.err == nil {
					t.err = err
				}
				cmd.ErrorChan() <- err
				continue
			}

			cmd.ErrorChan() <- nil
			close(t.errorCh)
			close(t.closedCh)
			return
		}

		if cmd.Type() == rpc.CmdReply {
			// Free resources connected to the request being resolved.
			repCmd := cmd.(*replyCmd)
			msg := repCmd.msg
			key := string(append(msg[0], msg[3]...))
			t.requestsMu.Lock()
			delete(t.incomingRequests, key)
			t.requestsMu.Unlock()
		}

		// Other command types are to be forwarded into the message loop.
		if err := messageLoop.PushCommand(cmd); err != nil {
			cmd.ErrorChan() <- err
			t.abort(err)
		}
	}
}

func (t *Transport) newRequest(msg [][]byte) (*remoteRequest, error) {
	key := string(append(msg[0], msg[3]...))
	t.requestsMu.Lock()
	if _, ok := t.incomingRequests[key]; ok {
		t.requestsMu.Unlock()
		return nil, ErrDuplicateRequest
	}

	req := newRequest(t, msg)
	t.incomingRequests[key] = req
	t.requestsMu.Unlock()
	return req, nil
}

// Errors ----------------------------------------------------------------------

var (
	ErrDuplicateRequest = errors.New("duplicate request ID")
	ErrTerminated       = &services.ErrTerminated{"ZeroMQ 3.x RPC transport"}
)
