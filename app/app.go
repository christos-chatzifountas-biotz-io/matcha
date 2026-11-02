package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/mail"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	overlay "github.com/floatpane/bubble-overlay"
	"github.com/floatpane/matcha/backend"
	_ "github.com/floatpane/matcha/backend/imap"    // register the IMAP backend
	_ "github.com/floatpane/matcha/backend/jmap"    // register the JMAP backend
	_ "github.com/floatpane/matcha/backend/maildir" // register the Maildir backend
	_ "github.com/floatpane/matcha/backend/pop3"    // register the POP3 backend
	matchaCli "github.com/floatpane/matcha/cli"
	"github.com/floatpane/matcha/clib/macos"
	"github.com/floatpane/matcha/config"
	"github.com/floatpane/matcha/daemonclient"
	"github.com/floatpane/matcha/daemonrpc"
	"github.com/floatpane/matcha/fetcher"
	"github.com/floatpane/matcha/i18n"
	"github.com/floatpane/matcha/internal/editor"
	"github.com/floatpane/matcha/internal/emailactions"
	"github.com/floatpane/matcha/internal/emailstore"
	"github.com/floatpane/matcha/internal/exportcmd"
	"github.com/floatpane/matcha/internal/fetchcmd"
	"github.com/floatpane/matcha/internal/headerindex"
	"github.com/floatpane/matcha/internal/idledaemon"
	"github.com/floatpane/matcha/internal/logging"
	"github.com/floatpane/matcha/internal/loglevel"
	"github.com/floatpane/matcha/internal/mailrules"
	"github.com/floatpane/matcha/internal/notifications"
	"github.com/floatpane/matcha/internal/palette"
	"github.com/floatpane/matcha/internal/pluginbridge"
	"github.com/floatpane/matcha/internal/send"
	"github.com/floatpane/matcha/notify"
	"github.com/floatpane/matcha/plugin"
	"github.com/floatpane/matcha/theme"
	"github.com/floatpane/matcha/tui"
	"github.com/google/uuid"
)

// goosDarwin is the runtime.GOOS value for macOS.
const goosDarwin = "darwin"

// goosLinux is the runtime.GOOS value for Linux.
const goosLinux = "linux"

type logEntryMsg struct {
	entry logging.Entry
}

type clearErrorNotifMsg struct{}
type clearPluginNotifMsg struct{}

const maxBodyFetchRetries = 3

// Model is the main Bubble Tea application model that orchestrates the TUI.
type Model struct {
	current       tea.Model
	previousModel tea.Model
	config        *config.Config
	plugins       *plugin.Manager
	store         *emailstore.Store
	folderInbox   *tui.FolderInbox
	width         int
	height        int

	idleWatcher *fetcher.IdleWatcher
	idleUpdates chan fetcher.IdleUpdate

	providers   map[string]backend.Provider
	providersMu sync.RWMutex
	service     daemonclient.Service

	// headerIndex is the header copy the search box matches while typing.
	headerIndex               *headerindex.Index
	discoveredAddressAccounts map[string]bool
	addressSettings           map[string]string

	pendingPrompt *plugin.PendingPrompt
	mailtoURL     *url.URL

	showLogPanel bool
	logCh        <-chan logging.Entry
	logPanel     *tui.LogPanel

	palette *palette.Palette

	pendingJobID  string
	sendNotice    string
	pendingExport *emailstore.PendingExport

	actionManager *emailactions.Manager
	pluginBridge  *pluginbridge.Manager

	errorNotification overlay.Notification
	showErrorNotif    bool

	pluginNotification overlay.Notification
	showPluginNotif    bool

	// Track body fetch retries to prevent infinite loops
	pendingBodyFetchUID   uint32
	pendingBodyFetchCount int

	mailRuleRunner    *mailrules.Runner
	mailRuleRunning   bool
	mailRuleLastError string
}

func replyFromOverride(cfg *config.Config, email fetcher.Email) string {
	if cfg == nil {
		return ""
	}
	accountID := email.AccountID
	if accountID == "" && len(cfg.Accounts) > 0 {
		accountID = cfg.GetFirstAccount().ID
	}
	var account *config.Account
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == accountID {
			account = &cfg.Accounts[i]
			break
		}
	}
	if account == nil {
		return ""
	}
	bare := func(value string) string {
		if address, err := mail.ParseAddress(value); err == nil {
			value = address.Address
		}
		return strings.ToLower(strings.TrimSpace(value))
	}
	fetchAddress := bare(account.GetFetchEmail())
	recipients := [2][]string{email.To, email.DeliveryRecipients}
	for _, source := range recipients {
		for _, recipient := range source {
			if bare(recipient) == fetchAddress {
				return ""
			}
		}
	}
	formatFrom := func(address string) string {
		if account.Name != "" {
			return fmt.Sprintf("%s <%s>", account.Name, address)
		}
		return address
	}
	for _, source := range recipients {
		for _, recipient := range source {
			address := bare(recipient)
			for _, extra := range account.ExtraAddresses {
				if address == bare(extra) {
					return formatFrom(address)
				}
			}
		}
	}
	if account.CatchAll {
		if len(email.DeliveryRecipients) > 0 {
			return formatFrom(bare(email.DeliveryRecipients[0]))
		}
		if len(email.To) > 0 {
			return formatFrom(bare(email.To[0]))
		}
	}
	return ""
}

// NewModel creates the initial TUI model.
func NewModel(cfg *config.Config, mailtoURL *url.URL, plugins *plugin.Manager, showLogPanel bool, logCh <-chan logging.Entry, logPanel *tui.LogPanel) *Model {
	idleUpdates := make(chan fetcher.IdleUpdate, 16)
	store := emailstore.NewStore()
	m := &Model{
		current:                   tui.NewChoice(),
		config:                    cfg,
		plugins:                   plugins,
		store:                     store,
		idleUpdates:               idleUpdates,
		idleWatcher:               fetcher.NewIdleWatcher(idleUpdates),
		providers:                 make(map[string]backend.Provider),
		headerIndex:               headerindex.New(),
		discoveredAddressAccounts: make(map[string]bool),
		mailtoURL:                 mailtoURL,
		showLogPanel:              showLogPanel,
		logCh:                     logCh,
		logPanel:                  logPanel,
		palette:                   palette.New(),
		actionManager:             emailactions.NewManager(emailactions.Dependencies{Config: cfg}, store, nil),
		pluginBridge:              pluginbridge.NewManager(plugins, store, cfg, nil),
	}

	m.updateCurrentFromConfig(cfg, mailtoURL)
	m.addressSettings = accountAddressSettings(cfg)
	m.updateSubordinates()
	return m
}

func (m *Model) updateCurrentFromConfig(cfg *config.Config, mailtoURL *url.URL) {
	if cfg == nil || !cfg.HasAccounts() {
		hideTips := false
		if cfg != nil {
			hideTips = cfg.HideTips
		}
		m.current = tui.NewLogin(hideTips)
		m.config = nil
		return
	}

	m.config = cfg
	config.MouseEnabled = cfg.MouseEnabled

	switch {
	case mailtoURL != nil:
		to, subject, body := parseMailto(mailtoURL)
		composer := tui.NewComposerWithAccounts(cfg.Accounts, cfg.Accounts[0].ID, to, subject, body, cfg.HideTips)
		m.applyComposerOptions(composer)
		m.current = composer
	case !cfg.HasSeenSetupGuide:
		m.current = newSetupGuide()
	default:
		m.current = tui.NewChoice()
	}
}

func parseMailto(u *url.URL) (to, subject, body string) {
	to = u.Opaque
	if to == "" {
		to = u.Path
	}
	if to == "" {
		to = u.Query().Get("to")
	}
	return to, u.Query().Get("subject"), u.Query().Get("body")
}

func newSetupGuide() *tui.SetupGuide {
	isMac := runtime.GOOS == goosDarwin
	isLinux := runtime.GOOS == goosLinux

	var installHelper func() error
	if isMac {
		installHelper = func() error {
			return silenced(func() error { return matchaCli.RunHelper([]string{"install"}) })
		}
	}
	var setupMailto func() error
	if isMac || isLinux {
		setupMailto = func() error { return silenced(matchaCli.SetupMailto) }
	}

	return tui.NewSetupGuide(isMac, isLinux, installHelper, setupMailto)
}

func silenced(fn func() error) error {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fn()
	}
	defer func() { _ = devNull.Close() }()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, devNull
	runErr := fn()
	os.Stdout, os.Stderr = origOut, origErr
	return runErr
}

func (m *Model) newSettings() *tui.Settings {
	s := tui.NewSettings(m.config)
	if m.plugins != nil {
		s.SetPlugins(m.plugins)
	}
	s.SetHiddenSearcher(m.headerIndex)
	return s
}

func getCurrentComposer(current tea.Model) *tui.Composer {
	if c, ok := current.(*tui.Composer); ok {
		return c
	}
	if rs, ok := current.(*tui.ReplySplitView); ok {
		return rs.Composer()
	}
	return nil
}

func (m *Model) applyComposerOptions(c *tui.Composer) {
	if c == nil || m.config == nil {
		return
	}
	c.SetSpellcheckOptions(m.config.DisableSpellcheck, m.config.DisableSpellSuggestions)
	c.SetVimMode(m.config.ComposerVimMode)
}

func (m *Model) ensureProviders() {
	if m.config == nil {
		return
	}
	for _, acct := range m.config.Accounts {
		m.providersMu.RLock()
		_, ok := m.providers[acct.ID]
		m.providersMu.RUnlock()
		if ok {
			continue
		}
		p, err := backend.New(&acct)
		if err != nil {
			log.Printf("backend: failed to create provider for %s: %v", acct.Email, err)
			continue
		}
		m.providersMu.Lock()
		m.providers[acct.ID] = p
		m.providersMu.Unlock()
	}
}

func (m *Model) resolveProvider(acct *config.Account) backend.Provider {
	if acct == nil {
		return nil
	}
	m.providersMu.RLock()
	p := m.providers[acct.ID]
	m.providersMu.RUnlock()
	return p
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.current.Init(), nextMailRuleRun(time.Second)}
	if m.showLogPanel && m.logCh != nil {
		cmds = append(cmds, waitForLogEntry(m.logCh))
	}
	return tea.Batch(cmds...)
}

func waitForLogEntry(ch <-chan logging.Entry) tea.Cmd {
	return func() tea.Msg {
		entry := <-ch
		return logEntryMsg{entry: entry}
	}
}

func (m *Model) syncUnreadBadge() {
	if runtime.GOOS != "darwin" && loglevel.Get() < loglevel.LevelDebug {
		return
	}
	count := notifications.CountUnread(m.store.EmailsByAcct, m.store.FolderEmails)
	loglevel.Debugf("unread badge count: %d", count)
	if runtime.GOOS != "darwin" {
		return
	}
	_ = macos.SetBadge(count)
}

func (m *Model) currentWindowSize() tea.WindowSizeMsg {
	return tea.WindowSizeMsg{
		Width:  m.width,
		Height: m.contentHeight(),
	}
}

func (m *Model) contentHeight() int {
	height := m.height - m.logPanelHeight()
	if height < 1 {
		return 1
	}
	return height
}

func (m *Model) renderWithLogPanel(content string) string {
	panelHeight := m.logPanelHeight()
	if panelHeight == 0 || m.logPanel == nil {
		return content
	}
	contentHeight := m.contentHeight()
	mainContent := lipgloss.NewStyle().
		MaxHeight(contentHeight).
		Height(contentHeight).
		Render(content)
	m.logPanel.SetSize(m.width, panelHeight)
	return lipgloss.JoinVertical(lipgloss.Left, mainContent, m.logPanel.View())
}

func (m *Model) logPanelHeight() int {
	if !m.showLogPanel || m.height < 12 || m.width < 20 {
		return 0
	}
	if m.height < 20 {
		return 4
	}
	return 7
}

func (m *Model) showErrorCmd(msg string) tea.Cmd {
	col := max(0, m.width-44)
	m.errorNotification = overlay.NewError(
		overlay.WithMessage(msg),
		overlay.WithKey(config.Keybinds.Global.DismissNotification),
		overlay.WithPosition(0, col),
	)
	m.showErrorNotif = true
	return tea.Tick(8*time.Second, func(time.Time) tea.Msg { return clearErrorNotifMsg{} })
}

func (m *Model) showInfoCmd(msg string) tea.Cmd {
	col := max(0, m.width-44)
	m.errorNotification = overlay.NewInfo(
		overlay.WithMessage(msg),
		overlay.WithKey(config.Keybinds.Global.DismissNotification),
		overlay.WithPosition(0, col),
	)
	m.showErrorNotif = true
	return tea.Tick(4*time.Second, func(time.Time) tea.Msg { return clearErrorNotifMsg{} })
}

func (m *Model) renderSendNoticeOverlay(content string) string {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.ActiveTheme.Accent).
		Padding(0, 1).
		Render(m.sendNotice)
	lines := strings.Split(box, "\n")
	boxWidth := lipgloss.Width(lines[0])
	col := max(0, m.width-boxWidth)
	return overlay.Block(content, lines, 0, col)
}

func (m *Model) renderActionNoticeOverlay(content string) string {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.ActiveTheme.Accent).
		Padding(0, 1).
		Render(m.actionManager.ActionNotice)
	lines := strings.Split(box, "\n")
	boxWidth := lipgloss.Width(lines[0])
	col := max(0, m.width-boxWidth)
	return overlay.Block(content, lines, 0, col)
}

func (m *Model) folderName() string {
	if m.folderInbox != nil {
		return m.folderInbox.GetCurrentFolder()
	}
	return emailstore.FolderInbox
}

func (m *Model) updateSubordinates() {
	m.actionManager.UpdateDependencies(emailactions.Dependencies{Service: m.service, Config: m.config})
	m.actionManager.UpdateStore(m.store)
	m.actionManager.SetFolderInbox(m.folderInbox)
	m.pluginBridge.SetConfig(m.config)
	m.pluginBridge.SetFolderInbox(m.folderInbox)
}

func (m *Model) notifyNewMail(accountID, folderName string) {
	if m.config != nil && !m.config.DisableNotifications {
		accountName := accountID
		if acc := m.config.GetAccountByID(accountID); acc != nil {
			accountName = acc.Email
		}
		go notify.Send("Matcha", fmt.Sprintf("New mail in %s (%s)", folderName, accountName)) //nolint:errcheck
	}
}

func (m *Model) unsubscribeFolder(accountID, folderName string) {
	if m.service != nil && m.service.IsDaemon() {
		_ = m.service.Unsubscribe(accountID, folderName)
	} else {
		m.idleWatcher.Stop(accountID)
	}
}

func (m *Model) subscribeFolder(accountID, folderName string) {
	if m.service != nil && m.service.IsDaemon() {
		_ = m.service.Subscribe(accountID, folderName)
	} else {
		acc := m.config.GetAccountByID(accountID)
		if acc != nil {
			m.idleWatcher.Watch(acc, folderName)
		}
	}
}

func (m *Model) cachedAttachmentsToFetcher(cached []config.CachedAttachment) []fetcher.Attachment {
	attachments := make([]fetcher.Attachment, 0, len(cached))
	for _, ca := range cached {
		att := fetcher.Attachment{
			Filename:         ca.Filename,
			PartID:           ca.PartID,
			Encoding:         ca.Encoding,
			MIMEType:         ca.MIMEType,
			ContentID:        ca.ContentID,
			Inline:           ca.Inline,
			IsSMIMESignature: ca.IsSMIMESignature,
			SMIMEVerified:    ca.SMIMEVerified,
			IsSMIMEEncrypted: ca.IsSMIMEEncrypted,
			IsCalendarInvite: ca.IsCalendarInvite,
		}
		if ca.IsCalendarInvite && len(ca.CalendarData) > 0 {
			att.Data = ca.CalendarData
		}
		if ca.Inline && len(ca.Data) > 0 {
			att.Data = ca.Data
		}
		attachments = append(attachments, att)
	}
	return attachments
}

func cachedAttachmentToConfig(a fetcher.Attachment) config.CachedAttachment {
	ca := config.CachedAttachment{
		Filename:         a.Filename,
		PartID:           a.PartID,
		Encoding:         a.Encoding,
		MIMEType:         a.MIMEType,
		ContentID:        a.ContentID,
		Inline:           a.Inline,
		IsSMIMESignature: a.IsSMIMESignature,
		SMIMEVerified:    a.SMIMEVerified,
		IsSMIMEEncrypted: a.IsSMIMEEncrypted,
		IsCalendarInvite: a.IsCalendarInvite,
	}
	if a.IsCalendarInvite && len(a.Data) > 0 {
		ca.CalendarData = a.Data
	}
	if a.Inline && len(a.Data) > 0 {
		ca.Data = a.Data
	}
	return ca
}

func (m *Model) cacheEmailBody(folderName string, uid uint32, accountID, body, bodyMIMEType string, attachments []fetcher.Attachment) {
	cachedAttachments := make([]config.CachedAttachment, 0, len(attachments))
	for _, a := range attachments {
		cachedAttachments = append(cachedAttachments, cachedAttachmentToConfig(a))
	}
	go func() {
		if err := config.SaveEmailBody(folderName, config.CachedEmailBody{
			UID:          uid,
			AccountID:    accountID,
			Body:         body,
			BodyMIMEType: bodyMIMEType,
			Attachments:  cachedAttachments,
		}, m.config.GetBodyCacheThreshold()); err != nil {
			loglevel.Debugf("error caching email body for UID %d: %v", uid, err)
		}
	}()
}

func (m *Model) pluginFlagCmds() []tea.Cmd {
	return m.pluginBridge.FlagCmds()
}

func (m *Model) pluginNotifyCmd() tea.Cmd {
	return m.pluginBridge.NotifyCmd()
}

func (m *Model) syncPluginStatus() {
	m.pluginBridge.SyncStatus(m.current)
}

func (m *Model) handlePluginKeyBinding(msg tea.KeyPressMsg) tea.Cmd {
	return m.pluginBridge.HandleKeyBinding(msg, m.current, &m.pendingPrompt)
}

func (m *Model) isSearchOverlayOpen() bool {
	return m.pluginBridge.IsSearchOverlayOpen(m.current)
}

func (m *Model) syncPluginKeyBindings() {
	m.pluginBridge.SyncKeyBindings(m.current)
}

func (m *Model) applyPluginFields(composer *tui.Composer) {
	m.pluginBridge.ApplyFields(composer)
}

func (m *Model) startComposerHooks(composer *tui.Composer) {
	m.plugins.CallComposerHook(plugin.HookComposerUpdated, composer.GetBody(), composer.GetSubject(), composer.GetTo(), composer.GetCc(), composer.GetBcc())
	m.syncPluginStatus()
	m.applyPluginFields(composer)
}

func (m *Model) saveDraftOnQuit(composer *tui.Composer) {
	if err := config.SaveDraft(composer.ToDraft()); err != nil {
		log.Printf("Error saving draft on quit: %v", err)
	}
}

func (m *Model) deleteDraftAfterSend(draftID string) {
	if draftID != "" {
		if err := config.DeleteDraft(draftID); err != nil {
			log.Printf("Error deleting draft after send: %v", err)
		}
	}
}

func (m *Model) saveContactsAfterSend(msg tui.SendEmailMsg) {
	if msg.To == "" {
		return
	}
	for _, r := range strings.Split(msg.To, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		name, email := send.ParseEmailAddress(r)
		if err := config.AddContactForAccount(name, email, msg.AccountID); err != nil {
			log.Printf("Error saving contact: %v", err)
		}
	}
}

func (m *Model) sendEmailNotice(msg tui.SendEmailMsg) string {
	if msg.SignPGP {
		if account := m.config.GetAccountByID(msg.AccountID); account != nil && account.PGPKeySource == "yubikey" {
			return "Touch your YubiKey to sign..."
		}
	}
	return "Sending email..."
}

func (m *Model) shutdownService() {
	m.idleWatcher.StopAll()
	if m.service != nil {
		m.service.Close() //nolint:errcheck,gosec
		// Drop the closed service so a later shutdown (e.g. ctrl+c after
		// esc-ing back to the menu) doesn't close it twice and panic, and
		// re-entering the inbox builds a fresh one.
		m.service = nil
	}
	fetcher.CloseIMAPPool()
}

func (m *Model) saveConfigAfterCredentials() bool {
	if err := config.SaveConfig(m.config); err != nil {
		log.Printf("could not save config: %v", err)
		return false
	}
	return true
}

// runOAuth2Flow hands the terminal to the browser sign-in helper, which prints
// instructions and, for Yahoo, reads the pasted redirect address.
func (m *Model) runOAuth2Flow(account config.Account, returnToSettings bool) tea.Cmd {
	done := func(err error) tea.Msg {
		return tui.OAuth2CompleteMsg{AccountID: account.ID, Email: account.Email, Err: err, ReturnToSettings: returnToSettings}
	}
	cmd, err := account.OAuth2SignInCommand()
	if err != nil {
		return func() tea.Msg { return done(err) }
	}
	return tea.Exec(&pauseOnFailure{cmd: cmd}, done)
}

// pauseOnFailure runs a terminal command for tea.Exec and, when it fails, waits
// for Enter so its error output stays readable before the TUI redraws.
type pauseOnFailure struct {
	cmd    *exec.Cmd
	stdin  io.Reader
	stdout io.Writer
}

func (p *pauseOnFailure) SetStdin(r io.Reader)  { p.stdin, p.cmd.Stdin = r, r }
func (p *pauseOnFailure) SetStdout(w io.Writer) { p.stdout, p.cmd.Stdout = w, w }
func (p *pauseOnFailure) SetStderr(w io.Writer) { p.cmd.Stderr = w }

func (p *pauseOnFailure) Run() error {
	err := p.cmd.Run()
	if err != nil && p.stdin != nil && p.stdout != nil {
		_, _ = fmt.Fprint(p.stdout, "\nSign-in failed. Press Enter to return to Matcha.")
		_, _ = bufio.NewReader(p.stdin).ReadString('\n')
	}
	return err
}

func (m *Model) afterConfigSaved() {
	if m.service != nil {
		if err := m.service.ReloadConfig(); err != nil {
			log.Printf("config reload: %v", err)
		}
	}
	if m.folderInbox != nil {
		if inbox := m.folderInbox.GetInbox(); inbox != nil {
			inbox.SetAccountSorting(m.config.Accounts)
		}
		m.folderInbox.SetDateFormat(m.config.GetDateFormat())
		m.folderInbox.SetDetailedDates(m.config.EnableDetailedDates)
		m.folderInbox.SetDefaultThreaded(m.config.EnableThreaded)
		m.folderInbox.SetDisableImages(m.config.DisableImages)
		m.folderInbox.SetSplitOrientation(m.config.GetSplitPaneOrientation())
	}
	m.updateSubordinates()
}

func (m *Model) afterLanguageChanged() {
	switch m.current.(type) {
	case *tui.Composer, *tui.Inbox, *tui.FolderInbox:
		m.current = tui.NewChoice()
	default:
		m.current = m.newSettings()
	}
	m.current, _ = m.current.Update(m.currentWindowSize())
}

func (m *Model) deleteAccount(accountID string) {
	if m.config == nil {
		return
	}
	if m.config.RemoveAccount(accountID) {
		if err := config.CleanupAccountCache(accountID); err != nil {
			log.Printf("could not clean account cache: %v", err)
		}
		if err := config.SaveConfig(m.config); err != nil {
			log.Printf("could not save config: %v", err)
		}
	}
	m.store.RemoveAccount(accountID)
	m.current = m.newSettings()
	m.current, _ = m.current.Update(m.currentWindowSize())
	m.updateSubordinates()
}

func (m *Model) loadFolderInboxCache(folderName string) {
	if cached := m.store.FolderEmails[folderName]; len(cached) > 0 {
		m.folderInbox.SetEmails(cached, m.config.Accounts)
	} else if diskCached := emailstore.LoadFolderEmailsFromCache(folderName); len(diskCached) > 0 {
		m.store.FolderEmails[folderName] = diskCached
		m.store.SetFolder(folderName, diskCached)
		m.folderInbox.SetEmails(diskCached, m.config.Accounts)
	}
}

func (m *Model) setFolderInboxDefaults() {
	m.folderInbox.SetDateFormat(m.config.GetDateFormat())
	m.folderInbox.SetDetailedDates(m.config.EnableDetailedDates)
	m.folderInbox.SetDefaultThreaded(m.config.EnableThreaded)
	m.folderInbox.SetDisableImages(m.config.DisableImages)
	m.folderInbox.SetSplitOrientation(m.config.GetSplitPaneOrientation())
	m.folderInbox.GetInbox().SetPaginationPageSize(m.config.GetPaginationPageSize())
}

func (m *Model) fetchFolderEmailsCmd(folderName string) tea.Cmd {
	return fetchcmd.FetchFolderEmailsCmd(m.config, folderName, m.folderInbox.PaginationPageSize())
}

func (m *Model) setFolderInboxEmails(folderName string, emails []fetcher.Email) {
	m.store.SetFolder(folderName, emails)
	m.folderInbox.SetEmails(emails, m.config.Accounts)
	m.folderInbox.GetInbox().SetFolderName(folderName)
	m.folderInbox.SetLoadingEmails(false)
	m.syncPluginStatus()
	m.syncPluginKeyBindings()
}

func (m *Model) cacheFolderEmailBodies(folderName string, emails []fetcher.Email) {
	go emailstore.SaveFolderEmailsToCache(folderName, emails)
	go func() {
		validUIDs := make(map[uint32]string, len(emails))
		for _, e := range emails {
			validUIDs[e.UID] = e.AccountID
		}
		_ = config.PruneEmailBodyCache(folderName, validUIDs, m.config.GetBodyCacheThreshold())
	}()
}

func (m *Model) refreshFolderEmails(folderName string) tea.Cmd {
	delete(m.store.FolderEmails, folderName)
	m.folderInbox.SetRefreshing(true)
	return m.fetchFolderEmailsCmd(folderName)
}

// keepEmailsOfFailedAccounts adds the last known emails of accounts that could
// not be fetched, so a failed fetch does not empty their part of the folder.
func (m *Model) keepEmailsOfFailedAccounts(folderName string, fetched []fetcher.Email, failed map[string]string) []fetcher.Email {
	if len(failed) == 0 {
		return fetched
	}
	previous, ok := m.store.FolderEmails[folderName]
	if !ok {
		previous = emailstore.LoadFolderEmailsFromCache(folderName)
	}
	merged := slices.Clone(fetched)
	for _, email := range previous {
		if _, accountFailed := failed[email.AccountID]; accountFailed {
			merged = append(merged, email)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Date.After(merged[j].Date) })
	return merged
}

// folderLoadFailureMessage names the accounts whose folder fetch failed.
func (m *Model) folderLoadFailureMessage(folderName string, failed map[string]string) string {
	var parts []string
	for _, acc := range m.config.Accounts {
		if errText, ok := failed[acc.ID]; ok {
			parts = append(parts, fmt.Sprintf("%s: %s", acc.Email, errText))
		}
	}
	return fmt.Sprintf("Could not load %s from %s. Press %s to retry.",
		folderName, strings.Join(parts, "; "), config.Keybinds.Inbox.Refresh)
}

func (m *Model) fetchCurrentFolderEmails(folderName string) tea.Cmd {
	if cached, ok := m.store.FolderEmails[folderName]; ok {
		m.setFolderInboxEmails(folderName, cached)
		return tea.Batch(m.folderInbox.ContinueUnreadFetch(), m.pluginNotifyCmd())
	}
	if diskCached := emailstore.LoadFolderEmailsFromCache(folderName); len(diskCached) > 0 {
		m.store.FolderEmails[folderName] = diskCached
		m.setFolderInboxEmails(folderName, diskCached)
		return tea.Batch(m.fetchFolderEmailsCmd(folderName), m.pluginNotifyCmd())
	}
	m.folderInbox.SetLoadingEmails(true)
	return tea.Batch(m.fetchFolderEmailsCmd(folderName), m.pluginNotifyCmd())
}

func (m *Model) updateDaemonSubscriptions(previousFolder, newFolder string) {
	for i := range m.config.Accounts {
		accID := m.config.Accounts[i].ID
		folders, _ := config.GetCachedFolders(accID)
		if !slices.Contains(folders, newFolder) {
			m.unsubscribeFolder(accID, previousFolder)
			continue
		}
		if previousFolder != "" {
			m.unsubscribeFolder(accID, previousFolder)
		}
		m.subscribeFolder(accID, newFolder)
	}
}

func (m *Model) markEmailReadAndQueue(accountID string, uid uint32, suppress bool) tea.Cmd {
	if suppress {
		return nil
	}
	m.store.MarkEmailAsReadInStores(uid, accountID)
	var listCmd tea.Cmd
	if m.folderInbox != nil {
		listCmd = m.folderInbox.MarkEmailAsRead(uid, accountID)
	}
	account := m.config.GetAccountByID(accountID)
	if account != nil {
		return tea.Batch(listCmd, fetchcmd.MarkEmailAsReadCmd(account, uid, accountID, m.folderName()))
	}
	return listCmd
}

func (m *Model) maybeYubiKeyNotification(accountID string, attachments []fetcher.Attachment) tea.Cmd {
	if acct := m.config.GetAccountByID(accountID); acct != nil && acct.PGPKeySource == "yubikey" {
		for _, att := range attachments {
			if att.Filename == "pgp-status.internal" && att.IsPGPEncrypted {
				return m.showErrorCmd("Email decrypted with YubiKey")
			}
		}
	}
	return nil
}

func (m *Model) updateConfigFromCredentials(msg tui.Credentials) {
	fetchEmails := []string{""}
	if msg.FetchEmail != "" {
		fetchEmails = fetchEmails[:0]
		for _, fe := range strings.Split(msg.FetchEmail, ",") {
			if trimmed := strings.TrimSpace(fe); trimmed != "" {
				fetchEmails = append(fetchEmails, trimmed)
			}
		}
		if len(fetchEmails) == 0 {
			fetchEmails = []string{""}
		}
	}

	if m.config == nil {
		m.config = &config.Config{}
	}

	if login, ok := m.current.(*tui.Login); ok && login.IsEditMode() {
		existingID := login.GetAccountID()
		edited := buildAccount(existingID, msg, fetchEmails[0])
		for i, acc := range m.config.Accounts {
			if acc.ID == existingID {
				m.config.Accounts[i] = withEditedFields(acc, edited)
				break
			}
		}
	} else {
		for _, fe := range fetchEmails {
			account := buildAccount(uuid.New().String(), msg, fe)
			m.config.AddAccount(account)
		}
	}
}

// withEditedFields returns existing with the fields the account form shows
// taken from edited. Settings the form does not show, such as S/MIME and PGP
// keys or the OAuth2 token and password commands, stay as they were.
func withEditedFields(existing, edited config.Account) config.Account {
	existing.Name = edited.Name
	existing.Email = edited.Email
	existing.ServiceProvider = edited.ServiceProvider
	existing.FetchEmail = edited.FetchEmail
	existing.SendAsEmail = edited.SendAsEmail
	existing.AlwaysShowMailFrom = edited.AlwaysShowMailFrom
	existing.CatchAll = edited.CatchAll
	existing.ShowUnaddressedMail = edited.ShowUnaddressedMail
	existing.DiscoverExtraAddresses = edited.DiscoverExtraAddresses
	existing.IgnoredAddresses = slices.Clone(existing.IgnoredAddresses)
	for _, address := range existing.ExtraAddresses {
		address = strings.ToLower(strings.TrimSpace(address))
		if !containsAddress(edited.ExtraAddresses, address) && !containsAddress(existing.IgnoredAddresses, address) {
			existing.IgnoredAddresses = append(existing.IgnoredAddresses, address)
		}
	}
	for _, address := range edited.ExtraAddresses {
		address = strings.ToLower(strings.TrimSpace(address))
		existing.IgnoredAddresses = removeAddress(existing.IgnoredAddresses, address)
	}
	existing.ExtraAddresses = edited.ExtraAddresses
	existing.AuthMethod = edited.AuthMethod
	existing.Protocol = edited.Protocol
	existing.Insecure = edited.Insecure
	existing.JMAPEndpoint = edited.JMAPEndpoint
	existing.POP3Server = edited.POP3Server
	existing.POP3Port = edited.POP3Port
	existing.MaildirPath = edited.MaildirPath
	existing.IMAPServer = edited.IMAPServer
	existing.IMAPPort = edited.IMAPPort
	existing.SMTPServer = edited.SMTPServer
	existing.SMTPPort = edited.SMTPPort
	if edited.Password != "" {
		existing.Password = edited.Password
	}
	return existing
}

func buildAccount(id string, msg tui.Credentials, fetchEmail string) config.Account {
	account := config.Account{
		ID:                     id,
		Name:                   msg.Name,
		Email:                  msg.Host,
		Password:               msg.Password,
		ServiceProvider:        msg.Provider,
		FetchEmail:             fetchEmail,
		SendAsEmail:            msg.SendAsEmail,
		CatchAll:               msg.CatchAll,
		ShowUnaddressedMail:    msg.ShowUnaddressedMail,
		DiscoverExtraAddresses: msg.DiscoverExtraAddresses,
		ExtraAddresses:         msg.ExtraAddresses,
		AlwaysShowMailFrom:     msg.AlwaysShowMailFrom,
		AuthMethod:             msg.AuthMethod,
		Protocol:               msg.Protocol,
		Insecure:               msg.Insecure,
		JMAPEndpoint:           msg.JMAPEndpoint,
		POP3Server:             msg.POP3Server,
		POP3Port:               msg.POP3Port,
		MaildirPath:            msg.MaildirPath,
		SC:                     &config.SessionCache{},
	}
	if msg.Provider == "custom" || msg.Protocol == "pop3" {
		account.IMAPServer = msg.IMAPServer
		account.IMAPPort = msg.IMAPPort
		account.SMTPServer = msg.SMTPServer
		account.SMTPPort = msg.SMTPPort
	}
	if account.FetchEmail == "" && account.Email != "" {
		account.FetchEmail = account.Email
	}
	return account
}

func (m *Model) SetPasswordPrompt() {
	m.current = tui.NewPasswordPrompt()
}

func (m *Model) SetPlugins(plugins *plugin.Manager) {
	m.plugins = plugins
	m.pluginBridge.SetPlugins(plugins)
}

func (m *Model) SetLogPanel(logger *logging.Buffer) {
	m.showLogPanel = true
	m.logCh = logger.Subscribe()
	m.logPanel = tui.NewLogPanel(logger)
}

func (m *Model) Config() *config.Config {
	return m.config
}

func (m *Model) FolderInbox() *tui.FolderInbox {
	return m.folderInbox
}

func (m *Model) sendEmailDependencies() *send.Dependencies {
	return &send.Dependencies{Service: m.service, Config: m.config}
}

func (m *Model) batchActionCmd() tea.Cmd {
	if m.actionManager.IsPending() {
		return m.actionManager.FlushPendingAction()
	}
	return nil
}

func (m *Model) updateCurrentWindowSize() {
	m.current, _ = m.current.Update(m.currentWindowSize())
}

func (m *Model) setChoiceMenu() tea.Cmd {
	m.current = tui.NewChoice()
	m.updateCurrentWindowSize()
	return m.current.Init()
}

func (m *Model) restoreView() {
	if m.previousModel != nil {
		m.current = m.previousModel
		m.previousModel = nil
	}
}

// Update handles all incoming Bubble Tea messages and orchestrates the TUI.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo
	var cmd tea.Cmd
	var cmds []tea.Cmd
	searchWasActive := false
	filterWasActive := false
	splitWasOpen := false

	if _, ok := msg.(logEntryMsg); ok {
		return m, waitForLogEntry(m.logCh)
	}

	if msg, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = msg.Width
		m.height = msg.Height
		m.palette.UpdateSize(m.width, m.contentHeight())
		m.current, cmd = m.current.Update(m.currentWindowSize())
		return m, cmd
	}

	switch msg := msg.(type) {
	case mailRuleTickMsg:
		return m, m.startMailRules()
	case mailRuleDoneMsg:
		return m, m.finishMailRules(msg)
	}

	if m.palette.IsOpen() {
		if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
			switch keyMsg.String() {
			case "ctrl+c":
				m.shutdownService()
				return m, tea.Quit
			case config.Keybinds.Global.Cancel:
				m.palette.Close()
				return m, nil
			case "enter":
				actionCmd := m.palette.HandleKey(keyMsg)
				m.palette.Close()
				return m, actionCmd
			default:
				return m, m.palette.HandleKey(keyMsg)
			}
		}
		if _, ok := msg.(tea.MouseWheelMsg); ok {
			return m, m.palette.Update(msg)
		}
	} else if keyMsg, ok := msg.(tea.KeyPressMsg); ok &&
		config.Keybinds.Global.CommandPalette != "" &&
		keyMsg.String() == config.Keybinds.Global.CommandPalette &&
		palette.Allowed(m.current) && !m.composerHasVimFocus() {
		cmd := m.palette.Open(palette.BuildCommands(m.current, m.folderInbox, m.config), m.width, m.contentHeight())
		return m, cmd
	}

	if keyMsg, ok := msg.(tea.KeyPressMsg); ok && keyMsg.String() == config.Keybinds.Global.Cancel {
		switch current := m.current.(type) {
		case *tui.Inbox:
			searchWasActive = current.IsSearchActive()
			filterWasActive = current.IsFilterActive()
		case *tui.FolderInbox:
			if inbox := current.GetInbox(); inbox != nil {
				searchWasActive = inbox.IsSearchActive()
				filterWasActive = inbox.IsFilterActive()
			}
			splitWasOpen = current.HasSplitPreview()
		}
	}

	// Folder fetch results are applied below to both the store and FolderInbox.
	// Forwarding them here would append a page twice and lose the next-page cmd.
	switch msg.(type) {
	case tui.FolderEmailsFetchedMsg, tui.FolderEmailsAppendedMsg:
	default:
		m.current, cmd = m.current.Update(msg)
		cmds = append(cmds, cmd)
	}

	if keyMsg, isKey := msg.(tea.KeyPressMsg); isKey {
		if composer := getCurrentComposer(m.current); composer != nil && m.plugins != nil {
			m.startComposerHooks(composer)
		}
		if m.plugins != nil && !m.isSearchOverlayOpen() && !m.composerHasVimFocus() {
			if bindingCmd := m.handlePluginKeyBinding(keyMsg); bindingCmd != nil {
				cmds = append(cmds, bindingCmd)
			}
		}
	}

	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if m.showErrorNotif && msg.String() == config.Keybinds.Global.DismissNotification && !m.composerHasVimFocus() {
			m.showErrorNotif = false
			return m, nil
		}
		if m.showPluginNotif && m.pluginNotification.Done() {
			m.showPluginNotif = false
		}
		if m.showPluginNotif && msg.String() == config.Keybinds.Global.DismissNotification && !m.composerHasVimFocus() {
			m.showPluginNotif = false
			return m, nil
		}
		if msg.String() == config.Keybinds.Composer.UndoSend && !m.composerHasVimFocus() {
			if m.actionManager.IsPending() {
				m.actionManager.RestorePendingAction()
				return m, nil
			}
			if m.pendingJobID != "" {
				jobID := m.pendingJobID
				m.pendingJobID = ""
				return m, func() tea.Msg {
					if err := m.service.CancelEmail(jobID); err != nil {
						return tui.EmailResultMsg{Err: fmt.Errorf("could not undo: email may have already been sent")}
					}
					return tui.UndoSendMsg{JobID: jobID}
				}
			}
		}
		if msg.String() == "ctrl+c" {
			if composer := getCurrentComposer(m.current); composer != nil && composer.HasContent() {
				m.saveDraftOnQuit(composer)
			}
			m.shutdownService()
			return m, tea.Quit
		}
		if msg.String() == "esc" {
			switch m.current.(type) {
			case *tui.FilePicker:
				return m, func() tea.Msg { return tui.CancelFilePickerMsg{} }
			case *tui.FolderInbox, *tui.Inbox, *tui.Login:
				if searchWasActive || filterWasActive || splitWasOpen {
					return m, tea.Batch(cmds...)
				}
				m.shutdownService()
				return m, m.setChoiceMenu()
			}
		}

	case tui.BackToInboxMsg:
		if _, ok := m.current.(*tui.ReplySplitView); ok {
			tui.ClearKittyGraphics()
		}
		if m.folderInbox != nil {
			m.current = m.folderInbox
		} else {
			return m, m.setChoiceMenu()
		}
		return m, nil

	case tui.BackToMailboxMsg:
		tui.ClearKittyGraphics()
		if m.folderInbox != nil {
			m.current = m.folderInbox
			return m, nil
		}
		return m, m.setChoiceMenu()

	case tui.DiscardDraftMsg:
		if _, ok := m.current.(*tui.ReplySplitView); ok {
			tui.ClearKittyGraphics()
		}
		if msg.ComposerState != nil {
			draft := msg.ComposerState.ToDraft()
			if err := config.SaveDraft(draft); err != nil {
				log.Printf("Error saving draft: %v", err)
			}
		}
		return m, m.setChoiceMenu()

	case tui.SignInWithBrowserMsg:
		if acc := m.config.GetAccountByID(msg.AccountID); acc != nil {
			return m, m.runOAuth2Flow(*acc, true)
		}
		return m, nil

	case tui.OAuth2CompleteMsg:
		if msg.Err != nil {
			log.Printf("OAuth2 sign-in for %s failed: %v", msg.Email, msg.Err)
		} else if acc := m.config.GetAccountByID(msg.AccountID); acc != nil && acc.OAuthTokenCmd != "" {
			// The account now has Matcha's own tokens; stop asking the external command.
			acc.OAuthTokenCmd = ""
			if err := config.SaveConfig(m.config); err != nil {
				log.Printf("could not save config: %v", err)
			} else {
				m.afterConfigSaved()
			}
		}
		if msg.ReturnToSettings {
			if _, ok := m.current.(*tui.Settings); !ok {
				m.current = m.newSettings()
				m.updateCurrentWindowSize()
			}
			return m, nil
		}
		return m, m.setChoiceMenu()

	case tui.Credentials:
		m.updateConfigFromCredentials(msg)
		if !m.saveConfigAfterCredentials() {
			return m, tea.Quit
		}
		lastAccount := m.config.Accounts[len(m.config.Accounts)-1]
		isEdit := false
		if login, ok := m.current.(*tui.Login); ok {
			isEdit = login.IsEditMode()
			if acc := m.config.GetAccountByID(login.GetAccountID()); acc != nil {
				lastAccount = *acc
			}
		}
		// Editing server settings keeps the existing sign-in; use "o" in the
		// account settings to sign in again.
		if lastAccount.IsOAuth2() && !isEdit {
			return m, m.runOAuth2Flow(lastAccount, false)
		}
		if isEdit {
			m.current = m.newSettings()
		} else {
			m.current = tui.NewChoice()
		}
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.GoToInboxMsg:
		if m.config == nil || !m.config.HasAccounts() {
			hideTips := false
			if m.config != nil {
				hideTips = m.config.HideTips
			}
			m.current = tui.NewLogin(hideTips)
			return m, m.current.Init()
		}
		m.ensureProviders()
		seen := make(map[string]bool)
		var cachedFolders []string
		unread := make(map[string]int)
		foldersByAccount := make(map[string][]string)
		unreadByAccount := make(map[string]map[string]int)
		for _, acc := range m.config.Accounts {
			folders, counters := config.GetCachedFolders(acc.ID)
			foldersByAccount[acc.ID] = folders
			unreadByAccount[acc.ID] = counters
			for _, f := range folders {
				if !seen[f] {
					seen[f] = true
					cachedFolders = append(cachedFolders, f)
				}
				if count, ok := counters[f]; ok {
					unread[f] += count
				}
			}
		}
		if !seen[emailstore.FolderInbox] {
			cachedFolders = append([]string{emailstore.FolderInbox}, cachedFolders...)
		}
		m.folderInbox = tui.NewFolderInbox(cachedFolders, m.config.Accounts)
		m.folderInbox.SetFolderData(cachedFolders, unread, foldersByAccount, unreadByAccount)
		m.folderInbox.GetInbox().SetLocalSearcher(m.headerIndex)
		m.folderInbox.GetInbox().SetHiddenSearcher(m.headerIndex)
		m.folderInbox.GetInbox().SetSearchHistory(config.SearchHistoryFile{})
		m.setFolderInboxDefaults()
		m.loadFolderInboxCache(emailstore.FolderInbox)
		m.current = m.folderInbox
		m.updateCurrentWindowSize()
		if m.service == nil {
			m.service = daemonclient.NewService(m.config)
			m.updateSubordinates()
		}
		batchCmds := []tea.Cmd{
			m.current.Init(),
			fetchcmd.FetchFoldersCmd(m.config),
			m.fetchFolderEmailsCmd(emailstore.FolderInbox),
			idledaemon.ListenForIdleUpdates(m.idleUpdates),
		}
		if msg.Hidden {
			batchCmds = append(batchCmds, m.folderInbox.GetInbox().ShowHiddenMail())
		} else {
			// Copy INBOX headers in the background so the first search matches while typing.
			batchCmds = append(batchCmds, m.updateHeaderIndexCmd(emailstore.FolderInbox, ""))
		}
		if m.service.IsDaemon() {
			for _, acct := range m.config.Accounts {
				_ = m.service.Subscribe(acct.ID, emailstore.FolderInbox)
			}
			batchCmds = append(batchCmds, idledaemon.ListenForDaemonEvents(m.service.Events()))
		} else {
			for i := range m.config.Accounts {
				m.idleWatcher.Watch(&m.config.Accounts[i], emailstore.FolderInbox)
			}
		}
		return m, tea.Batch(batchCmds...)

	case tui.FoldersFetchedMsg:
		if m.folderInbox == nil {
			return m, nil
		}
		seen := make(map[string]bool)
		var folderNames []string
		unread := make(map[string]int)
		foldersByAccount := make(map[string][]string)
		unreadByAccount := make(map[string]map[string]int)
		for accID, folders := range msg.FoldersByAccount {
			var names []string
			counts := make(map[string]int)
			for _, f := range folders {
				names = append(names, f.Name)
				if !seen[f.Name] {
					seen[f.Name] = true
					folderNames = append(folderNames, f.Name)
				}
				if f.Unread > 0 {
					counts[f.Name] = int(f.Unread)
					unread[f.Name] += int(f.Unread)
				}
			}
			foldersByAccount[accID] = names
			unreadByAccount[accID] = counts
			_ = config.SaveAccountFolders(accID, names, counts)
		}
		for _, acc := range m.config.Accounts {
			if _, ok := foldersByAccount[acc.ID]; ok {
				continue
			}
			folders, counts := config.GetCachedFolders(acc.ID)
			foldersByAccount[acc.ID] = folders
			unreadByAccount[acc.ID] = counts
			for _, folder := range folders {
				if !seen[folder] {
					seen[folder] = true
					folderNames = append(folderNames, folder)
				}
				unread[folder] += counts[folder]
			}
		}
		if !seen[emailstore.FolderInbox] {
			folderNames = append(folderNames, emailstore.FolderInbox)
		}
		folderCmd := m.folderInbox.SetFolderData(folderNames, unread, foldersByAccount, unreadByAccount)
		if len(msg.Errors) > 0 {
			lookup := map[string]string{}
			for _, acc := range m.config.Accounts {
				name := acc.Email
				if name == "" {
					name = acc.Name
				}
				if name == "" {
					name = acc.ID
				}
				lookup[acc.ID] = name
			}
			parts := make([]string, 0, len(msg.Errors))
			for accID, err := range msg.Errors {
				name := lookup[accID]
				if name == "" {
					name = accID
				}
				parts = append(parts, fmt.Sprintf("%s: %v", name, err))
			}
			sort.Strings(parts)
			return m, tea.Batch(folderCmd, m.showErrorCmd(fmt.Sprintf(
				"Folder fetch failed for %d account(s): %s",
				len(parts), strings.Join(parts, "; "),
			)))
		}
		return m, folderCmd

	case tui.SwitchFolderMsg:
		if m.config == nil {
			return m, nil
		}
		m.updateDaemonSubscriptions(msg.PreviousFolder, msg.FolderName)
		if m.plugins != nil {
			m.plugins.CallFolderHook(plugin.HookFolderChanged, msg.FolderName)
			m.syncPluginStatus()
			m.syncPluginKeyBindings()
		}
		return m, m.fetchCurrentFolderEmails(msg.FolderName)

	case tui.PluginNotifyMsg:
		// Build a non-blocking overlay notification instead of swapping the
		// full-screen view. The notification is composited on top of the
		// current view via PlaceOn() in View().
		dur := time.Duration(msg.Duration * float64(time.Second))
		if dur <= 0 {
			dur = 2 * time.Second
		}
		col := max(0, m.width-44)
		opts := []overlay.Option{
			overlay.WithMessage(msg.Message),
			overlay.WithPosition(0, col),
			overlay.WithDuration(dur),
		}
		if msg.Title != "" {
			opts = append(opts, overlay.WithTitle(msg.Title))
		}
		if msg.Closable {
			opts = append(opts,
				overlay.WithKey(config.Keybinds.Global.DismissNotification),
				overlay.WithDismissMode(overlay.DismissEither),
			)
		} else {
			opts = append(opts, overlay.WithDismissMode(overlay.DismissAfterTimer))
		}
		switch msg.Kind {
		case "warning":
			m.pluginNotification = overlay.NewWarning(opts...)
		case "error":
			m.pluginNotification = overlay.NewError(opts...)
		default:
			m.pluginNotification = overlay.NewInfo(opts...)
		}
		m.showPluginNotif = true
		return m, tea.Tick(dur, func(time.Time) tea.Msg { return clearPluginNotifMsg{} })

	case tui.PluginPromptSubmitMsg:
		if m.pendingPrompt != nil {
			if composer := getCurrentComposer(m.current); composer != nil {
				composer.HidePluginPrompt()
				m.plugins.ResolvePrompt(m.pendingPrompt, msg.Value)
				m.applyPluginFields(composer)
				m.syncPluginStatus()
			}
			m.pendingPrompt = nil
		}
		return m, nil

	case tui.PluginPromptCancelMsg:
		if composer := getCurrentComposer(m.current); composer != nil {
			composer.HidePluginPrompt()
		}
		m.pendingPrompt = nil
		return m, nil

	case tui.FolderEmailsFetchedMsg:
		if m.folderInbox == nil {
			return m, nil
		}
		if m.plugins != nil {
			for _, email := range msg.Emails {
				t := m.plugins.EmailToTable(email.UID, email.From, email.To, email.Subject, email.Date, email.IsRead, email.AccountID, msg.FolderName)
				m.plugins.CallHook(plugin.HookEmailReceived, t)
			}
		}
		emails := m.keepEmailsOfFailedAccounts(msg.FolderName, msg.Emails, msg.FailedAccounts)
		m.store.FolderEmails[msg.FolderName] = emails
		m.cacheFolderEmailBodies(msg.FolderName, emails)
		if m.folderInbox.GetCurrentFolder() != msg.FolderName {
			return m, nil
		}
		m.setFolderInboxEmails(msg.FolderName, emails)
		// The FolderInbox never sees this message, so end the "r" refresh here.
		m.folderInbox.SetRefreshing(false)
		m.folderInbox.GetInbox().SetFailedAccounts(msg.FolderName, msg.FailedAccounts)
		cmds := append(m.pluginFlagCmds(), m.pluginNotifyCmd(), m.folderInbox.ContinueUnreadFetch())
		if len(msg.FailedAccounts) > 0 {
			cmds = append(cmds, m.showErrorCmd(m.folderLoadFailureMessage(msg.FolderName, msg.FailedAccounts)))
		}
		return m, tea.Batch(cmds...)

	case tui.FetchFolderMoreEmailsMsg:
		if msg.AccountID == "" || m.config == nil || m.folderInbox == nil || m.folderInbox.GetCurrentFolder() != msg.FolderName {
			return m, nil
		}
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			return m, nil
		}
		limit := m.folderInbox.PaginationPageSize()
		if msg.Limit > 0 {
			limit = msg.Limit
		}
		_, _ = m.folderInbox.Update(tui.FetchingMoreEmailsMsg{FolderName: msg.FolderName})
		return m, fetchcmd.FetchFolderEmailsPaginatedCmd(account, msg.FolderName, limit, msg.Offset)

	case tui.FolderEmailsAppendedMsg:
		if m.folderInbox == nil || m.folderInbox.GetCurrentFolder() != msg.FolderName {
			return m, nil
		}
		_, fetchCmd := m.folderInbox.Update(msg)
		m.store.AppendToFolder(msg.FolderName, msg.Emails)
		m.cacheFolderEmailBodies(msg.FolderName, m.store.FolderEmails[msg.FolderName])
		m.syncUnreadBadge()
		return m, fetchCmd

	case tui.MoveEmailToFolderMsg:
		if m.config == nil {
			return m, nil
		}
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			return m, nil
		}
		folderName := m.folderName()
		if m.folderInbox != nil {
			m.folderInbox.GetInbox().RemoveEmail(msg.UID, msg.AccountID)
		}
		flushCmd := m.batchActionCmd()
		pa, notice := m.actionManager.HandleMoveEmailMsg(msg, folderName)
		return m, tea.Batch(flushCmd, m.actionManager.StartActionGracePeriod(pa, notice))

	case tui.UpdatePreviewMsg:
		if m.folderInbox == nil {
			return m, nil
		}
		folderName := m.folderName()
		if cached := config.GetCachedEmailBody(folderName, msg.UID, msg.AccountID, m.config.GetBodyCacheThreshold()); cached != nil {
			attachments := m.cachedAttachmentsToFetcher(cached.Attachments)
			return m, func() tea.Msg {
				return tui.PreviewBodyFetchedMsg{
					UID:          msg.UID,
					Body:         cached.Body,
					BodyMIMEType: cached.BodyMIMEType,
					Attachments:  attachments,
					AccountID:    msg.AccountID,
				}
			}
		}
		return m, fetchcmd.FetchPreviewBodyCmd(m.config, msg.UID, msg.AccountID, folderName)

	case tui.PreviewBodyFetchedMsg:
		if msg.Err != nil {
			log.Printf("preview body fetch error for UID %d: %v", msg.UID, msg.Err)
			m.pendingBodyFetchUID = 0
			m.pendingBodyFetchCount = 0
			return m, m.showErrorCmd(msg.Err.Error())
		}
		if m.folderInbox == nil {
			m.pendingBodyFetchUID = 0
			m.pendingBodyFetchCount = 0
			return m, nil
		}
		// If body is empty or whitespace-only, retry the fetch (up to max retries)
		if strings.TrimSpace(msg.Body) == "" {
			if m.pendingBodyFetchUID == msg.UID && m.pendingBodyFetchCount >= maxBodyFetchRetries {
				log.Printf("preview body still empty after %d retries for UID %d, showing error", maxBodyFetchRetries, msg.UID)
				m.pendingBodyFetchUID = 0
				m.pendingBodyFetchCount = 0
				return m, m.showErrorCmd("Email body could not be loaded after multiple attempts")
			}
			m.pendingBodyFetchUID = msg.UID
			m.pendingBodyFetchCount++
			log.Printf("preview body empty for UID %d, retrying fetch (%d/%d)", msg.UID, m.pendingBodyFetchCount, maxBodyFetchRetries)
			return m, tea.Batch(
				m.current.Init(),
				fetchcmd.FetchPreviewBodyCmd(m.config, msg.UID, msg.AccountID, m.folderName()),
			)
		}
		// Reset retry counter on successful fetch
		m.pendingBodyFetchUID = 0
		m.pendingBodyFetchCount = 0
		// Cache the valid body
		m.cacheEmailBody(m.folderName(), msg.UID, msg.AccountID, msg.Body, msg.BodyMIMEType, msg.Attachments)
		m.current, cmd = m.current.Update(msg)
		return m, cmd

	case tui.EmailMovedMsg:
		if msg.Err != nil {
			log.Printf("Move failed: %v", msg.Err)
			return m, m.showErrorCmd(msg.Err.Error())
		}
		return m, nil

	case tui.CachedEmailsLoadedMsg:
		if m.folderInbox == nil {
			return m, nil
		}
		return m, m.fetchFolderEmailsCmd(m.folderInbox.GetCurrentFolder())

	case tui.IdleNewMailMsg:
		m.notifyNewMail(msg.AccountID, msg.FolderName)
		if m.folderInbox != nil && m.folderInbox.GetCurrentFolder() == msg.FolderName {
			return m, tea.Batch(
				m.fetchFolderEmailsCmd(msg.FolderName),
				idledaemon.ListenForIdleUpdates(m.idleUpdates),
			)
		}
		return m, idledaemon.ListenForIdleUpdates(m.idleUpdates)

	case tui.DaemonEventMsg:
		if msg.Event == nil {
			// Events channel closed — daemon may have reconnected with a
			// fresh channel. Re-subscribe if still in daemon mode.
			if m.service != nil && m.service.IsDaemon() {
				return m, idledaemon.ListenForDaemonEvents(m.service.Events())
			}
			return m, nil
		}
		var daemonCmds []tea.Cmd
		if m.service != nil && m.service.IsDaemon() {
			daemonCmds = append(daemonCmds, idledaemon.ListenForDaemonEvents(m.service.Events()))
		}
		switch msg.Event.Type {
		case daemonrpc.EventNewMail:
			var ev daemonrpc.NewMailEvent
			if err := json.Unmarshal(msg.Event.Data, &ev); err == nil {
				m.notifyNewMail(ev.AccountID, ev.Folder)
				if m.folderInbox != nil && m.folderInbox.GetCurrentFolder() == ev.Folder {
					daemonCmds = append(daemonCmds, m.fetchFolderEmailsCmd(ev.Folder))
				}
			}
		case daemonrpc.EventSyncComplete:
			var ev daemonrpc.SyncCompleteEvent
			if err := json.Unmarshal(msg.Event.Data, &ev); err == nil {
				if m.folderInbox != nil && m.folderInbox.GetCurrentFolder() == ev.Folder {
					daemonCmds = append(daemonCmds, m.fetchFolderEmailsCmd(ev.Folder))
				}
			}
		}
		return m, tea.Batch(daemonCmds...)

	case tui.RequestRefreshMsg:
		if msg.FolderName != "" && m.config != nil {
			return m, m.refreshFolderEmails(msg.FolderName)
		}
		return m, tea.Batch(
			func() tea.Msg { return tui.RefreshingEmailsMsg{Mailbox: msg.Mailbox} },
			fetchcmd.RefreshEmails(m.config, msg.Mailbox, msg.Counts),
		)

	case tui.EmailsRefreshedMsg:
		m.store.MergeRefreshed(msg.EmailsByAccount)
		m.syncUnreadBadge()
		if m.folderInbox != nil {
			m.folderInbox.SetEmails(m.store.Emails, m.config.Accounts)
			m.folderInbox.GetInbox().Update(msg)
		}
		return m, nil

	case tui.AllEmailsFetchedMsg:
		m.store.EmailsByAcct = msg.EmailsByAccount
		m.store.Emails = emailstore.FlattenAndSort(msg.EmailsByAccount)
		m.syncUnreadBadge()
		if m.folderInbox != nil {
			m.folderInbox.SetEmails(m.store.Emails, m.config.Accounts)
			m.folderInbox.SetLoadingEmails(false)
		}
		return m, nil

	case tui.EmailsFetchedMsg:
		if m.store.EmailsByAcct == nil {
			m.store.EmailsByAcct = make(map[string][]fetcher.Email)
		}
		m.store.EmailsByAcct[msg.AccountID] = msg.Emails
		m.store.Emails = emailstore.FlattenAndSort(m.store.EmailsByAcct)
		m.syncUnreadBadge()
		if m.folderInbox != nil {
			m.folderInbox.SetEmails(m.store.Emails, m.config.Accounts)
		}
		return m, nil

	case tui.FetchMoreEmailsMsg:
		if msg.AccountID == "" {
			return m, nil
		}
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			return m, nil
		}
		limit := m.config.GetPaginationPageSize().LimitForHeight(m.height)
		if m.folderInbox != nil {
			limit = m.folderInbox.PaginationPageSize()
		}
		if msg.Limit > 0 {
			limit = msg.Limit
		}
		folderName := m.folderName()
		m.current, _ = m.current.Update(tui.FetchingMoreEmailsMsg{FolderName: folderName})
		return m, fetchcmd.FetchFolderEmailsPaginatedCmd(account, folderName, limit, msg.Offset)

	case tui.SearchOpenedMsg:
		return m, m.updateHeaderIndexCmd(msg.FolderName, msg.AccountID)

	case headerIndexLoadedMsg:
		return m, m.handleHeaderIndexLoaded(msg)

	case headerSyncStepMsg:
		return m, m.handleHeaderSyncStep(msg)
	case addressDiscoveryMsg:
		return m, m.handleAddressDiscoveryResult(msg)

	case tui.SearchRequestedMsg:
		folderName := msg.FolderName
		if folderName == "" {
			folderName = emailstore.FolderInbox
		}
		return m, fetchcmd.SearchEmailsCmd(m.config, m.resolveProvider, msg.Query, folderName, msg.AccountID)

	case tui.EmailsAppendedMsg:
		if m.store.EmailsByAcct == nil {
			m.store.EmailsByAcct = make(map[string][]fetcher.Email)
		}
		unique := filterUnique(m.store.EmailsByAcct[msg.AccountID], msg.Emails)
		m.store.EmailsByAcct[msg.AccountID] = append(m.store.EmailsByAcct[msg.AccountID], unique...)
		m.store.Emails = append(m.store.Emails, unique...)
		m.syncUnreadBadge()
		return m, nil

	case tui.GoToSendMsg:
		hideTips := false
		if m.config != nil {
			hideTips = m.config.HideTips
		}
		var composer *tui.Composer
		if m.config != nil && len(m.config.Accounts) > 0 {
			firstAccount := m.config.GetFirstAccount()
			composer = tui.NewComposerWithAccounts(m.config.Accounts, firstAccount.ID, msg.To, msg.Subject, msg.Body, hideTips)
		} else {
			composer = tui.NewComposer("", msg.To, msg.Subject, msg.Body, hideTips)
		}
		if m.config != nil {
			composer.SetShowCcBcc(m.config.ShowCcBccByDefault)
		}
		m.applyComposerOptions(composer)
		m.current = composer
		m.updateCurrentWindowSize()
		m.syncPluginKeyBindings()
		return m, m.current.Init()

	case tui.GoToDraftsMsg:
		drafts := config.GetAllDrafts()
		m.current = tui.NewDrafts(drafts)
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.OpenDraftMsg:
		var accounts []config.Account
		hideTips := false
		if m.config != nil {
			accounts = m.config.Accounts
			hideTips = m.config.HideTips
		}
		composer := tui.NewComposerFromDraft(msg.Draft, accounts, hideTips)
		if m.config != nil {
			composer.SetShowCcBcc(m.config.ShowCcBccByDefault)
		}
		m.applyComposerOptions(composer)
		m.current = composer
		m.updateCurrentWindowSize()
		m.syncPluginKeyBindings()
		return m, m.current.Init()

	case tui.DeleteSavedDraftMsg:
		go func() {
			if err := config.DeleteDraft(msg.DraftID); err != nil {
				log.Printf("Error deleting draft: %v", err)
			}
		}()
		m.current, cmd = m.current.Update(tui.DraftDeletedMsg{DraftID: msg.DraftID})
		return m, cmd

	case tui.GoToMarketplaceMsg:
		m.current = tui.NewMarketplace(false)
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.ConfigSavedMsg:
		m.afterConfigSaved()
		return m, m.refreshAddressSettingsCmd()

	case tui.ToggleSplitOrientationMsg:
		if m.config.EnableSplitPane && m.folderInbox != nil && m.folderInbox.HasSplitPreview() {
			if m.config.GetSplitPaneOrientation() == config.SplitPaneVertical {
				m.config.SplitPaneOrientation = config.SplitPaneHorizontal
			} else {
				m.config.SplitPaneOrientation = config.SplitPaneVertical
			}
			tui.ClearKittyGraphics()
			m.folderInbox.SetSplitOrientation(m.config.GetSplitPaneOrientation())
			m.current, cmd = m.current.Update(m.currentWindowSize())
			return m, cmd
		}
		return m, nil

	case tui.OpenFullscreenFromSplitMsg:
		if m.folderInbox != nil && m.folderInbox.HasSplitPreview() {
			previewPane := m.folderInbox.GetPreviewPane()
			if previewPane == nil {
				return m, nil
			}
			email := previewPane.GetEmail()
			m.folderInbox.CloseSplitPreview()
			mailbox := tui.MailboxInbox
			if inbox := m.folderInbox.GetInbox(); inbox != nil {
				mailbox = inbox.GetMailbox()
			}
			emailView := tui.NewEmailView(email, m.width, m.height, mailbox, m.config.DisableImages)
			m.current = emailView
			m.syncPluginStatus()
			m.syncPluginKeyBindings()
			m.updateCurrentWindowSize()
			return m, tea.Batch(append(m.pluginFlagCmds(), m.current.Init())...)
		}
		return m, nil

	case tui.OpenSplitFromFullscreenMsg:
		if ev, ok := m.current.(*tui.EmailView); ok && m.config.EnableSplitPane && m.folderInbox != nil {
			email := ev.GetEmail()
			m.folderInbox.OpenSplitPreview(email.UID, email.AccountID, &email)
			m.current = m.folderInbox
			m.updateCurrentWindowSize()
			markReadCmd := m.markEmailReadAndQueue(email.AccountID, email.UID, false)
			return m, tea.Batch(append(m.pluginFlagCmds(), markReadCmd, func() tea.Msg {
				return tui.UpdatePreviewMsg{UID: email.UID, AccountID: email.AccountID}
			})...)
		}
		return m, nil

	case tui.LanguageChangedMsg:
		m.afterLanguageChanged()
		return m, m.current.Init()

	case tui.GoToSettingsMsg:
		m.current = m.newSettings()
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.GoToAddAccountMsg:
		hideTips := false
		if m.config != nil {
			hideTips = m.config.HideTips
		}
		m.current = tui.NewLogin(hideTips)
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.GoToAddMailingListMsg:
		m.current = tui.NewMailingListEditor()
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.GoToEditAccountMsg:
		hideTips := false
		if m.config != nil {
			hideTips = m.config.HideTips
		}
		login := tui.NewLogin(hideTips)
		login.SetEditMode(msg)
		m.current = login
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.GoToEditMailingListMsg:
		editor := tui.NewMailingListEditor()
		editor.SetEditMode(msg.Index, msg.Name, msg.Addresses)
		m.current = editor
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.GoToAddContactMsg:
		m.current = tui.NewContactEditor()
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.GoToEditContactMsg:
		editor := tui.NewContactEditor()
		editor.SetEditMode(msg.OriginalEmail, msg.Name, msg.Email)
		m.current = editor
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.SaveContactMsg:
		if msg.IsEdit {
			_ = config.UpdateContact(msg.OriginalEmail, msg.Name, msg.Email)
		} else {
			_ = config.AddContact(msg.Name, msg.Email)
		}
		s := m.newSettings()
		s.RestoreState(tui.SettingsState{
			ActivePane:     tui.PaneContent,
			ActiveCategory: tui.CategoryContacts,
		})
		m.current = s
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.SaveMailingListMsg:
		if m.config != nil {
			var addrs []string
			for _, part := range strings.Split(msg.Addresses, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					addrs = append(addrs, trimmed)
				}
			}
			if msg.EditIndex >= 0 && msg.EditIndex < len(m.config.MailingLists) {
				m.config.MailingLists[msg.EditIndex] = config.MailingList{
					Name:      msg.Name,
					Addresses: addrs,
				}
			} else {
				m.config.MailingLists = append(m.config.MailingLists, config.MailingList{
					Name:      msg.Name,
					Addresses: addrs,
				})
			}
			if err := config.SaveConfig(m.config); err != nil {
				log.Printf("could not save config: %v", err)
			}
		}
		return m, m.setChoiceMenu()

	case tui.GoToSignatureEditorMsg:
		m.current = tui.NewSignatureEditor(msg.AccountID)
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.PasswordVerifiedMsg:
		if msg.Err != nil {
			return m, nil
		}
		config.SetSessionKey(msg.Key)
		cfg, err := config.LoadConfig()
		if err == nil {
			if migrateErr := config.MigrateContactsCacheUsage(cfg.GetAccountIDs()); migrateErr != nil {
				log.Printf("warning: contacts migration failed: %v", migrateErr)
			}
			if cfg.Theme != "" {
				theme.SetTheme(cfg.Theme)
				tui.RebuildStyles()
			}
			lang := i18n.DetectLanguage(cfg)
			loglevel.Verbosef("Detected language: %s", lang)
			if err := i18n.GetManager().SetLanguage(lang); err != nil {
				log.Printf("Failed to set language %s: %v", lang, err)
			} else {
				loglevel.Verbosef("Language set to: %s", i18n.GetManager().GetLanguage())
				loglevel.Verbosef("Test translation: %s", i18n.GetManager().T("composer.title"))
			}
		}
		_ = config.EnsurePGPDir()
		if err != nil {
			m.config = nil
			m.current = tui.NewLogin(false)
		} else {
			m.config = cfg
			if m.mailtoURL != nil {
				to, subject, body := parseMailto(m.mailtoURL)
				composer := tui.NewComposerWithAccounts(cfg.Accounts, cfg.Accounts[0].ID, to, subject, body, cfg.HideTips)
				m.applyComposerOptions(composer)
				m.current = composer
			} else {
				m.current = tui.NewChoice()
			}
			m.updateSubordinates()
		}
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.SecureModeEnabledMsg:
		if msg.Err != nil {
			log.Printf("Failed to enable encryption: %v", msg.Err)
		}
		return m, nil

	case tui.SecureModeDisabledMsg:
		if msg.Err != nil {
			log.Printf("Failed to disable encryption: %v", msg.Err)
		}
		return m, nil

	case tui.GoToChoiceMenuMsg:
		return m, m.setChoiceMenu()

	case tui.MouseSupportChosenMsg:
		if m.config != nil {
			enabled := msg.Enabled
			m.config.MouseEnabled = &enabled
			config.MouseEnabled = &enabled
			if err := config.SaveConfig(m.config); err != nil {
				log.Printf("could not save mouse-enabled flag: %v", err)
			}
		}
		return m, nil

	case tui.SetupGuideDoneMsg:
		if m.config != nil && !m.config.HasSeenSetupGuide {
			m.config.HasSeenSetupGuide = true
			if err := config.SaveConfig(m.config); err != nil {
				log.Printf("could not save setup-guide flag: %v", err)
			}
		}
		return m, m.setChoiceMenu()

	case tui.DeleteAccountMsg:
		m.deleteAccount(msg.AccountID)
		return m, m.current.Init()

	case tui.ViewEmailMsg:
		email := msg.Email
		if email == nil {
			email = m.store.GetEmailByUIDAndAccount(msg.UID, msg.AccountID)
		} else {
			m.store.AddEmailToStoresIfMissing(*email, msg.Mailbox)
		}
		if email == nil {
			return m, nil
		}
		folderName := m.folderName()
		suppressRead := false
		if m.plugins != nil {
			t := m.plugins.EmailToTable(email.UID, email.From, email.To, email.Subject, email.Date, email.IsRead, email.AccountID, folderName)
			m.plugins.CallHook(plugin.HookEmailViewed, t)
			suppressRead = m.plugins.TakeAutoReadSuppressed()
		}
		if m.config.EnableSplitPane && m.folderInbox != nil {
			m.folderInbox.OpenSplitPreview(msg.UID, msg.AccountID, email)
			m.current = m.folderInbox
			cmd = m.markEmailReadAndQueue(msg.AccountID, msg.UID, suppressRead)
			return m, tea.Batch(append(m.pluginFlagCmds(), cmd, func() tea.Msg {
				return tui.UpdatePreviewMsg{UID: msg.UID, AccountID: msg.AccountID}
			})...)
		}
		if cached := config.GetCachedEmailBody(folderName, msg.UID, msg.AccountID, m.config.GetBodyCacheThreshold()); cached != nil {
			attachments := m.cachedAttachmentsToFetcher(cached.Attachments)
			return m, func() tea.Msg {
				return tui.EmailBodyFetchedMsg{
					UID:          msg.UID,
					Body:         cached.Body,
					BodyMIMEType: cached.BodyMIMEType,
					Attachments:  attachments,
					AccountID:    msg.AccountID,
					Mailbox:      msg.Mailbox,
				}
			}
		}
		m.current = tui.NewStatus("Fetching email content...")
		return m, tea.Batch(append(m.pluginFlagCmds(), m.current.Init(), fetchcmd.FetchFolderEmailBodyCmd(m.config, msg.UID, msg.AccountID, folderName, msg.Mailbox), m.pluginNotifyCmd())...)

	case tui.FolderEmailsPageFailedMsg:
		if m.folderInbox != nil && m.folderInbox.GetCurrentFolder() != msg.FolderName {
			return m, nil
		}
		if m.folderInbox != nil && m.current != m.folderInbox {
			_, _ = m.folderInbox.Update(msg)
		}
		log.Printf("paginated fetch error for %s: %v", msg.FolderName, msg.Err)
		return m, m.showErrorCmd(msg.Err.Error())

	case tui.EmailBodyFetchedMsg:
		if msg.Err != nil {
			log.Printf("could not fetch email body: %v", msg.Err)
			m.pendingBodyFetchUID = 0
			m.pendingBodyFetchCount = 0
			if m.folderInbox != nil {
				m.current = m.folderInbox
			}
			return m, m.showErrorCmd(msg.Err.Error())
		}
		// If body is empty or whitespace-only, retry the fetch instead of showing incomplete content
		if strings.TrimSpace(msg.Body) == "" {
			if m.pendingBodyFetchUID == msg.UID && m.pendingBodyFetchCount >= maxBodyFetchRetries {
				log.Printf("email body still empty after %d retries for UID %d, showing error", maxBodyFetchRetries, msg.UID)
				m.pendingBodyFetchUID = 0
				m.pendingBodyFetchCount = 0
				if m.folderInbox != nil {
					m.current = m.folderInbox
				}
				return m, m.showErrorCmd("Email body could not be loaded after multiple attempts")
			}
			m.pendingBodyFetchUID = msg.UID
			m.pendingBodyFetchCount++
			log.Printf("email body empty for UID %d, retrying fetch (%d/%d)", msg.UID, m.pendingBodyFetchCount, maxBodyFetchRetries)
			m.current = tui.NewStatus("Fetching email content...")
			return m, tea.Batch(
				append(m.pluginFlagCmds(), m.current.Init(), fetchcmd.FetchFolderEmailBodyCmd(m.config, msg.UID, msg.AccountID, m.folderName(), msg.Mailbox), m.pluginNotifyCmd())...,
			)
		}
		// Reset retry counter on successful fetch
		m.pendingBodyFetchUID = 0
		m.pendingBodyFetchCount = 0
		m.store.UpdateEmailBodyByUID(msg.UID, msg.AccountID, msg.Body, msg.BodyMIMEType, msg.Attachments)
		folderForCache := m.folderName()
		m.cacheEmailBody(folderForCache, msg.UID, msg.AccountID, msg.Body, msg.BodyMIMEType, msg.Attachments)
		email := m.store.GetEmailByUIDAndAccount(msg.UID, msg.AccountID)
		if email == nil {
			if m.folderInbox != nil {
				m.current = m.folderInbox
			}
			return m, nil
		}
		var markReadCmd tea.Cmd
		pluginSuppressed := m.plugins != nil && m.plugins.TakeAutoReadSuppressed()
		if !email.IsRead && !pluginSuppressed {
			markReadCmd = m.markEmailReadAndQueue(msg.AccountID, msg.UID, false)
		}
		emailView := tui.NewEmailView(*email, m.width, m.height, msg.Mailbox, m.config.DisableImages)
		m.current = emailView
		m.syncPluginStatus()
		m.syncPluginKeyBindings()
		cmds := []tea.Cmd{m.current.Init()}
		if markReadCmd != nil {
			cmds = append(cmds, markReadCmd)
		}
		cmds = append(cmds, m.pluginFlagCmds()...)
		if yubiCmd := m.maybeYubiKeyNotification(msg.AccountID, msg.Attachments); yubiCmd != nil {
			cmds = append(cmds, yubiCmd)
		}
		return m, tea.Batch(cmds...)

	case tui.ReplyToEmailMsg:
		var to string
		if len(msg.Email.ReplyTo) > 0 {
			to = strings.Join(msg.Email.ReplyTo, ", ")
		} else {
			to = msg.Email.From
		}
		subject := msg.Email.Subject
		normalizedSubject := strings.ToLower(strings.TrimSpace(subject))
		if !strings.HasPrefix(normalizedSubject, "re:") {
			subject = "Re: " + subject
		}
		quotedText := fmt.Sprintf("\n\nOn %s, %s wrote:\n> %s", msg.Email.Date.Local().Format("Jan 2, 2006 at 3:04 PM"), msg.Email.From, strings.ReplaceAll(msg.Email.Body, "\n", "\n> "))
		var composer *tui.Composer
		hideTips := false
		if m.config != nil {
			hideTips = m.config.HideTips
		}
		if m.config != nil && len(m.config.Accounts) > 0 {
			accountID := msg.Email.AccountID
			if accountID == "" {
				accountID = m.config.GetFirstAccount().ID
			}
			composer = tui.NewComposerWithAccounts(m.config.Accounts, accountID, to, subject, "", hideTips)
			if fromOverride := replyFromOverride(m.config, msg.Email); fromOverride != "" {
				composer.SetFromOverride(fromOverride)
			}
		} else {
			composer = tui.NewComposer("", to, subject, "", hideTips)
		}
		if m.config != nil {
			composer.SetShowCcBcc(m.config.ShowCcBccByDefault)
		}
		composer.SetQuotedText(quotedText)
		inReplyTo := msg.Email.MessageID
		references := append(msg.Email.References, msg.Email.MessageID) //nolint:gocritic
		composer.SetReplyContext(inReplyTo, references)
		m.applyComposerOptions(composer)
		if m.config != nil && m.config.ShowOriginalOnReply {
			sz := m.currentWindowSize()
			replySplit := tui.NewReplySplitView(
				msg.Email,
				composer,
				m.config.GetSplitPaneOrientation(),
				m.config.DisableImages,
				sz.Width,
				sz.Height,
			)
			m.current = replySplit
		} else {
			m.current = composer
		}
		m.updateCurrentWindowSize()
		m.syncPluginKeyBindings()
		return m, m.current.Init()

	case tui.ReplyAllEmailMsg:
		// Determine the original sender (Reply-To if present, else From)
		var sender string
		if len(msg.Email.ReplyTo) > 0 {
			sender = strings.Join(msg.Email.ReplyTo, ", ")
		} else {
			sender = msg.Email.From
		}
		subject := msg.Email.Subject
		normalizedSubject := strings.ToLower(strings.TrimSpace(subject))
		if !strings.HasPrefix(normalizedSubject, "re:") {
			subject = "Re: " + subject
		}
		quotedText := fmt.Sprintf("\n\nOn %s, %s wrote:\n> %s", msg.Email.Date.Local().Format("Jan 2, 2006 at 3:04 PM"), msg.Email.From, strings.ReplaceAll(msg.Email.Body, "\n", "\n> "))

		// Build set of the user's own addresses to exclude from reply-all
		selfAddrs := make(map[string]bool)
		if m.config != nil {
			for _, acc := range m.config.Accounts {
				selfAddrs[strings.ToLower(acc.Email)] = true
				selfAddrs[strings.ToLower(acc.GetFetchEmail())] = true
				selfAddrs[strings.ToLower(acc.GetSendAsEmail())] = true
				for _, extra := range acc.ExtraAddresses {
					selfAddrs[strings.ToLower(strings.TrimSpace(extra))] = true
				}
			}
			// For catch-all accounts, also exclude the delivery address
			accountID := msg.Email.AccountID
			if accountID == "" && len(m.config.Accounts) > 0 {
				accountID = m.config.GetFirstAccount().ID
			}
			for _, acc := range m.config.Accounts {
				if acc.ID == accountID && acc.CatchAll && len(msg.Email.To) > 0 {
					if addr, err := mail.ParseAddress(msg.Email.To[0]); err == nil {
						selfAddrs[strings.ToLower(addr.Address)] = true
					} else {
						selfAddrs[strings.ToLower(strings.TrimSpace(msg.Email.To[0]))] = true
					}
					break
				}
			}
		}

		// Collect Cc recipients from original To list, excluding self and the sender
		seen := make(map[string]bool)
		var ccRecipients []string
		for _, addr := range msg.Email.To {
			parsed, err := mail.ParseAddress(addr)
			var addrLower string
			displayAddr := addr
			if err == nil {
				addrLower = strings.ToLower(parsed.Address)
				displayAddr = parsed.Address
			} else {
				addrLower = strings.ToLower(strings.TrimSpace(addr))
			}
			if selfAddrs[addrLower] || seen[addrLower] {
				continue
			}
			seen[addrLower] = true
			ccRecipients = append(ccRecipients, displayAddr)
		}
		cc := strings.Join(ccRecipients, ", ")

		var composer *tui.Composer
		hideTips := false
		if m.config != nil {
			hideTips = m.config.HideTips
		}
		if m.config != nil && len(m.config.Accounts) > 0 {
			accountID := msg.Email.AccountID
			if accountID == "" {
				accountID = m.config.GetFirstAccount().ID
			}
			composer = tui.NewComposerWithAccounts(m.config.Accounts, accountID, sender, subject, "", hideTips)
			if fromOverride := replyFromOverride(m.config, msg.Email); fromOverride != "" {
				composer.SetFromOverride(fromOverride)
			}
		} else {
			composer = tui.NewComposer("", sender, subject, "", hideTips)
		}
		if m.config != nil {
			composer.SetShowCcBcc(m.config.ShowCcBccByDefault)
		}
		if cc != "" {
			composer.SetCc(cc)
		}
		composer.SetQuotedText(quotedText)

		// Set reply headers
		inReplyTo := msg.Email.MessageID
		references := append(msg.Email.References, msg.Email.MessageID) //nolint:gocritic
		composer.SetReplyContext(inReplyTo, references)

		m.applyComposerOptions(composer)
		if m.config != nil && m.config.ShowOriginalOnReply {
			sz := m.currentWindowSize()
			replySplit := tui.NewReplySplitView(
				msg.Email,
				composer,
				m.config.GetSplitPaneOrientation(),
				m.config.DisableImages,
				sz.Width,
				sz.Height,
			)
			m.current = replySplit
		} else {
			m.current = composer
		}
		m.updateCurrentWindowSize()
		m.syncPluginKeyBindings()
		return m, m.current.Init()

	case tui.ForwardEmailMsg:
		subject := msg.Email.Subject
		if !strings.HasPrefix(strings.ToLower(subject), "fwd:") {
			subject = "Fwd: " + subject
		}
		forwardHeader := fmt.Sprintf("\n\n---------- Forwarded message ----------\nFrom: %s\nDate: %s\nSubject: %s\nTo: %s\n\n",
			msg.Email.From,
			msg.Email.Date.Local().Format("Mon, Jan 2, 2006 at 3:04 PM"),
			msg.Email.Subject,
			msg.Email.To,
		)
		body := forwardHeader + msg.Email.Body
		var composer *tui.Composer
		hideTips := false
		if m.config != nil {
			hideTips = m.config.HideTips
		}
		if m.config != nil && len(m.config.Accounts) > 0 {
			accountID := msg.Email.AccountID
			if accountID == "" {
				accountID = m.config.GetFirstAccount().ID
			}
			composer = tui.NewComposerWithAccounts(m.config.Accounts, accountID, "", subject, body, hideTips)
		} else {
			composer = tui.NewComposer("", "", subject, body, hideTips)
		}
		if m.config != nil {
			composer.SetShowCcBcc(m.config.ShowCcBccByDefault)
		}
		m.applyComposerOptions(composer)
		m.current = composer
		m.updateCurrentWindowSize()
		m.syncPluginKeyBindings()
		return m, m.current.Init()

	case tui.OpenEditorMsg:
		composer := getCurrentComposer(m.current)
		if composer == nil {
			return m, nil
		}
		return m, editor.OpenExternalEditor(m.config.ExternalEditorCommand(), composer.GetBody())

	case tui.EditorFinishedMsg:
		if msg.Err != nil {
			log.Printf("Editor error: %v", msg.Err)
			return m, nil
		}
		if composer := getCurrentComposer(m.current); composer != nil {
			composer.SetBody(msg.Body)
		}
		return m, nil

	case tui.GoToFilePickerMsg:
		if runtime.GOOS == "darwin" {
			return m, func() tea.Msg {
				wd, _ := os.Getwd()
				paths, err := macos.OpenFilePicker(wd)
				if err != nil || len(paths) == 0 {
					return tui.CancelFilePickerMsg{}
				}
				return tui.FileSelectedMsg{Paths: paths}
			}
		}
		m.previousModel = m.current
		wd, _ := os.Getwd()
		m.current = tui.NewFilePicker(wd)
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.FileSelectedMsg:
		m.pendingExport = nil
		m.restoreView()
		// Forward the selected paths to the restored composer so its
		// FileSelectedMsg handler can append them to attachmentPaths.
		// Without this, attachments selected in the picker are dropped.
		if m.current != nil {
			newModel, cmd := m.current.Update(msg)
			m.current = newModel
			return m, cmd
		}
		return m, nil

	case tui.CancelFilePickerMsg:
		m.pendingExport = nil
		m.restoreView()
		return m, nil

	case tui.GoToSaveFilePickerMsg:
		if runtime.GOOS == "darwin" {
			return m, func() tea.Msg {
				home, _ := os.UserHomeDir()
				savePath, err := macos.SaveFilePicker(home, msg.SuggestedName)
				if err != nil || savePath == "" {
					return tui.CancelFilePickerMsg{}
				}
				return tui.ExportEmailMsg{
					Email:    msg.Email,
					Account:  msg.Account,
					Folder:   msg.Folder,
					Mailbox:  msg.Mailbox,
					Format:   msg.Format,
					SavePath: savePath,
				}
			}
		}
		m.previousModel = m.current
		home, _ := os.UserHomeDir()
		m.current = tui.NewSaveFilePicker(home, msg.SuggestedName)
		m.updateCurrentWindowSize()
		m.pendingExport = &emailstore.PendingExport{
			Email:   msg.Email,
			Account: msg.Account,
			Folder:  msg.Folder,
			Mailbox: msg.Mailbox,
			Format:  msg.Format,
		}
		return m, m.current.Init()

	case tui.SaveFileSelectedMsg:
		pe := m.pendingExport
		m.pendingExport = nil
		m.restoreView()
		if pe == nil {
			return m, nil
		}
		return m, exportcmd.ExportEmailCmd(m.config, pe.Email, pe.Account, pe.Folder, pe.Format, msg.Path)

	case tui.ExportEmailMsg:
		return m, exportcmd.ExportEmailCmd(m.config, msg.Email, msg.Account, msg.Folder, msg.Format, msg.SavePath)

	case tui.EmailExportedMsg:
		if msg.Err != nil {
			return m, m.showErrorCmd(fmt.Sprintf("Export failed: %v", msg.Err))
		}
		return m, m.showInfoCmd(fmt.Sprintf("Exported to %s", msg.Path))

	case tui.OpenEmailInBrowserMsg:
		return m, exportcmd.OpenEmailInBrowserCmd(m.config, msg.Email, msg.Account, msg.Folder)

	case tui.EmailOpenedInBrowserMsg:
		if msg.Err != nil {
			return m, m.showErrorCmd(fmt.Sprintf("Failed to open in browser: %v", msg.Err))
		}
		return m, m.showInfoCmd("Opened in browser")

	case tui.OpenCalendarResponseMsg:
		return m, exportcmd.OpenCalendarResponseCmd(msg.URL)

	case tui.CalendarResponseOpenedMsg:
		if msg.Err != nil {
			return m, m.showErrorCmd(fmt.Sprintf("Failed to open calendar response: %v", msg.Err))
		}
		return m, m.showInfoCmd("Opened calendar response in browser; complete it there.")

	case tui.SendEmailMsg:
		if m.plugins != nil {
			m.plugins.CallSendHook(plugin.HookEmailSendBefore, msg.To, msg.Cc, msg.Subject, msg.AccountID)
		}
		m.previousModel = m.current
		var draftID string
		if composer := getCurrentComposer(m.current); composer != nil {
			draftID = composer.GetDraftID()
		}
		if m.service == nil && m.config != nil {
			m.service = daemonclient.NewService(m.config)
			m.updateSubordinates()
		}
		m.sendNotice = m.sendEmailNotice(msg)
		if _, ok := m.current.(*tui.ReplySplitView); ok {
			tui.ClearKittyGraphics()
		}
		m.current = tui.NewChoice()
		m.updateCurrentWindowSize()
		go func() {
			m.saveContactsAfterSend(msg)
			m.deleteDraftAfterSend(draftID)
		}()
		return m, tea.Batch(m.current.Init(), send.SendEmailCmd(m.sendEmailDependencies(), msg))

	case tui.EmailQueuedMsg:
		m.pendingJobID = msg.JobID
		m.sendNotice = fmt.Sprintf("Message sent (%s to undo)", config.Keybinds.Composer.UndoSend)
		return m, tea.Tick(
			time.Duration(msg.DelaySeconds)*time.Second, func(t time.Time) tea.Msg {
				return tui.EmailDelayExpiredMsg{JobID: msg.JobID}
			})

	case tui.EmailDelayExpiredMsg:
		if m.pendingJobID == msg.JobID {
			m.pendingJobID = ""
			m.sendNotice = ""
			m.previousModel = nil
			if m.plugins != nil {
				m.plugins.CallHook(plugin.HookEmailSendAfter)
			}
			return m, m.setChoiceMenu()
		}
		return m, nil

	case tui.UndoSendMsg:
		m.sendNotice = ""
		if m.previousModel != nil {
			m.current = m.previousModel
			m.previousModel = nil
			m.updateCurrentWindowSize()
			return m, m.current.Init()
		}
		return m, m.setChoiceMenu()

	case tui.ActionGracePeriodExpiredMsg:
		return m, m.actionManager.OnGracePeriodExpired(msg)

	case clearErrorNotifMsg:
		m.showErrorNotif = false
		return m, nil

	case clearPluginNotifMsg:
		m.showPluginNotif = false
		return m, nil

	case tui.InfoNotifyMsg:
		dur := time.Duration(msg.Duration * float64(time.Second))
		if dur <= 0 {
			dur = 2 * time.Second
		}
		col := max(0, m.width-44)
		m.errorNotification = overlay.NewInfo(
			overlay.WithMessage(msg.Message),
			overlay.WithKey(config.Keybinds.Global.DismissNotification),
			overlay.WithPosition(0, col),
			overlay.WithDismissMode(overlay.DismissAfterTimer),
			overlay.WithDuration(dur),
		)
		m.showErrorNotif = true
		return m, tea.Tick(dur, func(time.Time) tea.Msg { return clearErrorNotifMsg{} })

	case tui.NotifyMsg:
		return m, m.showErrorCmd(msg.Message)

	case tui.SendRSVPMsg:
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			return m, m.showErrorCmd("account not found")
		}
		m.previousModel = m.current
		m.current = tui.NewStatus("Sending RSVP...")
		return m, tea.Batch(m.current.Init(), send.SendRSVP(account, msg))

	case tui.RSVPResultMsg:
		m.restoreView()
		m.updateCurrentWindowSize()
		if msg.Err != nil {
			log.Printf("Failed to send RSVP: %v", msg.Err)
			return m, m.showErrorCmd("RSVP not sent: " + msg.Err.Error())
		}
		m.current, cmd = m.current.Update(msg)
		if strings.HasSuffix(strings.ToLower(msg.Organizer), "@gmail.com") || strings.HasSuffix(strings.ToLower(msg.Organizer), "@googlemail.com") {
			return m, tea.Batch(cmd, m.showInfoCmd("RSVP sent; Google Calendar may not auto-update."))
		}
		return m, cmd

	case tui.EmailResultMsg:
		m.sendNotice = ""
		if msg.Err != nil {
			log.Printf("Failed to send email: %v", msg.Err)
			m.current = tui.NewChoice()
			m.updateCurrentWindowSize()
			return m, tea.Batch(m.current.Init(), m.showErrorCmd(msg.Err.Error()))
		}
		if msg.Warning != "" {
			log.Printf("Email send warning: %s", msg.Warning)
			m.current = tui.NewChoice()
			m.updateCurrentWindowSize()
			return m, tea.Batch(m.current.Init(), m.showInfoCmd(msg.Warning))
		}
		if m.plugins != nil {
			m.plugins.CallHook(plugin.HookEmailSendAfter)
		}
		return m, m.setChoiceMenu()

	case tui.ApplyPatchMsg:
		// Apply patch to current directory by default; user can change via config.
		repoDir := "."
		if cwd, err := os.Getwd(); err == nil {
			repoDir = cwd
		}
		return m, send.ApplyPatchCmd(repoDir, msg)

	case tui.PatchStagedMsg:
		// Patch has been applied to disk and staged. Run git commit via
		// tea.ExecProcess so the terminal is released for GPG pinentry.
		repoDir := "."
		if cwd, err := os.Getwd(); err == nil {
			repoDir = cwd
		}
		return m, send.CommitPatchCmd(repoDir, msg)

	case tui.PatchApplyResultMsg:
		if msg.Err != nil {
			errMsg := fmt.Sprintf("Patch apply failed: %v", msg.Err)
			return m, tea.Batch(m.showErrorCmd(errMsg))
		}
		verb := "applied"
		if msg.DryRun {
			verb = "validated"
		}
		status := fmt.Sprintf("Patch %s: %s", verb, msg.Subject)
		if len(msg.Files) > 0 {
			status += fmt.Sprintf(" (%d files)", len(msg.Files))
		}
		if len(msg.Warnings) > 0 {
			status += "\n" + strings.Join(msg.Warnings, "\n")
		}
		return m, tea.Batch(m.showInfoCmd(status))

	case tui.GoToSendPatchMsg:
		if m.config == nil || len(m.config.Accounts) == 0 {
			return m, m.showErrorCmd("no accounts configured")
		}
		m.previousModel = m.current
		m.current = tui.NewPatchSend(m.config.Accounts)
		m.updateCurrentWindowSize()
		return m, m.current.Init()

	case tui.SendPatchMsg:
		m.previousModel = m.current
		m.current = tui.NewStatus("Generating and sending patch...")
		return m, tea.Batch(m.current.Init(), send.SendPatchCmd(m.sendEmailDependencies(), msg))

	case tui.PatchGeneratedMsg:
		if msg.Err != nil {
			errMsg := fmt.Sprintf("Patch generation failed: %v", msg.Err)
			if m.previousModel != nil {
				m.current = m.previousModel
				m.previousModel = nil
				m.updateCurrentWindowSize()
			}
			return m, tea.Batch(m.current.Init(), m.showErrorCmd(errMsg))
		}
		// Patch generated — now send the raw patch email via SMTP.
		// We need the original SendPatchMsg (with To/Cc) to inject the
		// recipients into the raw RFC 5322 message. The patch-send form
		// stores it in the PatchGeneratedMsg.
		return m, send.SendRawPatchCmd(m.sendEmailDependencies(), msg.SendPatchMsg, msg.RawPatch)

	case tui.DeleteEmailMsg:
		tui.ClearKittyGraphics()
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			if m.folderInbox != nil {
				m.current = m.folderInbox
			}
			return m, nil
		}
		folderName := m.folderName()
		if m.folderInbox != nil {
			m.current = m.folderInbox
			m.folderInbox.GetInbox().RemoveEmail(msg.UID, msg.AccountID)
		}
		flushCmd := m.batchActionCmd()
		pa, notice := m.actionManager.HandleDeleteEmailMsg(msg, folderName)
		return m, tea.Batch(flushCmd, m.actionManager.StartActionGracePeriod(pa, notice))

	case tui.ArchiveEmailMsg:
		tui.ClearKittyGraphics()
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			if m.folderInbox != nil {
				m.current = m.folderInbox
			}
			return m, nil
		}
		folderName := m.folderName()
		if m.folderInbox != nil {
			m.current = m.folderInbox
			m.folderInbox.GetInbox().RemoveEmail(msg.UID, msg.AccountID)
		}
		flushCmd := m.batchActionCmd()
		pa, notice := m.actionManager.HandleArchiveEmailMsg(msg, folderName)
		return m, tea.Batch(flushCmd, m.actionManager.StartActionGracePeriod(pa, notice))

	case tui.EmailMarkedReadMsg:
		if msg.Err != nil {
			log.Printf("Error marking email as read: %v", msg.Err)
		}
		m.syncUnreadBadge()
		return m, nil

	case tui.EmailMarkedUnreadMsg:
		if msg.Err != nil {
			log.Printf("Error marking email as unread: %v", msg.Err)
		}
		m.syncUnreadBadge()
		return m, nil

	case tui.EmailActionDoneMsg:
		if msg.Err != nil {
			log.Printf("Action failed: %v", msg.Err)
			return m, m.showErrorCmd(msg.Err.Error())
		}
		return m, nil

	case tui.BatchDeleteEmailsMsg:
		tui.ClearKittyGraphics()
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			if m.folderInbox != nil {
				m.current = m.folderInbox
			}
			return m, nil
		}
		folderName := m.folderName()
		if m.folderInbox != nil {
			m.folderInbox.GetInbox().RemoveEmails(msg.UIDs, msg.AccountID)
		}
		flushCmd := m.batchActionCmd()
		pa, notice := m.actionManager.HandleBatchDeleteEmailsMsg(msg, folderName)
		return m, tea.Batch(flushCmd, m.actionManager.StartActionGracePeriod(pa, notice))

	case tui.BatchArchiveEmailsMsg:
		tui.ClearKittyGraphics()
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			if m.folderInbox != nil {
				m.current = m.folderInbox
			}
			return m, nil
		}
		folderName := m.folderName()
		if m.folderInbox != nil {
			m.folderInbox.GetInbox().RemoveEmails(msg.UIDs, msg.AccountID)
		}
		flushCmd := m.batchActionCmd()
		pa, notice := m.actionManager.HandleBatchArchiveEmailsMsg(msg, folderName)
		return m, tea.Batch(flushCmd, m.actionManager.StartActionGracePeriod(pa, notice))

	case tui.BatchMoveEmailsMsg:
		if m.config == nil {
			return m, nil
		}
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			return m, nil
		}
		folderName := m.folderName()
		if m.folderInbox != nil {
			m.folderInbox.GetInbox().RemoveEmails(msg.UIDs, msg.AccountID)
		}
		flushCmd := m.batchActionCmd()
		pa, notice := m.actionManager.HandleBatchMoveEmailsMsg(msg, folderName)
		return m, tea.Batch(flushCmd, m.actionManager.StartActionGracePeriod(pa, notice))

	case tui.BatchEmailActionDoneMsg:
		if msg.Err != nil {
			log.Printf("Batch %s failed: %v", msg.Action, msg.Err)
			return m, m.showErrorCmd(msg.Err.Error())
		}
		return m, nil

	case tui.DownloadAttachmentMsg:
		m.previousModel = m.current
		m.current = tui.NewStatus(fmt.Sprintf("Downloading %s...", msg.Filename))
		account := m.config.GetAccountByID(msg.AccountID)
		if account == nil {
			m.current = m.previousModel
			return m, nil
		}
		// Fetch from the folder the email was read from, as the body fetch does.
		if m.folderInbox != nil {
			msg.Folder = m.folderName()
		}
		return m, tea.Batch(m.current.Init(), exportcmd.DownloadAttachmentCmd(account, msg))

	case tui.AttachmentDownloadedMsg:
		var statusMsg string
		if msg.Err != nil {
			statusMsg = fmt.Sprintf("Error downloading: %v", msg.Err)
		} else {
			statusMsg = fmt.Sprintf("Saved to %s", msg.Path)
		}
		m.current = tui.NewStatus(statusMsg)
		return m, tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tui.RestoreViewMsg{} })

	case tui.RestoreViewMsg:
		m.restoreView()
		return m, nil
	}

	if cmd := m.pluginNotifyCmd(); cmd != nil {
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// View renders the current TUI view with overlays.
func (m *Model) View() tea.View {
	v := m.current.View()
	if m.showLogPanel {
		v.Content = m.renderWithLogPanel(v.Content)
	}
	if m.palette.IsOpen() && m.palette.CommandPalette != nil {
		v.Content = m.palette.Render(v.Content, m.width, m.contentHeight())
	}
	if m.sendNotice != "" {
		v.Content = m.renderSendNoticeOverlay(v.Content)
	}
	if m.actionManager.ActionNotice != "" {
		v.Content = m.renderActionNoticeOverlay(v.Content)
	}
	if m.showErrorNotif {
		v.Content = m.errorNotification.PlaceOn(v.Content)
	}
	if m.showPluginNotif {
		v.Content = m.pluginNotification.PlaceOn(v.Content)
	}
	v.AltScreen = true
	return v
}

func filterUnique(existing, incoming []fetcher.Email) []fetcher.Email {
	seen := make(map[uint32]struct{})
	for _, e := range existing {
		seen[e.UID] = struct{}{}
	}
	var unique []fetcher.Email
	for _, e := range incoming {
		if _, ok := seen[e.UID]; !ok {
			unique = append(unique, e)
		}
	}
	return unique
}
func containsAddress(addresses []string, candidate string) bool {
	for _, address := range addresses {
		if strings.EqualFold(strings.TrimSpace(address), candidate) {
			return true
		}
	}
	return false
}

func removeAddress(addresses []string, candidate string) []string {
	filtered := addresses[:0]
	for _, address := range addresses {
		if !strings.EqualFold(strings.TrimSpace(address), candidate) {
			filtered = append(filtered, address)
		}
	}
	return filtered
}
