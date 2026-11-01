# fetcher

The `fetcher` package handles all email retrieval operations over IMAP. It connects to mail servers, fetches email headers and bodies, manages attachments, and supports mailbox operations like delete, archive, and move.

## Architecture

This package is the IMAP client layer for Matcha. It:

- Establishes TLS/STARTTLS connections to IMAP servers based on account configuration
- Closes the socket on every failed connection setup, including token retrieval and password/XOAUTH2 rejection.
- Shares authenticated clients across short IMAP operations, with at most three working connections per server login in each Matcha process and at most two unused clients retained for five minutes. Additional work waits for a client; IDLE uses a separate connection. Mailbox discovery releases its client before starting another operation. Ordinary fetches and mutations are not repeated automatically.
- Fetches email lists with pagination and per-account filtering (`FetchEmail`, `ExtraAddresses` and `AlwaysShowMailFrom` decide which messages an account shows; see `account_addresses.go`)
- Keeps To and Cc recipients together in `Email.To`, and preserves `Delivered-To`, `X-Forwarded-To`, and `X-Original-To` separately in `Email.DeliveryRecipients` through IMAP fetches, searches, and cached headers. Replies prefer the fetch address, then configured aliases; catch-all replies use a delivery header before falling back to To/Cc.
- Retrieves full email bodies with MIME part traversal (preferring HTML over plain text)
- Handles attachments including inline images (with CID references) and file attachments
- Supports S/MIME decryption (opaque and enveloped) and detached signature verification
- Provides mailbox operations: delete (expunge), archive (move), and folder-to-folder moves
- Exposes both mailbox-specific and convenience functions (e.g., `FetchEmails` defaults to INBOX)
- Supports XOAUTH2 SASL authentication for Gmail OAuth2 accounts (see `xoauth2.go`)

## XOAUTH2

The `xoauth2.go` file implements the XOAUTH2 SASL mechanism as a `sasl.Client`. When an account uses `auth_method: "oauth2"`, the fetcher calls `account.OAuth2AccessToken()` to get a fresh access token (from the account's `oauth_token_cmd` or the built-in helper), then authenticates the IMAP connection using this SASL client instead of a password. The initial response follows Google's XOAUTH2 protocol: `user=<email>\x01auth=Bearer <token>\x01\x01`.
