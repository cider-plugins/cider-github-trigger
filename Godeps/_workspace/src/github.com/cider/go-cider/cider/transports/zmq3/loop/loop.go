// Copyright (c) 2013 The go-cider AUTHORS
//
// Use of this source code is governed by The MIT License
// that can be found in the LICENSE file.

package loop

import (
	"crypto/rand"
	"errors"

	log "github.com/cihub/seelog"
	zmq "github.com/pebbe/zmq3"
)

//------------------------------------------------------------------------------
// MessageLoop
//------------------------------------------------------------------------------

type MessageHandler func(msg [][]byte)

type PollItem struct {
	Socket   *zmq.Socket
	Callback MessageHandler
}

type PollItems []PollItem

type Cmd interface {
	Type() int
}

type (
	CommandHandler  func(cmd Cmd)
	CommandHandlers []CommandHandler
)

// MessageLoop is NOT THREAD-SAFE. It is supposed to be used from within
// another object that handles the synchronization.
type MessageLoop struct {
	cmdCh     chan Cmd
	intPipe   *zmq.Socket
	termCh    chan bool
	termAckCh chan error
}

func New(items PollItems, handlers CommandHandlers) (*MessageLoop, error) {
	if len(items) == 0 {
		panic(ErrEmptyPollItems)
	}
	if len(items) > 255 {
		panic(ErrTooManyPollItems)
	}

	ep, err := randomInprocEndpoint()
	if err != nil {
		return nil, err
	}

	// Interrupt pipe - receiver
	intPipeRecv, err := zmq.NewSocket(zmq.PAIR)
	if err != nil {
		return nil, err
	}

	if err := intPipeRecv.Bind(ep); err != nil {
		intPipeRecv.Close()
		return nil, err
	}

	// Interrupt pipe - sender
	intPipeSend, err := zmq.NewSocket(zmq.PAIR)
	if err != nil {
		intPipeRecv.Close()
		return nil, err
	}

	if err := intPipeSend.Connect(ep); err != nil {
		intPipeSend.Close()
		intPipeRecv.Close()
		return nil, err
	}

	msgLoop := &MessageLoop{
		cmdCh:     make(chan Cmd, 1),
		intPipe:   intPipeSend,
		termCh:    make(chan bool, 1),
		termAckCh: make(chan error, 1),
	}

	go msgLoop.loop(items, handlers, intPipeRecv)
	return msgLoop, nil
}

func (ml *MessageLoop) loop(items PollItems, handlers CommandHandlers, intPipe *zmq.Socket) {
	// Set up the poller.
	poller := zmq.NewPoller()
	poller.Add(intPipe, zmq.POLLIN)
	for _, item := range items {
		poller.Add(item.Socket, zmq.POLLIN)
	}

	// Start processing messages.
	var (
		polled []zmq.Polled
		msg    [][]byte
		err    error
	)
LOOP:
	for {
		polled, err = poller.PollAll(-1)
		if err != nil {
			if err.Error() == "interrupted system call" {
				log.Debug("zmq3: Ignoring EINTR ...")
				continue
			}
			log.Errorf("zmq3: Message loop crashed with error=%v", err)
			break LOOP
		}

		// Interrupt received, check the channels.
		if polled[0].Events != 0 {
			// Read the interrupt from the interrupt socket.
			_, err = polled[0].Socket.RecvBytes(0)
			if err != nil {
				break LOOP
			}

			// Process the enqueued command.
			select {
			case cmd := <-ml.cmdCh:
				handlers[cmd.Type()](cmd)
			default:
				select {
				case <-ml.termCh:
					break LOOP
				}
			}
		}

		// Check the user poll items.
		for i := 1; i < len(polled); i++ {
			if polled[i].Events != 0 {
				msg, err = items[i-1].Socket.RecvMessageBytes(0)
				if err != nil {
					break LOOP
				}

				items[i-1].Callback(msg)
			}
		}
	}

	intPipe.Close()
	for _, item := range items {
		item.Socket.Close()
	}

	ml.termAckCh <- err
	close(ml.termAckCh)
}

var intFrame = []byte{0}

func (ml *MessageLoop) PushCommand(cmd Cmd) error {
	// XXX: This can theoretically block forever if the loop is used badly.
	ml.cmdCh <- cmd
	_, err := ml.intPipe.SendBytes(intFrame, 0)
	return err
}

func (ml *MessageLoop) Terminate() error {
	select {
	case ml.termCh <- true:
	case err := <-ml.termAckCh:
		ml.intPipe.Close()
		return err
	}

	_, err := ml.intPipe.SendBytes(intFrame, 0)
	if err == nil {
		<-ml.termAckCh
	}

	ml.intPipe.Close()
	return err
}

// Utilities -------------------------------------------------------------------

func randomInprocEndpoint() (string, error) {
	rnd := make([]byte, 10)
	if _, err := rand.Read(rnd); err != nil {
		return "", err
	}
	return "inproc://rnd" + string(rnd), nil
}

// Errors ----------------------------------------------------------------------

var (
	ErrEmptyPollItems   = errors.New("empty PollItems")
	ErrTooManyPollItems = errors.New("no more than 255 PollItems supported")
)
