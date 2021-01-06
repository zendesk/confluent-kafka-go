package kafka

/**
 * Copyright 2016 Confluent Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"
)

/*
#include "rdkafka_select.h"
#include <stdlib.h>
*/
import "C"

var globalCgoMapLock sync.Mutex
var globalCgoMap map[unsafe.Pointer]*handle = make(map[unsafe.Pointer]*handle)

// OAuthBearerToken represents the data to be transmitted
// to a broker during SASL/OAUTHBEARER authentication.
type OAuthBearerToken struct {
	// Token value, often (but not necessarily) a JWS compact serialization
	// as per https://tools.ietf.org/html/rfc7515#section-3.1; it must meet
	// the regular expression for a SASL/OAUTHBEARER value defined at
	// https://tools.ietf.org/html/rfc7628#section-3.1
	TokenValue string
	// Metadata about the token indicating when it expires (local time);
	// it must represent a time in the future
	Expiration time.Time
	// Metadata about the token indicating the Kafka principal name
	// to which it applies (for example, "admin")
	Principal string
	// SASL extensions, if any, to be communicated to the broker during
	// authentication (all keys and values of which must meet the regular
	// expressions defined at https://tools.ietf.org/html/rfc7628#section-3.1,
	// and it must not contain the reserved "auth" key)
	Extensions map[string]string
}

// Handle represents a generic client handle containing common parts for
// both Producer and Consumer.
type Handle interface {
	// SetOAuthBearerToken sets the the data to be transmitted
	// to a broker during SASL/OAUTHBEARER authentication. It will return nil
	// on success, otherwise an error if:
	// 1) the token data is invalid (meaning an expiration time in the past
	// or either a token value or an extension key or value that does not meet
	// the regular expression requirements as per
	// https://tools.ietf.org/html/rfc7628#section-3.1);
	// 2) SASL/OAUTHBEARER is not supported by the underlying librdkafka build;
	// 3) SASL/OAUTHBEARER is supported but is not configured as the client's
	// authentication mechanism.
	SetOAuthBearerToken(oauthBearerToken OAuthBearerToken) error

	// SetOAuthBearerTokenFailure sets the error message describing why token
	// retrieval/setting failed; it also schedules a new token refresh event for 10
	// seconds later so the attempt may be retried. It will return nil on
	// success, otherwise an error if:
	// 1) SASL/OAUTHBEARER is not supported by the underlying librdkafka build;
	// 2) SASL/OAUTHBEARER is supported but is not configured as the client's
	// authentication mechanism.
	SetOAuthBearerTokenFailure(errstr string) error

	// gethandle() returns the internal handle struct pointer
	gethandle() *handle
}

// key for rkqtAssignedPartitions
type topicPartitionKey struct {
	Topic     string
	Partition int32
}

// Common instance handle for both Producer and Consumer
type handle struct {
	rk  *C.rd_kafka_t
	rkq *C.rd_kafka_queue_t

	// Forward logs from librdkafka log queue to logs channel.
	logs          chan LogEvent
	logq          *C.rd_kafka_queue_t
	closeLogsChan bool

	// Topic <-> rkt caches
	rktCacheLock sync.Mutex
	// topic name -> rkt cache
	rktCache map[string]*C.rd_kafka_topic_t
	// rkt -> topic name cache
	rktNameCache map[*C.rd_kafka_topic_t]string

	// topic partition - rkqt
	// to store partition queues for currently assigned partitions
	rkqtAssignedPartitions map[topicPartitionKey]*handleIOTrigger

	// Cached instance name to avoid CGo call in String()
	name string

	//
	// cgo map
	// Maps C callbacks based on cgoid back to its Go object
	cgoLock          sync.Mutex
	cgoidNext        uintptr
	cgomap           map[int]cgoif
	globalCgoPointer unsafe.Pointer

	//
	// producer
	//
	p *Producer

	// Forward delivery reports on Producer.Events channel
	fwdDr bool

	// Include DeliveryReportError objects in DR events channels
	fwdDrErrEvents bool

	//
	// consumer
	//
	c *Consumer

	// Forward rebalancing ack responsibility to application (current setting)
	currAppRebalanceEnable bool

	// WaitGroup to wait for spawned go-routines to finish.
	waitGroup sync.WaitGroup

	// IO-trigger functionality (wake golang up when an event is ready, without polling)
	ioPollTrigger    *handleIOTrigger
	logIOPollTrigger *handleIOTrigger

	tlsConfig     *tls.Config
	intermediates *x509.CertPool
}

func (h *handle) String() string {
	return h.name
}

// setupGlobalCgoMap needs to be called before preRdkafkaSetup, because
// globalCgoPointer needs to be put into the config as the opaque pointer
func (h *handle) setupGlobalCgoMap() {
	h.globalCgoPointer = C.malloc(C.size_t(1))
	globalCgoMapLock.Lock()
	globalCgoMap[h.globalCgoPointer] = h
	globalCgoMapLock.Unlock()
}

// preRdkafkaSetup needs to be called _before_ rd_kafka_new
func (h *handle) preRdkafkaSetup() {
	h.rktCache = make(map[string]*C.rd_kafka_topic_t)
	h.rktNameCache = make(map[*C.rd_kafka_topic_t]string)
	h.rkqtAssignedPartitions = make(map[topicPartitionKey]*handleIOTrigger)
	h.cgomap = make(map[int]cgoif)
	h.intermediates = x509.NewCertPool()
}

// setup needs to be called _after_ rd_kafka_new
func (h *handle) setup() {
	h.name = C.GoString(C.rd_kafka_name(h.rk))
}

func (h *handle) cleanup() {
	if h.logs != nil {
		C.rd_kafka_queue_destroy(h.logq)
		if h.closeLogsChan {
			close(h.logs)
		}
	}

	for _, crkt := range h.rktCache {
		C.rd_kafka_topic_destroy(crkt)
	}

	if h.rkq != nil {
		C.rd_kafka_queue_destroy(h.rkq)
	}

	h.closePartitionQueues()

	globalCgoMapLock.Lock()
	delete(globalCgoMap, h.globalCgoPointer)
	globalCgoMapLock.Unlock()
	C.free(h.globalCgoPointer)
}

func (h *handle) closePartitionQueues() {
	for _, parQueueTrigger := range h.rkqtAssignedPartitions {
		parQueueTrigger.stop()
		C.rd_kafka_queue_destroy(parQueueTrigger.rkq)
	}

	h.rkqtAssignedPartitions = make(map[topicPartitionKey]*handleIOTrigger)
}

func (h *handle) setupLogQueue(logsChan chan LogEvent, termChan chan bool) error {
	if logsChan == nil {
		logsChan = make(chan LogEvent, 10000)
		h.closeLogsChan = true
	}

	h.logs = logsChan

	// Let librdkafka forward logs to our log queue instead of the main queue
	h.logq = C.rd_kafka_queue_new(h.rk)
	C.rd_kafka_set_log_queue(h.rk, h.logq)

	// Use librdkafka's FD-based event notifications to find out when there are log events.
	var err error
	h.logIOPollTrigger, err = startIOTrigger(h.logq)
	if err != nil {
		return nil
	}
	h.waitGroup.Add(1)
	// Start a goroutine to consume the log queue by waiting for log events
	go func() {
		h.pollLogEvents(h.logs, termChan)
		_ = h.logIOPollTrigger.stop()
		h.waitGroup.Done()
	}()

	return nil
}

// getRkt0 finds or creates and returns a C topic_t object from the local cache.
func (h *handle) getRkt0(topic string, ctopic *C.char, doLock bool) (crkt *C.rd_kafka_topic_t) {
	if doLock {
		h.rktCacheLock.Lock()
		defer h.rktCacheLock.Unlock()
	}
	crkt, ok := h.rktCache[topic]
	if ok {
		return crkt
	}

	if ctopic == nil {
		ctopic = C.CString(topic)
		defer C.free(unsafe.Pointer(ctopic))
	}

	crkt = C.rd_kafka_topic_new(h.rk, ctopic, nil)
	if crkt == nil {
		panic(fmt.Sprintf("Unable to create new C topic \"%s\": %s",
			topic, C.GoString(C.rd_kafka_err2str(C.rd_kafka_last_error()))))
	}

	h.rktCache[topic] = crkt
	h.rktNameCache[crkt] = topic

	return crkt
}

// getRkt finds or creates and returns a C topic_t object from the local cache.
func (h *handle) getRkt(topic string) (crkt *C.rd_kafka_topic_t) {
	return h.getRkt0(topic, nil, true)
}

// getTopicNameFromRkt returns the topic name for a C topic_t object, preferably
// using the local cache to avoid a cgo call.
func (h *handle) getTopicNameFromRkt(crkt *C.rd_kafka_topic_t) (topic string) {
	h.rktCacheLock.Lock()
	defer h.rktCacheLock.Unlock()

	topic, ok := h.rktNameCache[crkt]
	if ok {
		return topic
	}

	// we need our own copy/refcount of the crkt
	ctopic := C.rd_kafka_topic_name(crkt)
	topic = C.GoString(ctopic)

	crkt = h.getRkt0(topic, ctopic, false /* dont lock */)

	return topic
}

// disablePartitionQueueForwarding disables forwarding messages from the topic
// partition queue to consumer queue.
// Stores C rd_kafka_queue_t object in rkqtAssignedPartitions
func (h *handle) disablePartitionQueueForwarding(topic string, partition int32) {
	partitionQueue := C.rd_kafka_queue_get_partition(h.rk, C.CString(topic), C.int32_t(partition))
	if partitionQueue == nil {
		return
	}

	C.rd_kafka_queue_forward(partitionQueue, nil)

	ioHandle, err := startIOTrigger(partitionQueue)
	if err != nil {
		return
	}

	tp := topicPartitionKey{
		Topic:     topic,
		Partition: partition,
	}
	h.rkqtAssignedPartitions[tp] = ioHandle
}

func (h *handle) getAssignedPartitionQueue(topic string, partition int32) *handleIOTrigger {
	tp := topicPartitionKey{
		Topic:     topic,
		Partition: partition,
	}
	return h.rkqtAssignedPartitions[tp]
}

// cgoif is a generic interface for holding Go state passed as opaque
// value to the C code.
// Since pointers to complex Go types cannot be passed to C we instead create
// a cgoif object, generate a unique id that is added to the cgomap,
// and then pass that id to the C code. When the C code callback is called we
// use the id to look up the cgoif object in the cgomap.
type cgoif interface{}

// delivery report cgoif container
type cgoDr struct {
	deliveryChan chan Event
	opaque       interface{}
}

// cgoPut adds object cg to the handle's cgo map and returns a
// unique id for the added entry.
// Thread-safe.
// FIXME: the uniquity of the id is questionable over time.
func (h *handle) cgoPut(cg cgoif) (cgoid int) {
	h.cgoLock.Lock()
	defer h.cgoLock.Unlock()

	h.cgoidNext++
	if h.cgoidNext == 0 {
		h.cgoidNext++
	}
	cgoid = (int)(h.cgoidNext)
	h.cgomap[cgoid] = cg
	return cgoid
}

// cgoGet looks up cgoid in the cgo map, deletes the reference from the map
// and returns the object, if found. Else returns nil, false.
// Thread-safe.
func (h *handle) cgoGet(cgoid int) (cg cgoif, found bool) {
	if cgoid == 0 {
		return nil, false
	}

	h.cgoLock.Lock()
	defer h.cgoLock.Unlock()
	cg, found = h.cgomap[cgoid]
	if found {
		delete(h.cgomap, cgoid)
	}

	return cg, found
}

// setOauthBearerToken - see rd_kafka_oauthbearer_set_token()
func (h *handle) setOAuthBearerToken(oauthBearerToken OAuthBearerToken) error {
	cTokenValue := C.CString(oauthBearerToken.TokenValue)
	defer C.free(unsafe.Pointer(cTokenValue))

	cPrincipal := C.CString(oauthBearerToken.Principal)
	defer C.free(unsafe.Pointer(cPrincipal))

	cErrstrSize := C.size_t(512)
	cErrstr := (*C.char)(C.malloc(cErrstrSize))
	defer C.free(unsafe.Pointer(cErrstr))

	cExtensions := make([]*C.char, 2*len(oauthBearerToken.Extensions))
	extensionSize := 0
	for key, value := range oauthBearerToken.Extensions {
		cExtensions[extensionSize] = C.CString(key)
		defer C.free(unsafe.Pointer(cExtensions[extensionSize]))
		extensionSize++
		cExtensions[extensionSize] = C.CString(value)
		defer C.free(unsafe.Pointer(cExtensions[extensionSize]))
		extensionSize++
	}

	var cExtensionsToUse **C.char
	if extensionSize > 0 {
		cExtensionsToUse = (**C.char)(unsafe.Pointer(&cExtensions[0]))
	}

	cErr := C.rd_kafka_oauthbearer_set_token(h.rk, cTokenValue,
		C.int64_t(oauthBearerToken.Expiration.UnixNano()/(1000*1000)), cPrincipal,
		cExtensionsToUse, C.size_t(extensionSize), cErrstr, cErrstrSize)
	if cErr == C.RD_KAFKA_RESP_ERR_NO_ERROR {
		return nil
	}
	return newErrorFromCString(cErr, cErrstr)
}

// setOauthBearerTokenFailure - see rd_kafka_oauthbearer_set_token_failure()
func (h *handle) setOAuthBearerTokenFailure(errstr string) error {
	cerrstr := C.CString(errstr)
	defer C.free(unsafe.Pointer(cerrstr))
	cErr := C.rd_kafka_oauthbearer_set_token_failure(h.rk, cerrstr)
	if cErr == C.RD_KAFKA_RESP_ERR_NO_ERROR {
		return nil
	}
	return newError(cErr)
}

// handleIOTrigger wraps up librdkafka's rd_kafka_queue_io_event_enable functionality to let Go be notified of
// new events available on librdkafka queues. The idea is that we set up a pair of pipe FD's in startIOTrigger,
// and ask librdkafka to write a byte to the pipe when we go from 0 -> 1 events in a queue (i.e. it's edge
// triggered).
//
// Because the Golang scheduler understands how to wake this goroutine up when there's data on the pipe, this
// enables us to "bridge" librdkafka's event-loop and the Golang event-loop.
type handleIOTrigger struct {
	notifyChan chan struct{}
	readerPipe *os.File
	writerPipe *os.File
	rkq        *C.rd_kafka_queue_t
	wg         sync.WaitGroup
}

// startIOTrigger hooks up IO based event triggering on the provided librdkafka queue, and sets up a goroutine
// to read from the pipe and signal other goutines waiting for events on the notifyChan.
func startIOTrigger(rkq *C.rd_kafka_queue_t) (*handleIOTrigger, error) {
	t := &handleIOTrigger{}

	t.rkq = rkq
	// It's important that this is a buffered channel, so that in the case where _nobody_ is waiting for
	// events, we don't block an internal librdkafka thread on the other side of the pipe.
	t.notifyChan = make(chan struct{}, 1)
	var err error
	t.readerPipe, t.writerPipe, err = os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("failed constructing IO poll pipe: %s", err.Error())
	}
	edgeMessage := C.CBytes([]byte{9})
	C.rd_kafka_queue_io_event_enable(rkq, C.int(t.writerPipe.Fd()), edgeMessage, 1)
	C.free(edgeMessage)

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer close(t.notifyChan)
		oneByteBuffer := make([]byte, 1)

		for {
			_, err := t.readerPipe.Read(oneByteBuffer)
			if err != nil {
				return
			}
			// If the channel write fails, that means the buffer (of 1) was already full, which is fine; nobody is
			// currently waiting for events, and they'll see the existing wakeup singal the next time they look at
			// the channel anyway.
			select {
			case t.notifyChan <- struct{}{}:
			default:
			}
		}
	}()
	return t, nil
}

// stop stops the internal goroutine watching for events from librdkafka and disables IO event triggering.
func (t *handleIOTrigger) stop() error {
	C.rd_kafka_queue_io_event_enable(t.rkq, -1, nil, 0)
	if err := t.writerPipe.Close(); err != nil {
		return err
	}
	if err := t.readerPipe.Close(); err != nil {
		return err
	}
	t.wg.Wait()
	return nil
}
