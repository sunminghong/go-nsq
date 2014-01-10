package nsq

import (
	"bufio"
	"bytes"
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Writer is a high-level type to publish to NSQ.
//
// A Writer instance is 1:1 with a destination `nsqd`
// and will lazily connect to that instance (and re-connect)
// when Publish commands are executed.
type Writer struct {
	net.Conn

	WriteTimeout      time.Duration
	Addr              string
	HeartbeatInterval time.Duration
	ShortIdentifier   string
	LongIdentifier    string

	concurrentWriters int32

	transactionChan chan *WriterTransaction
	dataChan        chan []byte
	transactions    []*WriterTransaction
	state           int32
	stopFlag        int32
	exitChan        chan int
	closeChan       chan int
	wg              sync.WaitGroup

    authenticationPassword string
}

// WriterTransaction is returned by the async publish methods
// to retrieve metadata about the command after the
// response is received.
type WriterTransaction struct {
	cmd       *Command
	doneChan  chan *WriterTransaction
	FrameType int32         // the frame type received in response to the publish command
	Data      []byte        // the response data of the publish command
	Error     error         // the error (or nil) of the publish command
	Args      []interface{} // the slice of variadic arguments passed to PublishAsync or MultiPublishAsync
}

func (t *WriterTransaction) finish() {
	if t.doneChan != nil {
		t.doneChan <- t
	}
}

// returned when a publish command is made against a Writer that is not connected
var ErrNotConnected = errors.New("not connected")

// returned when a publish command is made against a Writer that has been stopped
var ErrStopped = errors.New("stopped")

// NewWriter returns an instance of Writer for the specified address
func NewWriter(addr string, authenticationPassword string) *Writer {
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("ERROR: unable to get hostname %s", err.Error())
	}
	return &Writer{
		transactionChan: make(chan *WriterTransaction),
		exitChan:        make(chan int),
		closeChan:       make(chan int),
		dataChan:        make(chan []byte),

		// can be overriden before connecting
		Addr:              addr,
		WriteTimeout:      time.Second,
		HeartbeatInterval: DefaultClientTimeout / 2,
		ShortIdentifier:   strings.Split(hostname, ".")[0],
		LongIdentifier:    hostname,

        authenticationPassword:     authenticationPassword,
	}
}

// String returns the address of the Writer
func (w *Writer) String() string {
	return w.Addr
}

// Stop disconnects and permanently stops the Writer
func (w *Writer) Stop() {
	if !atomic.CompareAndSwapInt32(&w.stopFlag, 0, 1) {
		return
	}
	w.close()
	w.wg.Wait()
}

// PublishAsync publishes a message body to the specified topic
// but does not wait for the response from `nsqd`.
//
// When the Writer eventually receives the response from `nsqd`,
// the supplied `doneChan` (if specified)
// will receive a `WriterTransaction` instance with the supplied variadic arguments
// (and the response `FrameType`, `Data`, and `Error`)
func (w *Writer) PublishAsync(topic string, body []byte, doneChan chan *WriterTransaction, args ...interface{}) error {
	return w.sendCommandAsync(Publish(topic, body), doneChan, args)
}

// MultiPublishAsync publishes a slice of message bodies to the specified topic
// but does not wait for the response from `nsqd`.
//
// When the Writer eventually receives the response from `nsqd`,
// the supplied `doneChan` (if specified)
// will receive a `WriterTransaction` instance with the supplied variadic arguments
// (and the response `FrameType`, `Data`, and `Error`)
func (w *Writer) MultiPublishAsync(topic string, body [][]byte, doneChan chan *WriterTransaction, args ...interface{}) error {
	cmd, err := MultiPublish(topic, body)
	if err != nil {
		return err
	}
	return w.sendCommandAsync(cmd, doneChan, args)
}

// Publish synchronously publishes a message body to the specified topic, returning
// the response frameType, data, and error
func (w *Writer) Publish(topic string, body []byte) (int32, []byte, error) {
	return w.sendCommand(Publish(topic, body))
}

// MultiPublish synchronously publishes a slice of message bodies to the specified topic, returning
// the response frameType, data, and error
func (w *Writer) MultiPublish(topic string, body [][]byte) (int32, []byte, error) {
	cmd, err := MultiPublish(topic, body)
	if err != nil {
		return -1, nil, err
	}
	return w.sendCommand(cmd)
}

func (w *Writer) sendCommand(cmd *Command) (int32, []byte, error) {
	doneChan := make(chan *WriterTransaction)
	err := w.sendCommandAsync(cmd, doneChan, nil)
	if err != nil {
		close(doneChan)
		return -1, nil, err
	}
	t := <-doneChan
	return t.FrameType, t.Data, t.Error
}

func (w *Writer) sendCommandAsync(cmd *Command, doneChan chan *WriterTransaction, args []interface{}) error {
	// keep track of how many outstanding writers we're dealing with
	// in order to later ensure that we clean them all up...
	atomic.AddInt32(&w.concurrentWriters, 1)
	defer atomic.AddInt32(&w.concurrentWriters, -1)

	if atomic.LoadInt32(&w.state) != StateConnected {
		err := w.connect()
		if err != nil {
			return err
		}
	}

	t := &WriterTransaction{
		cmd:       cmd,
		doneChan:  doneChan,
		FrameType: -1,
		Args:      args,
	}

	select {
	case w.transactionChan <- t:
	case <-w.exitChan:
		return ErrStopped
	}

	return nil
}

func (w *Writer) connect() error {
	if atomic.LoadInt32(&w.stopFlag) == 1 {
		return ErrStopped
	}

	if !atomic.CompareAndSwapInt32(&w.state, StateInit, StateConnected) {
		return ErrNotConnected
	}

	log.Printf("[%s] connecting...", w)
	conn, err := net.DialTimeout("tcp", w.Addr, time.Second*5)
	if err != nil {
		log.Printf("ERROR: [%s] failed to dial %s - %s", w, w.Addr, err)
		atomic.StoreInt32(&w.state, StateInit)
		return err
	}

	w.closeChan = make(chan int)
	w.Conn = conn

	w.SetWriteDeadline(time.Now().Add(w.WriteTimeout))
	_, err = w.Write(MagicV2)
	if err != nil {
		log.Printf("ERROR: [%s] failed to write magic - %s", w, err)
		w.close()
		return err
	}

	ci := make(map[string]interface{})
	ci["short_id"] = w.ShortIdentifier
	ci["long_id"] = w.LongIdentifier
	ci["heartbeat_interval"] = int64(w.HeartbeatInterval / time.Millisecond)
	ci["feature_negotiation"] = true
	ci["authentication_password"] = w.authenticationPassword
	cmd, err := Identify(ci)
	if err != nil {
		log.Printf("ERROR: [%s] failed to create IDENTIFY command - %s", w, err)
		w.close()
		return err
	}

	w.SetWriteDeadline(time.Now().Add(w.WriteTimeout))
	err = cmd.Write(w)
	if err != nil {
		log.Printf("ERROR: [%s] failed to write IDENTIFY - %s", w, err)
		w.close()
		return err
	}

	w.SetReadDeadline(time.Now().Add(w.HeartbeatInterval * 2))
	resp, err := ReadResponse(w)
	if err != nil {
		log.Printf("ERROR: [%s] failed to read IDENTIFY response - %s", w, err)
		w.close()
		return err
	}

	frameType, data, err := UnpackResponse(resp)
	if err != nil {
		log.Printf("ERROR: [%s] failed to unpack IDENTIFY response - %s", w, resp)
		w.close()
		return err
	}

	if frameType == FrameTypeError {
		log.Printf("ERROR: [%s] IDENTIFY returned error response - %s", w, data)
		w.close()
		return errors.New(string(data))
	}

	w.wg.Add(2)
	go w.readLoop()
	go w.messageRouter()

	return nil
}

func (w *Writer) close() {
	if !atomic.CompareAndSwapInt32(&w.state, StateConnected, StateDisconnected) {
		return
	}
	close(w.closeChan)
	w.Conn.Close()
	go func() {
		// we need to handle this in a goroutine so we don't
		// block the caller from making progress
		w.wg.Wait()
		atomic.StoreInt32(&w.state, StateInit)
	}()
}

func (w *Writer) messageRouter() {
	for {
		select {
		case t := <-w.transactionChan:
			w.transactions = append(w.transactions, t)
			w.SetWriteDeadline(time.Now().Add(w.WriteTimeout))
			err := t.cmd.Write(w.Conn)
			if err != nil {
				log.Printf("ERROR: [%s] failed writing %s", w, err)
				w.close()
				goto exit
			}
		case buf := <-w.dataChan:
			frameType, data, err := UnpackResponse(buf)
			if err != nil {
				log.Printf("ERROR: [%s] failed (%s) unpacking response %d %s", w, err, frameType, data)
				w.close()
				goto exit
			}

			if frameType == FrameTypeResponse && bytes.Equal(data, []byte("_heartbeat_")) {
				log.Printf("[%s] heartbeat received", w)
				w.SetWriteDeadline(time.Now().Add(w.WriteTimeout))
				err := Nop().Write(w.Conn)
				if err != nil {
					log.Printf("ERROR: [%s] failed sending heartbeat - %s", w, err)
					w.close()
					goto exit
				}
				continue
			}

			t := w.transactions[0]
			w.transactions = w.transactions[1:]
			t.FrameType = frameType
			t.Data = data
			t.Error = nil
			t.finish()
		case <-w.closeChan:
			goto exit
		}
	}

exit:
	w.transactionCleanup()
	w.wg.Done()
	log.Printf("[%s] exiting messageRouter()", w)
}

func (w *Writer) transactionCleanup() {
	// clean up transactions we can easily account for
	for _, t := range w.transactions {
		t.Error = ErrNotConnected
		t.finish()
	}
	w.transactions = w.transactions[:0]

	// spin and free up any writes that might have raced
	// with the cleanup process (blocked on writing
	// to transactionChan)
	for {
		select {
		case t := <-w.transactionChan:
			t.Error = ErrNotConnected
			t.finish()
		default:
			// keep spinning until there are 0 concurrent writers
			if atomic.LoadInt32(&w.concurrentWriters) == 0 {
				return
			}
			// give the runtime a chance to schedule other racing goroutines
			time.Sleep(5 * time.Millisecond)
			continue
		}
	}
}

func (w *Writer) readLoop() {
	rbuf := bufio.NewReader(w.Conn)
	for {
		w.SetReadDeadline(time.Now().Add(w.HeartbeatInterval * 2))
		resp, err := ReadResponse(rbuf)
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("ERROR: [%s] reading response %s", w, err)
			}
			w.close()
			goto exit
		}
		select {
		case w.dataChan <- resp:
		case <-w.closeChan:
			goto exit
		}
	}

exit:
	w.wg.Done()
	log.Printf("[%s] exiting readLoop()", w)
}
