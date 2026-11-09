package overload

import (
	"errors"
	"slices"
	"strings"
	"time"
)

// API key scopes. ScopeModels calls models through the gateway;
// ScopeReviews requests pull request reviews.
const (
	ScopeModels  = "models"
	ScopeReviews = "reviews"
)

// MaxAPIKeyLifetime bounds how long an API key can live.
const MaxAPIKeyLifetime = 365 * 24 * time.Hour

// APIKey is a per-person credential for agents and CLIs. Only a hash of the
// secret is stored; Prefix identifies the key in lists and logs.
type APIKey struct {
	ID int64
	// UserID is the owner; 0 for keys created without OIDC sign-in.
	UserID     int64
	UserEmail  string
	Name       string
	Prefix     string
	Scopes     []string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// HasScope reports whether the key grants scope.
func (key APIKey) HasScope(scope string) bool { return slices.Contains(key.Scopes, scope) }

// ValidateAPIKey checks a key about to be created.
func ValidateAPIKey(name string, scopes []string, lifetime time.Duration) error {
	if strings.TrimSpace(name) == "" || len(name) > 100 {
		return errors.New("name the key (at most 100 characters)")
	}
	if len(scopes) == 0 {
		return errors.New("choose at least one scope")
	}
	for _, scope := range scopes {
		if scope != ScopeModels && scope != ScopeReviews {
			return errors.New("scopes are models and reviews")
		}
	}
	if lifetime < time.Hour || lifetime > MaxAPIKeyLifetime {
		return errors.New("keys expire after 1 to 365 days")
	}
	return nil
}

// Usage is one model call through the gateway.
type Usage struct {
	KeyID        int64
	UserID       int64
	Model        string
	Endpoint     string
	Status       int
	InputTokens  int64
	OutputTokens int64
	QueueTime    time.Duration
	Duration     time.Duration
}

// UsageTotal sums gateway calls for one person and model.
type UsageTotal struct {
	UserEmail    string
	Model        string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	Minutes      float64
}
