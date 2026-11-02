---
title: Configuration
sidebar_position: 5
---

# Configuration

Configuration is stored in `~/.config/matcha/config.json`.

## Sorting by account

In Settings → Accounts, select an account and press `t` to switch between email date
and server arrival date. Each account keeps its own choice; both sort newest first.
The date shown on each message remains the email's original date.

The account's `sort_by` field accepts `email_date` (the default when omitted) or
`arrival_date`. Server arrival date is useful for messages with missing or incorrect
date headers. When a backend or an older cached message has no arrival date, Matcha
uses the email date until that information is available.

## Example Configuration

> Passwords have been removed since [v0.19.0](https://github.com/floatpane/matcha/releases/tag/v0.19.0)

```json
{
  "accounts": [
    {
      "id": "unique-id-1",
      "name": "John Doe",
      "email": "john@gmail.com",
      "service_provider": "gmail",
      "fetch_email": "john@gmail.com",
      "send_as_email": "john@alias.example",
      "smime_cert": "/home/jane/.certs/jane_smime_cert.pem",
      "smime_key": "/home/jane/.certs/jane_smime_private.pem"
    },
    {
      "id": "unique-id-2",
      "name": "Work Email",
      "email": "john@company.com",
      "service_provider": "custom",
      "fetch_email": "john@company.com",
      "extra_addresses": ["j.doe@company.com", "john@old-company.com"],
      "always_show_mail_from": ["professor@university.edu", "university.edu"],
      "imap_server": "imap.company.com",
      "imap_port": 993,
      "smtp_server": "smtp.company.com",
      "smtp_port": 587,
      "smtp_username": "jdoe"
    }
  ],
  "mailing_lists": [
    {
      "name": "Team",
      "addresses": ["alice@example.com", "bob@example.com"]
    }
  ],
  "theme": "Matcha",
  "enable_split_pane": true,
  "enable_detailed_dates": true,
  "date_format": "DD/MM/YYYY HH:MM",
  "disable_images": true,
  "hide_tips": true,
  "disable_spellcheck": false,
  "disable_spell_suggestions": false,
  "show_cc_bcc_by_default": true,
  "body_cache_threshold_mb": 100,
  "pagination_page_size": 40,
  "undo_delay_seconds": 5,
  "external_editor": "nvim"
}
```

`send_as_email` is optional. When set, Matcha uses it for the outgoing `From` header while continuing to authenticate with the account's login address.

Matcha shows mail in an account when its addresses appear in `To`, `Cc` or a `Delivered-To`/`X-Original-To`/`X-Forwarded-To` header. In Sent, one of the account's addresses must be the sender. Settings → Accounts → select an account → `e` includes these address and visibility settings:

- **Fetch all mail** (`catch_all`): type `true` and press Enter to save if you want every message in this account's mailbox, regardless of its recipient address. This includes Bcc mail and mail to other addresses in a shared mailbox. Leave it `false` to filter by configured addresses.
- `show_unaddressed_mail` shows mail only when its To, Cc and delivery headers name none of this account's addresses or any other configured account's addresses. This can reveal Bcc mail such as a teacher's course announcements. Mail naming another configured account stays hidden here. Sent is unaffected.
- `always_show_mail_from` lists senders whose mail is shown even when it names none of the account's addresses. Use an address or a domain; a domain also covers its subdomains.
- `extra_addresses` lists more addresses that deliver to the account, such as aliases or old addresses. Mail sent to them, or sent from them in Sent, is shown like mail for `fetch_email`. Reply-all also leaves them out of the recipients.
- `discover_extra_addresses` adds addresses found in Sent and hidden-mail headers to `extra_addresses` automatically. Addresses in `ignored_addresses` are not added or suggested again; removing an address from Extra addresses adds it to this list, and adding it back removes it.

Automatic discovery is off by default and supports IMAP accounts. After the INBOX header copy finishes, it scans Sent once per account per application run. It requires at least three Sent messages from an address and at least three hidden INBOX messages addressed to it, excluding configured, ignored and other-account addresses. Matcha reports each addition with both counts and refreshes the inbox. This is a heuristic, not proof of ownership: shared or imported Sent folders can produce false matches. Leave it off and use Suggested addresses for manual review when that risk matters.

Changing visibility settings, `fetch_email` or `catch_all` refreshes the inbox and fetches the headers on this computer again. Mail that now matches appears in normal results, and mail that no longer matches drops out.

`smtp_username` is optional. When set, Matcha uses it instead of `email` when authenticating with the SMTP server. Set this when your mail provider uses different credentials for IMAP and SMTP — for example, a full email address for IMAP but a short username for SMTP. The corresponding SMTP password is stored separately in the OS keyring under the key `<email>:smtp-password`; if no SMTP-specific password is stored, the main account password is used as a fallback.

`enable_split_pane` enables a side-by-side view where the email list and the selected email are shown on the same screen.

`enable_detailed_dates` shows absolute inbox dates using your configured `date_format` instead of relative labels like "2 hours ago".

`disable_spellcheck` (default `false`) turns off the composer spellcheck entirely — no underline highlights, no dictionary download, no popup. Toggle via Settings → General → Spellcheck.

`disable_spell_suggestions` (default `false`) keeps the misspelled-word underline but suppresses the inline suggestion popup. Useful if you want a quiet check without an autocomplete-style overlay. Toggle via Settings → General → Spell Suggestions.

`show_cc_bcc_by_default` (default `true`) controls whether CC and BCC fields are visible when composing new emails. When set to `false`, the fields are hidden by default but can be toggled on/off via the command palette (Ctrl+P → "Toggle CC/BCC fields"). Toggle via Settings → General → Show CC/BCC by Default.

`body_cache_threshold_mb` sets the maximum size (in megabytes) for the local email body cache. When this limit is reached, least recently accessed cached emails are evicted across all folders to make room for new ones. Defaults to `100` MB if not specified.

`pagination_page_size` accepts a positive number, `"height"`, or a fraction such as `"0.5*height"`. A number caps the messages shown per page and fetched per account at that value; a shorter inbox pane shows and fetches fewer. `"height"` fills the available pane, while `"0.5*height"` uses half the pane height, rounded down. Fractions must be greater than `0` and at most `1`. The default is `40`. Tabs and status lines reserve some rows, so a full-height page displays fewer messages than the pane's total height. This setting applies to the first folder fetch and later pages.

`undo_delay_seconds` sets the delay (in seconds) before a sent email is actually delivered, giving you a chance to cancel mistakes. During this window, a countdown shows "Sending in Xs... (u to undo)". Pressing the configured undo key cancels the send. After the delay expires, the email is transmitted and cannot be undone. Set to `0` to send immediately with no undo window. Defaults to `5` seconds if not specified.

`external_editor` sets the command Matcha runs to edit text outside the app: the composer body (`ctrl+e` by default) and `matcha config`. It may include arguments, for example `"nvim -c startinsert"`. When it is unset or blank, Matcha uses `$EDITOR`, then `$VISUAL`, then `vi`.

## Data Locations

Configuration and persistent data are stored in `~/.config/matcha/`:

| File | Description |
|------|-------------|
| `config.json` | Account settings, preferences |
| `keybinds.json` | Custom keyboard shortcuts (see [Keybinds](/Features/Keybinds)) |
| `signatures/` | Email signatures |
| `pgp/` | PGP keys |
| `plugins/` | Installed Lua plugins |
| `themes/` | Custom theme JSON files |
| `dicts/` | Hunspell spellcheck dictionaries (see [Spellcheck](/Features/Spellcheck)) |
| `secure.meta` | Encryption metadata (only when encryption is enabled) |

Cache data is stored in `~/.cache/matcha/`:

| File | Description |
|------|-------------|
| `email_cache.json` | Email metadata cache |
| `contacts.json` | Contact autocomplete data |
| `drafts.json` | Saved email drafts |
| `folder_cache.json` | Folder listings per account |
| `search_history.json` | Recent searches from the search box (last 50) |
| `folder_emails/` | Per-folder email list cache |
| `email_bodies/` | Cached email body content |

Cache files are automatically refreshed from the server on each app launch and manual refresh. If an email is removed from the server, its cache entry is cleaned up on the next refresh.

## Encryption

All data files can optionally be encrypted with a password. See [Encryption](/Features/Encryption) for details.

When encryption is enabled, account passwords are stored inside the encrypted `config.json` instead of the OS keyring.

## Password Command

Instead of storing a password in the OS keyring, you can set `pass_cmd` on an account to have matcha fetch the password from an external command (e.g. `pass`, `gopass`, or an age script). See [Password Command](/Features/PassCmd) for details.

## OAuth2 Token Command

For an account with `"auth_method": "oauth2"`, set `oauth_token_cmd` to a shell command that prints a current access token on stdout. Matcha runs it for every IMAP and SMTP login instead of its own OAuth2 helper, so you can reuse tokens another mail client already holds (for example a script that refreshes Thunderbird's saved login). The command is responsible for caching and refreshing; its error output is shown in Matcha's error message.

```json
{
  "email": "you@gmail.com",
  "auth_method": "oauth2",
  "oauth_token_cmd": "~/.local/libexec/thunderbird-oauth-token you@gmail.com"
}
```

Arrival sorting uses IMAP INTERNALDATE, JMAP receivedAt, or Maildir file modification time. JMAP already uses receivedAt for its default date, so both choices keep its existing order. POP3 and older cached messages without an arrival timestamp fall back to the email date; refresh to fetch available arrival timestamps. Sorting changes list and thread order while the displayed email date stays unchanged.
