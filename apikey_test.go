package overload

import (
	"testing"
	"time"
)

func TestAPIKeyState(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	revoked := now.Add(-time.Minute)
	for _, test := range []struct {
		name string
		key  APIKey
		want string
	}{
		{"active", APIKey{ExpiresAt: now.Add(time.Hour)}, APIKeyActive},
		{"expired", APIKey{ExpiresAt: now}, APIKeyExpired},
		{"revoked", APIKey{ExpiresAt: now.Add(time.Hour), RevokedAt: &revoked}, APIKeyRevoked},
		{"revoked after expiry", APIKey{ExpiresAt: now.Add(-time.Hour), RevokedAt: &revoked}, APIKeyRevoked},
	} {
		if got := test.key.State(now); got != test.want {
			t.Errorf("%s: %s", test.name, got)
		}
	}
}

func TestValidateAPIKey(t *testing.T) {
	for _, test := range []struct {
		name     string
		key      string
		scopes   []string
		lifetime time.Duration
		valid    bool
	}{
		{"valid", "laptop", []string{ScopeModels}, 24 * time.Hour, true},
		{"no name", " ", []string{ScopeModels}, 24 * time.Hour, false},
		{"no scope", "laptop", nil, 24 * time.Hour, false},
		{"reviews not accepted yet", "laptop", []string{ScopeReviews}, 24 * time.Hour, false},
		{"too long", "laptop", []string{ScopeModels}, MaxAPIKeyLifetime + time.Hour, false},
	} {
		if err := ValidateAPIKey(test.key, test.scopes, test.lifetime); (err == nil) != test.valid {
			t.Errorf("%s: %v", test.name, err)
		}
	}
}
