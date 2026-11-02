package fetchcmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/floatpane/matcha/backend"
	"github.com/floatpane/matcha/config"
	"github.com/floatpane/matcha/fetcher"
	"github.com/floatpane/matcha/internal/httpclient"
	"github.com/floatpane/matcha/tui"
)

const (
	InitialEmailLimit = 0
	PaginationLimit   = 50
)

// RefreshEmails fetches the latest emails for all configured accounts.
func RefreshEmails(cfg *config.Config, mailbox tui.MailboxKind, counts map[string]int) tea.Cmd {
	return func() tea.Msg {
		emailsByAccount := make(map[string][]fetcher.Email)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, account := range cfg.Accounts {
			wg.Add(1)
			go func(acc config.Account) {
				defer wg.Done()
				var emails []fetcher.Email
				var err error

				limit := uint32(InitialEmailLimit)
				if counts != nil {
					if c, ok := counts[acc.ID]; ok && c > 0 {
						limit = uint32(c)
					}
				}

				if mailbox == tui.MailboxSent {
					emails, err = fetcher.FetchSentEmails(&acc, limit, 0)
				} else {
					emails, err = fetcher.FetchEmails(&acc, limit, 0)
				}
				if err != nil {
					log.Printf("Error fetching from %s: %v", acc.Email, err)
					return
				}
				mu.Lock()
				emailsByAccount[acc.ID] = emails
				mu.Unlock()
			}(account)
		}

		wg.Wait()
		return tui.EmailsRefreshedMsg{EmailsByAccount: emailsByAccount, Mailbox: mailbox}
	}
}

// SearchEmailsCmd searches backend providers across all matching accounts and
// returns the merged results. resolveProvider is the caller-supplied callback
// used to obtain a backend.Provider for each account; it is called from one
// goroutine per account, so it must be safe for concurrent use.
//
// All accounts are searched at the same time and share one timeout. Accounts
// whose backend does not support search are skipped. An error is reported
// only when no account succeeded; it is the error of the first failing
// account in configuration order.
func SearchEmailsCmd(cfg *config.Config, resolveProvider ProviderResolver, query backend.SearchQuery, folderName, accountID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), httpclient.IMAPSearchTimeout)
		defer cancel()

		var accounts []config.Account
		for _, acc := range cfg.Accounts {
			if accountID == "" || acc.ID == accountID {
				accounts = append(accounts, acc)
			}
		}

		// Each goroutine writes only its own slot, so no lock is needed.
		type accountResult struct {
			emails []backend.Email
			err    error
		}
		perAccount := make([]accountResult, len(accounts))
		var wg sync.WaitGroup
		for i := range accounts {
			wg.Go(func() {
				acc := &accounts[i]
				p := resolveProvider(acc)
				if p == nil {
					perAccount[i].err = fmt.Errorf("provider not found for account %s", acc.ID)
					return
				}
				perAccount[i].emails, perAccount[i].err = p.Search(ctx, folderName, query)
			})
		}
		wg.Wait()

		var results []fetcher.Email
		var firstErr error
		succeeded := false
		for _, r := range perAccount {
			if r.err != nil {
				if !errors.Is(r.err, backend.ErrNotSupported) && firstErr == nil {
					firstErr = r.err
				}
				continue
			}
			succeeded = true
			results = append(results, BackendEmailsToFetcher(r.emails)...)
		}
		if !succeeded && firstErr != nil {
			return tui.SearchResultsMsg{Query: query, Err: firstErr}
		}
		SortFetcherEmails(results)

		return tui.SearchResultsMsg{Query: query, Emails: results}
	}
}

// BackendEmailsToFetcher converts backend.Email values into the fetcher.Email
// type used by the TUI.
func BackendEmailsToFetcher(emails []backend.Email) []fetcher.Email {
	result := make([]fetcher.Email, len(emails))
	for i, e := range emails {
		result[i] = fetcher.Email{
			UID: e.UID, From: e.From, To: e.To, ReplyTo: e.ReplyTo,
			DeliveryRecipients: e.DeliveryRecipients,
			Subject:            e.Subject, Body: e.Body, Date: e.Date, ArrivalDate: e.ArrivalDate, IsRead: e.IsRead,
			MessageID: e.MessageID, References: e.References, AccountID: e.AccountID,
		}
	}
	return result
}

// SortFetcherEmails sorts emails newest-first, using UID as a tie-breaker.
func SortFetcherEmails(emails []fetcher.Email) {
	sort.Slice(emails, func(i, j int) bool {
		if emails[i].Date.Equal(emails[j].Date) {
			return emails[i].UID > emails[j].UID
		}
		return emails[i].Date.After(emails[j].Date)
	})
}

// FetchFoldersCmd fetches the folder list across all configured accounts.
func FetchFoldersCmd(cfg *config.Config) tea.Cmd {
	return func() tea.Msg {
		if !cfg.HasAccounts() {
			return nil
		}
		foldersByAccount := make(map[string][]fetcher.Folder)
		errsByAccount := make(map[string]error)
		seen := make(map[string]fetcher.Folder)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, account := range cfg.Accounts {
			wg.Add(1)
			go func(acc config.Account) {
				defer wg.Done()
				folders, err := fetcher.FetchFolders(&acc)
				if err != nil {
					mu.Lock()
					errsByAccount[acc.ID] = err
					mu.Unlock()
					return
				}
				mu.Lock()
				foldersByAccount[acc.ID] = folders
				for _, f := range folders {
					if _, ok := seen[f.Name]; !ok {
						seen[f.Name] = f
					}
				}
				mu.Unlock()
			}(account)
		}
		wg.Wait()

		var merged []fetcher.Folder
		for _, f := range seen {
			merged = append(merged, f)
		}

		return tui.FoldersFetchedMsg{
			FoldersByAccount: foldersByAccount,
			MergedFolders:    merged,
			Errors:           errsByAccount,
		}
	}
}

// AccountMayHaveFolder reports whether the account's cached folder list
// contains folderName. Without a cached list it assumes the folder exists.
func AccountMayHaveFolder(accountID, folderName string) bool {
	folders, _ := config.GetCachedFolders(accountID)
	return len(folders) == 0 || slices.Contains(folders, folderName)
}

// folderFetchRetryDelay is how long a failed account waits before its one retry.
var folderFetchRetryDelay = 2 * time.Second

// FetchFolderEmailsCmd fetches the first page of a folder across all accounts.
// An account whose fetch fails is retried once; if it fails again it is listed
// in FailedAccounts so the inbox can say so instead of showing it as empty.
// Accounts whose cached folder list lacks the folder are not reported.
func FetchFolderEmailsCmd(cfg *config.Config, folderName string, limit uint32) tea.Cmd {
	return func() tea.Msg {
		emailsByAccount := make(map[string][]fetcher.Email)
		failedAccounts := make(map[string]string)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, account := range cfg.Accounts {
			wg.Add(1)
			go func(acc config.Account) {
				defer wg.Done()
				// Accounts known to lack the folder fail as expected: no retry, no report.
				expectedToHaveFolder := AccountMayHaveFolder(acc.ID, folderName)
				emails, err := fetcher.FetchFolderEmails(&acc, folderName, limit, 0)
				if err != nil && expectedToHaveFolder {
					time.Sleep(folderFetchRetryDelay)
					emails, err = fetcher.FetchFolderEmails(&acc, folderName, limit, 0)
				}
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if expectedToHaveFolder {
						log.Printf("Fetching %s from %s failed: %v", folderName, acc.Email, err)
						failedAccounts[acc.ID] = err.Error()
					}
					return
				}
				emailsByAccount[acc.ID] = emails
			}(account)
		}

		wg.Wait()

		var allEmails []fetcher.Email
		for _, emails := range emailsByAccount {
			allEmails = append(allEmails, emails...)
		}
		sort.SliceStable(allEmails, func(i, j int) bool { return allEmails[i].Date.After(allEmails[j].Date) })

		return tui.FolderEmailsFetchedMsg{
			Emails:         allEmails,
			FolderName:     folderName,
			FailedAccounts: failedAccounts,
		}
	}
}

// FetchFolderEmailsPaginatedCmd fetches the next page of folder emails for a single account.
// An account whose cached folder list lacks the folder gets an empty page, which
// marks it as having no more emails, instead of a failing request.
func FetchFolderEmailsPaginatedCmd(account *config.Account, folderName string, limit, offset uint32) tea.Cmd {
	return func() tea.Msg {
		if !AccountMayHaveFolder(account.ID, folderName) {
			return tui.FolderEmailsAppendedMsg{AccountID: account.ID, FolderName: folderName}
		}
		emails, err := fetcher.FetchFolderEmails(account, folderName, limit, offset)
		if err != nil {
			return tui.FolderEmailsPageFailedMsg{FolderName: folderName, Err: err}
		}
		return tui.FolderEmailsAppendedMsg{
			Emails:     emails,
			AccountID:  account.ID,
			FolderName: folderName,
		}
	}
}

// FetchFolderEmailBodyCmd fetches the full body of a folder email.
func FetchFolderEmailBodyCmd(cfg *config.Config, uid uint32, accountID string, folderName string, mailbox tui.MailboxKind) tea.Cmd {
	return func() tea.Msg {
		account := cfg.GetAccountByID(accountID)
		if account == nil {
			return tui.EmailBodyFetchedMsg{UID: uid, AccountID: accountID, Mailbox: mailbox, Err: fmt.Errorf("account not found")}
		}

		body, bodyMIMEType, attachments, err := fetcher.FetchFolderEmailBody(account, folderName, uid)
		if err != nil {
			return tui.EmailBodyFetchedMsg{UID: uid, AccountID: accountID, Mailbox: mailbox, Err: err}
		}

		return tui.EmailBodyFetchedMsg{
			UID:          uid,
			Body:         body,
			BodyMIMEType: bodyMIMEType,
			Attachments:  attachments,
			AccountID:    accountID,
			Mailbox:      mailbox,
		}
	}
}

// FetchPreviewBodyCmd fetches a preview body for an email without a mailbox kind.
func FetchPreviewBodyCmd(cfg *config.Config, uid uint32, accountID string, folderName string) tea.Cmd {
	return func() tea.Msg {
		account := cfg.GetAccountByID(accountID)
		if account == nil {
			return tui.PreviewBodyFetchedMsg{UID: uid, AccountID: accountID, Err: fmt.Errorf("account not found")}
		}

		body, bodyMIMEType, attachments, err := fetcher.FetchFolderEmailBody(account, folderName, uid)
		if err != nil {
			return tui.PreviewBodyFetchedMsg{UID: uid, AccountID: accountID, Err: err}
		}

		return tui.PreviewBodyFetchedMsg{
			UID:          uid,
			Body:         body,
			BodyMIMEType: bodyMIMEType,
			Attachments:  attachments,
			AccountID:    accountID,
		}
	}
}

// MarkEmailAsReadCmd marks a single email as read in its folder mailbox.
func MarkEmailAsReadCmd(account *config.Account, uid uint32, accountID string, folderName string) tea.Cmd {
	return func() tea.Msg {
		err := fetcher.MarkEmailAsReadInMailbox(account, folderName, uid)
		return tui.EmailMarkedReadMsg{UID: uid, AccountID: accountID, Err: err}
	}
}

// MarkEmailAsUnreadCmd marks a single email as unread in its folder mailbox.
func MarkEmailAsUnreadCmd(account *config.Account, uid uint32, accountID string, folderName string) tea.Cmd {
	return func() tea.Msg {
		err := fetcher.MarkEmailAsUnreadInMailbox(account, folderName, uid)
		return tui.EmailMarkedUnreadMsg{UID: uid, AccountID: accountID, Err: err}
	}
}
