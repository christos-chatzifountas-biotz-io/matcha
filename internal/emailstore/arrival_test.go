package emailstore

import (
	"github.com/floatpane/matcha/fetcher"
	"testing"
	"time"
)

func TestArrivalDateSurvivesCache(t *testing.T) {
	original := fetcher.Email{UID: 1, Date: time.Unix(0, 0), ArrivalDate: time.Now().UTC()}
	got := CacheToEmails(EmailsToCache([]fetcher.Email{original}))[0]
	if !got.ArrivalDate.Equal(original.ArrivalDate) || !got.Date.Equal(original.Date) {
		t.Fatal("cache lost email dates")
	}
}
