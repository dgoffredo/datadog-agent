// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package defaultforwarder

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/DataDog/datadog-agent/comp/core/config"
	"github.com/DataDog/datadog-agent/comp/forwarder/defaultforwarder/transaction"
	httputils "github.com/DataDog/datadog-agent/pkg/util/http"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// Worker consumes Transaction (aka transactions) from the Forwarder and
// processes them. If the transaction fails to be processed the Worker will send
// it back to the Forwarder to be retried later.
type Worker struct {
	config config.Component

	// Client the http client used to processed transactions.
	Client *http.Client
	// HighPrio is the channel used to receive high priority transaction from the Forwarder.
	HighPrio <-chan transaction.Transaction
	// LowPrio is the channel used to receive low priority transaction from the Forwarder.
	LowPrio <-chan transaction.Transaction
	// RequeueChan is the channel used to send failed transaction back to the Forwarder.
	RequeueChan chan<- transaction.Transaction

	resetConnectionChan   chan struct{}
	stopChan              chan struct{}
	stopped               chan struct{}
	blockedList           *blockedEndpoints
	pointSuccessfullySent PointSuccessfullySent
}

// PointSuccessfullySent is called when sending successfully a point to the intake.
type PointSuccessfullySent interface {
	OnPointSuccessfullySent(int)
}

// NewWorker returns a new worker to consume Transaction from inputChan
// and push back erroneous ones into requeueChan.
func NewWorker(
	config config.Component,
	highPrioChan <-chan transaction.Transaction,
	lowPrioChan <-chan transaction.Transaction,
	requeueChan chan<- transaction.Transaction,
	blocked *blockedEndpoints,
	pointSuccessfullySent PointSuccessfullySent) *Worker {
	return &Worker{
		config:                config,
		HighPrio:              highPrioChan,
		LowPrio:               lowPrioChan,
		RequeueChan:           requeueChan,
		resetConnectionChan:   make(chan struct{}, 1),
		stopChan:              make(chan struct{}),
		stopped:               make(chan struct{}),
		Client:                NewHTTPClient(config),
		blockedList:           blocked,
		pointSuccessfullySent: pointSuccessfullySent,
	}
}

// NewHTTPClient creates a new http.Client
func NewHTTPClient(config config.Component) *http.Client {
	transport := httputils.CreateHTTPTransport()

	return &http.Client{
		Timeout:   config.GetDuration("forwarder_timeout") * time.Second,
		Transport: transport,
	}
}

// Stop stops the worker.
func (w *Worker) Stop(purgeHighPrio bool) {
	w.stopChan <- struct{}{}
	<-w.stopped

	if purgeHighPrio {
		// purging waiting transactions
	L:
		for {
			select {
			case t := <-w.HighPrio:
				log.Debugf("Flushing one new transaction before stopping Worker")
				w.callProcess(t) //nolint:errcheck
			default:
				break L
			}
		}
	}

}

// Start starts a Worker.
func (w *Worker) Start() {
	go func() {
		// notify that the worker did stop
		defer close(w.stopped)

		for {
			// handling high priority transactions first
			select {
			case t := <-w.HighPrio:
				if w.callProcess(t) == nil {
					continue
				}
				return
			case <-w.stopChan:
				return
			default:
			}

			select {
			case t := <-w.HighPrio:
				if w.callProcess(t) != nil {
					return
				}
			case t := <-w.LowPrio:
				if w.callProcess(t) != nil {
					return
				}
			case <-w.stopChan:
				return
			}
		}
	}()
}

// ScheduleConnectionReset allows signaling the worker that all connections should
// be recreated before sending the next transaction. Returns immediately.
func (w *Worker) ScheduleConnectionReset() {
	select {
	case w.resetConnectionChan <- struct{}{}:
	default:
		// a reset is already planned, we can ignore this one
	}
}

// callProcess will process a transaction and cancel it if we need to stop the
// worker.
func (w *Worker) callProcess(t transaction.Transaction) error {
	// poll for connection reset events first
	select {
	case <-w.resetConnectionChan:
		w.resetConnections()
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctx = httptrace.WithClientTrace(ctx, transaction.Trace)
	done := make(chan interface{})
	go func() {
		w.process(ctx, t)
		done <- nil
	}()

	select {
	case <-done:
		// wait for the Transaction process to be over
	case <-w.stopChan:
		// cancel current Transaction if we need to stop the worker
		cancel()
		<-done // We still need to wait for the process func to return
		return fmt.Errorf("Worker was requested to stop")
	}
	cancel()
	return nil
}

func (w *Worker) process(ctx context.Context, t transaction.Transaction) {
	requeue := func() {
		select {
		case w.RequeueChan <- t:
		default:
			log.Errorf("dropping transaction because the retry goroutine is too busy to handle another one")
		}
	}

	// Run the endpoint through our blockedEndpoints circuit breaker
	target := t.GetTarget()
	if w.blockedList.isBlock(target) {
		requeue()
		log.Errorf("Too many errors for endpoint '%s': retrying later", target)
	} else if err := t.Process(ctx, w.config, w.Client); err != nil {
		w.blockedList.close(target)
		requeue()
		log.Errorf("Error while processing transaction: %v", err)
	} else {
		w.pointSuccessfullySent.OnPointSuccessfullySent(t.GetPointCount())
		w.blockedList.recover(target)
	}
}

// resetConnections resets the connections by replacing the HTTP client used by
// the worker, in order to create new connections when the next transactions are processed.
// It must not be called while a transaction is being processed.
func (w *Worker) resetConnections() {
	log.Debug("Resetting worker's connections")
	w.Client.CloseIdleConnections()
	w.Client = NewHTTPClient(w.config)
}
