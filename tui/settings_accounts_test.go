package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/floatpane/matcha/config"
	"github.com/floatpane/matcha/fetcher"
	"github.com/floatpane/matcha/internal/headerindex"
)

func TestAccountSortingSavesOnlySelectedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := &config.Config{Accounts: []config.Account{{ID: "first"}, {ID: "second"}}}
	settings := NewSettings(cfg)
	settings.accountsCursor = 1
	for _, want := range []string{"arrival_date", "email_date"} {
		_, cmd := settings.updateAccounts(tea.KeyPressMsg{Code: 't', Text: "t"})
		if cmd == nil {
			t.Fatal("sorting change did not save")
		}
		if _, ok := cmd().(ConfigSavedMsg); !ok {
			t.Fatalf("sorting change returned %T", cmd())
		}
		saved, err := config.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if saved.Accounts[0].SortBy != "" || saved.Accounts[1].SortBy != want {
			t.Fatalf("saved account sorting = %q, %q", saved.Accounts[0].SortBy, saved.Accounts[1].SortBy)
		}
	}
	settings.accountsCursor = len(cfg.Accounts)
	if _, cmd := settings.updateAccounts(tea.KeyPressMsg{Code: 't', Text: "t"}); cmd != nil {
		t.Fatal("add-account row changed sorting")
	}
}

func TestAccountSortingRestoresChoiceWhenSaveFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.WriteFile(filepath.Join(home, ".config"), []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Accounts: []config.Account{{ID: "one", SortBy: "email_date"}}}
	settings := NewSettings(cfg)
	_, cmd := settings.updateAccounts(tea.KeyPressMsg{Code: 't', Text: "t"})
	if cfg.Accounts[0].SortBy != "email_date" {
		t.Fatal("failed save changed sorting")
	}
	if _, ok := cmd().(NotifyMsg); !ok {
		t.Fatalf("failed save returned %T", cmd())
	}
}

func TestEnterOpensAccountSettings(t *testing.T) {
	settings := NewSettings(&config.Config{Accounts: []config.Account{{ID: "duth", Email: "chrichat57"}}})
	_, cmd := settings.updateAccounts(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter did not open account settings")
	}
	if msg, ok := cmd().(GoToEditAccountMsg); !ok || msg.AccountID != "duth" {
		t.Fatalf("enter returned %#v", cmd())
	}
	settings.accountsCursor = 1
	_, cmd = settings.updateAccounts(tea.KeyPressMsg{Code: tea.KeyEnter})
	if _, ok := cmd().(GoToAddAccountMsg); !ok {
		t.Fatal("enter did not open add account")
	}
}

func TestSuggestedAddressesSaveAndIgnore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := &config.Config{Accounts: []config.Account{
		{
			ID: "a", Email: "me@example.com", FetchEmail: "me@example.com",
			ExtraAddresses:        []string{"existing@example.com"},
			IgnoredAddresses:      []string{"ignored@example.com"},
			OtherAccountAddresses: []string{"other@example.com"},
		},
		{ID: "b", Email: "other@example.com"},
	}}
	settings := NewSettings(cfg)
	settings.activePane = PaneContent
	settings.activeCategory = CategoryAccounts
	settings.SetHiddenSearcher(fakeAddressSuggestionIndex{
		{Address: "me@example.com", Count: 8},
		{Address: "other@example.com", Count: 7},
		{Address: "ignored@example.com", Count: 6},
		{Address: "existing@example.com", Count: 5},
		{Address: "guest@example.com", Count: 4},
		{Address: "third@example.com", Count: 3},
	})

	model, _ := settings.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	settings = model.(*Settings)
	if !settings.suggestionsOpen || len(settings.suggestedAddresses) != 2 || settings.suggestedAddresses[0].Address != "guest@example.com" {
		t.Fatalf("suggestions = %+v", settings.suggestedAddresses)
	}
	model, cmd := settings.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	settings = model.(*Settings)
	if cmd == nil {
		t.Fatal("adding an address did not save config")
	}
	if _, ok := cmd().(ConfigSavedMsg); !ok {
		t.Fatalf("add result = %T, want ConfigSavedMsg", cmd())
	}
	if !slices.Contains(cfg.Accounts[0].ExtraAddresses, "guest@example.com") {
		t.Fatalf("extra addresses = %v", cfg.Accounts[0].ExtraAddresses)
	}

	model, cmd = settings.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	settings = model.(*Settings)
	if cmd == nil {
		t.Fatal("ignoring an address did not save config")
	}
	if _, ok := cmd().(ConfigSavedMsg); !ok {
		t.Fatalf("ignore result = %T, want ConfigSavedMsg", cmd())
	}
	if !slices.Contains(cfg.Accounts[0].IgnoredAddresses, "third@example.com") {
		t.Fatalf("ignored addresses = %v", cfg.Accounts[0].IgnoredAddresses)
	}
	saved, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(saved.Accounts[0].ExtraAddresses, "guest@example.com") || !slices.Contains(saved.Accounts[0].IgnoredAddresses, "third@example.com") {
		t.Fatalf("saved suggestions not in config: %+v", saved.Accounts[0])
	}

	model, _ = settings.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	settings = model.(*Settings)
	if settings.suggestionsOpen {
		t.Fatal("Escape did not return to the accounts list")
	}
}

func TestSuggestedAddressesShowsFirst15AfterFiltering(t *testing.T) {
	var rows fakeAddressSuggestionIndex
	for i := range 20 {
		rows = append(rows, headerindex.AddressCount{Address: "person" + strconv.Itoa(i) + "@example.com", Count: 20 - i})
	}
	settings := NewSettings(&config.Config{Accounts: []config.Account{{ID: "a"}}})
	settings.SetHiddenSearcher(rows)
	settings.openAddressSuggestions(0)
	if len(settings.suggestedAddresses) != 15 || settings.suggestedAddresses[0].Address != "person0@example.com" || settings.suggestedAddresses[14].Address != "person14@example.com" {
		t.Fatalf("suggestions = %+v", settings.suggestedAddresses)
	}
}

type fakeAddressSuggestionIndex []headerindex.AddressCount

func (fakeAddressSuggestionIndex) HiddenCount(string, []string) int        { return 0 }
func (fakeAddressSuggestionIndex) Hidden(string, []string) []fetcher.Email { return nil }
func (f fakeAddressSuggestionIndex) HiddenRecipients(string, string) []headerindex.AddressCount {
	return f
}
