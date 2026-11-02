package fetcher

import (
	"github.com/emersion/go-imap/v2"
	"github.com/floatpane/matcha/backend"
	"github.com/floatpane/matcha/config"
	"testing"
	"time"
)

func TestIMAPArrivalDateSurvivesFetchAndSearch(t *testing.T) {
	c := memIMAP(t)
	arrival := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	raw := "From: sender@example.com\r\nTo: me@example.com\r\nSubject: arrival\r\nDate: Thu, 01 Jan 1970 00:00:00 +0000\r\n\r\nbody"
	cmd := c.Append("INBOX", int64(len(raw)), &imap.AppendOptions{Time: arrival})
	if _, err := cmd.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	account := &config.Account{ID: "a", Email: "me@example.com"}
	headers, _, err := scanFolderHeaders(c, account, "INBOX", FolderHeaders{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	check := func(emails []Email) {
		t.Helper()
		if len(emails) != 1 || !emails[0].ArrivalDate.Equal(arrival) || !emails[0].Date.Equal(time.Unix(0, 0)) {
			t.Fatalf("incorrect email dates: %+v", emails)
		}
	}
	check(headers.Emails)
	found, err := searchWithClient(c, account, "INBOX", backend.SearchQuery{Subject: "arrival"})
	if err != nil {
		t.Fatal(err)
	}
	check(found)
	// Old local metadata is fetched again so it gains the server timestamp.
	headers.Emails[0].ArrivalDate = time.Time{}
	updated, _, err := scanFolderHeaders(c, account, "INBOX", headers, 10)
	if err != nil {
		t.Fatal(err)
	}
	check(updated.Emails)
}
