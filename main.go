package main

import (
	// Stdlib
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	// Paprika
	"github.com/paprikaci/paprika/data"

	// Cider
	"github.com/cider/go-cider/cider/services/logging"
	"github.com/cider/go-cider/cider/services/pubsub"
	"github.com/cider/go-cider/cider/services/rpc"
	zlogging "github.com/cider/go-cider/cider/transports/zmq3/logging"
	zpubsub "github.com/cider/go-cider/cider/transports/zmq3/pubsub"
	zrpc "github.com/cider/go-cider/cider/transports/zmq3/rpc"

	// Others
	"code.google.com/p/goauth2/oauth"
	"github.com/garyburd/redigo/redis"
	"github.com/google/go-github/github"
	zmq "github.com/pebbe/zmq3"
)

const RedisOutputSequenceKey = "next-build-id"

func main() {
	// Initialise the Logging service.
	logger, err := logging.NewService(func() (logging.Transport, error) {
		factory := zlogging.NewTransportFactory()
		factory.MustReadConfigFromEnv("CIDER_ZMQ3_LOGGING_").MustBeFullyConfigured()
		return factory.NewTransport(os.Getenv("CIDER_ALIAS"))
	})
	if err != nil {
		panic(err)
	}
	defer zmq.Term()
	defer logger.Flush()

	if err := innerMain(logger); err != nil {
		panic(err)
	}
}

func innerMain(logger *logging.Service) error {
	// Make sure the the required environment variables are set.
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return logger.Critical("GITHUB_TOKEN is not set")
	}
	canonicalURL := os.Getenv("CANONICAL_URL")
	if canonicalURL == "" {
		return logger.Critical("CANONICAL_URL is not set")
	}
	if _, err := url.Parse(canonicalURL); err != nil {
		return logger.Critical(err)
	}
	listenAddress := os.Getenv("HTTP_LISTEN")
	if listenAddress == "" {
		return logger.Critical("HTTP_LISTEN is not set")
	}
	redisAddress := os.Getenv("REDIS_ADDRESS")
	if redisAddress == "" {
		return logger.Critical("REDIS_ADDRESS is not set")
	}

	// Connect to Redis.
	redisConn, err := redis.Dial("tcp", redisAddress)
	if err != nil {
		return logger.Critical(err)
	}
	var redisConnMu sync.Mutex

	// Initialise the GitHub client.
	t := oauth.Transport{
		Token: &oauth.Token{AccessToken: token},
	}
	gh := github.NewClient(t.Client())

	// Initialise the PubSub service.
	eventBus, err := pubsub.NewService(func() (pubsub.Transport, error) {
		factory := zpubsub.NewTransportFactory()
		factory.MustReadConfigFromEnv("CIDER_ZMQ3_PUBSUB_").MustBeFullyConfigured()
		return factory.NewTransport(os.Getenv("CIDER_ALIAS"))
	})
	if err != nil {
		return logger.Critical(err)
	}
	defer func() {
		select {
		case <-eventBus.Closed():
			goto Wait
		default:
		}
		if err := eventBus.Close(); err != nil {
			logger.Critical(err)
		}
	Wait:
		if err := eventBus.Wait(); err != nil {
			logger.Critical(err)
		}
	}()

	// Initialise the RPC service.
	executor, err := rpc.NewService(func() (rpc.Transport, error) {
		factory := zrpc.NewTransportFactory()
		factory.MustReadConfigFromEnv("CIDER_ZMQ3_RPC_").MustBeFullyConfigured()
		return factory.NewTransport(os.Getenv("CIDER_ALIAS"))
	})
	if err != nil {
		return logger.Critical(err)
	}
	defer func() {
		select {
		case <-executor.Closed():
			goto Wait
		default:
		}
		if err := executor.Close(); err != nil {
			logger.Critical(err)
		}
	Wait:
		if err := executor.Wait(); err != nil {
			logger.Critical(err)
		}
	}()

	// Start catching signals.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)

	// Trigger Paprika build on github.pull_request.
	if _, err := eventBus.Subscribe("github.pull_request", func(event pubsub.Event) {
		// Unmarshal the event object. Once into a struct to be accessed directly,
		// once for passing it on in build events.
		var body PullRequestEvent
		if err := event.Unmarshal(&body); err != nil {
			logger.Warn(err)
			return
		}

		// Only continue if the pull request sources were modified.
		pr := body.PullRequest
		if body.Action != "opened" && body.Action != "synchronized" {
			logger.Infof("Skipping the build, pull request %v not updated", pr.HTMLURL)
			return
		}

		logger.Infof("Preparing to build %v", pr.HTMLURL)

		// Unmarshal the whole pull request object as well so that we can later
		// pass it on in the build events.
		var prMap struct {
			PullRequest map[string]interface{} `codec:"pull_request"`
		}
		if err := event.Unmarshal(&prMap); err != nil {
			logger.Warn(err)
			return
		}

		// Emit paprika.build.enqueued at the beginning.
		if len(prMap.PullRequest) == 0 {
			logger.Error("Invalid github.pull_request event")
			return
		}

		logger.Infof("Emitting paprika.build.enqueued for pull request %v", pr.HTMLURL)
		if err := eventBus.Publish("paprika.build.enqueued", &BuildEnqueuedEvent{
			PullRequest: prMap.PullRequest,
		}); err != nil {
			logger.Error(err)
			return
		}

		// Emit paprika.build.finished.{success|failure|error} on return.
		var (
			call       *rpc.RemoteCall
			err        error
			buildError error
			outputKey  int64
		)
		defer func() {
			var result string
			if err != nil {
				result = "error"
			} else {
				switch rc := call.ReturnCode(); {
				case rc == 0:
					result = "success"
				case rc == 1:
					result = "failure"
				default:
					result = "error"
				}
			}

			var outputURL string
			if result != "error" {
				if !strings.HasSuffix(canonicalURL, "/") {
					canonicalURL += "/"
				}
				outputURL = fmt.Sprintf("%vbuild/%v", canonicalURL, outputKey)
			}

			finishedEvent := &BuildFinishedEvent{
				Result:      result,
				PullRequest: prMap.PullRequest,
				OutputURL:   outputURL,
			}
			if buildError != nil {
				finishedEvent.Error = buildError.Error()
			}

			kind := "paprika.build.finished." + result
			logger.Infof("Emitting %v for pull request %v", kind, pr.HTMLURL)
			err = eventBus.Publish(kind, finishedEvent)
			if err != nil {
				logger.Error(err)
				return
			}
		}()

		// Fetch paprika.yml first.
		logger.Debugf("Fetching %v for pull request %v", data.ConfigFileName, pr.HTMLURL)
		head := pr.Head
		opts := &github.RepositoryContentGetOptions{head.SHA}
		content, _, _, err := gh.Repositories.GetContents(head.Owner.Login,
			head.Repository.Name, data.ConfigFileName, opts)
		if err != nil {
			logger.Warn(err)
			buildError = err
			return
		}
		if content == nil {
			err = fmt.Errorf("%v is not a regular file", data.ConfigFileName)
			logger.Info(err)
			buildError = err
			return
		}

		// Decode the config file.
		decodedContent, err := content.Decode()
		if err != nil {
			logger.Warn(err)
			buildError = err
			return
		}

		logger.Debugf("Parsing %v for pull request %v", data.ConfigFileName, pr.HTMLURL)
		config, err := data.ParseConfig(decodedContent)
		if err != nil {
			logger.Info(err)
			buildError = err
			return
		}

		// Generate a build request and dispatch it.
		method, args, err := data.ParseArgs(config.Slave.Label, config.Repository.URL,
			config.Script.Path, config.Script.Runner, config.Script.Env)
		if err != nil {
			logger.Info(err)
			buildError = err
			return
		}

		var output bytes.Buffer
		call = executor.NewRemoteCall(method, args)
		// XXX: output is not thread-safe, but the client is single-threaded
		// right now, so it is fine, but not good to depend on that.
		call.Stdout = &output
		call.Stderr = &output
		logger.Debugf("Dispatching build request for pull request %v", pr.HTMLURL)
		err = call.Execute()
		if err != nil {
			logger.Error(err)
			return
		}
		if rc := call.ReturnCode(); rc > 1 {
			logger.Errorf("Paprika returned an error return code: %v", rc)
			return
		}

		// Save the output.
		logger.Debugf("Saving build output for pull request %v", pr.HTMLURL)
		redisConnMu.Lock()
		err = redisConn.Err()
		if err != nil {
			logger.Error(err)
			redisConn.Close()
			redisConn, err = redis.Dial("tcp", redisAddress)
			if err != nil {
				logger.Error(err)
				return
			}
		}
		redisConnMu.Unlock()

		outputKey, err = redis.Int64(redisConn.Do("INCR", RedisOutputSequenceKey))
		if err != nil {
			logger.Error(err)
			return
		}

		_, err = redisConn.Do("SET", outputKey, output.Bytes())
		if err != nil {
			logger.Error(err)
			return
		}
	}); err != nil {
		return logger.Critical(err)
	}

	// Set up the HTTP server that serves the build output.
	http.HandleFunc("/build/", func(w http.ResponseWriter, r *http.Request) {
		// Parse the URL parameter, which is the build ID.
		fragments := strings.Split(r.URL.Path, "/")
		if len(fragments) != 3 || fragments[2] == "" {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		key, err := strconv.ParseInt(fragments[2], 10, 64)
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		// Fetch the output from the database.
		redisConnMu.Lock()
		defer redisConnMu.Unlock()

		err = redisConn.Err()
		if err != nil {
			logger.Error(err)
			redisConn.Close()
			redisConn, err = redis.Dial("tcp", redisAddress)
			if err != nil {
				logger.Error(err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		}

		v, err := redisConn.Do("GET", key)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		output := v.([]byte)
		if output == nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		// Write the output to the response.
		w.Header().Set("Content-Type", "text/plain")
		io.Copy(w, bytes.NewReader(output))
	})

	// Go listening for incoming HTTP requests.
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- http.ListenAndServe(listenAddress, nil)
	}()

	// Start processing signals, block until crashed or terminated.
	select {
	case err := <-serverErrCh:
		return logger.Critical(err)
	case <-eventBus.Closed():
		return eventBus.Wait()
	case <-executor.Closed():
		return executor.Wait()
	case <-signalCh:
	}
	return nil
}
