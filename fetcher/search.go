package fetcher

import (
	"context"
	"fmt"
	"sort"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/floatpane/matcha/backend"
	"github.com/floatpane/matcha/config"
)

// SearchMailbox searches a mailbox server-side and fetches matching envelopes.
func SearchMailbox(account *config.Account, folder string, query backend.SearchQuery) ([]Email, error) {
	if hasBackendProvider(account) {
		p, err := newBackendProvider(account)
		if err != nil {
			return nil, err
		}
		defer p.Close() //nolint:errcheck
		emails, err := p.Search(context.Background(), folder, query)
		if err != nil {
			return nil, err
		}
		return backendEmailsToFetcher(emails), nil
	}

	var emails []Email
	err := withIMAPClient(account, func(c *imapclient.Client) error {
		var err error
		emails, err = searchWithClient(c, account, folder, query)
		return err
	})
	if err != nil {
		return nil, err
	}
	return emails, nil
}

// searchWithClient selects folder on c, runs the search and fetches the
// envelopes of the newest matches.
func searchWithClient(c *imapclient.Client, account *config.Account, folder string, query backend.SearchQuery) ([]Email, error) {
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		return nil, err
	}

	criteria := buildSearchCriteria(query)
	options := (*imap.SearchOptions)(nil)
	if caps := c.Caps(); caps.Has(imap.CapESearch) || caps.Has(imap.CapIMAP4rev2) {
		options = &imap.SearchOptions{ReturnAll: true}
	}

	searchData, err := c.UIDSearch(criteria, options).Wait()
	if err != nil && options != nil {
		searchData, err = c.UIDSearch(criteria, nil).Wait()
	}
	if err != nil {
		return nil, fmt.Errorf("imap search: %w", err)
	}

	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		return []Email{}, nil
	}

	sort.Slice(uids, func(i, j int) bool {
		return uids[i] > uids[j]
	})
	if limit := searchLimit(query); len(uids) > int(limit) {
		uids = uids[:limit]
	}

	var uidSet imap.UIDSet
	for _, uid := range uids {
		uidSet.AddNum(uid)
	}

	deliveryHeaderSection := buildDeliveryHeaderSection()
	msgs, err := c.Fetch(uidSet, &imap.FetchOptions{
		Envelope:     true,
		InternalDate: true,
		UID:          true,
		Flags:        true,
		BodySection:  []*imap.FetchItemBodySection{deliveryHeaderSection},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap search fetch: %w", err)
	}

	emails := make([]Email, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Envelope == nil {
			continue
		}
		email := Email{
			UID:                uint32(msg.UID),
			Subject:            decodeHeader(msg.Envelope.Subject),
			Date:               msg.Envelope.Date,
			ArrivalDate:        msg.InternalDate,
			IsRead:             hasSeenFlag(msg.Flags),
			MessageID:          msg.Envelope.MessageID,
			AccountID:          account.ID,
			DeliveryRecipients: deliveryRecipients(msg.FindBodySection(deliveryHeaderSection)),
		}
		if len(msg.Envelope.From) > 0 {
			email.From = formatAddress(msg.Envelope.From[0])
		}
		for _, addr := range msg.Envelope.To {
			email.To = append(email.To, addr.Addr())
		}
		for _, addr := range msg.Envelope.Cc {
			email.To = append(email.To, addr.Addr())
		}
		for _, addr := range msg.Envelope.ReplyTo {
			email.ReplyTo = append(email.ReplyTo, addr.Addr())
		}
		emails = append(emails, email)
	}
	sort.Slice(emails, func(i, j int) bool {
		return emails[i].UID > emails[j].UID
	})

	return emails, nil
}

func buildSearchCriteria(query backend.SearchQuery) *imap.SearchCriteria {
	criteria := &imap.SearchCriteria{}
	if query.From != "" {
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "From", Value: query.From})
	}
	if query.To != "" {
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "To", Value: query.To})
	}
	if query.Subject != "" {
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "Subject", Value: query.Subject})
	}
	if query.Body != "" {
		criteria.Body = []string{query.Body}
	}
	// Several TEXT keys are ANDed by the server, so word order does not matter.
	criteria.Text = query.Terms()
	if !query.Since.IsZero() {
		criteria.Since = query.Since
	}
	if !query.Before.IsZero() {
		criteria.Before = query.Before
	}
	if query.LargerThan > 0 {
		criteria.Larger = int64(query.LargerThan)
	}
	return criteria
}

func searchLimit(query backend.SearchQuery) uint32 {
	if query.Limit > 0 {
		return query.Limit
	}
	return 100
}
