package tui

import (
	"fmt"
	"io"
	"net/mail"
	"reflect"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	threading "github.com/floatpane/jwz-go"
	"github.com/floatpane/matcha/config"
	"github.com/floatpane/matcha/fetcher"
	"github.com/floatpane/matcha/theme"
)

var (
	// In bubbles v2, list.DefaultStyles() takes a boolean for hasDarkBackground
	paginationStyle = list.DefaultStyles(true).PaginationStyle.PaddingLeft(4)
	inboxHelpStyle  = list.DefaultStyles(true).HelpStyle.PaddingLeft(4).PaddingBottom(1)
	tabStyle        = lipgloss.NewStyle().Padding(0, 2)
	activeTabStyle  = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("42")).Bold(true).Underline(true)
	tabBarStyle     = lipgloss.NewStyle().BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).PaddingBottom(1).MarginBottom(1)
)

var unreadEmailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
var readEmailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
var visualSelectedStyle lipgloss.Style
var selectedDateStyle lipgloss.Style

type item struct {
	title, desc   string
	originalIndex int
	uid           uint32
	accountID     string
	accountEmail  string
	date          time.Time
	isRead        bool
	threadKey     string
	threadCount   int
	threadRoot    bool
	threadHeader  bool
	threadChild   bool
	threadDepth   int
	expanded      bool
}

func (i item) Title() string       { return i.title }
func (i item) Description() string { return i.desc }
func (i item) FilterValue() string { return i.title + " " + i.desc }

func searchKey() string {
	if config.Keybinds.Inbox.Search != "" {
		return config.Keybinds.Inbox.Search
	}
	return "/"
}

func filterKey() string {
	if config.Keybinds.Inbox.Filter != "" {
		return config.Keybinds.Inbox.Filter
	}
	return "f"
}

func commandPaletteKey() string {
	return config.Keybinds.Global.CommandPalette
}

type itemDelegate struct {
	inbox *Inbox
}

func (d itemDelegate) Height() int                               { return 1 }
func (d itemDelegate) Spacing() int                              { return 0 }
func (d itemDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) { //nolint:gocyclo
	i, ok := listItem.(item)
	if !ok {
		return
	}

	prefix := fmt.Sprintf("%d. ", index+1)
	sender := parseSenderName(i.desc)
	statusStyle := unreadEmailStyle
	statusIcon := "\uf0e0"
	if i.isRead {
		statusStyle = readEmailStyle
		statusIcon = "\uf2b6"
	}
	if i.threadRoot && i.threadCount > 1 {
		if i.expanded {
			statusIcon = "▾"
		} else {
			statusIcon = "▸"
		}
	}
	if i.threadHeader {
		statusIcon = "▴"
	}
	styledIcon := statusStyle.Render(statusIcon)
	styledSender := statusStyle.Render(sender)
	separator := " · "

	// For "ALL" view, show account indicator instead of number
	if i.accountEmail != "" {
		prefix = fmt.Sprintf("%d. [%s] ", index+1, truncateEmail(i.accountEmail))
	}

	// Format and right-align date
	layout := ""
	detailedDates := false
	if d.inbox != nil {
		layout = d.inbox.dateFormat
		detailedDates = d.inbox.detailedDates
	}
	dateStr := formatInboxDate(i.date, layout, detailedDates)
	listWidth := m.Width() - 2 // account for PaddingLeft(2) in itemStyle
	isSelected := index == m.Index()

	var styledDate string
	if isSelected {
		styledDate = selectedDateStyle.Render(dateStr)
	} else {
		styledDate = statusStyle.Render(dateStr)
	}
	dateWidth := lipgloss.Width(styledDate)
	cursorWidth := 0
	if isSelected {
		cursorWidth = 2 // "> " prefix
	}

	// Available width for the whole left side (prefix + sender + separator + subject)
	maxLeft := listWidth - dateWidth - 2 - cursorWidth // 2 for spacing
	if maxLeft < 10 {
		maxLeft = 10
	}

	prefixWidth := lipgloss.Width(prefix)
	iconWidth := lipgloss.Width(styledIcon) + 1
	sepWidth := len(separator)

	availableForText := maxLeft - prefixWidth - iconWidth - sepWidth
	if availableForText < 10 {
		availableForText = 10
	}

	maxSenderWidth := availableForText / 2
	if lipgloss.Width(sender) > maxSenderWidth {
		runes := []rune(sender)
		for lipgloss.Width(string(runes)) > maxSenderWidth-1 && len(runes) > 0 {
			runes = runes[:len(runes)-1]
		}
		sender = string(runes) + "…"
		styledSender = statusStyle.Render(sender)
	}

	senderWidth := lipgloss.Width(styledSender)
	subjectBudget := maxLeft - prefixWidth - iconWidth - senderWidth - sepWidth

	subject := i.title
	if i.threadChild {
		subject = strings.Repeat("  ", i.threadDepth) + "↳ " + subject
	}
	if i.threadHeader {
		subject = fmt.Sprintf("Close thread (%d)", i.threadCount)
	}
	if i.threadRoot && i.threadCount > 1 {
		subject = fmt.Sprintf("%s (%d)", subject, i.threadCount)
	}
	if subjectBudget < 4 {
		subjectBudget = 4
	}
	if lipgloss.Width(subject) > subjectBudget {
		runes := []rune(subject)
		for lipgloss.Width(string(runes)) > subjectBudget-1 && len(runes) > 0 {
			runes = runes[:len(runes)-1]
		}
		subject = string(runes) + "…"
	}
	styledSubject := statusStyle.Render(subject)

	str := prefix + styledIcon + " " + styledSender + separator + styledSubject

	// Pad to push date to the right
	padding := listWidth - lipgloss.Width(str) - dateWidth - cursorWidth
	if padding < 1 {
		padding = 1
	}

	// Check if this item is in visual selection
	inVisualSelection := false
	if d.inbox != nil && d.inbox.visualMode {
		_, inVisualSelection = d.inbox.selectedUIDs[i.uid]
	}

	fn := itemStyle.Render
	if inVisualSelection && !isSelected {
		// Item is in visual selection but not the cursor
		fn = func(s ...string) string {
			return visualSelectedStyle.Render("* " + s[0])
		}
		cursorWidth = 2 // "* " prefix
		padding = listWidth - lipgloss.Width(str) - dateWidth - cursorWidth
		if padding < 1 {
			padding = 1
		}
	} else if isSelected {
		// Cursor position (may also be in selection)
		prefix := "> "
		if inVisualSelection {
			prefix = ">*"
		}
		fn = func(s ...string) string {
			return selectedItemStyle.Render(prefix + s[0])
		}
		cursorWidth = len(prefix)
		padding = listWidth - lipgloss.Width(str) - dateWidth - cursorWidth
		if padding < 1 {
			padding = 1
		}
	}

	fmt.Fprint(w, fn(str+strings.Repeat(" ", padding)+styledDate)) //nolint:errcheck
}

// formatInboxDate formats a time as relative unless detailed dates are enabled
// or the timestamp is older than a week. Absolute dates use the caller-supplied
// Go time layout.
// When layout is empty, falls back to the built-in short/long defaults.
func formatInboxDate(timestamp time.Time, layout string, detailedDates bool) string {
	if timestamp.IsZero() {
		return ""
	}
	now := time.Now()
	if detailedDates {
		return formatAbsoluteDate(timestamp, layout, now)
	}
	d := now.Sub(timestamp)

	switch {
	case d < time.Minute:
		return t("time.just_now")
	case d < time.Hour:
		mins := int(d.Minutes())
		return tn("time.minute_ago", mins, map[string]interface{}{keyCount: mins})
	case d < 24*time.Hour:
		hours := int(d.Hours())
		return tn("time.hour_ago", hours, map[string]interface{}{keyCount: hours})
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		return tn("time.day_ago", days, map[string]interface{}{keyCount: days})
	default:
		return formatAbsoluteDate(timestamp, layout, now)
	}
}

func formatAbsoluteDate(timestamp time.Time, layout string, now time.Time) string {
	timestamp = timestamp.Local()
	if layout != "" {
		return timestamp.Format(layout)
	}
	if timestamp.Year() == now.Year() {
		return timestamp.Format("Jan 02")
	}
	return timestamp.Format("Jan 02, 2006")
}

// parseSenderName extracts the display name from a "Name <email>" string,
// falling back to the local part of the email address.
func parseSenderName(from string) string {
	if idx := strings.Index(from, " <"); idx > 0 {
		return strings.TrimSpace(from[:idx])
	}
	// No display name — use local part of email
	if idx := strings.Index(from, "@"); idx > 0 {
		return from[:idx]
	}
	return from
}

// truncateEmail shortens an email for display
func truncateEmail(email string) string {
	maxLength := 18

	if len(email) <= maxLength {
		return email
	}

	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		if len(email) > maxLength {
			return email[:maxLength-3] + "..."
		}
		return email
	}

	local := parts[0]
	domain := parts[1]

	// Keep full domain visible (e.g. ...@gmail.com) and truncate local part first.
	if len(local) > 8 {
		return local[:8] + "...@" + domain
	}

	return local + "@" + domain
}

// AccountTab represents a tab for an account
type AccountTab struct {
	ID    string
	Label string
	Email string
}

type Inbox struct {
	list               list.Model
	isFetching         bool
	isRefreshing       bool
	emailsCount        int
	totalEmailCount    int // actual number of emails (not threads) for status bar
	accounts           []config.Account
	emailsByAccount    map[string][]fetcher.Email
	allEmails          []fetcher.Email
	tabs               []AccountTab
	activeTabIndex     int
	width              int
	height             int
	currentAccountID   string // Empty means "ALL"
	emailCountByAcct   map[string]int
	mailbox            MailboxKind
	folderName         string          // Custom folder name override for title
	noMoreByAccount    map[string]bool // Per-account: true when pagination returns 0 results
	extraShortHelpKeys []key.Binding
	pluginStatus       string // Persistent status text set by plugins
	// failedAccountsFolder and failedAccounts record which accounts could not
	// be fetched for that folder (account ID → error text).
	failedAccountsFolder string
	failedAccounts       map[string]string
	pluginKeyBindings    []PluginKeyBinding
	searchOverlay        *SearchOverlay
	localSearcher        LocalSearcher
	hiddenSearcher       HiddenSearcher
	searchHistory        SearchHistory
	searchActive         bool
	searchQuery          string
	searchResults        []fetcher.Email
	hiddenView           bool
	hiddenStatus         string
	unreadOnly           bool
	nextUnreadAccount    int
	threaded             map[string]bool
	expanded             map[string]bool
	defaultThreaded      bool
	paginationPageSize   config.PaginationPageSize

	// Visual mode state (Vim-style multi-select)
	visualMode     bool              // Whether visual mode is active
	visualAnchor   int               // Index where visual selection started
	selectedUIDs   map[uint32]string // map[uid]accountID for selected emails
	selectionOrder []uint32          // Ordered list of UIDs for display

	// dateFormat is the Go reference-time layout used for absolute dates
	// older than a week. When empty, the built-in defaults apply.
	dateFormat    string
	detailedDates bool

	lastClickTime time.Time
	lastClickY    int
}

// SetDateFormat configures the Go time layout used to render absolute
// dates in the email list. Pass the value returned by
// config.Config.GetDateFormat.
func (m *Inbox) SetDateFormat(layout string) {
	m.dateFormat = layout
}

// SetDetailedDates configures whether the email list should always render
// absolute dates instead of recent relative dates.
func (m *Inbox) SetDetailedDates(enabled bool) {
	m.detailedDates = enabled
	m.updateList()
}

func NewInbox(emails []fetcher.Email, accounts []config.Account) *Inbox {
	return NewInboxWithMailbox(emails, accounts, MailboxInbox)
}

func NewSentInbox(emails []fetcher.Email, accounts []config.Account) *Inbox {
	return NewInboxWithMailbox(emails, accounts, MailboxSent)
}

func NewTrashInbox(emails []fetcher.Email, accounts []config.Account) *Inbox {
	return NewInboxWithMailbox(emails, accounts, MailboxTrash)
}

func NewArchiveInbox(emails []fetcher.Email, accounts []config.Account) *Inbox {
	return NewInboxWithMailbox(emails, accounts, MailboxArchive)
}

func NewInboxWithMailbox(emails []fetcher.Email, accounts []config.Account, mailbox MailboxKind) *Inbox {
	// Build tabs: empty for single account, "ALL" + accounts for multiple
	tabs := make([]AccountTab, 0, 1+len(accounts))
	if len(accounts) <= 1 {
		tabs = []AccountTab{{ID: "", Label: "", Email: ""}}
	} else {
		tabs = append(tabs, AccountTab{ID: "", Label: "ALL", Email: ""})
		for _, acc := range accounts {
			// Use FetchEmail for display, fall back to Email if not set
			displayEmail := accountDisplayEmail(acc)
			tabs = append(tabs, AccountTab{ID: acc.ID, Label: displayEmail, Email: displayEmail})
		}
	}

	// Group emails by account
	emailsByAccount := make(map[string][]fetcher.Email)
	for _, email := range emails {
		emailsByAccount[email.AccountID] = append(emailsByAccount[email.AccountID], email)
	}

	// Track email counts per account
	emailCountByAcct := make(map[string]int)
	for accID, accEmails := range emailsByAccount {
		emailCountByAcct[accID] = len(accEmails)
	}

	inbox := &Inbox{
		accounts:         accounts,
		emailsByAccount:  emailsByAccount,
		allEmails:        dedupeEmailsForAccounts(emails, accounts),
		tabs:             tabs,
		activeTabIndex:   0,
		currentAccountID: "",
		emailCountByAcct: emailCountByAcct,
		mailbox:          mailbox,
		threaded:         make(map[string]bool),
		expanded:         make(map[string]bool),
		visualMode:       false,
		selectedUIDs:     make(map[uint32]string),
		selectionOrder:   []uint32{},
	}

	inbox.updateList()
	return inbox
}

// NewInboxSingleAccount creates an inbox for a single account (legacy support)
func NewInboxSingleAccount(emails []fetcher.Email) *Inbox {
	return NewInbox(emails, nil)
}

func (m *Inbox) updateList() {
	// Capture current index to restore later
	currentIndex := m.list.Index()

	displayEmails := m.displayEmails()
	m.emailsCount = len(displayEmails)
	m.totalEmailCount = len(displayEmails)

	showAccountLabel := false
	if m.currentAccountID == "" {
		showAccountLabel = len(m.accounts) > 1
		if !showAccountLabel {
			for i := range m.accounts {
				account := &m.accounts[i]
				if account.CatchAll || len(account.ExtraAddresses) > 0 {
					showAccountLabel = true
					break
				}
			}
		}
	}

	items := m.itemsForEmails(displayEmails, showAccountLabel)

	l := list.New(items, itemDelegate{inbox: m}, 20, 14)
	l.Title = m.getTitle()
	// When threaded, hide the built-in status bar (which counts list items =
	// threads) and render our own that counts actual emails.
	l.SetShowStatusBar(!m.isThreaded())
	l.SetFilteringEnabled(true)
	l.Styles.Title = lipgloss.NewStyle().Foreground(theme.ActiveTheme.Accent).Bold(true)
	l.Styles.PaginationStyle = paginationStyle
	l.Styles.HelpStyle = inboxHelpStyle
	l.SetStatusBarItemName("email", "emails")
	// Keep hidden mail ahead of less-common actions so its shortcut is easy
	// to discover alongside the main menu entry.
	l.AdditionalShortHelpKeys = func() []key.Binding {
		var bindings []key.Binding
		if m.hiddenSearcher != nil {
			label := "hidden mail"
			if m.hiddenView {
				label = "ordinary mail"
			}
			bindings = append(bindings, key.NewBinding(key.WithKeys("H"), key.WithHelp("H", label)))
		}
		if len(m.tabs) > 1 {
			prevTab := kbKey(config.Keybinds.Inbox.PrevTab, "h")
			nextTab := kbKey(config.Keybinds.Inbox.NextTab, "l")
			bindings = append(bindings,
				key.NewBinding(key.WithKeys("left", prevTab), key.WithHelp(joinKeys("←", prevTab), "prev tab")),
				key.NewBinding(key.WithKeys("right", nextTab), key.WithHelp(joinKeys("→", nextTab), "next tab")),
			)
		}
		bindings = append(bindings, m.extraShortHelpKeys...)
		if pk := commandPaletteKey(); pk != "" {
			bindings = append(bindings, key.NewBinding(key.WithKeys(pk), key.WithHelp(pk, "commands")))
		}
		for _, pk := range m.pluginKeyBindings {
			bindings = append(bindings, key.NewBinding(key.WithKeys(pk.Key), key.WithHelp(pk.Key, pk.Description)))
		}
		return bindings
	}

	l.KeyMap.Quit.SetEnabled(false)
	l.KeyMap.Filter = key.NewBinding(key.WithKeys(filterKey()), key.WithHelp(filterKey(), t("inbox.filter")))
	l.KeyMap.NextPage = key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdn", "next page"))
	l.KeyMap.PrevPage = key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "prev page"))
	l.KeyMap.CursorUp = key.NewBinding(
		key.WithKeys("up", kbKey(config.Keybinds.Global.NavUp, "k")),
		key.WithHelp(joinKeys("↑", kbKey(config.Keybinds.Global.NavUp, "k")), "up"),
	)
	l.KeyMap.CursorDown = key.NewBinding(
		key.WithKeys("down", kbKey(config.Keybinds.Global.NavDown, "j")),
		key.WithHelp(joinKeys("↓", kbKey(config.Keybinds.Global.NavDown, "j")), "down"),
	)
	if cancel := kbKey(config.Keybinds.Global.Cancel, "esc"); cancel != "esc" {
		l.KeyMap.ClearFilter = key.NewBinding(key.WithKeys(cancel), key.WithHelp(cancel, "clear filter"))
		l.KeyMap.CancelWhileFiltering = key.NewBinding(key.WithKeys(cancel), key.WithHelp(cancel, "cancel"))
	}

	// Disable default help to render it manually at the bottom
	l.SetShowHelp(false)

	if m.width > 0 {
		l.SetWidth(m.width)
	}
	// Restore index
	// If index is out of bounds (e.g. list shrank), clamp it.
	if currentIndex >= len(items) {
		currentIndex = len(items) - 1
	}
	if currentIndex < 0 {
		currentIndex = 0
	}
	l.Select(currentIndex)

	m.list = l
	m.resizeList()
}

func (m *Inbox) displayEmails() []fetcher.Email {
	var emails []fetcher.Email
	switch {
	case m.searchActive:
		emails = m.filteredSearchResults()
	case m.currentAccountID == "":
		emails = m.allEmails
	default:
		emails = m.emailsByAccount[m.currentAccountID]
	}
	useArrival := false
	for _, account := range m.accounts {
		if account.SortBy == "arrival_date" {
			useArrival = true
			break
		}
	}
	if useArrival {
		emails = slices.Clone(emails)
		slices.SortStableFunc(emails, func(a, b fetcher.Email) int {
			return m.sortTime(b).Compare(m.sortTime(a))
		})
	}
	if !m.unreadOnly || m.hiddenView {
		return emails
	}
	unread := make([]fetcher.Email, 0, len(emails))
	for _, email := range emails {
		if !email.IsRead {
			unread = append(unread, email)
		}
	}
	return unread
}

func (m *Inbox) filteredSearchResults() []fetcher.Email {
	if m.currentAccountID == "" {
		return m.searchResults
	}
	filtered := make([]fetcher.Email, 0, len(m.searchResults))
	for _, email := range m.searchResults {
		if email.AccountID == m.currentAccountID {
			filtered = append(filtered, email)
		}
	}
	return filtered
}

func (m *Inbox) accountLabelForEmail(email fetcher.Email) string {
	var owningAcc *config.Account
	for i := range m.accounts {
		if m.accounts[i].ID == email.AccountID {
			owningAcc = &m.accounts[i]
			break
		}
	}

	if owningAcc != nil {
		if recipient := recipientForAccount(email, owningAcc); recipient != "" {
			return recipient
		}
		if owningAcc.CatchAll {
			for _, recipients := range [...][]string{email.DeliveryRecipients, email.To} {
				if len(recipients) > 0 {
					return extractEmailAddress(recipients[0])
				}
			}
		}
	}
	for i := range m.accounts {
		account := &m.accounts[i]
		if account == owningAcc {
			continue
		}
		if recipient := recipientForAccount(email, account); recipient != "" {
			return recipient
		}
	}

	if owningAcc != nil {
		return accountDisplayEmail(*owningAcc)
	}
	return ""
}

func recipientForAccount(email fetcher.Email, account *config.Account) string {
	for _, recipients := range [...][]string{email.To, email.DeliveryRecipients} {
		for _, recipient := range recipients {
			if fetcher.IsOwnAddress(account, recipient) {
				return extractEmailAddress(recipient)
			}
		}
	}
	return ""
}

func dedupeEmailsForAccounts(emails []fetcher.Email, accounts []config.Account) []fetcher.Email {
	if len(emails) <= 1 {
		return emails
	}

	accountByID := make(map[string]config.Account, len(accounts))
	for _, acc := range accounts {
		accountByID[acc.ID] = acc
	}

	deduped := make([]fetcher.Email, 0, len(emails))
	indexByKey := make(map[string]int, len(emails))
	for _, email := range emails {
		key := emailDedupKey(email)
		if existingIndex, ok := indexByKey[key]; ok {
			existing := deduped[existingIndex]
			if !emailMatchesOwningAccount(existing, accountByID) && emailMatchesOwningAccount(email, accountByID) {
				deduped[existingIndex] = email
			}
			continue
		}
		indexByKey[key] = len(deduped)
		deduped = append(deduped, email)
	}
	return deduped
}

func emailDedupKey(email fetcher.Email) string {
	if email.MessageID != "" {
		return email.MessageID
	}
	// Malformed messages can omit Message-ID, so fall back to stable visible metadata.
	return fmt.Sprintf("%s|%s|%d", email.From, email.Subject, email.Date.UnixNano())
}

func emailMatchesOwningAccount(email fetcher.Email, accountByID map[string]config.Account) bool {
	acc, ok := accountByID[email.AccountID]
	if !ok {
		return false
	}
	fetchEmail := accountDisplayEmail(acc)
	for _, recipient := range email.To {
		if sameEmailAddress(recipient, fetchEmail) {
			return true
		}
	}
	return false
}

func accountDisplayEmail(acc config.Account) string {
	if acc.FetchEmail != "" {
		return acc.FetchEmail
	}
	return acc.Email
}

func sameEmailAddress(a, b string) bool {
	return strings.EqualFold(extractEmailAddress(a), extractEmailAddress(b))
}

func extractEmailAddress(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(value); err == nil {
		return strings.TrimSpace(addr.Address)
	}
	return strings.Trim(value, "<>")
}

func (m *Inbox) itemsForEmails(displayEmails []fetcher.Email, showAccountLabel bool) []list.Item {
	if !m.isThreaded() {
		items := make([]list.Item, len(displayEmails))
		for i, email := range displayEmails {
			items[i] = m.itemForEmail(email, i, showAccountLabel)
		}
		return items
	}

	emailIndex := make(map[string]int, len(displayEmails))
	headers := make([]threading.EmailHeader, 0, len(displayEmails))
	for i, email := range displayEmails {
		id := inboxEmailID(email)
		emailIndex[id] = i
		headers = append(headers, threading.EmailHeader{
			ID:         email.MessageID,
			InReplyTo:  email.InReplyTo,
			References: email.References,
			Subject:    email.Subject,
			Date:       m.sortTime(email),
			EmailID:    id,
			Sender:     email.From,
		})
	}

	var items []list.Item
	for _, thread := range threading.Build(headers) {
		key := threadItemKey(thread.Root)
		root := firstEmailNode(thread.Root)
		if root == nil {
			continue
		}
		idx := emailIndex[root.EmailID]
		rootEmail := displayEmails[idx]
		latest := latestEmailNode(thread.Root)
		if latest == nil {
			latest = root
		}

		rootItem := m.itemForEmail(rootEmail, idx, showAccountLabel)
		rootItem.title = firstNonEmpty(root.Subject, thread.Subject)
		rootItem.desc = latest.Sender
		rootItem.date = thread.LatestAt
		rootItem.isRead = threadRead(displayEmails, emailIndex, thread.Root)
		rootItem.threadKey = key
		rootItem.threadCount = thread.Count
		rootItem.threadRoot = true
		rootItem.expanded = m.expanded[key]

		if m.expanded[key] {
			// When expanded, insert a thread header row for collapsing,
			// then render the root email as a clickable item, then children.
			headerItem := item{
				title:        firstNonEmpty(root.Subject, thread.Subject),
				desc:         latest.Sender,
				date:         thread.LatestAt,
				isRead:       threadRead(displayEmails, emailIndex, thread.Root),
				threadKey:    key,
				threadCount:  thread.Count,
				threadRoot:   true,
				threadHeader: true,
				expanded:     true,
			}
			items = append(items, headerItem)

			// Root email as a clickable item
			clickableRoot := m.itemForEmail(rootEmail, idx, showAccountLabel)
			clickableRoot.threadKey = key
			clickableRoot.threadCount = thread.Count
			clickableRoot.threadChild = true
			clickableRoot.threadDepth = 1
			items = append(items, clickableRoot)

			items = appendThreadChildren(items, m, displayEmails, emailIndex, showAccountLabel, thread.Root.Children, 2)
		} else {
			items = append(items, rootItem)
		}
	}
	return items
}

func appendThreadChildren(items []list.Item, m *Inbox, emails []fetcher.Email, emailIndex map[string]int, showAccountLabel bool, nodes []*threading.ThreadNode, depth int) []list.Item {
	for _, node := range nodes {
		if node.EmailID != "" {
			idx := emailIndex[node.EmailID]
			child := m.itemForEmail(emails[idx], idx, showAccountLabel)
			child.threadChild = true
			child.threadDepth = depth
			items = append(items, child)
		}
		items = appendThreadChildren(items, m, emails, emailIndex, showAccountLabel, node.Children, depth+1)
	}
	return items
}

func (m *Inbox) itemForEmail(email fetcher.Email, index int, showAccountLabel bool) item {
	accountEmail := ""
	if showAccountLabel {
		accountEmail = m.accountLabelForEmail(email)
	}

	return item{
		title:         email.Subject,
		desc:          email.From,
		originalIndex: index,
		uid:           email.UID,
		accountID:     email.AccountID,
		accountEmail:  accountEmail,
		date:          email.Date,
		isRead:        email.IsRead,
	}
}

func (m *Inbox) getTitle() string {
	var title string
	switch {
	case m.hiddenView:
		title = "Hidden Mail"
	case m.searchActive:
		title = fmt.Sprintf("Search Results - %s", m.searchQuery)
	case m.currentAccountID == "":
		title = m.getBaseTitle() + " - " + t("inbox.all_accounts")
	default:
		title = m.getBaseTitle()
		for _, acc := range m.accounts {
			if acc.ID == m.currentAccountID {
				if acc.Name != "" {
					title = fmt.Sprintf("%s - %s", m.getBaseTitle(), acc.Name)
				} else {
					title = fmt.Sprintf("%s - %s", m.getBaseTitle(), accountDisplayEmail(acc))
				}
				break
			}
		}
	}
	if count := m.hiddenCount(); count > 0 {
		if m.hiddenView {
			title += fmt.Sprintf(" (%d hidden)", count)
		} else {
			// Put the access hint first so narrow titles do not truncate it.
			title = fmt.Sprintf("(%d hidden: H) %s", count, title)
		}
	}
	if m.isRefreshing {
		title += " (refreshing...)"
	}
	if m.isFetching {
		title += " (loading more...)"
	}
	if m.isThreaded() {
		title += " (threaded)"
	}
	if m.unreadOnly && !m.hiddenView {
		title += " (unread only)"
	}
	if m.pluginStatus != "" {
		title += " (" + m.pluginStatus + ")"
	}
	if failure := m.loadFailureNote(); failure != "" {
		title += " (" + failure + ")"
	}
	return title
}

// SetFailedAccounts records the accounts whose emails could not be fetched for
// folderName, replacing any earlier record. An empty map clears it.
func (m *Inbox) SetFailedAccounts(folderName string, failed map[string]string) {
	m.failedAccountsFolder = folderName
	m.failedAccounts = failed
	m.list.Title = m.getTitle()
}

// loadFailureNote describes accounts that failed to load for the shown folder
// and tab, or returns "" when there are none.
func (m *Inbox) loadFailureNote() string {
	if len(m.failedAccounts) == 0 || m.failedAccountsFolder != m.folderName || m.searchActive {
		return ""
	}
	retry := "press " + config.Keybinds.Inbox.Refresh + " to retry"
	if m.currentAccountID != "" {
		errText, failed := m.failedAccounts[m.currentAccountID]
		if !failed {
			return ""
		}
		return fmt.Sprintf("could not load: %s; %s", ansi.Truncate(errText, 60, "…"), retry)
	}
	var names []string
	for _, acc := range m.accounts {
		if _, failed := m.failedAccounts[acc.ID]; failed {
			names = append(names, accountDisplayEmail(acc))
		}
	}
	return fmt.Sprintf("could not load %s; %s", strings.Join(names, ", "), retry)
}

func (m *Inbox) getBaseTitle() string {
	if m.folderName != "" {
		return m.folderName
	}
	switch m.mailbox {
	case MailboxSent:
		return "Sent"
	case MailboxTrash:
		return "Trash"
	case MailboxArchive:
		return "Archive"
	case MailboxInbox:
		return "Inbox"
	}
	return "Inbox"
}

func (m *Inbox) folderKey() string {
	if m.folderName != "" {
		return m.folderName
	}
	return string(m.mailbox)
}

// SetDefaultThreaded sets the global default threading state used when no
// per-folder override exists. Pass Config.EnableThreaded.
func (m *Inbox) SetDefaultThreaded(v bool) {
	m.defaultThreaded = v
	// Drop the in-memory cache so the new default takes effect for folders
	// without an explicit override on the next render.
	m.threaded = nil
	m.expanded = nil
}

func (m *Inbox) isThreaded() bool {
	if m.threaded == nil {
		m.threaded = make(map[string]bool)
	}
	if m.expanded == nil {
		m.expanded = make(map[string]bool)
	}
	key := m.folderKey()
	if _, ok := m.threaded[key]; !ok {
		m.threaded[key] = config.IsFolderThreaded(key, m.defaultThreaded)
	}
	return m.threaded[key]
}

func (m *Inbox) toggleThreaded() {
	if m.threaded == nil {
		m.threaded = make(map[string]bool)
	}
	key := m.folderKey()
	next := !m.isThreaded()
	m.threaded[key] = next
	if !next {
		m.expanded = make(map[string]bool)
	}
	_ = config.SetFolderThreaded(key, next)
}

func (m *Inbox) toggleThreadedKey() string {
	if config.Keybinds.Inbox.ToggleThreaded != "" {
		return config.Keybinds.Inbox.ToggleThreaded
	}
	return "T"
}

func (m *Inbox) toggleUnreadOnlyKey() string {
	if config.Keybinds.Inbox.ToggleUnreadOnly != "" {
		return config.Keybinds.Inbox.ToggleUnreadOnly
	}
	return "U"
}

// continueUnreadFetch fills an unread-only view from subsequent pages when
// the loaded messages do not provide enough visible rows.
func (m *Inbox) continueUnreadFetch() tea.Cmd {
	if m.unreadOnly && m.shouldFetchMore() {
		cmds := m.fetchMoreCmds()
		if len(cmds) > 0 {
			m.isFetching = true
			m.list.Title = m.getTitle()
			return tea.Batch(cmds...)
		}
	}
	return nil
}

func (m *Inbox) Init() tea.Cmd {
	return nil
}

func (m *Inbox) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		delta := 0
		switch msg.Button {
		case tea.MouseWheelDown:
			delta = 1
		case tea.MouseWheelUp:
			delta = -1
		}
		if m.searchOverlay != nil {
			m.searchOverlay.MoveHighlight(delta)
			return m, nil
		}
		if delta > 0 {
			m.list.CursorDown()
		} else if delta < 0 {
			m.list.CursorUp()
		}
		return m, nil

	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return m, nil
		}
		// While the search box is open, clicks pick search results and never
		// move the email list behind it.
		if m.searchOverlay != nil {
			return m, m.searchOverlay.Click(msg.Y - m.searchOverlayTop())
		}
		// The account tab labels sit on the first row of the inbox.
		if msg.Y == 0 && isUIVisible("header") && len(m.tabs) > 1 {
			if i := m.tabAt(msg.X); i >= 0 && i != m.activeTabIndex {
				return m, m.switchToTab(i)
			}
			return m, nil
		}
		cursorInPage := msg.Y - m.emailRowsTop()
		if cursorInPage >= 0 {
			idx := m.list.Paginator.Page*m.list.Paginator.PerPage + cursorInPage
			if idx >= 0 && idx < len(m.list.Items()) {
				m.list.Select(idx)
				now := time.Now()
				isDoubleClick := msg.Y == m.lastClickY && now.Sub(m.lastClickTime) < 500*time.Millisecond
				m.lastClickTime = now
				m.lastClickY = msg.Y
				if isDoubleClick {
					if selectedItem, ok := m.list.SelectedItem().(item); ok && selectedItem.uid != 0 {
						return m, m.viewEmailCmd(selectedItem)
					}
				}
			}
		}
		return m, nil

	case tea.KeyPressMsg:
		if m.searchOverlay != nil {
			if msg.String() == config.Keybinds.Global.Cancel {
				m.searchOverlay = nil
				return m, nil
			}
			cmd := m.searchOverlay.Update(msg, m.mailbox, m.currentAccountID)
			return m, cmd
		}
		if m.list.FilterState() == list.Filtering {
			// Don't allow visual mode while filtering
			if m.visualMode {
				m.visualMode = false
				m.selectedUIDs = make(map[uint32]string)
				m.selectionOrder = []uint32{}
				m.updateListTitle()
			}
			break
		}
		kb := config.Keybinds
		searchBinding := searchKey()
		switch keypress := msg.String(); keypress {
		case "H":
			if m.hiddenView {
				m.searchActive = false
				m.searchQuery = ""
				m.searchResults = nil
				m.hiddenView = false
				m.hiddenStatus = ""
				m.visualMode = false
				m.selectedUIDs = make(map[uint32]string)
				m.selectionOrder = nil
				m.updateList()
				return m, nil
			}
			return m, m.ShowHiddenMail()
		case searchBinding:
			m.searchOverlay = NewSearchOverlay(m.width, m.height)
			cmds := []tea.Cmd{m.searchOverlay.Init()}
			if m.searchHistory != nil {
				cmds = append(cmds, m.searchOverlay.UseSearchHistory(m.searchHistory))
			}
			if m.localSearcher != nil && m.folderName != "" {
				m.searchOverlay.UseLocalSearch(m.localSearcher, m.folderName, m.searchAccountIDs())
				opened := SearchOpenedMsg{FolderName: m.folderName, AccountID: m.currentAccountID}
				cmds = append(cmds, func() tea.Msg { return opened })
			}
			return m, tea.Batch(cmds...)
		case m.toggleThreadedKey():
			m.toggleThreaded()
			m.updateList()
			return m, nil
		case m.toggleUnreadOnlyKey():
			m.unreadOnly = !m.unreadOnly
			m.visualMode = false
			m.selectedUIDs = make(map[uint32]string)
			m.selectionOrder = nil
			m.updateList()
			return m, m.continueUnreadFetch()
		case kb.Inbox.VisualMode:
			if !m.visualMode {
				// Enter visual mode
				m.visualMode = true
				m.visualAnchor = m.list.Index()
				selectedItem, ok := m.list.SelectedItem().(item)
				if ok {
					m.selectedUIDs = make(map[uint32]string)
					m.selectionOrder = []uint32{}
					m.selectedUIDs[selectedItem.uid] = selectedItem.accountID
					m.selectionOrder = append(m.selectionOrder, selectedItem.uid)
				}
				m.updateListTitle()
			} else {
				// Exit visual mode
				m.visualMode = false
				m.selectedUIDs = make(map[uint32]string)
				m.selectionOrder = []uint32{}
				m.updateListTitle()
			}
			return m, nil
		case kb.Global.Cancel:
			if m.searchActive {
				m.searchActive = false
				m.searchQuery = ""
				m.searchResults = nil
				m.hiddenView = false
				m.hiddenStatus = ""
				m.updateList()
				return m, nil
			}
			if m.visualMode {
				// Exit visual mode on cancel key
				m.visualMode = false
				m.selectedUIDs = make(map[uint32]string)
				m.selectionOrder = []uint32{}
				m.updateListTitle()
				return m, nil
			}
		case kb.Global.NavDown, keyDown, kb.Global.NavUp, "up":
			if m.visualMode {
				// Let the list handle navigation first
				var cmd tea.Cmd
				m.list, cmd = m.list.Update(msg)
				// Then update selection
				m.updateVisualSelection()
				return m, cmd
			}
		case "left", kb.Inbox.PrevTab:
			if len(m.tabs) > 1 {
				return m, m.switchToTab((m.activeTabIndex - 1 + len(m.tabs)) % len(m.tabs))
			}
		case keyRight, kb.Inbox.NextTab:
			if len(m.tabs) > 1 {
				return m, m.switchToTab((m.activeTabIndex + 1) % len(m.tabs))
			}
		case kb.Inbox.Delete:
			if m.visualMode && len(m.selectedUIDs) > 0 {
				// Batch delete
				uids := make([]uint32, len(m.selectionOrder))
				copy(uids, m.selectionOrder)
				accountID := ""
				for _, aid := range m.selectedUIDs {
					accountID = aid // Get any account (all should be same in single-account selection)
					break
				}

				// Exit visual mode
				m.visualMode = false
				m.selectedUIDs = make(map[uint32]string)
				m.selectionOrder = []uint32{}
				m.updateListTitle()

				return m, func() tea.Msg {
					return BatchDeleteEmailsMsg{UIDs: uids, AccountID: accountID, Mailbox: m.mailbox}
				}
			}
			// Single delete
			selectedItem, ok := m.list.SelectedItem().(item)
			if ok && selectedItem.uid != 0 {
				return m, func() tea.Msg {
					return DeleteEmailMsg{UID: selectedItem.uid, AccountID: selectedItem.accountID, Mailbox: m.mailbox}
				}
			}
		case kb.Inbox.Archive:
			if m.visualMode && len(m.selectedUIDs) > 0 {
				// Batch archive
				uids := make([]uint32, len(m.selectionOrder))
				copy(uids, m.selectionOrder)
				accountID := ""
				for _, aid := range m.selectedUIDs {
					accountID = aid
					break
				}

				// Exit visual mode
				m.visualMode = false
				m.selectedUIDs = make(map[uint32]string)
				m.selectionOrder = []uint32{}
				m.updateListTitle()

				return m, func() tea.Msg {
					return BatchArchiveEmailsMsg{UIDs: uids, AccountID: accountID, Mailbox: m.mailbox}
				}
			}
			// Single archive
			selectedItem, ok := m.list.SelectedItem().(item)
			if ok && selectedItem.uid != 0 {
				return m, func() tea.Msg {
					return ArchiveEmailMsg{UID: selectedItem.uid, AccountID: selectedItem.accountID, Mailbox: m.mailbox}
				}
			}
		case kb.Inbox.Refresh:
			m.isRefreshing = true
			m.list.Title = m.getTitle()
			// Copy counts to avoid race conditions if used elsewhere (though here it's just passing data)
			counts := make(map[string]int)
			for k, v := range m.emailCountByAcct {
				counts[k] = v
			}
			return m, func() tea.Msg {
				return RequestRefreshMsg{Mailbox: m.mailbox, Counts: counts}
			}
		case kb.Inbox.Open:
			selectedItem, ok := m.list.SelectedItem().(item)
			if ok {
				// Thread header row: collapse the thread
				if selectedItem.threadHeader {
					m.expanded[selectedItem.threadKey] = false
					m.updateList()
					return m, nil
				}
				// Collapsed thread root: expand it
				if selectedItem.threadRoot && selectedItem.threadCount > 1 && !selectedItem.expanded {
					m.expanded[selectedItem.threadKey] = true
					m.updateList()
					return m, nil
				}
				if selectedItem.uid == 0 {
					return m, nil
				}
				return m, m.viewEmailCmd(selectedItem)
			}
		}
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		if m.searchOverlay != nil {
			return m, m.searchOverlay.Update(msg, m.mailbox, m.currentAccountID)
		}
		if m.shouldFetchMore() {
			if m.unreadOnly {
				return m, m.continueUnreadFetch()
			}
			return m, tea.Batch(m.fetchMoreCmds()...)
		}
		return m, nil

	case LocalSearchIndexChangedMsg:
		if m.searchOverlay != nil {
			m.searchOverlay.RefreshLocal()
		}
		if m.hiddenView && m.refreshHiddenResults() {
			m.visualMode = false
			m.selectedUIDs = make(map[uint32]string)
			m.selectionOrder = nil
			m.updateList()
		}
		m.list.Title = m.getTitle()
		return m, nil

	case SearchResultsMsg:
		if m.searchOverlay == nil {
			return m, nil
		}
		return m, m.searchOverlay.Update(msg, m.mailbox, m.currentAccountID)

	case ApplySearchResultsMsg:
		m.searchOverlay = nil
		m.searchActive = true
		m.searchQuery = msg.Query.Raw
		m.hiddenView = false
		m.hiddenStatus = ""
		m.searchResults = dedupeEmailsForAccounts(msg.Emails, m.accounts)
		m.visualMode = false
		m.selectedUIDs = make(map[uint32]string)
		m.selectionOrder = []uint32{}
		m.updateList()
		if msg.Open != nil {
			return m, m.selectAndOpen(*msg.Open)
		}
		return m, nil

	case FetchingMoreEmailsMsg:
		if msg.FolderName != "" && msg.FolderName != m.folderKey() {
			return m, nil
		}
		m.isFetching = true
		m.list.Title = m.getTitle()
		return m, nil

	case FolderEmailsPageFailedMsg:
		if msg.FolderName == m.folderKey() {
			m.isFetching = false
			m.list.Title = m.getTitle()
		}
		return m, nil

	case EmailsAppendedMsg:
		if msg.Mailbox != m.mailbox {
			return m, nil
		}
		m.isFetching = false
		m.list.Title = m.getTitle()

		if len(msg.Emails) == 0 {
			if m.noMoreByAccount == nil {
				m.noMoreByAccount = make(map[string]bool)
			}
			m.noMoreByAccount[msg.AccountID] = true
			return m, m.continueUnreadFetch()
		}

		// Add emails to the appropriate account
		for _, email := range msg.Emails {
			m.emailsByAccount[email.AccountID] = append(m.emailsByAccount[email.AccountID], email)
			m.allEmails = append(m.allEmails, email)
		}
		m.emailCountByAcct[msg.AccountID] = len(m.emailsByAccount[msg.AccountID])

		m.updateList()
		return m, m.continueUnreadFetch()

	case RefreshingEmailsMsg:
		if msg.Mailbox != m.mailbox {
			return m, nil
		}
		m.isRefreshing = true
		m.list.Title = m.getTitle()
		return m, nil

	case EmailsRefreshedMsg:
		if msg.Mailbox != m.mailbox {
			return m, nil
		}
		// Only clear the refreshing indicator. The actual email data is
		// merged by the main model (preserving paginated emails) and
		// pushed to us via SetEmails, so we must not overwrite it here.
		m.isRefreshing = false
		m.list.Title = m.getTitle()
		return m, nil
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	cmds = append(cmds, cmd)

	if m.shouldFetchMore() {
		if m.unreadOnly {
			cmds = append(cmds, m.continueUnreadFetch())
		} else {
			cmds = append(cmds, m.fetchMoreCmds()...)
		}
	}
	return m, tea.Batch(cmds...)
}

func (m *Inbox) shouldFetchMore() bool {
	if m.isFetching || m.isRefreshing {
		return false
	}
	if m.searchActive {
		return false
	}
	if m.allAccountsExhausted() {
		return false
	}
	if len(m.list.Items()) == 0 && !m.unreadOnly {
		return false
	}
	if m.list.FilterState() == list.Filtering {
		return false
	}
	// Fetch if we've reached the bottom OR if we don't have enough items to fill the view
	return m.list.Index() >= len(m.list.Items())-1 || len(m.list.Items()) < m.list.Height()
}

// allAccountsExhausted returns true if all relevant accounts have no more emails to fetch.
func (m *Inbox) allAccountsExhausted() bool {
	if len(m.noMoreByAccount) == 0 {
		return false
	}
	if m.currentAccountID != "" {
		return m.noMoreByAccount[m.currentAccountID]
	}
	// "ALL" view: all accounts must be exhausted
	for _, acc := range m.accounts {
		if !m.noMoreByAccount[acc.ID] {
			return false
		}
	}
	return len(m.accounts) > 0
}

func (m *Inbox) fetchMoreCmds() []tea.Cmd {
	var cmds []tea.Cmd
	limit := m.PaginationPageSize()

	if m.currentAccountID == "" {
		if len(m.accounts) == 0 {
			return nil
		}
		if m.unreadOnly {
			for step := range len(m.accounts) {
				i := (m.nextUnreadAccount + step) % len(m.accounts)
				accountID := m.accounts[i].ID
				if m.noMoreByAccount[accountID] {
					continue
				}
				m.nextUnreadAccount = (i + 1) % len(m.accounts)
				offset := uint32(len(m.emailsByAccount[accountID]))
				return []tea.Cmd{func() tea.Msg {
					return FetchMoreEmailsMsg{Offset: offset, AccountID: accountID, Mailbox: m.mailbox, Limit: limit}
				}}
			}
			return nil
		}
		for _, acc := range m.accounts {
			accountID := acc.ID
			if m.noMoreByAccount[accountID] {
				continue
			}
			offset := uint32(len(m.emailsByAccount[accountID]))
			cmds = append(cmds, func(id string, off uint32) tea.Cmd {
				return func() tea.Msg {
					return FetchMoreEmailsMsg{Offset: off, AccountID: id, Mailbox: m.mailbox, Limit: limit}
				}
			}(accountID, offset))
		}
		return cmds
	}

	if m.noMoreByAccount[m.currentAccountID] {
		return nil
	}
	offset := uint32(len(m.emailsByAccount[m.currentAccountID]))
	cmds = append(cmds, func(id string, off uint32) tea.Cmd {
		return func() tea.Msg {
			return FetchMoreEmailsMsg{Offset: off, AccountID: id, Mailbox: m.mailbox, Limit: limit}
		}
	}(m.currentAccountID, offset))
	return cmds
}

// SetPaginationPageSize configures a numeric page cap or a height-based size.
func (m *Inbox) SetPaginationPageSize(size config.PaginationPageSize) {
	m.paginationPageSize = size
	m.resizeList()
}

// PaginationPageSize uses the configured cap or a fraction of the pane height.
func (m *Inbox) PaginationPageSize() uint32 {
	return m.paginationPageSize.LimitForHeight(m.height)
}

// resizeList gives the message pager the pane's available rows after the
// surrounding tab bar, help, and any search or thread status are laid out.
func (m *Inbox) resizeList() {
	if m.height <= 0 {
		return
	}

	headerVisible := isUIVisible("header")
	m.list.SetShowTitle(headerVisible)
	available := m.height
	if headerVisible && len(m.tabs) > 1 {
		available -= lipgloss.Height(tabBarStyle.Render("ALL"))
	}
	if m.isThreaded() {
		available -= lipgloss.Height(m.threadedStatusBar())
	}
	if m.hiddenStatus != "" {
		available -= strings.Count(m.hiddenStatus, "\n") + 1
	}
	if m.searchOverlay != nil {
		available -= 1 + lipgloss.Height(m.searchOverlay.View())
	}
	if isUIVisible("status_bar") {
		available -= lipgloss.Height(inboxHelpStyle.Render(m.list.Help.View(m.list)))
	}
	m.list.SetHeight(max(1, available))
	pageLimit := int(m.PaginationPageSize())
	if excess := m.list.Paginator.PerPage - pageLimit; excess > 0 {
		m.list.SetHeight(max(1, m.list.Height()-excess))
	}
}

// emailRowsTop returns the row of the first email in View: below the tab bar
// and whatever title and status rows the list draws above its items.
func (m *Inbox) emailRowsTop() int {
	top := 0
	if isUIVisible("header") && len(m.tabs) > 1 {
		top += lipgloss.Height(tabBarStyle.Render("ALL"))
	}
	switch {
	case m.list.ShowTitle() || m.list.FilterState() == list.Filtering:
		top += lipgloss.Height(m.list.Styles.TitleBar.Render(" "))
	case m.list.ShowFilter() && m.list.FilteringEnabled():
		// With the title hidden the list still draws an empty title row.
		top++
	}
	if m.list.ShowStatusBar() {
		top += lipgloss.Height(m.list.Styles.StatusBar.Render(" "))
	}
	return top
}

// switchToTab shows the emails of the account tab at index and leaves
// visual mode, since the selected emails may not be on the new tab.
func (m *Inbox) switchToTab(index int) tea.Cmd {
	m.activeTabIndex = index
	m.currentAccountID = m.tabs[index].ID
	if m.hiddenView {
		return m.ShowHiddenMail()
	}
	m.visualMode = false
	m.selectedUIDs = make(map[uint32]string)
	m.selectionOrder = []uint32{}
	m.updateList()
	return m.continueUnreadFetch()
}

// renderedTabs returns each account tab as drawn in the tab bar, left to right.
// When the full labels do not fit the inbox width, the longest labels are
// shortened first so every tab stays on screen and can be clicked.
func (m *Inbox) renderedTabs() []string {
	labels := make([]string, len(m.tabs))
	for i, tab := range m.tabs {
		labels[i] = tab.Label
		if tab.ID == "" {
			labels[i] = "ALL"
		}
	}
	if m.width > 0 {
		padding := tabStyle.GetHorizontalFrameSize()
		limit := 0
		for _, label := range labels {
			limit = max(limit, ansi.StringWidth(label))
		}
		fits := func() bool {
			total := 0
			for _, label := range labels {
				total += min(ansi.StringWidth(label), limit) + padding
			}
			return total <= m.width
		}
		for limit > 3 && !fits() {
			limit--
		}
		for i := range labels {
			labels[i] = shortenTabLabel(labels[i], limit)
		}
	}
	tabViews := make([]string, 0, len(labels))
	for i, label := range labels {
		if i == m.activeTabIndex {
			tabViews = append(tabViews, activeTabStyle.Render(label))
		} else {
			tabViews = append(tabViews, tabStyle.Render(label))
		}
	}
	return tabViews
}

// shortenTabLabel fits label into width columns. For an email address the
// domain is kept and the name before "@" is cut, since the domain is what
// tells accounts apart ("christos.ch…@outlook.com").
func shortenTabLabel(label string, width int) string {
	if ansi.StringWidth(label) <= width {
		return label
	}
	if at := strings.LastIndex(label, "@"); at > 0 {
		domain := label[at:]
		if nameWidth := width - ansi.StringWidth(domain); nameWidth >= 2 {
			return ansi.Truncate(label[:at], nameWidth, "…") + domain
		}
	}
	return ansi.Truncate(label, width, "…")
}

// tabAt returns the index of the account tab drawn at column x of the tab
// row, or -1 when x is past the last tab.
func (m *Inbox) tabAt(x int) int {
	left := 0
	for i, tab := range m.renderedTabs() {
		right := left + lipgloss.Width(tab)
		if x >= left && x < right {
			return i
		}
		left = right
	}
	return -1
}

// viewAboveSearchOverlay renders the tab bar, the email list and the threaded
// status line: everything View draws before the search box.
func (m *Inbox) viewAboveSearchOverlay() string {
	var b strings.Builder
	// The tab bar is part of the "header" component. Skip it when a plugin
	// has hidden the header via matcha.ui.set_visible("header", false).
	if isUIVisible("header") && len(m.tabs) > 1 {
		b.WriteString(tabBarStyle.Render(lipgloss.JoinHorizontal(lipgloss.Top, m.renderedTabs()...)))
		b.WriteString("\n")
	}
	b.WriteString(m.list.View())
	if m.hiddenStatus != "" {
		b.WriteString("\n")
		b.WriteString(m.hiddenStatus)
	}
	// When threaded, the built-in status bar is hidden because it counts
	// list items (threads) instead of actual emails. Render our own.
	if m.isThreaded() {
		b.WriteString(m.threadedStatusBar())
	}
	return b.String()
}

// searchOverlayTop returns the row where View starts drawing the search box.
func (m *Inbox) searchOverlayTop() int {
	m.resizeList()
	return lipgloss.Height(m.viewAboveSearchOverlay())
}

// viewEmailCmd opens the email of a list item.
func (m *Inbox) viewEmailCmd(it item) tea.Cmd {
	index, uid, accountID := it.originalIndex, it.uid, it.accountID
	var email *fetcher.Email
	if m.searchActive {
		email = m.GetEmailAtIndex(index)
	}
	return func() tea.Msg {
		return ViewEmailMsg{Index: index, UID: uid, AccountID: accountID, Mailbox: m.mailbox, Email: email}
	}
}

// selectAndOpen moves the list cursor to email and opens it. Shared mailbox
// copies are merged in the list, so the Message-ID also counts as a match.
func (m *Inbox) selectAndOpen(email fetcher.Email) tea.Cmd {
	for i, listItem := range m.list.Items() {
		it, ok := listItem.(item)
		if !ok || it.uid == 0 {
			continue
		}
		sameCopy := it.uid == email.UID && it.accountID == email.AccountID
		sameMessage := email.MessageID != "" && m.GetEmailAtIndex(it.originalIndex) != nil &&
			m.GetEmailAtIndex(it.originalIndex).MessageID == email.MessageID
		if sameCopy || sameMessage {
			m.list.Select(i)
			return m.viewEmailCmd(it)
		}
	}
	return nil
}

func (m *Inbox) View() tea.View {
	m.resizeList()
	var b strings.Builder
	b.WriteString(m.viewAboveSearchOverlay())

	if m.searchOverlay != nil {
		b.WriteString("\n")
		b.WriteString(m.searchOverlay.View())
	}

	// Ensure we don't start gap calculation on the same line as the list
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}

	// The help/status bar is only rendered when the "status_bar" component
	// is visible (plugin can hide it via matcha.ui.set_visible).
	helpView := ""
	if isUIVisible("status_bar") {
		helpView = inboxHelpStyle.Render(m.list.Help.View(m.list))
	}

	if m.height > 0 {
		usedHeight := lipgloss.Height(b.String())
		helpHeight := lipgloss.Height(helpView)

		gap := m.height - usedHeight - helpHeight
		if gap > 0 {
			b.WriteString(strings.Repeat("\n", gap))
		}
	} else {
		b.WriteString("\n")
	}

	b.WriteString(helpView)

	v := tea.NewView(b.String())
	if config.MouseEnabled != nil && *config.MouseEnabled {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// threadedStatusBar renders a custom status bar for threaded view that shows
// the actual number of emails (not the number of threads/list-items).
func (m *Inbox) threadedStatusBar() string {
	total := m.totalEmailCount
	visible := total
	itemName := "emails"
	if visible == 1 {
		itemName = "email"
	}
	status := fmt.Sprintf("%d %s", visible, itemName)

	// When a filter is applied, show filtered count too
	if m.list.FilterState() == list.FilterApplied {
		filteredCount := len(m.list.VisibleItems())
		numFiltered := total - filteredCount
		if numFiltered > 0 {
			status += fmt.Sprintf(" • %d filtered", numFiltered)
		}
	}

	return m.list.Styles.StatusBar.Render(status)
}

// GetCurrentAccountID returns the currently selected account ID
func (m *Inbox) GetCurrentAccountID() string {
	return m.currentAccountID
}

func (m *Inbox) IsSearchActive() bool {
	return m != nil && (m.searchOverlay != nil || m.searchActive)
}

// SetLocalSearcher gives the search box the header copy on this computer.
func (m *Inbox) SetLocalSearcher(local LocalSearcher) {
	m.localSearcher = local
}

// SetHiddenSearcher gives the inbox hidden headers from the local copy.
func (m *Inbox) SetHiddenSearcher(searcher HiddenSearcher) {
	m.hiddenSearcher = searcher
	m.list.Title = m.getTitle()
}

// ShowHiddenMail opens hidden headers from the current folder and account tab.
// The returned command asks the app to refresh that folder's local header copy.
func (m *Inbox) ShowHiddenMail() tea.Cmd {
	if m.hiddenSearcher == nil {
		return nil
	}
	m.searchOverlay = nil
	m.searchActive = true
	m.searchQuery = "Hidden Mail"
	m.hiddenView = true
	m.refreshHiddenResults()
	m.visualMode = false
	m.selectedUIDs = make(map[uint32]string)
	m.selectionOrder = []uint32{}
	m.updateList()
	opened := SearchOpenedMsg{FolderName: m.headerFolderName(), AccountID: m.currentAccountID}
	return func() tea.Msg { return opened }
}

// refreshHiddenResults reads the current folder and tab without disturbing list
// selection when an index update leaves the displayed mail unchanged.
func (m *Inbox) refreshHiddenResults() bool {
	emails := dedupeEmailsForAccounts(m.hiddenSearcher.Hidden(m.headerFolderName(), m.searchAccountIDs()), m.accounts)
	mailLabel := "mails"
	if len(emails) == 1 {
		mailLabel = "mail"
	}
	m.hiddenStatus = fmt.Sprintf("%d hidden %s: they name none of this account's addresses\nAdd extra addresses, always show mail from, or a show-in-account rule to show such mail.", len(emails), mailLabel)
	if reflect.DeepEqual(m.searchResults, emails) {
		return false
	}
	m.searchResults = emails
	return true
}

func (m *Inbox) hiddenCount() int {
	if m.hiddenSearcher == nil {
		return 0
	}
	return m.hiddenSearcher.HiddenCount(m.headerFolderName(), m.searchAccountIDs())
}

func (m *Inbox) headerFolderName() string {
	if m.folderName != "" {
		return m.folderName
	}
	if m.mailbox == MailboxInbox {
		return "INBOX"
	}
	return string(m.mailbox)
}

// SetSearchHistory gives the search box the saved recent searches.
func (m *Inbox) SetSearchHistory(history SearchHistory) {
	m.searchHistory = history
}

// searchAccountIDs returns the accounts the open tab shows: one account, or
// every account on the ALL tab.
func (m *Inbox) searchAccountIDs() []string {
	if m.currentAccountID != "" {
		return []string{m.currentAccountID}
	}
	ids := make([]string, 0, len(m.accounts))
	for _, account := range m.accounts {
		ids = append(ids, account.ID)
	}
	return ids
}

func (m *Inbox) IsSearchOverlayOpen() bool {
	return m != nil && m.searchOverlay != nil
}

func (m *Inbox) IsFilterActive() bool {
	return m != nil && (m.list.FilterState() == list.Filtering || m.list.FilterState() == list.FilterApplied)
}

// GetEmailAtIndex returns the email at the given index for the current view
func (m *Inbox) GetEmailAtIndex(index int) *fetcher.Email {
	displayEmails := m.displayEmails()

	if index >= 0 && index < len(displayEmails) {
		return &displayEmails[index]
	}
	return nil
}

func (m *Inbox) GetMailbox() MailboxKind {
	return m.mailbox
}

// GetSelectedEmail returns the currently selected email, or nil if none is selected.
func (m *Inbox) GetSelectedEmail() *fetcher.Email {
	selectedItem, ok := m.list.SelectedItem().(item)
	if !ok {
		return nil
	}
	return m.GetEmailAtIndex(selectedItem.originalIndex)
}

// MarkEmailAsRead marks an email as read by UID and account ID, updating it in all stores.
func (m *Inbox) MarkEmailAsRead(uid uint32, accountID string) {
	for i := range m.allEmails {
		if m.allEmails[i].UID == uid && m.allEmails[i].AccountID == accountID {
			m.allEmails[i].IsRead = true
			break
		}
	}
	if emails, ok := m.emailsByAccount[accountID]; ok {
		for i := range emails {
			if emails[i].UID == uid {
				emails[i].IsRead = true
				break
			}
		}
	}
	m.updateList()
}

// MarkEmailAsUnread marks an email as unread by UID and account ID, updating it in all stores.
func (m *Inbox) MarkEmailAsUnread(uid uint32, accountID string) {
	for i := range m.allEmails {
		if m.allEmails[i].UID == uid && m.allEmails[i].AccountID == accountID {
			m.allEmails[i].IsRead = false
			break
		}
	}
	if emails, ok := m.emailsByAccount[accountID]; ok {
		for i := range emails {
			if emails[i].UID == uid {
				emails[i].IsRead = false
				break
			}
		}
	}
	m.updateList()
}

// updateVisualSelection updates the selected UIDs based on anchor and current index
func (m *Inbox) updateVisualSelection() {
	if !m.visualMode {
		return
	}

	currentIdx := m.list.Index()
	start := m.visualAnchor
	end := currentIdx

	if start > end {
		start, end = end, start
	}

	// Clear and rebuild selection
	m.selectedUIDs = make(map[uint32]string)
	m.selectionOrder = []uint32{}

	items := m.list.Items()
	firstAccountID := ""
	for i := start; i <= end && i < len(items); i++ {
		if itm, ok := items[i].(item); ok {
			if itm.uid == 0 {
				continue
			}
			// Ensure all selected emails are from the same account (prevent cross-account batch ops)
			if firstAccountID == "" {
				firstAccountID = itm.accountID
			}
			if itm.accountID != firstAccountID {
				// Don't add emails from different accounts
				continue
			}

			if _, exists := m.selectedUIDs[itm.uid]; !exists {
				m.selectedUIDs[itm.uid] = itm.accountID
				m.selectionOrder = append(m.selectionOrder, itm.uid)
			}
		}
	}

	m.updateListTitle()
}

// updateListTitle updates the title to show selection count when in visual mode
func (m *Inbox) updateListTitle() {
	if m.visualMode && len(m.selectedUIDs) > 0 {
		baseTitle := m.getBaseTitle()
		m.list.Title = fmt.Sprintf("%s - VISUAL (%d selected)", baseTitle, len(m.selectedUIDs))
	} else {
		m.list.Title = m.getTitle()
	}
}

// RemoveEmails removes multiple emails by UID and account ID (batch operation)
func (m *Inbox) RemoveEmails(uids []uint32, accountID string) {
	uidSet := make(map[uint32]bool)
	for _, uid := range uids {
		uidSet[uid] = true
	}

	// Remove from account-specific list
	if emails, ok := m.emailsByAccount[accountID]; ok {
		var filtered []fetcher.Email
		for _, e := range emails {
			if !uidSet[e.UID] {
				filtered = append(filtered, e)
			}
		}
		m.emailsByAccount[accountID] = filtered
	}

	// Remove from all emails list
	var filteredAll []fetcher.Email
	for _, e := range m.allEmails {
		if !uidSet[e.UID] || e.AccountID != accountID {
			filteredAll = append(filteredAll, e)
		}
	}
	m.allEmails = filteredAll

	m.updateList()
}

// RemoveEmail removes an email by UID and account ID
func (m *Inbox) RemoveEmail(uid uint32, accountID string) {
	// Remove from account-specific list
	if emails, ok := m.emailsByAccount[accountID]; ok {
		var filtered []fetcher.Email
		for _, e := range emails {
			if e.UID != uid {
				filtered = append(filtered, e)
			}
		}
		m.emailsByAccount[accountID] = filtered
	}

	// Remove from all emails list
	var filteredAll []fetcher.Email
	for _, e := range m.allEmails {
		if e.UID != uid || e.AccountID != accountID {
			filteredAll = append(filteredAll, e)
		}
	}
	m.allEmails = filteredAll

	m.updateList()
}

// SetSize sets the width and height of the inbox, then updates the list.
func (m *Inbox) SetSize(width, height int) {
	m.width = width
	m.height = height
	m.list.SetWidth(width)
	m.resizeList()
}

// SetFolderName sets a custom folder name for the inbox title.
func (m *Inbox) SetFolderName(name string) {
	if name != m.folderName && m.hiddenView {
		m.searchActive = false
		m.searchQuery = ""
		m.searchResults = nil
		m.hiddenView = false
		m.hiddenStatus = ""
	}
	m.folderName = name
	m.updateList()
}

// SetPluginStatus sets a persistent status string from plugins, shown in the title.
func (m *Inbox) SetPluginStatus(status string) {
	m.pluginStatus = status
	m.list.Title = m.getTitle()
}

// SetPluginKeyBindings sets the plugin-registered key bindings for display in the help bar.
func (m *Inbox) SetPluginKeyBindings(bindings []PluginKeyBinding) {
	m.pluginKeyBindings = bindings
}

// SetEmails updates all emails (used after fetch)
func (m *Inbox) SetEmails(emails []fetcher.Email, accounts []config.Account) {
	m.accounts = accounts
	m.allEmails = dedupeEmailsForAccounts(emails, accounts)
	m.noMoreByAccount = make(map[string]bool)

	// Rebuild tabs: empty for single account, "ALL" + accounts for multiple
	tabs := make([]AccountTab, 0, 1+len(accounts))
	if len(accounts) <= 1 {
		tabs = []AccountTab{{ID: "", Label: "", Email: ""}}
	} else {
		tabs = append(tabs, AccountTab{ID: "", Label: "ALL", Email: ""})
		for _, acc := range accounts {
			displayEmail := accountDisplayEmail(acc)
			tabs = append(tabs, AccountTab{ID: acc.ID, Label: displayEmail, Email: displayEmail})
		}
	}
	m.tabs = tabs

	// Re-group emails by account
	m.emailsByAccount = make(map[string][]fetcher.Email)
	for _, email := range emails {
		m.emailsByAccount[email.AccountID] = append(m.emailsByAccount[email.AccountID], email)
	}

	// Update email counts
	m.emailCountByAcct = make(map[string]int)
	for accID, accEmails := range m.emailsByAccount {
		m.emailCountByAcct[accID] = len(accEmails)
	}

	m.updateList()
}

func inboxEmailID(email fetcher.Email) string {
	return fmt.Sprintf("%s:%d", email.AccountID, email.UID)
}

func threadItemKey(node *threading.ThreadNode) string {
	if node == nil {
		return ""
	}
	if node.EmailID != "" {
		return node.EmailID
	}
	for _, child := range node.Children {
		if key := threadItemKey(child); key != "" {
			return key
		}
	}
	return ""
}

func firstEmailNode(node *threading.ThreadNode) *threading.ThreadNode {
	if node == nil {
		return nil
	}
	if node.EmailID != "" {
		return node
	}
	for _, child := range node.Children {
		if first := firstEmailNode(child); first != nil {
			return first
		}
	}
	return nil
}

func latestEmailNode(node *threading.ThreadNode) *threading.ThreadNode {
	if node == nil {
		return nil
	}
	var latest *threading.ThreadNode
	if node.EmailID != "" {
		latest = node
	}
	for _, child := range node.Children {
		candidate := latestEmailNode(child)
		if candidate == nil {
			continue
		}
		if latest == nil || candidate.Date.After(latest.Date) ||
			(candidate.Date.Equal(latest.Date) && candidate.EmailID < latest.EmailID) {
			latest = candidate
		}
	}
	return latest
}

func threadRead(emails []fetcher.Email, emailIndex map[string]int, node *threading.ThreadNode) bool {
	if node == nil {
		return true
	}
	read := true
	if node.EmailID != "" {
		if idx, ok := emailIndex[node.EmailID]; ok && !emails[idx].IsRead {
			read = false
		}
	}
	for _, child := range node.Children {
		if !threadRead(emails, emailIndex, child) {
			read = false
		}
	}
	return read
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// sortTime preserves the displayed email date while choosing its list position.
func (m *Inbox) sortTime(email fetcher.Email) time.Time {
	for _, account := range m.accounts {
		if account.ID == email.AccountID && account.SortBy == "arrival_date" && !email.ArrivalDate.IsZero() {
			return email.ArrivalDate
		}
	}
	return email.Date
}

// SetAccountSorting applies saved account choices without replacing folder mail.
func (m *Inbox) SetAccountSorting(accounts []config.Account) {
	selected, hasSelected := m.list.SelectedItem().(item)
	m.accounts = accounts
	m.updateList()
	if hasSelected {
		for i, entry := range m.list.Items() {
			if candidate, ok := entry.(item); ok && candidate.uid == selected.uid && candidate.accountID == selected.accountID {
				m.list.Select(i)
				break
			}
		}
	}
}
