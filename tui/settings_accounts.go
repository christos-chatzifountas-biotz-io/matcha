package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/floatpane/matcha/config"
)

func (m *Settings) updateAccounts(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.suggestionsOpen {
		return m.updateSuggestedAddresses(msg)
	}
	if m.isCryptoConfig {
		var m2 *Settings
		var cmd tea.Cmd
		m2, cmd = m.updateSMIMEConfig(msg)
		return m2, cmd
	}

	if m.confirmingDelete {
		switch msg.String() {
		case "y", "Y":
			if m.accountsCursor < len(m.cfg.Accounts) {
				accountID := m.cfg.Accounts[m.accountsCursor].ID
				m.confirmingDelete = false
				return m, func() tea.Msg {
					return DeleteAccountMsg{AccountID: accountID}
				}
			}
		case "n", "N", "esc":
			m.confirmingDelete = false
			return m, nil
		}
		return m, nil
	}

	switch msg.String() {
	case "up", "k":
		itemCount := len(m.cfg.Accounts) + 1
		m.accountsCursor = (m.accountsCursor - 1 + itemCount) % itemCount
	case keyDown, "j":
		itemCount := len(m.cfg.Accounts) + 1
		m.accountsCursor = (m.accountsCursor + 1) % itemCount
	case "a":
		if m.accountsCursor < len(m.cfg.Accounts) {
			m.openAddressSuggestions(m.accountsCursor)
			opened := SearchOpenedMsg{FolderName: "INBOX", AccountID: m.cfg.Accounts[m.accountsCursor].ID}
			return m, func() tea.Msg { return opened }
		}
	case "t":
		if m.accountsCursor < len(m.cfg.Accounts) {
			account := &m.cfg.Accounts[m.accountsCursor]
			previous := account.SortBy
			account.SortBy = "arrival_date"
			if previous == "arrival_date" {
				account.SortBy = "email_date"
			}
			if err := config.SaveConfig(m.cfg); err != nil {
				account.SortBy = previous
				return m, func() tea.Msg { return NotifyMsg{Message: err.Error()} }
			}
			return m, func() tea.Msg { return ConfigSavedMsg{} }
		}
	case "d":
		if m.accountsCursor < len(m.cfg.Accounts) && len(m.cfg.Accounts) > 0 {
			m.confirmingDelete = true
		}
	case "e", keyEnter:
		if m.accountsCursor == len(m.cfg.Accounts) {
			return m, func() tea.Msg { return GoToAddAccountMsg{} }
		}
		if m.accountsCursor < len(m.cfg.Accounts) {
			acc := m.cfg.Accounts[m.accountsCursor]
			return m, func() tea.Msg {
				return GoToEditAccountMsg{
					AccountID:              acc.ID,
					Provider:               acc.ServiceProvider,
					Name:                   acc.Name,
					Email:                  acc.Email,
					FetchEmail:             acc.FetchEmail,
					SendAsEmail:            acc.SendAsEmail,
					CatchAll:               acc.CatchAll,
					ExtraAddresses:         acc.ExtraAddresses,
					AlwaysShowMailFrom:     acc.AlwaysShowMailFrom,
					ShowUnaddressedMail:    acc.ShowUnaddressedMail,
					DiscoverExtraAddresses: acc.DiscoverExtraAddresses,
					AuthMethod:             acc.AuthMethod,
					IMAPServer:             acc.IMAPServer,
					IMAPPort:               acc.IMAPPort,
					SMTPServer:             acc.SMTPServer,
					SMTPPort:               acc.SMTPPort,
					Insecure:               acc.Insecure,
					Protocol:               acc.Protocol,
					JMAPEndpoint:           acc.JMAPEndpoint,
					POP3Server:             acc.POP3Server,
					POP3Port:               acc.POP3Port,
					MaildirPath:            acc.MaildirPath,
				}
			}
		}
	case "o": // Sign an OAuth2 account in again through the browser
		if m.accountsCursor < len(m.cfg.Accounts) && m.cfg.Accounts[m.accountsCursor].IsOAuth2() {
			accountID := m.cfg.Accounts[m.accountsCursor].ID
			return m, func() tea.Msg { return SignInWithBrowserMsg{AccountID: accountID} }
		}
	case "s": // Edit account signature
		if m.accountsCursor < len(m.cfg.Accounts) {
			return m, func() tea.Msg { return GoToSignatureEditorMsg{AccountID: m.cfg.Accounts[m.accountsCursor].ID} }
		}
	case "c": // Quick shortcut to crypto config
		if m.accountsCursor < len(m.cfg.Accounts) {
			m.enterCryptoConfig()
			return m, textinput.Blink
		}
	}
	return m, nil
}

func (m *Settings) enterCryptoConfig() {
	m.isCryptoConfig = true
	m.editingAccountIdx = m.accountsCursor
	acc := m.cfg.Accounts[m.accountsCursor]

	m.smimeCertInput.SetValue(acc.SMIMECert)
	m.smimeKeyInput.SetValue(acc.SMIMEKey)
	m.pgpPublicKeyInput.SetValue(acc.PGPPublicKey)
	m.pgpPrivateKeyInput.SetValue(acc.PGPPrivateKey)
	if acc.PGPKeySource == "" {
		m.pgpKeySource = "file"
	} else {
		m.pgpKeySource = acc.PGPKeySource
	}
	m.pgpPINInput.SetValue(acc.PGPPIN)

	m.cryptoFocusIndex = 0
	m.smimeCertInput.Focus()
	m.smimeKeyInput.Blur()
	m.pgpPublicKeyInput.Blur()
	m.pgpPrivateKeyInput.Blur()
	m.pgpPINInput.Blur()
}

func (m *Settings) viewAccounts() string {
	if m.suggestionsOpen {
		return m.viewAddressSuggestions()
	}
	if m.isCryptoConfig {
		return m.viewSMIMEConfig()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(t("settings_accounts.title")) + "\n\n")

	if len(m.cfg.Accounts) == 0 {
		b.WriteString(accountEmailStyle.Render("  " + t("settings_accounts.no_accounts") + "\n\n"))
	}

	for i, account := range m.cfg.Accounts {
		displayName := account.Email
		if account.Name != "" {
			displayName = fmt.Sprintf("%s (%s)", account.Name, account.FetchEmail)
		}

		providerInfo := account.ServiceProvider
		if account.ServiceProvider == "custom" {
			providerInfo = fmt.Sprintf("custom: %s", account.IMAPServer)
		}
		if account.SortBy == "arrival_date" {
			providerInfo += " [Sort: server arrival date]"
		} else {
			providerInfo += " [Sort: email date]"
		}

		if account.SMIMECert != "" && account.SMIMEKey != "" {
			providerInfo += " [S/MIME Configured]"
		}
		if account.PGPPublicKey != "" && account.PGPPrivateKey != "" {
			providerInfo += " [PGP Configured]"
		}
		if config.HasAccountSignature(&account) {
			providerInfo += " [Signature]"
		}
		if account.CatchAll {
			providerInfo += " [Fetch all mail]"
		}
		if n := len(account.ExtraAddresses); n > 0 {
			providerInfo += fmt.Sprintf(" [+%d addresses]", n)
		}
		if n := len(account.AlwaysShowMailFrom); n > 0 {
			providerInfo += fmt.Sprintf(" [always shows %d senders]", n)
		}
		if account.ShowUnaddressedMail {
			providerInfo += " [unaddressed mail]"
		}
		if account.DiscoverExtraAddresses {
			providerInfo += " [discovers addresses]"
		}

		line := fmt.Sprintf("%s - %s", displayName, accountEmailStyle.Render(providerInfo))

		selected := m.accountsCursor == i
		cursor := m.contentCursor(selected)
		style := m.contentItemStyle(selected)

		b.WriteString(style.Render(cursor+line) + "\n")
	}

	// Add Account option
	selected := m.accountsCursor == len(m.cfg.Accounts)
	cursor := m.contentCursor(selected)
	style := m.contentItemStyle(selected)
	b.WriteString(style.Render(cursor+t("settings_accounts.add_account")) + "\n\n")

	b.WriteString(helpStyle.Render(t("settings_accounts.help") + " • t: change sorting"))

	if m.confirmingDelete {
		accountName := m.cfg.Accounts[m.accountsCursor].Email
		dialog := DialogBoxStyle.Render(
			lipgloss.JoinVertical(lipgloss.Center,
				dangerStyle.Render("Delete account?"),
				accountEmailStyle.Render(accountName),
				HelpStyle.Render("\n(y/n)"),
			),
		)
		// Try to overlay dialog in a reasonable way, since we don't have full screen width access easily here.
		// Just append it.
		b.WriteString("\n\n" + dialog)
	}

	return b.String()
}

func (m *Settings) openAddressSuggestions(accountIndex int) {
	m.suggestionsOpen = true
	m.suggestionsAccount = accountIndex
	m.suggestionsCursor = 0
	m.suggestedAddresses = nil
	account := &m.cfg.Accounts[accountIndex]
	if updater, ok := m.hiddenSearcher.(interface{ SetAccount(*config.Account) }); ok {
		updater.SetAccount(account)
	}
	if m.hiddenSearcher == nil {
		return
	}
	for _, suggestion := range m.hiddenSearcher.HiddenRecipients(account.ID, "INBOX") {
		if configuredAddress(account.GetFetchEmail(), suggestion.Address) ||
			configuredAddress(account.Email, suggestion.Address) ||
			addressInList(account.ExtraAddresses, suggestion.Address) ||
			addressInList(account.IgnoredAddresses, suggestion.Address) ||
			addressInList(account.OtherAccountAddresses, suggestion.Address) {
			continue
		}
		m.suggestedAddresses = append(m.suggestedAddresses, suggestion)
		if len(m.suggestedAddresses) == 15 {
			break
		}
	}
}

func (m *Settings) updateSuggestedAddresses(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.suggestionsOpen = false
		return m, nil
	case "up", "k":
		if len(m.suggestedAddresses) > 0 {
			m.suggestionsCursor = (m.suggestionsCursor - 1 + len(m.suggestedAddresses)) % len(m.suggestedAddresses)
		}
	case keyDown, "j":
		if len(m.suggestedAddresses) > 0 {
			m.suggestionsCursor = (m.suggestionsCursor + 1) % len(m.suggestedAddresses)
		}
	case keyEnter:
		return m.saveSuggestedAddress(false)
	case "x":
		return m.saveSuggestedAddress(true)
	}
	return m, nil
}

func (m *Settings) saveSuggestedAddress(ignore bool) (tea.Model, tea.Cmd) {
	if len(m.suggestedAddresses) == 0 || m.suggestionsAccount >= len(m.cfg.Accounts) {
		return m, nil
	}
	account := &m.cfg.Accounts[m.suggestionsAccount]
	address := m.suggestedAddresses[m.suggestionsCursor].Address
	if ignore {
		account.IgnoredAddresses = append(account.IgnoredAddresses, address)
	} else {
		account.ExtraAddresses = append(account.ExtraAddresses, address)
	}
	if err := config.SaveConfig(m.cfg); err != nil {
		if ignore {
			account.IgnoredAddresses = account.IgnoredAddresses[:len(account.IgnoredAddresses)-1]
		} else {
			account.ExtraAddresses = account.ExtraAddresses[:len(account.ExtraAddresses)-1]
		}
		return m, func() tea.Msg { return NotifyMsg{Message: err.Error()} }
	}
	m.openAddressSuggestions(m.suggestionsAccount)
	return m, func() tea.Msg { return ConfigSavedMsg{} }
}

func (m *Settings) viewAddressSuggestions() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Suggested addresses") + "\n\n")
	b.WriteString("Only add addresses that belong to you; these are unverified recipients.\nUse Always Show Mail From for a teacher or mailing list instead.\n\n")
	if len(m.suggestedAddresses) == 0 {
		b.WriteString(accountEmailStyle.Render("  No suggestions. The INBOX header copy may not be ready, or there may be no eligible recipients.") + "\n\n")
	} else {
		for i, suggestion := range m.suggestedAddresses {
			line := fmt.Sprintf("%s - %d hidden mails", suggestion.Address, suggestion.Count)
			b.WriteString(m.contentItemStyle(i == m.suggestionsCursor).Render(m.contentCursor(i == m.suggestionsCursor)+line) + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(helpStyle.Render("↑/↓: navigate • enter: add address • x: ignore • esc: accounts"))
	return b.String()
}

func configuredAddress(configured, candidate string) bool {
	return strings.EqualFold(strings.TrimSpace(configured), strings.TrimSpace(candidate))
}

func addressInList(addresses []string, candidate string) bool {
	for _, address := range addresses {
		if configuredAddress(address, candidate) {
			return true
		}
	}
	return false
}
