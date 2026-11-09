package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"golang.org/x/oauth2"
)

// SessionStore keeps OIDC users, dashboard sessions and sign-ins in
// progress.
type SessionStore interface {
	SignInUser(context.Context, overload.User) (overload.User, error)
	CreateSession(ctx context.Context, userID int64, ttl time.Duration) (string, error)
	SessionUser(ctx context.Context, token string, idle time.Duration) (overload.User, error)
	EndSession(ctx context.Context, token string) error
	StartLogin(ctx context.Context, state string, login postgres.OIDCLogin, ttl time.Duration) error
	TakeLogin(ctx context.Context, state string) (postgres.OIDCLogin, error)
	ListUsers(context.Context) ([]overload.User, error)
	SetUserDisabled(ctx context.Context, id int64, disabled bool) error
}

const (
	sessionCookie = "overload_session"
	loginCookie   = "overload_login"
	loginTTL      = 10 * time.Minute
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
	settings.scopes = append(settings.scopes, strings.Fields(strings.ReplaceAll(os.Getenv("OVERLOAD_OIDC_SCOPES"), ",", " "))...)
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
}

func (auth *oidcAuth) secureCookies() bool {
	return strings.HasPrefix(auth.settings.baseURL, "https://")
}

func (auth *oidcAuth) discover(ctx context.Context) (*oidc.Provider, error) {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if auth.provider != nil {
		return auth.provider, nil
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, auth.client), auth.settings.issuer)
	if err != nil {
		return nil, err
	}
	auth.provider = provider
	return provider, nil
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
	provider, err := auth.discover(r.Context())
	if err != nil {
		slog.Warn("OIDC discovery failed", "issuer", auth.settings.issuer, "error", err)
		http.Error(w, "The sign-in provider is unavailable. Administrators can use break-glass access at /?break-glass=1.", http.StatusBadGateway)
		return
	}
	state, nonce, verifier := randomString(), randomString(), oauth2.GenerateVerifier()
	if err := auth.store.StartLogin(r.Context(), state, postgres.OIDCLogin{Nonce: nonce, Verifier: verifier, Next: localPath(r.URL.Query().Get("next"))}, loginTTL); err != nil {
		http.Error(w, "Unable to start sign-in", http.StatusInternalServerError)
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
		http.Error(w, "Sign-in expired or was started in another browser. Sign in again.", http.StatusBadRequest)
		return
	}
	login, err := auth.store.TakeLogin(r.Context(), state)
	if err != nil {
		http.Error(w, "Sign-in expired. Sign in again.", http.StatusBadRequest)
		return
	}
	if problem := query.Get("error"); problem != "" {
		http.Error(w, "The sign-in provider refused the sign-in.", http.StatusForbidden)
		return
	}
	provider, err := auth.discover(r.Context())
	if err != nil {
		http.Error(w, "The sign-in provider is unavailable.", http.StatusBadGateway)
		return
	}
	config := auth.oauthConfig(provider)
	token, err := config.Exchange(oidc.ClientContext(r.Context(), auth.client), query.Get("code"), oauth2.VerifierOption(login.Verifier))
	if err != nil {
		slog.Warn("OIDC code exchange failed", "error", err)
		http.Error(w, "Sign-in failed.", http.StatusForbidden)
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	idToken, err := provider.Verifier(&oidc.Config{ClientID: auth.settings.clientID}).Verify(oidc.ClientContext(r.Context(), auth.client), rawIDToken)
	if err != nil || idToken.Nonce != login.Nonce {
		slog.Warn("OIDC ID token rejected", "error", err)
		http.Error(w, "Sign-in failed.", http.StatusForbidden)
		return
	}
	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "Sign-in failed.", http.StatusForbidden)
		return
	}
	email, name := stringClaim(claims["email"]), stringClaim(claims["name"])
	role := auth.settings.roleFor(groupsClaim(claims[auth.settings.groupsClaim]))
	if role == "" {
		slog.Info("OIDC sign-in without a role", "subject", idToken.Subject, "email", email)
		http.Error(w, "Your account is not in a group that has access to overload. Ask an administrator.", http.StatusForbidden)
		return
	}
	user, err := auth.store.SignInUser(r.Context(), overload.User{Issuer: idToken.Issuer, Subject: idToken.Subject, Email: email, Name: name, Role: role})
	if errors.Is(err, postgres.ErrUserDisabled) {
		http.Error(w, "Your overload account is disabled. Ask an administrator.", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "Unable to sign in", http.StatusInternalServerError)
		return
	}
	session, err := auth.store.CreateSession(r.Context(), user.ID, auth.settings.sessionTTL)
	if err != nil {
		http.Error(w, "Unable to sign in", http.StatusInternalServerError)
		return
	}
	slog.Info("signed in", "user_id", user.ID, "role", role)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: session, Path: "/", MaxAge: int(auth.settings.sessionTTL.Seconds()), HttpOnly: true, Secure: auth.secureCookies(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, localPath(login.Next), http.StatusSeeOther)
}

// logout ends the session. The provider's own session is ended from the
// signed-out page, because a redirect after a form post would break the
// form-action policy.
func (auth *oidcAuth) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := auth.store.EndSession(r.Context(), cookie.Value); err != nil {
			http.Error(w, "Unable to sign out", http.StatusInternalServerError)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: auth.secureCookies(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/auth/signed-out", http.StatusSeeOther)
}

// endSessionURL is the provider's RP-initiated logout address, if it has
// one.
func (auth *oidcAuth) endSessionURL(ctx context.Context) string {
	provider, err := auth.discover(ctx)
	if err != nil {
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
	query.Set("post_logout_redirect_uri", auth.settings.baseURL+"/")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// sessionUser returns the signed-in user, if any.
func (auth *oidcAuth) sessionUser(r *http.Request) (overload.User, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return overload.User{}, false
	}
	user, err := auth.store.SessionUser(r.Context(), cookie.Value, auth.settings.idleTTL)
	if err != nil {
		if !errors.Is(err, postgres.ErrNoSession) {
			slog.Warn("session lookup failed", "error", err)
		}
		return overload.User{}, false
	}
	return user, true
}

func stringClaim(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return overload.TruncateUTF8(value, 320)
}

// groupsClaim reads a groups claim given as a list or a single string.
func groupsClaim(raw json.RawMessage) []string {
	var groups []string
	if json.Unmarshal(raw, &groups) == nil {
		return groups
	}
	if single := stringClaim(raw); single != "" {
		return []string{single}
	}
	return nil
}
