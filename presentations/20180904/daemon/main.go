package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	routeTimeout       = 5 * time.Second
	svrShutdownTimeout = 10 * time.Second
	ctxCancelWait      = 3 * time.Second
)

type versionKey struct{}

func main() {
	// create a root context that all future contexts will derive from, so that this
	// cancel func will propagate through all requests
	ctx, cancelFunc := context.WithCancel(context.Background())

	// seed context with appropriate values
	ctx = context.WithValue(ctx, versionKey{}, os.Getenv("APP_VERSION"))

	// listen for OS level signals to stop the program
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGQUIT, syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM)

	// create a handler for our main server requests
	// DO NOT USE http.DefaultServeMux because you don't know what's registered there
	// e.g. pprof automatically registers endpoints
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// you should derive request contexts from the base server context
		reqCtx, reqCancelFunc := context.WithTimeout(ctx, routeTimeout)
		defer reqCancelFunc()
		// execute requests with new context
		doThings(reqCtx)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello world"))
	})

	// create our main server
	s := http.Server{
		Addr:    ":" + os.Getenv("APP_PORT"),
		Handler: mux,
	}
	// start listening for requests in a goroutine
	go func() {
		// ListenAndServer blocks until it errors or until s.Shutdown is called
		err := s.ListenAndServe()
		switch err {
		// don't do anything on a nil error
		case nil:
		// this error is immediately returned when s.Shutdown is called, so there's no reason
		// to log the error
		case http.ErrServerClosed:
		default:
			fmt.Println(err)
		}
	}()

	// set up a separate internal server for handling health checks, pprof and
	// other things you don't want to expose to the world
	internalMux := http.NewServeMux()

	// always return 200 for liveness
	internalMux.HandleFunc("/liveness", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// for readiness checks, you might check connectivity to databases or other network services
	// we'll also add a ready bool so we can turn it off while shutting down
	ready := true
	readyMu := sync.Mutex{}

	internalMux.HandleFunc("/readiness", func(w http.ResponseWriter, r *http.Request) {
		// lock the mutex to prevent race conditions during shutdown
		readyMu.Lock()
		isReady := ready
		readyMu.Unlock()

		if isReady {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})

	// start the internal server
	internalServer := http.Server{
		Addr:    ":" + os.Getenv("INTERNAL_PORT"),
		Handler: internalMux,
	}
	go func() {
		err := internalServer.ListenAndServe()
		switch err {
		case nil:
		case http.ErrServerClosed:
		default:
			fmt.Println(err)
		}
	}()

	// now that both servers have been launched, we're going to block here waiting for
	// an OS signal to start shutting down cleanly. We're not capturing the actual
	// signal coming through since we aren't going to shut down any differently
	// based on SIGTERM vs SIGQUIT
	<-signalChan

	// make readiness check start failing so load balancers will stop sending requests here
	readyMu.Lock()
	ready = false
	readyMu.Unlock()

	// first we want to gracefully shut down the main server but leave the internal server running.
	// we can't guarantee how long the shutdown will take if there are long-running / misbehaving requests,
	// so we'll create a timeout after which we'll forcefully shutdown and exit
	t := time.NewTimer(svrShutdownTimeout)

	// create a blocking channel that will keep main from exiting until we have
	// cleanly finished processing requests
	shutdownChan := make(chan error)
	go func() {
		// we're not canceling the context yet because that will cause requests respecting it
		// to error out and return, when what we want is for them to finish successfully if possible
		shutdownChan <- s.Shutdown(ctx)
	}()

	// block here until either the main server shuts down successfully or the timeout is exceeded
	select {
	case err := <-shutdownChan:
		if err != nil {
			fmt.Println("shutdown finished with an error:", err)
		} else {
			fmt.Println("shutdown finished successfully")
		}
	case <-t.C:
		fmt.Println("shutdown timed out")
	}

	// regardless whether the server successfully exited, now we are going to cancel all contexts.
	// hopefully all your handlers respect these timeouts and will quit executing any long running requests
	// if they are still running. If s.Shutdown finished successfully, it's still good practice to
	// cancel your contexts when you are done with them
	cancelFunc()
	// give any remaining processes some time to return
	time.Sleep(ctxCancelWait)

	// do any other cleanup processes here, eliminating temp files you may have created, close connections
	// to databases or other services you may have instantiated, etc.
	fmt.Println("closing down database connections")

	// now shutdown the internal health check server. you could also implement a timeout here,
	// but since presumably you are in control of both the client and server, it may be unnecessary
	if err := internalServer.Shutdown(ctx); err != nil {
		fmt.Println(err)
	}

	fmt.Println("exiting cleanly!")
}

func doThings(ctx context.Context) {}
