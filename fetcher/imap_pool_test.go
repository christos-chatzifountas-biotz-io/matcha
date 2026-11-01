package fetcher

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/floatpane/matcha/config"
)

type fakePoolClient struct {
	id     int
	closed atomic.Int32
}

func (c *fakePoolClient) Close() error {
	c.closed.Add(1)
	return nil
}

func (c *fakePoolClient) isClosed() bool { return c.closed.Load() > 0 }

// fakeDialer hands out numbered fake clients and remembers every one it made.
type fakeDialer struct {
	mu      sync.Mutex
	clients []*fakePoolClient
	err     error
}

func (d *fakeDialer) dial(*config.Account) (*fakePoolClient, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	c := &fakePoolClient{id: len(d.clients) + 1}
	d.clients = append(d.clients, c)
	return c, nil
}

func (d *fakeDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.clients)
}

func (d *fakeDialer) client(i int) *fakePoolClient {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clients[i]
}

func newTestPool(t *testing.T, idleTimeout time.Duration) (*clientPool[*fakePoolClient], *fakeDialer) {
	t.Helper()
	d := &fakeDialer{}
	p := newClientPool(d.dial, idleTimeout)
	p.closed = (*fakePoolClient).isClosed
	t.Cleanup(p.closeIdle)
	return p, d
}

func testPoolAccount(id string) *config.Account {
	return &config.Account{ID: id, Email: id + "@example.com", ServiceProvider: config.ProviderCustom, IMAPServer: "imap.example.com", IMAPPort: 993}
}

func (p *clientPool[C]) idleCount(account *config.Account) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle[poolKey(account)])
}

func TestClientPoolReusesReturnedClient(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	acc := testPoolAccount("a")

	var used []*fakePoolClient
	for range 3 {
		if err := p.run(acc, func(c *fakePoolClient) error {
			used = append(used, c)
			return nil
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if d.count() != 1 {
		t.Fatalf("dialed %d clients, want 1", d.count())
	}
	if used[0] != used[1] || used[1] != used[2] {
		t.Fatalf("runs used clients %d, %d, %d, want the same client", used[0].id, used[1].id, used[2].id)
	}
	if used[0].isClosed() {
		t.Fatal("reused client was closed")
	}
	if p.idleCount(acc) != 1 {
		t.Fatalf("idle clients = %d, want 1", p.idleCount(acc))
	}
}

func TestClientPoolSeparatesAccounts(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	ok := func(*fakePoolClient) error { return nil }

	if err := p.run(testPoolAccount("a"), ok); err != nil {
		t.Fatal(err)
	}
	if err := p.run(testPoolAccount("b"), ok); err != nil {
		t.Fatal(err)
	}
	if d.count() != 2 {
		t.Fatalf("dialed %d clients, want one per account", d.count())
	}
}

func TestClientPoolKeepsAtMostTwoIdleClients(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	acc := testPoolAccount("a")

	const callers = 3
	var started sync.WaitGroup
	started.Add(callers)
	release := make(chan struct{})
	got := make([]*fakePoolClient, callers)
	errs := make(chan error, callers)

	var done sync.WaitGroup
	for i := range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			errs <- p.run(acc, func(c *fakePoolClient) error {
				got[i] = c
				started.Done()
				<-release
				return nil
			})
		}()
	}

	started.Wait()
	close(release)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if d.count() != callers {
		t.Fatalf("dialed %d clients, want %d (one per concurrent caller)", d.count(), callers)
	}
	if got[0] == got[1] || got[1] == got[2] || got[0] == got[2] {
		t.Fatal("two concurrent callers shared one client")
	}
	if p.idleCount(acc) != imapPoolMaxIdle {
		t.Fatalf("idle clients = %d, want %d", p.idleCount(acc), imapPoolMaxIdle)
	}
	closed := 0
	for i := range callers {
		if d.client(i).isClosed() {
			closed++
		}
	}
	if closed != callers-imapPoolMaxIdle {
		t.Fatalf("closed %d clients, want %d", closed, callers-imapPoolMaxIdle)
	}
}

func TestClientPoolRetriesOnNewClientAfterReusedClientFails(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	acc := testPoolAccount("a")

	if err := p.run(acc, func(*fakePoolClient) error { return nil }); err != nil {
		t.Fatal(err)
	}
	stale := d.client(0)

	var calls []int
	err := p.run(acc, func(c *fakePoolClient) error {
		calls = append(calls, c.id)
		if c == stale {
			return errors.New("connection reset")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run after retry: %v", err)
	}
	if len(calls) != 2 || calls[0] != stale.id || calls[1] == stale.id {
		t.Fatalf("fn ran on clients %v, want the pooled client then a new one", calls)
	}
	if !stale.isClosed() {
		t.Fatal("failed pooled client was not closed")
	}
	fresh := d.client(1)
	if fresh.isClosed() || p.idleCount(acc) != 1 {
		t.Fatalf("new client closed=%v idle=%d, want it back in the pool", fresh.isClosed(), p.idleCount(acc))
	}
}

func TestClientPoolDoesNotRetryTwice(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	acc := testPoolAccount("a")

	if err := p.run(acc, func(*fakePoolClient) error { return nil }); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("mailbox does not exist")
	calls := 0
	err := p.run(acc, func(*fakePoolClient) error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if calls != 2 || d.count() != 2 {
		t.Fatalf("fn calls = %d, dials = %d, want 2 and 2", calls, d.count())
	}
	if !d.client(0).isClosed() || !d.client(1).isClosed() {
		t.Fatal("failed clients were not closed")
	}
	if p.idleCount(acc) != 0 {
		t.Fatalf("idle clients = %d, want 0", p.idleCount(acc))
	}
}

func TestClientPoolNewClientFailureIsNotRetried(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	acc := testPoolAccount("a")

	wantErr := errors.New("search failed")
	calls := 0
	err := p.run(acc, func(*fakePoolClient) error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) || calls != 1 || d.count() != 1 {
		t.Fatalf("err = %v, calls = %d, dials = %d, want the error after one call on one client", err, calls, d.count())
	}
	if !d.client(0).isClosed() || p.idleCount(acc) != 0 {
		t.Fatal("failed new client was kept")
	}
}

func TestClientPoolReturnsDialError(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	d.err = errors.New("dial refused")

	called := false
	err := p.run(testPoolAccount("a"), func(*fakePoolClient) error {
		called = true
		return nil
	})
	if !errors.Is(err, d.err) || called {
		t.Fatalf("err = %v, fn called = %v, want dial error and no call", err, called)
	}
}

func TestClientPoolClosesIdleClientAfterTimeout(t *testing.T) {
	p, d := newTestPool(t, 10*time.Millisecond)
	acc := testPoolAccount("a")

	if err := p.run(acc, func(*fakePoolClient) error { return nil }); err != nil {
		t.Fatal(err)
	}
	first := d.client(0)

	deadline := time.Now().Add(2 * time.Second)
	for !first.isClosed() {
		if time.Now().After(deadline) {
			t.Fatal("idle client was not closed after the idle timeout")
		}
		time.Sleep(time.Millisecond)
	}
	if p.idleCount(acc) != 0 {
		t.Fatalf("idle clients = %d after timeout, want 0", p.idleCount(acc))
	}

	if err := p.run(acc, func(*fakePoolClient) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if d.count() != 2 {
		t.Fatalf("dialed %d clients, want a new one after the idle timeout", d.count())
	}
	if first.closed.Load() != 1 {
		t.Fatalf("idle client closed %d times, want 1", first.closed.Load())
	}
}

func TestClientPoolCloseIdle(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	ok := func(*fakePoolClient) error { return nil }
	if err := p.run(testPoolAccount("a"), ok); err != nil {
		t.Fatal(err)
	}
	if err := p.run(testPoolAccount("b"), ok); err != nil {
		t.Fatal(err)
	}

	p.closeIdle()

	if !d.client(0).isClosed() || !d.client(1).isClosed() {
		t.Fatal("closeIdle left a client open")
	}
	if p.idleCount(testPoolAccount("a")) != 0 || p.idleCount(testPoolAccount("b")) != 0 {
		t.Fatal("closeIdle left clients in the pool")
	}
}

func TestClientPoolKeepsClientWhenServerAnswersNo(t *testing.T) {
	p, d := newTestPool(t, time.Minute)
	account := testPoolAccount("a")
	if err := p.run(account, func(*fakePoolClient) error { return nil }); err != nil {
		t.Fatal(err)
	}
	calls := 0
	no := &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox does not exist"}
	err := p.run(account, func(*fakePoolClient) error { calls++; return no })
	if !errors.Is(err, no) || calls != 1 {
		t.Fatalf("err %v after %d calls; a NO reply must be returned without a second try", err, calls)
	}
	if d.count() != 1 || d.client(0).isClosed() || p.idleCount(account) != 1 {
		t.Fatalf("dialed %d, first closed %t, idle %d; the working client must stay in the pool", d.count(), d.client(0).isClosed(), p.idleCount(account))
	}
}

func TestClientPoolDialsAgainWhenPooledClientDoesNotAnswer(t *testing.T) {
	p, d := newTestPool(t, time.Minute)
	p.answers = func(c *fakePoolClient) bool { return c.id != 1 }
	account := testPoolAccount("a")
	p.run(account, func(*fakePoolClient) error { return nil }) //nolint:errcheck
	var used int
	if err := p.run(account, func(c *fakePoolClient) error { used = c.id; return nil }); err != nil {
		t.Fatal(err)
	}
	if used != 2 || !d.client(0).isClosed() {
		t.Fatalf("used client %d, first closed %t; a client that does not answer NOOP must be replaced", used, d.client(0).isClosed())
	}
}

func TestClientPoolDropsClientIdleTooLongByWallClock(t *testing.T) {
	p, d := newTestPool(t, time.Minute)
	account := testPoolAccount("a")
	p.run(account, func(*fakePoolClient) error { return nil }) //nolint:errcheck
	// As after a suspend: the timer has not fired, but the wall clock moved on.
	p.mu.Lock()
	p.idle[poolKey(account)][0].returned = time.Now().Add(-2 * time.Minute)
	p.mu.Unlock()
	var used int
	p.run(account, func(c *fakePoolClient) error { used = c.id; return nil }) //nolint:errcheck
	if used != 2 || !d.client(0).isClosed() {
		t.Fatalf("used client %d, first closed %t; a client unused past the timeout must not be reused", used, d.client(0).isClosed())
	}
}

func TestClientPoolLimitsConcurrentOperations(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	account := testPoolAccount("a")
	const callers = 9
	type operation struct {
		client *fakePoolClient
		finish chan struct{}
	}
	entered := make(chan operation, callers)
	errs := make(chan error, callers)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var active atomic.Int32
	var maximum atomic.Int32
	for range callers {
		go func() {
			errs <- p.run(account, func(c *fakePoolClient) error {
				n := active.Add(1)
				for previous := maximum.Load(); n > previous; previous = maximum.Load() {
					if maximum.CompareAndSwap(previous, n) {
						break
					}
				}
				op := operation{client: c, finish: make(chan struct{})}
				entered <- op
				select {
				case <-op.finish:
				case <-release:
				}
				active.Add(-1)
				return nil
			})
		}()
	}
	first := make([]operation, 0, 3)
	clients := make(map[*fakePoolClient]bool)
	for range 3 {
		select {
		case op := <-entered:
			first = append(first, op)
			clients[op.client] = true
		case <-time.After(2 * time.Second):
			t.Fatal("three operations did not start")
		}
	}
	if len(clients) != 3 {
		t.Fatal("concurrent operations shared a client")
	}
	select {
	case <-entered:
		t.Fatal("a fourth operation started before a client was released")
	case <-time.After(50 * time.Millisecond):
	}
	previous := first[0]
	for range callers - 3 {
		close(previous.finish)
		select {
		case next := <-entered:
			if next.client != previous.client {
				t.Error("waiting operation did not reuse the released client")
			}
			previous = next
		case <-time.After(2 * time.Second):
			t.Fatal("waiting operation did not start after release")
		}
	}
	close(previous.finish)
	close(first[1].finish)
	close(first[2].finish)
	for range callers {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("waiting operation did not finish")
		}
	}
	if maximum.Load() > 3 {
		t.Errorf("maximum concurrent operations = %d, want at most 3", maximum.Load())
	}
	if d.count() > 3 {
		t.Errorf("dialed %d clients, want waiting callers to reuse at most 3", d.count())
	}
}

func TestClientPoolSharesServerLoginAcrossAccountIDs(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	account := testPoolAccount("a")
	alias := *account
	alias.ID = "another-account"
	var first *fakePoolClient
	if err := p.run(account, func(c *fakePoolClient) error { first = c; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.run(&alias, func(c *fakePoolClient) error {
		if c != first {
			t.Error("the same server login used a different client for another account ID")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if d.count() != 1 {
		t.Errorf("dialed %d clients for one server login, want 1", d.count())
	}
}

func TestClientPoolSeparatesServerLogins(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	base := testPoolAccount("a")
	differentLogin := *base
	differentLogin.Email = "other@example.com"
	differentServer := *base
	differentServer.IMAPServer = "other.example.com"
	differentPort := *base
	differentPort.IMAPPort = 143
	for _, account := range []*config.Account{base, &differentLogin, &differentServer, &differentPort} {
		if err := p.run(account, func(*fakePoolClient) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if d.count() != 4 {
		t.Errorf("dialed %d clients for four server logins, want 4", d.count())
	}
}

type acquiredPoolClient struct {
	client  *fakePoolClient
	release func()
	err     error
}

func acquireTestPoolClient(t *testing.T, p *clientPool[*fakePoolClient], account *config.Account) acquiredPoolClient {
	t.Helper()
	result := make(chan acquiredPoolClient, 1)
	go func() {
		c, release, err := p.acquire(account)
		result <- acquiredPoolClient{client: c, release: release, err: err}
	}()
	select {
	case got := <-result:
		if got.release != nil {
			got.release = sync.OnceFunc(got.release)
			t.Cleanup(got.release)
		}
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("client acquisition did not finish")
		return acquiredPoolClient{}
	}
}

func TestClientPoolAcquisitionSharesLimitAcrossAccountIDs(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	account := testPoolAccount("a")
	alias := *account
	alias.ID = "alias"
	first := make([]acquiredPoolClient, 0, 3)
	clients := make(map[*fakePoolClient]bool)
	for range 3 {
		got := acquireTestPoolClient(t, p, account)
		if got.err != nil {
			t.Fatal(got.err)
		}
		first = append(first, got)
		clients[got.client] = true
	}
	if len(clients) != 3 {
		t.Fatal("concurrent acquisitions shared a client")
	}
	waiting := make(chan acquiredPoolClient, 1)
	go func() {
		c, release, err := p.acquire(&alias)
		waiting <- acquiredPoolClient{client: c, release: release, err: err}
	}()
	other := acquireTestPoolClient(t, p, testPoolAccount("b"))
	if other.err != nil {
		t.Fatalf("another account could not progress: %v", other.err)
	}
	other.release()
	select {
	case got := <-waiting:
		if got.release != nil {
			got.release()
		}
		t.Fatal("another account ID bypassed the same login's operation limit")
	case <-time.After(50 * time.Millisecond):
	}
	first[0].release()
	select {
	case got := <-waiting:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.release()
		if got.client != first[0].client {
			t.Error("waiting acquisition did not reuse the released client")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting acquisition did not start after release")
	}
	if d.count() != 4 {
		t.Errorf("dialed %d clients, want three for one login and one for the other", d.count())
	}
}

func TestClientPoolRunSharesAcquisitionLimit(t *testing.T) {
	p, _ := newTestPool(t, time.Hour)
	account := testPoolAccount("a")
	var first acquiredPoolClient
	for i := range 3 {
		got := acquireTestPoolClient(t, p, account)
		if got.err != nil {
			t.Fatal(got.err)
		}
		if i == 0 {
			first = got
		}
	}
	entered := make(chan *fakePoolClient, 1)
	done := make(chan error, 1)
	go func() {
		done <- p.run(account, func(c *fakePoolClient) error { entered <- c; return nil })
	}()
	select {
	case <-entered:
		t.Fatal("run bypassed the slots held by direct acquisitions")
	case <-time.After(50 * time.Millisecond):
	}
	first.release()
	select {
	case c := <-entered:
		if c != first.client {
			t.Error("run did not reuse the released client")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not start after a direct acquisition released its client")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not finish")
	}
}

func TestClientPoolAcquisitionReplacesUnresponsiveIdleClient(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	account := testPoolAccount("a")
	var checked atomic.Int32
	p.answers = func(c *fakePoolClient) bool {
		checked.Add(1)
		return c.id != 1
	}
	first := acquireTestPoolClient(t, p, account)
	if first.err != nil {
		t.Fatal(first.err)
	}
	first.release()
	second := acquireTestPoolClient(t, p, account)
	if second.err != nil {
		t.Fatal(second.err)
	}
	if second.client == first.client || !first.client.isClosed() || checked.Load() != 1 {
		t.Fatalf("reused=%t, old closed=%t, NOOP checks=%d; want one check and a replacement", second.client == first.client, first.client.isClosed(), checked.Load())
	}
	second.release()
	third := acquireTestPoolClient(t, p, account)
	if third.err != nil {
		t.Fatal(third.err)
	}
	if third.client != second.client || checked.Load() != 2 || d.count() != 2 {
		t.Fatalf("dials=%d, NOOP checks=%d; want a healthy idle client reused after NOOP", d.count(), checked.Load())
	}
}

func TestClientPoolAcquisitionDiscardsClosedClient(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	account := testPoolAccount("a")
	for range 5 {
		got := acquireTestPoolClient(t, p, account)
		if got.err != nil {
			t.Fatal(got.err)
		}
		got.client.Close() //nolint:errcheck
		got.release()
		if p.idleCount(account) != 0 {
			t.Fatal("release retained a closed client")
		}
	}
	if d.count() != 5 {
		t.Errorf("dialed %d clients, want one per closed acquisition", d.count())
	}
}

func TestClientPoolReleasesSlotsAfterDialFailure(t *testing.T) {
	p, _ := newTestPool(t, time.Hour)
	account := testPoolAccount("a")
	wantErr := errors.New("authentication failed")
	p.dial = func(*config.Account) (*fakePoolClient, error) { return nil, wantErr }
	for range 5 {
		got := acquireTestPoolClient(t, p, account)
		if !errors.Is(got.err, wantErr) || got.release != nil {
			t.Fatalf("err=%v, release present=%t; want auth error and no borrowed client", got.err, got.release != nil)
		}
	}
	dialer := &fakeDialer{}
	p.dial = dialer.dial
	got := acquireTestPoolClient(t, p, account)
	if got.err != nil {
		t.Fatalf("acquisition after failed auth: %v", got.err)
	}
}

func TestClientPoolReleasesSlotsAfterRunFailure(t *testing.T) {
	for _, dialFailure := range []bool{false, true} {
		name := "operation"
		if dialFailure {
			name = "authentication"
		}
		t.Run(name, func(t *testing.T) {
			p, _ := newTestPool(t, time.Hour)
			account := testPoolAccount("a")
			wantErr := errors.New("failed")
			if dialFailure {
				p.dial = func(*config.Account) (*fakePoolClient, error) { return nil, wantErr }
			}
			for range 5 {
				done := make(chan error, 1)
				go func() {
					done <- p.run(account, func(*fakePoolClient) error { return wantErr })
				}()
				select {
				case err := <-done:
					if !errors.Is(err, wantErr) {
						t.Fatalf("err=%v, want %v", err, wantErr)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("failed operation did not release its slot")
				}
			}
		})
	}
}

func TestClientPoolReleasesSlotsAfterNoopFailure(t *testing.T) {
	p, _ := newTestPool(t, time.Hour)
	account := testPoolAccount("a")
	p.answers = func(*fakePoolClient) bool { return false }
	for range 6 {
		got := acquireTestPoolClient(t, p, account)
		if got.err != nil {
			t.Fatal(got.err)
		}
		got.release()
	}
}

func TestClientPoolKeepsClientWhenServerAnswersBad(t *testing.T) {
	p, d := newTestPool(t, time.Hour)
	account := testPoolAccount("a")
	bad := &imap.Error{Type: imap.StatusResponseTypeBad, Text: "Command rejected"}
	for range 2 {
		calls := 0
		err := p.run(account, func(*fakePoolClient) error { calls++; return bad })
		if !errors.Is(err, bad) || calls != 1 {
			t.Fatalf("err=%v after %d calls, want BAD reply without a retry", err, calls)
		}
	}
	if d.count() != 1 || p.idleCount(account) != 1 || d.client(0).isClosed() {
		t.Fatal("server BAD reply discarded a usable connection")
	}
}
