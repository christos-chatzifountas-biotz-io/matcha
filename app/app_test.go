package app

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/floatpane/matcha/config"
	"github.com/floatpane/matcha/fetcher"
	"github.com/floatpane/matcha/internal/emailstore"
	"github.com/floatpane/matcha/tui"
)

func TestOpeningUnreadEmailUpdatesVisibleFilter(t *testing.T) {
	accounts := []config.Account{{ID: "a", FetchEmail: "a@example.com"}}
	email := fetcher.Email{UID: 7, AccountID: "a", Subject: "unread"}
	store := emailstore.NewStore()
	store.SetFolder("INBOX", []fetcher.Email{email})
	folders := tui.NewFolderInbox([]string{"INBOX"}, accounts)
	folders.SetEmails([]fetcher.Email{email}, accounts)
	folders.Update(tea.KeyPressMsg{Code: 'U', Text: "U"})
	m := &Model{config: &config.Config{Accounts: accounts}, store: store, folderInbox: folders}
	m.markEmailReadAndQueue("a", 7, false)

	if selected := folders.GetInbox().GetSelectedEmail(); selected != nil {
		t.Fatalf("opened message remains visible in unread view: %+v", selected)
	}
	if got := store.GetEmailByUIDAndAccount(7, "a"); got == nil || !got.IsRead {
		t.Fatalf("opened message was not marked read in the store: %+v", got)
	}
}
