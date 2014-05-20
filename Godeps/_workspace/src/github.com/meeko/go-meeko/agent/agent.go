// Copyright (c) 2013 The go-meeko AUTHORS
//
// Use of this source code is governed by The MIT License
// that can be found in the LICENSE file.

// This package provides some convenient auto-configuration functionality
// for Meeko agents. All available Meeko service clients are configured from
// the environment variables and then they are accessible as global exported
// variables.
//
// Make sure to call Terminate() before your agent exits.
package agent

import (
	// Stdlib
	"os"

	// Meeko
	"github.com/meeko/go-meeko/meeko/services/logging"
	"github.com/meeko/go-meeko/meeko/services/pubsub"
	"github.com/meeko/go-meeko/meeko/services/rpc"
	zlogging "github.com/meeko/go-meeko/meeko/transports/zmq3/logging"
	zpubsub "github.com/meeko/go-meeko/meeko/transports/zmq3/pubsub"
	zrpc "github.com/meeko/go-meeko/meeko/transports/zmq3/rpc"

	// Other
	zmq "github.com/pebbe/zmq3"
)

var (
	Logging *logging.Service
	PubSub  *pubsub.Service
	RPC     *rpc.Service
)

func init() {
	// Read the Meeko alias from the environment.
	alias := mustBeSet(os.Getenv("MEEKO_ALIAS"))

	// Initialise Logging service from the environment variables.
	var err error
	Logging, err = logging.NewService(func() (logging.Transport, error) {
		factory := zlogging.NewTransportFactory()
		factory.MustReadConfigFromEnv("MEEKO_ZMQ3_LOGGING_").MustBeFullyConfigured()
		return factory.NewTransport(alias)
	})
	if err != nil {
		panic(err)
	}
	Logging.Info("Logging service initialised")

	// Initialise PubSub service from the environment variables.
	PubSub, err = pubsub.NewService(func() (pubsub.Transport, error) {
		factory := zpubsub.NewTransportFactory()
		factory.MustReadConfigFromEnv("MEEKO_ZMQ3_PUBSUB_").MustBeFullyConfigured()
		return factory.NewTransport(alias)
	})
	if err != nil {
		Logging.Critical(err)
		Logging.Close()
		zmq.Term()
		panic(err)
	}
	Logging.Info("PubSub service initialised")

	// Initialise RPC service from the environment variables.
	RPC, err = rpc.NewService(func() (rpc.Transport, error) {
		factory := zrpc.NewTransportFactory()
		factory.MustReadConfigFromEnv("MEEKO_ZMQ3_RPC_").MustBeFullyConfigured()
		return factory.NewTransport(alias)
	})
	if err != nil {
		Logging.Critical(err)
		Logging.Close()
		PubSub.Close()
		zmq.Term()
		panic(err)
	}
	Logging.Info("RPC service initialised")
}

func mustBeSet(v string) string {
	if v == "" {
		panic("Required variable is not set")
	}
	return v
}

// Terminate closes all the initialised services.
//
// The agent developers must make sure that this functions is called before
// their agent terminates so that all pending messages are send.
//
// This function never returns any error, it just tries to shut down cleanly.
// Since the function is always called on agent termination, no other behaviour
// really makes sense.
func Terminate() {
	Logging.Info("Closing RPC service...")
	if err := RPC.Close(); err != nil {
		Logging.Error(err)
	}

	Logging.Info("Closing PubSub service...")
	if err := PubSub.Close(); err != nil {
		Logging.Error(err)
	}

	Logging.Info("Closing Logging service...")
	Logging.Info("Waiting for the ZeroMQ context to terminate...")
	Logging.Close()
	zmq.Term()
}
