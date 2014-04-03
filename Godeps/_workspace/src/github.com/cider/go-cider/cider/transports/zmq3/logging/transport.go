// Copyright (c) 2013 The go-cider AUTHORS
//
// Use of this source code is governed by The MIT License
// that can be found in the LICENSE file.

package logging

import (
	// Stdlib
	"errors"
	"fmt"

	// Cider
	"github.com/cider/go-cider/cider/services"

	// Other
	"github.com/dmotylev/nutrition"
	zmq "github.com/pebbe/zmq3"
)

type LogLevel byte

const (
	LevelUnset LogLevel = iota
	LevelTrace
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
	LevelCritical
	LevelOff
	numLogLevels
)

//------------------------------------------------------------------------------
// Transport
//------------------------------------------------------------------------------

type TransportFactory struct {
	Endpoint string
	Sndhwm   int
}

func NewTransportFactory() *TransportFactory {
	// Keep ZeroMQ defaults by default.
	return &TransportFactory{
		Sndhwm: 1000,
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
		return &services.ErrMissingConfig{"endpoint", "ZeroMQ 3.x Logging transport"}
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
	identity      []byte
	dispatchChans []chan string
	cmdChan       chan interface{}
	closeChan     chan bool
	closeAckChan  chan struct{}
	err           error
}

func (factory *TransportFactory) NewTransport(identity string) (*Transport, error) {
	// Create the internal 0MQ socket.
	sock, err := zmq.NewSocket(zmq.PUSH)
	if err != nil {
		return nil, err
	}

	if err := sock.SetSndhwm(factory.Sndhwm); err != nil {
		sock.Close()
		return nil, err
	}

	if err := sock.Connect(factory.Endpoint); err != nil {
		sock.Close()
		return nil, err
	}

	// Create the internal log record dispatch channels.
	dispatchChans := make([]chan string, int(numLogLevels))
	for i := range dispatchChans {
		dispatchChans[i] = make(chan string)
	}

	// Prepare a Transport instance.
	t := &Transport{
		identity:      []byte(identity),
		dispatchChans: dispatchChans,
		cmdChan:       make(chan interface{}),
		closeChan:     make(chan bool),
		closeAckChan:  make(chan struct{}),
	}

	// Spawn the background goroutine.
	go t.loop(sock)
	return t, nil
}

type setLogLevelCmd LogLevel

func (t *Transport) SetLogLevel(level LogLevel) {
	t.cmdChan <- setLogLevelCmd(level)
}

type setSndhwmCmd struct {
	sndhwm int
	errCh  chan error
}

func (t *Transport) SetSndhwm(sndhwm int) error {
	errCh := make(chan error, 1)
	t.cmdChan <- &setSndhwmCmd{sndhwm, errCh}
	return <-errCh
}

type connectCmd struct {
	endpoint string
	errCh    chan error
}

func (t *Transport) Connect(endpoint string) error {
	errCh := make(chan error, 1)
	t.cmdChan <- &connectCmd{endpoint, errCh}
	return <-errCh
}

// logging.Transport interface ---------------------------------------------------

func (t *Transport) Unsetf(format string, params ...interface{}) {
	t.enqueueLogRecord(LevelUnset, fmt.Sprintf(format, params...))
}

func (t *Transport) Tracef(format string, params ...interface{}) {
	t.enqueueLogRecord(LevelTrace, fmt.Sprintf(format, params...))
}

func (t *Transport) Debugf(format string, params ...interface{}) {
	t.enqueueLogRecord(LevelDebug, fmt.Sprintf(format, params...))
}

func (t *Transport) Infof(format string, params ...interface{}) {
	t.enqueueLogRecord(LevelInfo, fmt.Sprintf(format, params...))
}

func (t *Transport) Warnf(format string, params ...interface{}) error {
	msg := fmt.Sprintf(format, params...)
	t.enqueueLogRecord(LevelWarn, msg)
	return errors.New(msg)
}

func (t *Transport) Errorf(format string, params ...interface{}) error {
	msg := fmt.Sprintf(format, params...)
	t.enqueueLogRecord(LevelError, msg)
	return errors.New(msg)
}

func (t *Transport) Criticalf(format string, params ...interface{}) error {
	msg := fmt.Sprintf(format, params...)
	t.enqueueLogRecord(LevelCritical, msg)
	return errors.New(msg)
}

func (t *Transport) Unset(v ...interface{}) {
	t.enqueueLogRecord(LevelUnset, fmt.Sprint(v...))
}

func (t *Transport) Trace(v ...interface{}) {
	t.enqueueLogRecord(LevelTrace, fmt.Sprint(v...))
}

func (t *Transport) Debug(v ...interface{}) {
	t.enqueueLogRecord(LevelDebug, fmt.Sprint(v...))
}

func (t *Transport) Info(v ...interface{}) {
	t.enqueueLogRecord(LevelInfo, fmt.Sprint(v...))
}

func (t *Transport) Warn(v ...interface{}) error {
	msg := fmt.Sprint(v...)
	t.enqueueLogRecord(LevelWarn, fmt.Sprint(v...))
	return errors.New(msg)
}

func (t *Transport) Error(v ...interface{}) error {
	msg := fmt.Sprint(v...)
	t.enqueueLogRecord(LevelError, fmt.Sprint(v...))
	return errors.New(msg)
}

func (t *Transport) Critical(v ...interface{}) error {
	msg := fmt.Sprint(v...)
	t.enqueueLogRecord(LevelCritical, fmt.Sprint(v...))
	return errors.New(msg)
}

func (t *Transport) Flush() {
	return
}

func (t *Transport) Close() error {
	select {
	case t.closeChan <- true:
		break
	case <-t.closeAckChan:
		break
	}
	return nil
}

func (t *Transport) Closed() <-chan struct{} {
	return t.closeAckChan
}

func (t *Transport) Wait() error {
	<-t.Closed()
	return t.err
}

// Dispatching log records -----------------------------------------------------

// Must be sent to the server to identify the service and protocol.
var msgHeader = []byte("CDR#LOGGING@01")

var (
	levelUnsetFrame    = []byte{byte(LevelTrace)}
	levelTraceFrame    = []byte{byte(LevelTrace)}
	levelDebugFrame    = []byte{byte(LevelDebug)}
	levelInfoFrame     = []byte{byte(LevelInfo)}
	levelWarnFrame     = []byte{byte(LevelWarn)}
	levelErrorFrame    = []byte{byte(LevelError)}
	levelCriticalFrame = []byte{byte(LevelCritical)}
)

func (t *Transport) enqueueLogRecord(level LogLevel, msg string) {
	select {
	case t.dispatchChans[int(level)] <- msg:
		return
	case <-t.closeAckChan:
		return
	}
}

func (t *Transport) loop(sock *zmq.Socket) {
	var (
		currentLogLevel  LogLevel
		msgLogLevelFrame []byte
		msgPayload       string
	)
	for {
		select {
		case msg := <-t.dispatchChans[int(LevelUnset)]:
			msgLogLevelFrame = levelUnsetFrame
			msgPayload = msg

		case msg := <-t.dispatchChans[int(LevelTrace)]:
			msgLogLevelFrame = levelTraceFrame
			msgPayload = msg

		case msg := <-t.dispatchChans[int(LevelDebug)]:
			msgLogLevelFrame = levelDebugFrame
			msgPayload = msg

		case msg := <-t.dispatchChans[int(LevelInfo)]:
			msgLogLevelFrame = levelInfoFrame
			msgPayload = msg

		case msg := <-t.dispatchChans[int(LevelWarn)]:
			msgLogLevelFrame = levelWarnFrame
			msgPayload = msg

		case msg := <-t.dispatchChans[int(LevelError)]:
			msgLogLevelFrame = levelErrorFrame
			msgPayload = msg

		case msg := <-t.dispatchChans[int(LevelCritical)]:
			msgLogLevelFrame = levelCriticalFrame
			msgPayload = msg

		case cmd := <-t.cmdChan:
			switch cmd := cmd.(type) {
			case *setSndhwmCmd:
				cmd.errCh <- sock.SetSndhwm(cmd.sndhwm)
			case *connectCmd:
				cmd.errCh <- sock.Connect(cmd.endpoint)
			case setLogLevelCmd:
				currentLogLevel = LogLevel(cmd)
			}
			continue

		case <-t.closeChan:
			sock.Close()
			close(t.closeAckChan)
			return
		}

		if msgLogLevelFrame[0] < byte(currentLogLevel) {
			continue
		}

		if _, err := sock.SendBytes(t.identity, zmq.DONTWAIT|zmq.SNDMORE); err != nil {
			t.abort(err)
			continue
		}
		if _, err := sock.SendBytes(msgHeader, zmq.DONTWAIT|zmq.SNDMORE); err != nil {
			t.abort(err)
			continue
		}
		if _, err := sock.SendBytes(msgLogLevelFrame, zmq.DONTWAIT|zmq.SNDMORE); err != nil {
			t.abort(err)
			continue
		}
		if _, err := sock.SendBytes([]byte(msgPayload), zmq.DONTWAIT); err != nil {
			t.abort(err)
			continue
		}
	}
}

func (t *Transport) abort(err error) {
	t.err = err
	t.Close()
}
