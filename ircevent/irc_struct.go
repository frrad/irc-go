// Copyright 2009 Thomas Jager <mail@jager.no>  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ircevent

import (
	"crypto/tls"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ergochat/irc-go/ircmsg"
)

type empty struct{}

type Callback func(ircmsg.Message)

type callbackPair struct {
	id       uint64
	callback Callback
}

type BatchCallback func(*Batch) bool

type batchCallbackPair struct {
	id       uint64
	callback BatchCallback
}

type LabelCallback func(*Batch)

type capResult struct {
	capName string
	ack     bool
}

type Connection struct {
	// config data, user-settable
	Server          string
	TLSConfig       *tls.Config
	Nick            string
	User            string
	RealName        string   // IRC realname/gecos
	WebIRC          []string // parameters for the WEBIRC command
	Password        string   // server password (PASS command)
	RequestCaps     []string // IRCv3 capabilities to request (failure is non-fatal)
	SASLLogin       string   // SASL credentials to log in with (failure is fatal)
	SASLPassword    string
	SASLMech        string
	QuitMessage     string
	Version         string
	Timeout         time.Duration
	KeepAlive       time.Duration
	ReconnectFreq   time.Duration
	MaxLineLen      int // maximum line length, not including tags
	UseTLS          bool
	UseSASL         bool
	EnableCTCP      bool
	Debug           bool
	AllowPanic      bool // if set, don't recover() from panics in callbacks
	AllowTruncation bool // if set, truncate lines exceeding MaxLineLen and send them

	// networking and synchronization
	stateMutex sync.Mutex     // innermost mutex: don't block while holding this
	end        chan empty     // closing this causes the goroutines to exit
	pwrite     chan []byte    // receives IRC lines to be sent to the socket
	wg         sync.WaitGroup // after closing end, wait on this for all the goroutines to stop
	socket     net.Conn
	lastError  error
	quitAt     time.Time // time Quit() was called
	running    bool      // is a connection active? is `end` open?
	quit       bool      // user called Quit, do not reconnect
	pingSent   bool      // we sent PING and are waiting for PONG

	// IRC protocol connection state
	currentNick     string // nickname assigned by the server, empty before registration
	capsAdvertised  map[string]string
	capsAcked       map[string]string
	isupport        map[string]string
	isupportPartial map[string]string
	nickCounter     int
	registered      bool
	// Connect() builds these with sufficient capacity to receive all expected
	// responses during negotiation. Sends to them are nonblocking, so anything
	// sent outside of negotiation will not cause the relevant callbacks to block.
	welcomeChan chan empty      // signals that we got 001 and we are now connected
	saslChan    chan saslResult // transmits the final outcome of SASL negotiation
	capsChan    chan capResult  // transmits the final status of each CAP negotiated
	capFlags    uint32

	// callback state
	eventsMutex sync.Mutex
	events      map[string][]callbackPair
	// we assign ID numbers to callbacks so they can be removed. normally
	// the ID number is globally unique (generated by incrementing this counter).
	// if we add a callback in two places we might reuse the number (XXX)
	callbackCounter uint64
	// did we initialize the callbacks needed for the library itself?
	batchCallbacks   []batchCallbackPair
	hasBaseCallbacks bool

	batchMutex     sync.Mutex
	batches        map[string]batchInProgress
	labelCallbacks map[int64]pendingLabel
	labelCounter   int64

	Log *log.Logger
}

type batchInProgress struct {
	createdAt time.Time
	label     int64
	// needs to be heap-allocated so we can append to batch.Items:
	batch *Batch
}

type pendingLabel struct {
	createdAt time.Time
	callback  LabelCallback
}

// Batch represents an IRCv3 batch, or a line within one. There are
// two cases:
// 1. (Batch).Command == "BATCH". This indicates the start of an IRCv3
// batch; the embedded Message is the initial BATCH command, which
// may contain tags that pertain to the batch as a whole. (Batch).Items
// contains zero or more *Batch elements, pointing to the contents of
// the batch in order.
// 2. (Batch).Command != "BATCH". This is an ordinary IRC line; its
// tags, command, and parameters are available as members of the embedded
// Message.
// In the context of labeled-response, there is a third case: a `nil`
// value of *Batch indicates that the server failed to respond in time.
type Batch struct {
	ircmsg.Message
	Items []*Batch
}

const (
	capFlagBatch uint32 = 1 << iota
	capFlagMessageTags
	capFlagLabeledResponse
	capFlagMultiline
)

func (irc *Connection) processAckedCaps(acknowledgedCaps []string) {
	irc.stateMutex.Lock()
	defer irc.stateMutex.Unlock()
	var hasBatch, hasLabel, hasTags, hasMultiline bool
	for _, c := range acknowledgedCaps {
		irc.capsAcked[c] = irc.capsAdvertised[c]
		switch c {
		case "batch":
			hasBatch = true
		case "labeled-response":
			hasLabel = true
		case "message-tags":
			hasTags = true
		case "draft/multiline", "multiline":
			hasMultiline = true
		}
	}

	var capFlags uint32
	if hasBatch {
		capFlags |= capFlagBatch
	}
	if hasBatch && hasLabel {
		capFlags |= capFlagLabeledResponse
	}
	if hasTags {
		capFlags |= capFlagMessageTags
	}
	if hasTags && hasBatch && hasMultiline {
		capFlags |= capFlagMultiline
	}

	atomic.StoreUint32(&irc.capFlags, capFlags)
}

func (irc *Connection) batchNegotiated() bool {
	return atomic.LoadUint32(&irc.capFlags)&capFlagBatch != 0
}

func (irc *Connection) labelNegotiated() bool {
	return atomic.LoadUint32(&irc.capFlags)&capFlagLabeledResponse != 0
}

func ExtractNick(source string) string {
	nick, _, _ := SplitNUH(source)
	return nick
}

func SplitNUH(source string) (nick, user, host string) {
	if i, j := strings.Index(source, "!"), strings.Index(source, "@"); i > -1 && j > -1 && i < j {
		nick = source[0:i]
		user = source[i+1 : j]
		host = source[j+1:]
	}
	return
}

func lastParam(msg *ircmsg.Message) (result string) {
	if 0 < len(msg.Params) {
		return msg.Params[len(msg.Params)-1]
	}
	return
}
