package fetcher

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/floatpane/matcha/config"
)

// imapPoolMaxActive limits short operations for one server login.
const imapPoolMaxActive = 3

// imapPoolMaxIdle is how many unused, logged-in clients are kept per server login.
const imapPoolMaxIdle = 2

// imapPoolIdleTimeout is how long an unused client stays open before it is closed.
const imapPoolIdleTimeout = 5 * time.Minute

// imapPoolNoopTimeout is how long a pooled client may take to answer NOOP
// before it is treated as dead; a connection that vanished during suspend or
// behind a NAT would otherwise block the next command for minutes.
const imapPoolNoopTimeout = 5 * time.Second

// imapPool keeps logged-in IMAP clients so repeated searches skip the TLS
// handshake and login.
var imapPool = func() *clientPool[*imapclient.Client] {
	p := newClientPool(connect, imapPoolIdleTimeout)
	p.answers = answersNoop
	p.closed = func(c *imapclient.Client) bool {
		select {
		case <-c.Closed():
			return true
		default:
			return false
		}
	}
	return p
}()

// answersNoop reports whether c answers NOOP within imapPoolNoopTimeout.
func answersNoop(c *imapclient.Client) bool {
	timer := time.AfterFunc(imapPoolNoopTimeout, func() { c.Close() }) //nolint:errcheck
	err := c.Noop().Wait()
	return timer.Stop() && err == nil
}

// isServerReply reports whether err is the server answering NO or BAD. The
// connection still works then, so the client is kept and the command is not
// repeated on a new connection.
func isServerReply(err error) bool {
	var reply *imap.Error
	return errors.As(err, &reply)
}

// acquireIMAPClient borrows an authenticated client for one short operation.
// The caller must invoke release once, after all commands have finished.
// Reused clients must answer NOOP first; caller operations are never retried.
func acquireIMAPClient(account *config.Account) (*imapclient.Client, func(), error) {
	return imapPool.acquire(account)
}

// withIMAPClient runs fn on an authenticated IMAP connection for account.
//
// Each caller gets its own client and must SELECT the folder it needs.
// A transport failure on a reused client retries fn once on a new client.
// Server NO and BAD replies are returned without retrying.
func withIMAPClient(account *config.Account, fn func(c *imapclient.Client) error) error {
	return imapPool.run(account, fn)
}

// CloseIMAPPool closes every unused pooled IMAP client. Clients that are in
// use go back to the pool as usual when their caller finishes.
func CloseIMAPPool() {
	imapPool.closeIdle()
}

// clientPool holds unused clients and limits operations per server login.
// C is *imapclient.Client in production; tests use a fake client and dialer.
type clientPool[C io.Closer] struct {
	dial        func(*config.Account) (C, error)
	idleTimeout time.Duration
	// answers, when set, checks that a pooled client still responds before
	// it is reused.
	answers func(C) bool
	closed  func(C) bool

	mu    sync.Mutex
	idle  map[string][]*idleClient[C] // oldest first
	slots map[string]chan struct{}
}

// idleClient is an unused client, when it was returned, and the timer that
// closes it after the idle timeout. returned is wall-clock time, so time
// spent in suspend counts, which the timer's monotonic clock does not.
type idleClient[C io.Closer] struct {
	client   C
	returned time.Time
	timer    *time.Timer
}

func newClientPool[C io.Closer](dial func(*config.Account) (C, error), idleTimeout time.Duration) *clientPool[C] {
	return &clientPool[C]{
		dial:        dial,
		idleTimeout: idleTimeout,
		idle:        make(map[string][]*idleClient[C]),
		slots:       make(map[string]chan struct{}),
	}
}

// poolKey identifies the server login a client belongs to, not a config entry.
func poolKey(account *config.Account) string {
	return fmt.Sprintf("%s\x00%d\x00%s", account.GetIMAPServer(), account.GetIMAPPort(), account.Email)
}

// reserve waits for an operation slot without holding the pool lock.
func (p *clientPool[C]) reserve(key string) chan struct{} {
	p.mu.Lock()
	slots := p.slots[key]
	if slots == nil {
		slots = make(chan struct{}, imapPoolMaxActive)
		p.slots[key] = slots
	}
	p.mu.Unlock()
	slots <- struct{}{}
	return slots
}

// get takes a responsive idle client before dialing a new one. The caller
// must hold an operation slot until the client is returned or closed.
func (p *clientPool[C]) get(account *config.Account, key string) (C, bool, error) {
	for {
		c, ok := p.take(key)
		if !ok {
			c, err := p.dial(account)
			return c, false, err
		}
		if (p.closed == nil || !p.closed(c)) && (p.answers == nil || p.answers(c)) {
			return c, true, nil
		}
		c.Close() //nolint:errcheck
	}
}

// keep returns usable clients to the pool and discards clients already closed.
func (p *clientPool[C]) keep(key string, c C) {
	if p.closed != nil && p.closed(c) {
		c.Close() //nolint:errcheck
		return
	}
	p.put(key, c)
}

func (p *clientPool[C]) acquire(account *config.Account) (C, func(), error) {
	key := poolKey(account)
	slots := p.reserve(key)
	c, _, err := p.get(account, key)
	if err != nil {
		<-slots
		return c, nil, err
	}
	return c, func() {
		p.keep(key, c)
		<-slots
	}, nil
}

// run calls fn with a pooled or newly dialed client; see withIMAPClient.
func (p *clientPool[C]) run(account *config.Account, fn func(C) error) error {
	key := poolKey(account)
	slots := p.reserve(key)
	defer func() { <-slots }()
	c, reused, err := p.get(account, key)
	if err != nil {
		return err
	}
	err = fn(c)
	if err == nil || isServerReply(err) {
		p.keep(key, c)
		return err
	}
	c.Close() //nolint:errcheck
	if !reused {
		return err
	}

	c, err = p.dial(account)
	if err != nil {
		return err
	}
	err = fn(c)
	if err == nil || isServerReply(err) {
		p.keep(key, c)
	} else {
		c.Close() //nolint:errcheck
	}
	return err
}

// take removes and returns the most recently returned unused client for key.
// Clients unused for the idle timeout by wall-clock time (for example across
// a suspend, when their timer has not fired yet) are closed instead.
func (p *clientPool[C]) take(key string) (C, bool) {
	var stale []*idleClient[C]
	defer func() {
		for _, entry := range stale {
			entry.client.Close() //nolint:errcheck
		}
	}()
	p.mu.Lock()
	defer p.mu.Unlock()

	for list := p.idle[key]; len(list) > 0; list = p.idle[key] {
		last := len(list) - 1
		entry := list[last]
		p.setIdle(key, slices.Delete(list, last, last+1))
		// If the timer already fired, its callback finds the entry gone and does nothing.
		entry.timer.Stop()
		if time.Now().Round(0).Sub(entry.returned) >= p.idleTimeout {
			stale = append(stale, entry)
			continue
		}
		return entry.client, true
	}
	var zero C
	return zero, false
}

// put returns c to the pool for key. When the account already has the
// maximum number of unused clients, the oldest one is closed.
func (p *clientPool[C]) put(key string, c C) {
	entry := &idleClient[C]{client: c, returned: time.Now().Round(0)}

	p.mu.Lock()
	list := append(p.idle[key], entry)
	var oldest *idleClient[C]
	if len(list) > imapPoolMaxIdle {
		oldest = list[0]
		list = slices.Delete(list, 0, 1)
	}
	p.setIdle(key, list)
	entry.timer = time.AfterFunc(p.idleTimeout, func() { p.closeAfterIdle(key, entry) })
	p.mu.Unlock()

	if oldest != nil {
		oldest.timer.Stop()
		oldest.client.Close() //nolint:errcheck
	}
}

// closeAfterIdle closes entry if it is still unused when its timer fires.
func (p *clientPool[C]) closeAfterIdle(key string, entry *idleClient[C]) {
	p.mu.Lock()
	list := p.idle[key]
	i := slices.Index(list, entry)
	if i >= 0 {
		p.setIdle(key, slices.Delete(list, i, i+1))
	}
	p.mu.Unlock()

	if i >= 0 {
		entry.client.Close() //nolint:errcheck
	}
}

// closeIdle closes every unused client in the pool.
func (p *clientPool[C]) closeIdle() {
	p.mu.Lock()
	all := p.idle
	p.idle = make(map[string][]*idleClient[C])
	p.mu.Unlock()

	for _, list := range all {
		for _, entry := range list {
			entry.timer.Stop()
			entry.client.Close() //nolint:errcheck
		}
	}
}

// setIdle stores list for key, removing the key when the list is empty.
// The caller must hold p.mu.
func (p *clientPool[C]) setIdle(key string, list []*idleClient[C]) {
	if len(list) == 0 {
		delete(p.idle, key)
		return
	}
	p.idle[key] = list
}
