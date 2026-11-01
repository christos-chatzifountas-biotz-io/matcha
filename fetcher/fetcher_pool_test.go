package fetcher

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/floatpane/matcha/config"
)

func TestFetchSentEmailsSharesPoolWithoutNestedAcquisition(t *testing.T) {
	user := imapmemserver.NewUser("me@example.com", "isolated-password")
	if err := user.Create("Outgoing", nil); err != nil {
		t.Fatal(err)
	}
	raw := "From: Me <me@example.com>\r\nTo: recipient@example.net\r\nSubject: pooled sent mail\r\nDate: Mon, 19 Oct 2026 10:00:00 +0000\r\nMessage-ID: <pooled@example.com>\r\n\r\nbody\r\n"
	literal := &poolTestLiteral{Reader: strings.NewReader(raw), size: int64(len(raw))}
	if _, err := user.Append("Outgoing", literal, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{newTestTLSCertificate(t)},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	counted := &poolTestListener{Listener: listener}
	discoveryStarted := make(chan struct{}, 3)
	continueDiscovery := make(chan struct{})
	var openDiscovery sync.Once
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &poolSentSession{
				UserSession:       imapmemserver.NewUserSession(user),
				discoveryStarted:  discoveryStarted,
				continueDiscovery: continueDiscovery,
			}, nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		InsecureAuth: true,
	})
	go server.Serve(counted) //nolint:errcheck
	t.Cleanup(func() {
		openDiscovery.Do(func() { close(continueDiscovery) })
		CloseIMAPPool()
		server.Close() //nolint:errcheck
	})
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	account := &config.Account{
		ID: "pooled-sent-test", Email: "me@example.com", Password: "isolated-password",
		ServiceProvider: config.ProviderCustom, IMAPServer: host, IMAPPort: port,
		Insecure: true, SC: &config.SessionCache{},
	}

	const callers = 12
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			emails, err := FetchSentEmails(account, 10, 0)
			if err == nil && (len(emails) != 1 || emails[0].Subject != "pooled sent mail" || emails[0].UID != 1) {
				err = fmt.Errorf("sent emails = %+v, want the seeded message", emails)
			}
			results <- err
		}()
	}
	close(start)

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	// Hold the first three discovery clients together. Keeping any of them
	// while calling FetchMailboxEmails would leave all three slots occupied.
	for range 3 {
		select {
		case <-discoveryStarted:
		case <-deadline.C:
			t.Fatal("three concurrent Sent mailbox discoveries did not start")
		}
	}
	openDiscovery.Do(func() { close(continueDiscovery) })
	for range callers {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("FetchSentEmails: %v", err)
			}
		case <-deadline.C:
			t.Fatal("parallel Sent fetches did not complete; discovery clients may still hold pool slots")
		}
	}
	counted.mu.Lock()
	peak := counted.peak
	counted.mu.Unlock()
	if peak != 3 {
		t.Fatalf("peak IMAP sockets = %d, want 3", peak)
	}
}

type poolTestLiteral struct {
	*strings.Reader
	size int64
}

func (r *poolTestLiteral) Size() int64 { return r.size }

type poolSentSession struct {
	*imapmemserver.UserSession
	discoveryStarted  chan<- struct{}
	continueDiscovery <-chan struct{}
	firstList         sync.Once
}

func (s *poolSentSession) List(w *imapserver.ListWriter, _ string, _ []string, _ *imap.ListOptions) error {
	s.firstList.Do(func() {
		select {
		case s.discoveryStarted <- struct{}{}:
		default:
		}
		<-s.continueDiscovery
	})
	return w.WriteList(&imap.ListData{
		Attrs: []imap.MailboxAttr{imap.MailboxAttrSent}, Delim: '/', Mailbox: "Outgoing",
	})
}

type poolTestListener struct {
	net.Listener
	mu     sync.Mutex
	active int
	peak   int
}

func (l *poolTestListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.active++
	l.peak = max(l.peak, l.active)
	l.mu.Unlock()
	return &poolTestConn{Conn: conn, listener: l}, nil
}

type poolTestConn struct {
	net.Conn
	listener *poolTestListener
	once     sync.Once
}

func (c *poolTestConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.listener.mu.Lock()
		c.listener.active--
		c.listener.mu.Unlock()
	})
	return err
}
