// Copyright (c) 2013-2014 The cider-github-trigger AUTHORS
//
// Use of this source code is governed by the MIT license
// that can be found in the LICENSE file.

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

	// Cider
	"github.com/cider/cider/data"

	// Meeko
	"github.com/meeko/go-meeko/agent"
	"github.com/meeko/go-meeko/meeko/services/pubsub"
	"github.com/meeko/go-meeko/meeko/services/rpc"

	// Others
	"code.google.com/p/goauth2/oauth"
	"github.com/garyburd/redigo/redis"
	"github.com/google/go-github/github"
)

const RedisOutputSequenceKey = "next-build-id"

func main() {
	// Make sure the Meeko agent is terminated properly.
	defer agent.Terminate()

	// Run the main function.
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	// Some userful shortcuts.
	var (
		log      = agent.Logging
		eventBus = agent.PubSub
		executor = agent.RPC
	)

	// Parse the environment and make sure all the environment variables are set.
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return log.Critical("GITHUB_TOKEN is not set")
	}
	canonicalURL := os.Getenv("CANONICAL_URL")
	if canonicalURL == "" {
		return log.Critical("CANONICAL_URL is not set")
	}
	if _, err := url.Parse(canonicalURL); err != nil {
		return log.Critical(err)
	}
	listenAddress := os.Getenv("HTTP_LISTEN")
	if listenAddress == "" {
		return log.Critical("HTTP_LISTEN is not set")
	}
	redisAddress := os.Getenv("REDIS_ADDRESS")
	if redisAddress == "" {
		return log.Critical("REDIS_ADDRESS is not set")
	}

	// Connect to Redis.
	redisConn, err := redis.Dial("tcp", redisAddress)
	if err != nil {
		return log.Critical(err)
	}
	var redisConnMu sync.Mutex

	// Initialise the GitHub client.
	t := oauth.Transport{
		Token: &oauth.Token{AccessToken: token},
	}
	gh := github.NewClient(t.Client())

	// Start catching signals.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)

	// Trigger Cider build on github.pull_request.
	if _, err := eventBus.Subscribe("github.pull_request", func(event pubsub.Event) {
		// Unmarshal the event object. Once into a struct to be accessed directly,
		// once for passing it on in build events.
		var body PullRequestEvent
		if err := event.Unmarshal(&body); err != nil {
			log.Warn(err)
			return
		}

		// Only continue if the pull request sources were modified.
		pr := body.PullRequest
		if body.Action != "opened" && body.Action != "synchronized" {
			log.Infof("Skipping the build, pull request %v not updated", pr.HTMLURL)
			return
		}

		log.Infof("Preparing to build %v", pr.HTMLURL)

		// Unmarshal the whole pull request object as well so that we can later
		// pass it on in the build events.
		var prMap struct {
			PullRequest map[string]interface{} `codec:"pull_request"`
		}
		if err := event.Unmarshal(&prMap); err != nil {
			log.Warn(err)
			return
		}

		// Emit cider.build.enqueued at the beginning.
		if len(prMap.PullRequest) == 0 {
			log.Error("Invalid github.pull_request event")
			return
		}

		log.Infof("Emitting cider.build.enqueued for pull request %v", pr.HTMLURL)
		if err := eventBus.Publish("cider.build.enqueued", &BuildEnqueuedEvent{
			PullRequest: prMap.PullRequest,
		}); err != nil {
			log.Error(err)
			return
		}

		// Emit cider.build.finished.{success|failure|error} on return.
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

			kind := "cider.build.finished." + result
			log.Infof("Emitting %v for pull request %v", kind, pr.HTMLURL)
			err = eventBus.Publish(kind, finishedEvent)
			if err != nil {
				log.Error(err)
				return
			}
		}()

		// Fetch cider.yml first.
		log.Debugf("Fetching %v for pull request %v", data.ConfigFileName, pr.HTMLURL)
		head := pr.Head
		opts := &github.RepositoryContentGetOptions{head.SHA}
		content, _, _, err := gh.Repositories.GetContents(head.Owner.Login,
			head.Repository.Name, data.ConfigFileName, opts)
		if err != nil {
			log.Warn(err)
			buildError = err
			return
		}
		if content == nil {
			err = fmt.Errorf("%v is not a regular file", data.ConfigFileName)
			log.Info(err)
			buildError = err
			return
		}

		// Decode the config file.
		decodedContent, err := content.Decode()
		if err != nil {
			log.Warn(err)
			buildError = err
			return
		}

		log.Debugf("Parsing %v for pull request %v", data.ConfigFileName, pr.HTMLURL)
		config, err := data.ParseConfig(decodedContent)
		if err != nil {
			log.Info(err)
			buildError = err
			return
		}

		// Generate a build request and dispatch it.
		method, args, err := data.ParseArgs(config.Slave.Label, config.Repository.URL,
			config.Script.Path, config.Script.Runner, config.Script.Env)
		if err != nil {
			log.Info(err)
			buildError = err
			return
		}

		var output bytes.Buffer
		call = executor.NewRemoteCall(method, args)
		// XXX: output is not thread-safe, but the client is single-threaded
		// right now, so it is fine, but not good to depend on that.
		call.Stdout = &output
		call.Stderr = &output
		log.Debugf("Dispatching build request for pull request %v", pr.HTMLURL)
		err = call.Execute()
		if err != nil {
			log.Error(err)
			return
		}
		if rc := call.ReturnCode(); rc > 1 {
			log.Errorf("Cider returned an error return code: %v", rc)
			return
		}

		// Save the output.
		log.Debugf("Saving build output for pull request %v", pr.HTMLURL)
		redisConnMu.Lock()
		err = redisConn.Err()
		if err != nil {
			log.Error(err)
			redisConn.Close()
			redisConn, err = redis.Dial("tcp", redisAddress)
			if err != nil {
				log.Error(err)
				return
			}
		}
		redisConnMu.Unlock()

		outputKey, err = redis.Int64(redisConn.Do("INCR", RedisOutputSequenceKey))
		if err != nil {
			log.Error(err)
			return
		}

		_, err = redisConn.Do("SET", outputKey, output.Bytes())
		if err != nil {
			log.Error(err)
			return
		}
	}); err != nil {
		return log.Critical(err)
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
			log.Error(err)
			redisConn.Close()
			redisConn, err = redis.Dial("tcp", redisAddress)
			if err != nil {
				log.Error(err)
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
		return log.Critical(err)
	case <-signalCh:
		log.Info("Signal received, exiting...")
		return nil
	}
}
