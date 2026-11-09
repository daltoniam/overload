package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/templates/layouts"
	"github.com/daltoniam/overload/web/templates/pages"
	"golang.org/x/oauth2"
)

// SessionStore keeps OIDC users, dashboard sessions and sign-ins in
// progress.
type SessionStore interface {
	SignInUser(context.Context, overload.User) (overload.User, error)
	CreateSession(ctx context.Context, userID int64, ttl time.Duration, idToken string) (string, error)
	SessionUser(ctx context.Context, token string, idle time.Duration) (overload.User, error)
	EndSession(ctx context.Context, token string) (idToken string, err error)
	StartLogin(ctx context.Context, state string, login postgres.OIDCLogin, ttl time.Duration, keep int) error
	TakeLogin(ctx context.Context, state string) (postgres.OIDCLogin, error)
	ListUsers(context.Context) ([]overload.User, error)
	SetUserDisabled(ctx context.Context, id int64, disabled bool) error
}

const (
	sessionCookie = "overload_session"
	// secureSessionCookie is the session cookie's name over https: the
	// __Host- prefix makes browsers refuse it unless it is Secure, for
	// this host only and for every path.
	secureSessionCookie = "__Host-overload_session"
	loginCookie         = "overload_login"
	loginTTL            = 10 * time.Minute
	// discoveryRetry is how long a failed discovery is remembered, so a
	// provider outage does not make every sign-in wait on a fetch.
	discoveryRetry = 30 * time.Second
	// maxPendingLogins bounds the sign-ins in progress kept in the
	// database, since each is stored before anyone has signed in; past it
	// the oldest are dropped.
	maxPendingLogins = 10000
)

// oidcSettings configures sign-in with an OpenID Connect provider.
type oidcSettings struct {
	issuer       string
	clientID     string
	clientSecret string
	baseURL      string
	scopes       []string
	groupsClaim  string
	// groups maps a provider group to the role it grants.
	groups     map[string]string
	sessionTTL time.Duration
	idleTTL    time.Duration
}

// oidcFromEnv returns nil when OIDC sign-in is not configured.
func oidcFromEnv() (*oidcSettings, error) {
	issuer := strings.TrimSpace(os.Getenv("OVERLOAD_OIDC_ISSUER"))
	if issuer == "" {
		return nil, nil
	}
	settings := &oidcSettings{
		issuer:       issuer,
		clientID:     strings.TrimSpace(os.Getenv("OVERLOAD_OIDC_CLIENT_ID")),
		clientSecret: os.Getenv("OVERLOAD_OIDC_CLIENT_SECRET"),
		baseURL:      strings.TrimRight(os.Getenv("OVERLOAD_BASE_URL"), "/"),
		scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		groupsClaim:  "groups",
		groups:       map[string]string{},
		sessionTTL:   12 * time.Hour,
		idleTTL:      2 * time.Hour,
	}
	if settings.clientID == "" {
		return nil, errors.New("OVERLOAD_OIDC_CLIENT_ID is required with OVERLOAD_OIDC_ISSUER")
	}
	parsed, err := url.Parse(settings.baseURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") || parsed.Host == "" {
		return nil, errors.New("OIDC sign-in needs OVERLOAD_BASE_URL set to the https address people use (http only for localhost)")
	}
	if claim := strings.TrimSpace(os.Getenv("OVERLOAD_OIDC_GROUPS_CLAIM")); claim != "" {
		settings.groupsClaim = claim
	}
	for _, scope := range strings.Fields(strings.ReplaceAll(os.Getenv("OVERLOAD_OIDC_SCOPES"), ",", " ")) {
		if !slices.Contains(settings.scopes, scope) {
			settings.scopes = append(settings.scopes, scope)
		}
	}
	for _, mapping := range []struct{ variable, role string }{{"OVERLOAD_OIDC_VIEWER_GROUPS", overload.RoleViewer}, {"OVERLOAD_OIDC_MEMBER_GROUPS", overload.RoleMember}, {"OVERLOAD_OIDC_ADMIN_GROUPS", overload.RoleAdmin}} {
		for _, group := range strings.Split(os.Getenv(mapping.variable), ",") {
			if group = strings.TrimSpace(group); group != "" {
				settings.groups[group] = mapping.role
			}
		}
	}
	if len(settings.groups) == 0 {
		return nil, errors.New("map at least one group to a role with OVERLOAD_OIDC_ADMIN_GROUPS, OVERLOAD_OIDC_MEMBER_GROUPS or OVERLOAD_OIDC_VIEWER_GROUPS")
	}
	for variable, target := range map[string]*time.Duration{"OVERLOAD_SESSION_HOURS": &settings.sessionTTL, "OVERLOAD_SESSION_IDLE_HOURS": &settings.idleTTL} {
		if value := os.Getenv(variable); value != "" {
			hours, err := strconv.Atoi(value)
			if err != nil || hours < 1 || hours > 24*30 {
				return nil, fmt.Errorf("%s must be a whole number of hours from 1 to 720", variable)
			}
			*target = time.Duration(hours) * time.Hour
		}
	}
	return settings, nil
}

// roleFor returns the highest role the groups grant, or "" for none.
func (settings *oidcSettings) roleFor(groups []string) string {
	role := ""
	for _, group := range groups {
		if granted := settings.groups[group]; granted != "" && (role == "" || overload.RoleAtLeast(granted, role)) {
			role = granted
		}
	}
	return role
}

// oidcAuth signs people in with the provider. The provider's discovery
// document is fetched on first use, so overload starts (and break-glass
// access works) while the provider is down.
type oidcAuth struct {
	settings *oidcSettings
	store    SessionStore
	client   *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
	fetching chan struct{}
	failed   error
	failedAt time.Time
	now      func() time.Time
}

func newOIDCAuth(settings *oidcSettings, store SessionStore) *oidcAuth {
	return &oidcAuth{settings: settings, store: store, client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
}

func (auth *oidcAuth) secureCookies() bool {
	return strings.HasPrefix(auth.settings.baseURL, "https://")
}

func (auth *oidcAuth) sessionCookieName() string {
	if auth.secureCookies() {
		return secureSessionCookie
	}
	return sessionCookie
}

// discover returns the provider's configuration, fetching it once at a time
// and remembering a failure for discoveryRetry.
func (auth *oidcAuth) discover(ctx context.Context) (*oidc.Provider, error) {
	for {
		auth.mu.Lock()
		switch {
		case auth.provider != nil:
			provider := auth.provider
			auth.mu.Unlock()
			return provider, nil
		case auth.failed != nil && auth.now().Sub(auth.failedAt) < discoveryRetry:
			err := auth.failed
			auth.mu.Unlock()
			return nil, err
		case auth.fetching != nil:
			fetching := auth.fetching
			auth.mu.Unlock()
			select {
			case <-fetching:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		fetching := make(chan struct{})
		auth.fetching = fetching
		auth.mu.Unlock()
		provider, err := oidc.NewProvider(oidc.ClientContext(context.WithoutCancel(ctx), auth.client), auth.settings.issuer)
		auth.mu.Lock()
		auth.fetching = nil
		if err != nil {
			auth.failed, auth.failedAt = err, auth.now()
		} else {
			auth.provider, auth.failed = provider, nil
		}
		auth.mu.Unlock()
		close(fetching)
		return provider, err
	}
}

// cachedProvider returns the provider's configuration if it has already
// been fetched.
func (auth *oidcAuth) cachedProvider() *oidc.Provider {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	return auth.provider
}

func (auth *oidcAuth) oauthConfig(provider *oidc.Provider) oauth2.Config {
	return oauth2.Config{ClientID: auth.settings.clientID, ClientSecret: auth.settings.clientSecret, Endpoint: provider.Endpoint(), RedirectURL: auth.settings.baseURL + "/auth/callback", Scopes: auth.settings.scopes}
}

func randomString() string {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

// localPath keeps a post-sign-in destination on this site.
func localPath(next string) string {
	parsed, err := url.Parse(next)
	if err != nil || next == "" || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") || strings.HasPrefix(parsed.Path, "/auth/") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return parsed.RequestURI()
}

func (auth *oidcAuth) login(w http.ResponseWriter, r *http.Request) {
	// The sign-in cookie only reaches the callback on the base URL's host,
	// so start there.
	if base, err := url.Parse(auth.settings.baseURL); err == nil && !strings.EqualFold(r.Host, base.Host) {
		http.Redirect(w, r, auth.settings.baseURL+r.URL.RequestURI(), http.StatusFound)
		return
	}
	provider, err := auth.discover(r.Context())
	if err != nil {
		slog.Warn("OIDC discovery failed", "issuer", auth.settings.issuer, "error", err)
		signInProblem(w, r, http.StatusBadGateway, "The sign-in provider is unavailable", "Try again in a minute. Administrators can use break-glass access at /?break-glass=1.")
		return
	}
	state, nonce, verifier := randomString(), randomString(), oauth2.GenerateVerifier()
	if err := auth.store.StartLogin(r.Context(), state, postgres.OIDCLogin{Nonce: nonce, Verifier: verifier, Next: localPath(r.URL.Query().Get("next"))}, loginTTL, maxPendingLogins); err != nil {
		signInProblem(w, r, http.StatusInternalServerError, "Unable to start sign-in", "Overload could not save the sign-in. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: state, Path: "/auth/", MaxAge: int(loginTTL.Seconds()), HttpOnly: true, Secure: auth.secureCookies(), SameSite: http.SameSiteLaxMode})
	config := auth.oauthConfig(provider)
	http.Redirect(w, r, config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (auth *oidcAuth) callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	cookie, err := r.Cookie(loginCookie)
	state := query.Get("state")
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: auth.secureCookies(), SameSite: http.SameSiteLaxMode})
	if err != nil || state == "" || cookie.Value != state {
		signInProblem(w, r, http.StatusBadRequest, "Sign-in expired", "The sign-in expired or was started in another browser or at another address.")
		return
	}
	login, err := auth.store.TakeLogin(r.Context(), state)
	if err != nil {
		signInProblem(w, r, http.StatusBadRequest, "Sign-in expired", "Sign-ins must finish within 10 minutes, and each can be used once.")
		return
	}
	if problem := query.Get("error"); problem != "" {
		slog.Info("OIDC provider refused the sign-in", "error", overload.TruncateUTF8(problem, 100))
		signInProblem(w, r, http.StatusForbidden, "The sign-in provider refused the sign-in", "Check with your provider's administrator that overload is assigned to you.")
		return
	}
	provider, err := auth.discover(r.Context())
	if err != nil {
		signInProblem(w, r, http.StatusBadGateway, "The sign-in provider is unavailable", "Try again in a minute. Administrators can use break-glass access at /?break-glass=1.")
		return
	}
	config := auth.oauthConfig(provider)
	token, err := config.Exchange(oidc.ClientContext(r.Context(), auth.client), query.Get("code"), oauth2.VerifierOption(login.Verifier))
	if err != nil {
		slog.Warn("OIDC code exchange failed", "error", err)
		signInProblem(w, r, http.StatusForbidden, "Sign-in failed", "The provider did not confirm the sign-in.")
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	idToken, err := provider.Verifier(&oidc.Config{ClientID: auth.settings.clientID}).Verify(oidc.ClientContext(r.Context(), auth.client), rawIDToken)
	if err != nil || idToken.Nonce != login.Nonce {
		slog.Warn("OIDC ID token rejected", "error", err)
		signInProblem(w, r, http.StatusForbidden, "Sign-in failed", "The provider's answer could not be verified.")
		return
	}
	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		signInProblem(w, r, http.StatusForbidden, "Sign-in failed", "The provider's answer could not be read.")
		return
	}
	email, name := stringClaim(claims["email"]), stringClaim(claims["name"])
	role := auth.settings.roleFor(groupsClaim(claims, auth.settings.groupsClaim))
	if role == "" {
		slog.Info("OIDC sign-in without a role", "subject", idToken.Subject, "email", email)
		signInProblem(w, r, http.StatusForbidden, "No access to overload", "Your account is not in a group that has access to overload. Ask an administrator to add you.")
		return
	}
	user, err := auth.store.SignInUser(r.Context(), overload.User{Issuer: idToken.Issuer, Subject: idToken.Subject, Email: email, Name: name, Role: role})
	if errors.Is(err, postgres.ErrUserDisabled) {
		signInProblem(w, r, http.StatusForbidden, "Your account is disabled", "An administrator disabled your overload account. Ask them to enable it.")
		return
	}
	if err != nil {
		signInProblem(w, r, http.StatusInternalServerError, "Unable to sign in", "Overload could not record the sign-in. Try again.")
		return
	}
	if previous, err := r.Cookie(auth.sessionCookieName()); err == nil && previous.Value != "" {
		if _, err := auth.store.EndSession(r.Context(), previous.Value); err != nil {
			slog.Warn("ending the previous session failed", "error", err)
		}
	}
	session, err := auth.store.CreateSession(r.Context(), user.ID, auth.settings.sessionTTL, rawIDToken)
	if err != nil {
		signInProblem(w, r, http.StatusInternalServerError, "Unable to sign in", "Overload could not start a session. Try again.")
		return
	}
	slog.Info("signed in", "user_id", user.ID, "role", role)
	http.SetCookie(w, &http.Cookie{Name: auth.sessionCookieName(), Value: session, Path: "/", MaxAge: int(auth.settings.sessionTTL.Seconds()), HttpOnly: true, Secure: auth.secureCookies(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, localPath(login.Next), http.StatusSeeOther)
}

// logout ends the session and shows the signed-out page, which links to the
// provider's own sign-out with the session's ID token as a hint. It renders
// rather than redirects because a redirect to the provider after a form post
// would break the form-action policy.
func (auth *oidcAuth) logout(w http.ResponseWriter, r *http.Request) {
	var idToken string
	if cookie, err := r.Cookie(auth.sessionCookieName()); err == nil {
		idToken, err = auth.store.EndSession(r.Context(), cookie.Value)
		if err != nil {
			signInProblem(w, r, http.StatusInternalServerError, "Unable to sign out", "Overload could not end the session. Try again.")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: auth.sessionCookieName(), Path: "/", MaxAge: -1, HttpOnly: true, Secure: auth.secureCookies(), SameSite: http.SameSiteLaxMode})
	// The page links to the provider with the ID token as a hint.
	w.Header().Set("Cache-Control", "no-store")
	_ = pages.SignedOut(auth.endSessionURL(idToken)).Render(layouts.WithoutViewer(r.Context()), w)
}

// clearStaleSession expires a session cookie the request carried but that
// no longer names a session, so later requests stop looking it up.
func (auth *oidcAuth) clearStaleSession(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.sessionCookieName()); err == nil && cookie.Value != "" {
		http.SetCookie(w, &http.Cookie{Name: auth.sessionCookieName(), Path: "/", MaxAge: -1, HttpOnly: true, Secure: auth.secureCookies(), SameSite: http.SameSiteLaxMode})
	}
}

// endSessionURL is the provider's RP-initiated logout address, if the
// provider's configuration is already known and has one. It never fetches,
// so the signed-out page does not wait on the provider.
func (auth *oidcAuth) endSessionURL(idToken string) string {
	provider := auth.cachedProvider()
	if provider == nil {
		return ""
	}
	var metadata struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&metadata); err != nil || !strings.HasPrefix(metadata.EndSession, "https://") && !strings.HasPrefix(metadata.EndSession, "http://") {
		return ""
	}
	parsed, err := url.Parse(metadata.EndSession)
	if err != nil {
		return ""
	}
	query := parsed.Query()
	query.Set("client_id", auth.settings.clientID)
	query.Set("post_logout_redirect_uri", auth.settings.baseURL+"/auth/signed-out")
	if idToken != "" {
		query.Set("id_token_hint", idToken)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// sessionUser returns the signed-in user and their session token, if any.
func (auth *oidcAuth) sessionUser(r *http.Request) (overload.User, string, bool) {
	cookie, err := r.Cookie(auth.sessionCookieName())
	if err != nil || cookie.Value == "" {
		return overload.User{}, "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	user, err := auth.store.SessionUser(ctx, cookie.Value, auth.settings.idleTTL)
	if err != nil {
		if !errors.Is(err, postgres.ErrNoSession) {
			slog.Warn("session lookup failed", "error", err)
		}
		return overload.User{}, "", false
	}
	return user, cookie.Value, true
}

// formToken is the form token for a session. It is derived from the session
// token, which only the signed-in browser has, so each person's forms carry
// their own token and it stops working when they sign out.
func (auth *oidcAuth) formToken(session string) string {
	sum := sha256.Sum256([]byte("overload-form:" + session))
	return hex.EncodeToString(sum[:])
}

// signInProblem shows a sign-in failure with a way to try again.
func signInProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pages.SignInProblem(title, detail).Render(r.Context(), w)
}

func stringClaim(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return overload.TruncateUTF8(value, 320)
}

// groupsClaim reads the groups claim, given as a list or a single string.
// A dotted name such as realm_access.roles reaches into nested claims.
func groupsClaim(claims map[string]json.RawMessage, name string) []string {
	raw, ok := claims[name]
	if path := strings.Split(name, "."); !ok && len(path) > 1 {
		raw, ok = claims[path[0]], true
		for _, key := range path[1:] {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) != nil {
				return nil
			}
			raw = nested[key]
		}
	}
	if !ok {
		return nil
	}
	var groups []string
	if json.Unmarshal(raw, &groups) == nil {
		return groups
	}
	if single := stringClaim(raw); single != "" {
		return []string{single}
	}
	return nil
}
