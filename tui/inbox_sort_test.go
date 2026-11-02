package tui

import (
	"github.com/floatpane/matcha/config"
	"github.com/floatpane/matcha/fetcher"
	"testing"
	"time"
)

func TestInboxSortByAccountDateChoice(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	emails := []fetcher.Email{
		{UID: 1, AccountID: "arrival", Date: time.Unix(0, 0), ArrivalDate: day.Add(time.Hour)},
		{UID: 2, AccountID: "header", Date: day, ArrivalDate: day.Add(2 * time.Hour)},
		{UID: 3, AccountID: "arrival", Date: day.Add(-time.Hour)},
	}
	accounts := []config.Account{{ID: "arrival", SortBy: "arrival_date"}, {ID: "header"}}
	inbox := NewInbox(emails, accounts)
	got := inbox.displayEmails()
	for i, uid := range []uint32{1, 2, 3} {
		if got[i].UID != uid {
			t.Fatalf("position %d: got %d want %d", i, got[i].UID, uid)
		}
	}
	if !got[0].Date.Equal(time.Unix(0, 0)) {
		t.Fatal("sorting changed displayed date")
	}
	inbox.SetAccountSorting([]config.Account{{ID: "arrival"}, {ID: "header"}})
	if !inbox.sortTime(emails[0]).Equal(emails[0].Date) {
		t.Fatal("choice did not update")
	}
	inbox.searchActive = true
	inbox.searchResults = []fetcher.Email{emails[1], emails[0]}
	inbox.SetAccountSorting(accounts)
	if got := inbox.displayEmails(); got[0].UID != 1 {
		t.Fatal("search ignored account sorting")
	}
}

func TestAccountSortingKeepsSelectedMessage(t *testing.T) {
	day := time.Now()
	accounts := []config.Account{{ID: "a"}}
	inbox := NewInbox([]fetcher.Email{{UID: 1, AccountID: "a", Date: day, ArrivalDate: day.Add(-time.Hour)}, {UID: 2, AccountID: "a", Date: day.Add(-time.Hour), ArrivalDate: day}}, accounts)
	inbox.SetAccountSorting([]config.Account{{ID: "a", SortBy: "arrival_date"}})
	selected := inbox.GetSelectedEmail()
	if selected == nil || selected.UID != 1 {
		t.Fatal("sorting moved selection to another message")
	}
}

func BenchmarkInboxArrivalSorting(b *testing.B) {
	day := time.Now()
	emails := make([]fetcher.Email, 10000)
	for i := range emails {
		emails[i] = fetcher.Email{UID: uint32(i + 1), AccountID: "a", Date: day, ArrivalDate: day.Add(time.Duration(i) * time.Second)}
	}
	inbox := &Inbox{accounts: []config.Account{{ID: "a", SortBy: "arrival_date"}}, allEmails: emails}
	b.ResetTimer()
	for b.Loop() {
		inbox.displayEmails()
	}
}

func TestThreadOrderUsesAccountSortChoice(t *testing.T) {
	day := time.Now()
	inbox := NewInbox([]fetcher.Email{
		{UID: 1, AccountID: "a", MessageID: "one@example.com", Subject: "first", Date: time.Unix(0, 0), ArrivalDate: day},
		{UID: 2, AccountID: "a", MessageID: "two@example.com", Subject: "second", Date: day.Add(-time.Hour), ArrivalDate: day.Add(-time.Hour)},
	}, []config.Account{{ID: "a", SortBy: "arrival_date"}})
	inbox.threaded[inbox.folderKey()] = true
	items := inbox.itemsForEmails(inbox.displayEmails(), false)
	if len(items) != 2 || items[0].(item).uid != 1 {
		t.Fatalf("thread order ignored arrival date: %+v", items)
	}
}
