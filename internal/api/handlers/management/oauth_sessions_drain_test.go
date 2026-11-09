package management

import (
	"testing"
	"time"
)

func TestMaintenancePendingOAuthIncludesCredentialCommits(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		session oauthSession
		want    bool
	}{
		{"pending", oauthSession{ExpiresAt: now.Add(time.Minute)}, true},
		{"completed", oauthSession{Completed: true, ExpiresAt: now.Add(time.Minute)}, false},
		{"failed", oauthSession{Status: "failed", ExpiresAt: now.Add(time.Minute)}, false},
		{"expired", oauthSession{ExpiresAt: now.Add(-time.Minute)}, false},
		{"committing past expiry", oauthSession{Committing: true, ExpiresAt: now.Add(-time.Minute)}, true},
		{"SIWC committing past expiry", oauthSession{SIWCCommitting: true, ExpiresAt: now.Add(-time.Minute)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newOAuthSessionStore(time.Minute)
			store.sessions["test"] = tc.session
			if got := store.hasPending(now); got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}
}
