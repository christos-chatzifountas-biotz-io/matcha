// Package headerindex keeps a copy of email headers (sender, recipients,
// subject, date, read state) on this computer and searches it with fuzzy
// matching, so search results appear while typing and partial words match on
// every provider. Bodies are not copied; body search stays on the server.
package headerindex

import (
	"cmp"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/floatpane/matcha/backend"
	"github.com/floatpane/matcha/config"
	"github.com/floatpane/matcha/fetcher"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
	"net/mail"
)

type folderKey struct {
	accountID string
	folder    string
}

// entry is one indexed email with its fields lowercased and stripped of
// accents, so "Ενημέρωση" matches "ενημερωση" and "Café" matches "cafe".
type entry struct {
	email   fetcher.Email
	from    string
	to      string
	subject string
}

type folderIndex struct {
	headers fetcher.FolderHeaders
	entries []entry
}

// saveEverySteps is how often a running sync writes its progress to disk.
// Each save rewrites the whole folder file, so a large first sync saves every
// tenth step and at the end instead of after each step.
const saveEverySteps = 10

// syncRun is the state of one running sync of a folder: the UIDs the scan
// found missing, still to be fetched, and how many steps ran.
type syncRun struct {
	scanned bool
	missing []uint32
	steps   int
}

// Index holds the loaded header copies of every account folder that was
// searched. It is safe for concurrent use: syncing runs in background
// commands while the search box reads it.
type Index struct {
	mu       sync.RWMutex
	folders  map[folderKey]*folderIndex
	syncing  map[folderKey]*syncRun
	finished map[folderKey]time.Time
	accounts map[string]*config.Account
}

func New() *Index {
	return &Index{
		folders:  make(map[folderKey]*folderIndex),
		syncing:  make(map[folderKey]*syncRun),
		finished: make(map[folderKey]time.Time),
		accounts: make(map[string]*config.Account),
	}
}

// Load reads the saved headers of one account folder from disk, once. A file
// that cannot be read or decoded (for example cut short by a crash) is logged
// and treated as empty, so the next sync rebuilds and overwrites it.
func (x *Index) Load(accountID, folder string) {
	key := folderKey{accountID, folder}
	x.mu.RLock()
	_, loaded := x.folders[key]
	x.mu.RUnlock()
	if loaded {
		return
	}
	saved, err := config.LoadHeaderIndex(accountID, folder)
	if err != nil {
		log.Printf("header index: starting %s of %s over: %v", folder, accountID, err)
		saved = &config.HeaderIndex{}
	}
	headers := fetcher.FolderHeaders{UIDValidity: saved.UIDValidity, AddressSettings: saved.AddressSettings}
	for _, cached := range saved.Emails {
		headers.Emails = append(headers.Emails, emailFromCache(cached))
	}
	for _, cached := range saved.HiddenEmails {
		headers.Hidden = append(headers.Hidden, emailFromCache(cached))
	}
	x.mu.Lock()
	if _, loaded := x.folders[key]; !loaded {
		x.folders[key] = newFolderIndex(headers)
	}
	x.mu.Unlock()
}

// StartSync marks one account folder as syncing. It returns false when a sync
// of that folder is already running, so two never fetch the same headers, or
// when the last sync finished less than wait ago.
func (x *Index) StartSync(accountID, folder string, wait time.Duration) bool {
	key := folderKey{accountID, folder}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.syncing[key] != nil || time.Since(x.finished[key]) < wait {
		return false
	}
	x.syncing[key] = &syncRun{}
	return true
}

// FinishSync clears the mark set by StartSync.
func (x *Index) FinishSync(accountID, folder string) {
	key := folderKey{accountID, folder}
	x.mu.Lock()
	delete(x.syncing, key)
	x.finished[key] = time.Now()
	x.mu.Unlock()
}

// Syncing reports whether any of the account folders is being synced.
func (x *Index) Syncing(folder string, accountIDs []string) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	for _, id := range accountIDs {
		if x.syncing[folderKey{id, folder}] != nil {
			return true
		}
	}
	return false
}

// SyncStep runs one step of the sync of an account folder and returns how
// many headers still wait. The first step of a run scans the whole folder
// (deleted mail, read state, which UIDs are missing); later steps only fetch
// the next maxNew missing headers. The caller must hold the StartSync mark.
func (x *Index) SyncStep(account *config.Account, folder string, maxNew int) (int, error) {
	x.mu.Lock()
	x.accounts[account.ID] = accountForAddressChecks(account)
	x.mu.Unlock()
	x.Load(account.ID, folder)
	key := folderKey{account.ID, folder}
	x.mu.RLock()
	known := x.folders[key].headers
	run := x.syncing[key]
	x.mu.RUnlock()
	if run == nil {
		return 0, errors.New("header index: SyncStep without StartSync")
	}

	var updated fetcher.FolderHeaders
	var missing []uint32
	var err error
	if run.scanned {
		updated, missing, err = fetcher.FetchMissingHeaders(account, folder, known, run.missing, maxNew)
	}
	if !run.scanned || errors.Is(err, fetcher.ErrFolderNeedsNewScan) {
		updated, missing, err = fetcher.SyncFolderHeaders(account, folder, known, maxNew)
		run.steps = 0
	}
	if err != nil {
		return 0, err
	}
	firstStep := !run.scanned || run.steps == 0
	run.scanned, run.missing = true, missing
	run.steps++

	if firstStep || len(missing) == 0 || run.steps%saveEverySteps == 0 {
		saved := &config.HeaderIndex{UIDValidity: updated.UIDValidity, AddressSettings: updated.AddressSettings}
		for _, email := range updated.Emails {
			saved.Emails = append(saved.Emails, emailToCache(email))
		}
		for _, email := range updated.Hidden {
			saved.HiddenEmails = append(saved.HiddenEmails, emailToCache(email))
		}
		if err := config.SaveHeaderIndex(account.ID, folder, saved); err != nil {
			return 0, err
		}
	}

	x.mu.Lock()
	if previous := x.folders[key]; firstStep || previous == nil {
		x.folders[key] = newFolderIndex(updated)
	} else {
		// Later steps only add headers, so only the new ones are folded.
		x.folders[key] = previous.withAdded(updated)
	}
	x.mu.Unlock()
	return len(missing), nil
}

// Count returns how many emails of the folder are indexed for the accounts.
func (x *Index) Count(folder string, accountIDs []string) int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	total := 0
	for _, id := range accountIDs {
		if index := x.folders[folderKey{id, folder}]; index != nil {
			total += len(index.entries)
		}
	}
	return total
}

// SetAccount updates address ownership used when counting hidden recipients.
func (x *Index) SetAccount(account *config.Account) {
	if account == nil {
		return
	}
	x.mu.Lock()
	x.accounts[account.ID] = accountForAddressChecks(account)
	x.mu.Unlock()
}

// HiddenCount returns the hidden mail count for the chosen accounts.
func (x *Index) HiddenCount(folder string, accountIDs []string) int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	total := 0
	for _, id := range accountIDs {
		if index := x.folders[folderKey{id, folder}]; index != nil {
			total += len(index.headers.Hidden)
		}
	}
	return total
}

// Hidden returns hidden mail newest first for the chosen accounts.
func (x *Index) Hidden(folder string, accountIDs []string) []fetcher.Email {
	x.mu.RLock()
	defer x.mu.RUnlock()
	var emails []fetcher.Email
	for _, id := range accountIDs {
		if index := x.folders[folderKey{id, folder}]; index != nil {
			emails = append(emails, index.headers.Hidden...)
		}
	}
	slices.SortFunc(emails, func(a, b fetcher.Email) int {
		if order := b.Date.Compare(a.Date); order != 0 {
			return order
		}
		if order := cmp.Compare(b.UID, a.UID); order != 0 {
			return order
		}
		return cmp.Compare(a.AccountID, b.AccountID)
	})
	return emails
}

type AddressCount struct {
	Address string
	Count   int
}

// HiddenRecipients counts To and Cc recipients in hidden mail, excluding this
// account's configured addresses.
func (x *Index) HiddenRecipients(accountID, folder string) []AddressCount {
	x.mu.RLock()
	defer x.mu.RUnlock()
	index := x.folders[folderKey{accountID, folder}]
	if index == nil {
		return nil
	}
	account := x.accounts[accountID]
	counts := make(map[string]int)
	for _, email := range index.headers.Hidden {
		seen := make(map[string]struct{}, len(email.To))
		for _, recipient := range email.To {
			address := bareRecipientAddress(recipient)
			if address == "" || fetcher.IsOwnAddress(account, address) {
				continue
			}
			if _, duplicate := seen[address]; duplicate {
				continue
			}
			seen[address] = struct{}{}
			counts[address]++
		}
	}
	recipients := make([]AddressCount, 0, len(counts))
	for address, count := range counts {
		recipients = append(recipients, AddressCount{Address: address, Count: count})
	}
	slices.SortFunc(recipients, func(a, b AddressCount) int {
		if order := cmp.Compare(b.Count, a.Count); order != 0 {
			return order
		}
		return cmp.Compare(a.Address, b.Address)
	})
	return recipients
}

func bareRecipientAddress(value string) string {
	if parsed, err := mail.ParseAddress(value); err == nil {
		value = parsed.Address
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "<>"))
}

func accountForAddressChecks(account *config.Account) *config.Account {
	copy := *account
	copy.ExtraAddresses = slices.Clone(account.ExtraAddresses)
	return &copy
}

// CanSearch reports whether the query only uses fields the local copy has.
// Body text and message size are not copied, so those need the server.
func CanSearch(query backend.SearchQuery) bool {
	hasTerms := query.Text != "" || query.From != "" || query.To != "" || query.Subject != ""
	return hasTerms && query.Body == "" && query.LargerThan == 0
}

// CanSearch is the package CanSearch, so the search box can ask the index.
func (x *Index) CanSearch(query backend.SearchQuery) bool { return CanSearch(query) }

type scored struct {
	entry *entry
	score int
}

// Search returns the indexed emails of the folder and accounts that match the
// query, best match first, at most limit. Every word must match: bare words
// may match the sender, the recipients or the subject; from:, to: and
// subject: words must match that field. Dates filter as on the server.
func (x *Index) Search(query backend.SearchQuery, folder string, accountIDs []string, limit int) []fetcher.Email {
	if !CanSearch(query) {
		return nil
	}
	x.mu.RLock()
	var candidates []*entry
	for _, id := range accountIDs {
		index := x.folders[folderKey{id, folder}]
		if index == nil {
			continue
		}
		for i := range index.entries {
			e := &index.entries[i]
			if !query.Since.IsZero() && e.email.Date.Before(query.Since) {
				continue
			}
			if !query.Before.IsZero() && !e.email.Date.Before(query.Before) {
				continue
			}
			candidates = append(candidates, e)
		}
	}
	x.mu.RUnlock()

	scores := make([]int, len(candidates))
	alive := make([]bool, len(candidates))
	for i := range alive {
		alive[i] = true
	}
	fieldTerms := []struct {
		text   string
		fields fieldSet
	}{
		{query.Text, fromField | toField | subjectField},
		{query.From, fromField},
		{query.To, toField},
		{query.Subject, subjectField},
	}
	for _, ft := range fieldTerms {
		for _, word := range strings.Fields(fold(ft.text)) {
			term := []rune(word)
			for i, e := range candidates {
				if !alive[i] {
					continue
				}
				best, ok := e.bestScore(word, term, ft.fields)
				if !ok {
					alive[i] = false
					continue
				}
				scores[i] += best
			}
		}
	}

	var matches []scored
	for i, e := range candidates {
		if alive[i] {
			matches = append(matches, scored{e, scores[i]})
		}
	}
	slices.SortFunc(matches, func(a, b scored) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return b.entry.email.Date.Compare(a.entry.email.Date)
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	results := make([]fetcher.Email, len(matches))
	for i, m := range matches {
		results[i] = m.entry.email
	}
	return results
}

type fieldSet uint8

const (
	fromField fieldSet = 1 << iota
	toField
	subjectField
)

// bestScore returns the best matchScore of the word over the chosen fields.
func (e *entry) bestScore(word string, term []rune, fields fieldSet) (int, bool) {
	best, found := 0, false
	for _, f := range [...]struct {
		set   fieldSet
		value string
	}{{fromField, e.from}, {toField, e.to}, {subjectField, e.subject}} {
		if fields&f.set == 0 {
			continue
		}
		if score, ok := matchScore(word, term, f.value); ok && (!found || score > best) {
			best, found = score, true
		}
	}
	return best, found
}

// matchScore scores how well a search word matches a field; both are folded.
// The word found as it is scores highest, more at the start of a word and
// nearer the start of the field. Otherwise its letters must appear in order
// inside one word of the field, spread over at most twice the word length
// plus two runes: "vrfctn" finds "verification", but "verif" does not match
// letters spread over "very nice offer". Fewer skipped letters score higher.
func matchScore(word string, term []rune, field string) (int, bool) {
	if len(term) == 0 {
		return 0, true
	}
	if at := strings.Index(field, word); at >= 0 {
		score := 1000 - min(utf8.RuneCountInString(field[:at]), 100)
		if startsWord(field, at) {
			score += 500
		}
		return score, true
	}
	maxSpan := 2*len(term) + 2
	bestSpan, bestAtWordStart := 0, false
	for start, r := range field {
		if r != term[0] {
			continue
		}
		matched, span := 1, 1
		for _, next := range field[start+utf8.RuneLen(r):] {
			if matched == len(term) || span >= maxSpan || !isWordRune(next) {
				break
			}
			span++
			if next == term[matched] {
				matched++
			}
		}
		if matched == len(term) && (bestSpan == 0 || span < bestSpan) {
			bestSpan, bestAtWordStart = span, startsWord(field, start)
		}
	}
	if bestSpan == 0 {
		return 0, false
	}
	score := 500 - 20*(bestSpan-len(term))
	if bestAtWordStart {
		score += 100
	}
	return score, true
}

// startsWord reports whether the rune at byte offset at begins a word.
func startsWord(s string, at int) bool {
	if at == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(s[:at])
	return !isWordRune(prev)
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func newFolderIndex(headers fetcher.FolderHeaders) *folderIndex {
	index := &folderIndex{headers: headers, entries: make([]entry, len(headers.Emails))}
	for i, email := range headers.Emails {
		index.entries[i] = newEntry(email)
	}
	return index
}

// withAdded returns the index for updated, which holds the headers of f plus
// new ones at the end; only the new ones are folded. f stays unchanged for
// searches still reading it.
func (f *folderIndex) withAdded(updated fetcher.FolderHeaders) *folderIndex {
	entries := slices.Clip(f.entries)
	for _, email := range updated.Emails[len(f.entries):] {
		entries = append(entries, newEntry(email))
	}
	return &folderIndex{headers: updated, entries: entries}
}

func newEntry(email fetcher.Email) entry {
	return entry{
		email:   email,
		from:    fold(email.From),
		to:      fold(strings.Join(email.To, " ")),
		subject: fold(email.Subject),
	}
}

var stripAccents = runes.Remove(runes.In(unicode.Mn))

// fold lowercases s and removes accents.
func fold(s string) string {
	folded, _, err := transform.String(transform.Chain(norm.NFD, stripAccents, norm.NFC), s)
	if err != nil {
		folded = s
	}
	return strings.ToLower(folded)
}

func emailFromCache(c config.CachedEmail) fetcher.Email {
	return fetcher.Email{
		UID: c.UID, From: c.From, To: c.To, Subject: c.Subject, Date: c.Date, ArrivalDate: c.ArrivalDate,
		DeliveryRecipients: c.DeliveryRecipients,
		MessageID:          c.MessageID, InReplyTo: c.InReplyTo, References: c.References,
		AccountID: c.AccountID, IsRead: c.IsRead,
	}
}

func emailToCache(e fetcher.Email) config.CachedEmail {
	return config.CachedEmail{
		UID: e.UID, From: e.From, To: e.To, Subject: e.Subject, Date: e.Date, ArrivalDate: e.ArrivalDate,
		DeliveryRecipients: e.DeliveryRecipients,
		MessageID:          e.MessageID, InReplyTo: e.InReplyTo, References: e.References,
		AccountID: e.AccountID, IsRead: e.IsRead,
	}
}
