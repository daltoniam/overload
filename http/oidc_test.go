package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
)

type fakeSessionStore struct {
	fakeConfigStore
	mu       sync.Mutex
	users    []overload.User
	sessions map[string]int64
	logins   map[string]postgres.OIDCLogin
}

func (store *fakeSessionStore) SignInUser(_ context.Context, user overload.User) (overload.User, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for index := range store.users {
		if store.users[index].Subject == user.Subject {
			if store.users[index].Disabled {
				return overload.User{}, postgres.ErrUserDisabled
			}
			user.ID = store.users[index].ID
			store.users[index] = user
			return user, nil
		}
	}
	user.ID = int64(len(store.users) + 1)
	store.users = append(store.users, user)
	return user, nil
}
func (store *fakeSessionStore) CreateSession(_ context.Context, userID int64, _ time.Duration) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	token := randomString()
	store.sessions[token] = userID
	return token, nil
}
func (store *fakeSessionStore) SessionUser(_ context.Context, token string, _ time.Duration) (overload.User, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	id, ok := store.sessions[token]
	if !ok || store.users[id-1].Disabled {
		return overload.User{}, postgres.ErrNoSession
	}
	return store.users[id-1], nil
}
func (store *fakeSessionStore) EndSession(_ context.Context, token string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.sessions, token)
	return nil
}
func (store *fakeSessionStore) StartLogin(_ context.Context, state string, login postgres.OIDCLogin, _ time.Duration) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.logins[state] = login
	return nil
}
func (store *fakeSessionStore) TakeLogin(_ context.Context, state string) (postgres.OIDCLogin, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	login, ok := store.logins[state]
	delete(store.logins, state)
	if !ok {
		return login, postgres.ErrNoSession
	}
	return login, nil
}
func (store *fakeSessionStore) ListUsers(context.Context) ([]overload.User, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]overload.User(nil), store.users...), nil
}
func (store *fakeSessionStore) SetUserDisabled(_ context.Context, id int64, disabled bool) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.users[id-1].Disabled = disabled
	return nil
}

// fakeProvider is an OpenID provider that issues codes for whoever the test
// says signs in next.
type fakeProvider struct {
	server  *httptest.Server
	key     *rsa.PrivateKey
	mu      sync.Mutex
	codes   map[string]providerCode
	subject string
	groups  []string
	nonce   string
}

type providerCode struct {
	challenge, nonce string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{key: key, codes: map[string]providerCode{}}
	discovery := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "test", Algorithm: "RS256"}}}
	mux := http.NewServeMux()
	mux.Handle("/", discovery)
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		provider.mu.Lock()
		code, ok := provider.codes[r.PostForm.Get("code")]
		delete(provider.codes, r.PostForm.Get("code"))
		provider.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != code.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		nonce := code.nonce
		if provider.nonce != "" {
			nonce = provider.nonce
		}
		groups, _ := json.Marshal(provider.groups)
		claims := fmt.Sprintf(`{"iss":%q,"aud":"overload","sub":%q,"email":"%s@example.com","name":"Test","groups":%s,"nonce":%q,"iat":%d,"exp":%d}`, provider.server.URL, provider.subject, provider.subject, groups, nonce, time.Now().Unix(), time.Now().Add(time.Hour).Unix())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3600, "id_token": oidctest.SignIDToken(provider.key, "test", "RS256", claims)})
	})
	provider.server = httptest.NewServer(mux)
	discovery.SetIssuer(provider.server.URL)
	t.Cleanup(provider.server.Close)
	return provider
}

// authorize plays the provider's sign-in page: it checks the request and
// returns the callback URL overload would be sent back to.
func (provider *fakeProvider) authorize(t *testing.T, location string) string {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, provider.server.URL+"/auth?") {
		t.Fatalf("sign-in went to %q", location)
	}
	query := parsed.Query()
	if query.Get("code_challenge_method") != "S256" || query.Get("client_id") != "overload" || query.Get("redirect_uri") != "https://overload.example.com/auth/callback" || query.Get("nonce") == "" || !strings.Contains(query.Get("scope"), "openid") {
		t.Fatalf("authorization request %v", query)
	}
	code := randomString()
	provider.mu.Lock()
	provider.codes[code] = providerCode{challenge: query.Get("code_challenge"), nonce: query.Get("nonce")}
	provider.mu.Unlock()
	return "/auth/callback?code=" + code + "&state=" + url.QueryEscape(query.Get("state"))
}

type browser struct {
	t       *testing.T
	handler http.Handler
	cookies map[string]string
}

func (b *browser) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	var request *http.Request
	if form != nil {
		request = httptest.NewRequest(method, "https://overload.example.com"+path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "https://overload.example.com")
	} else {
		request = httptest.NewRequest(method, "https://overload.example.com"+path, nil)
	}
	for name, value := range b.cookies {
		request.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	response := httptest.NewRecorder()
	b.handler.ServeHTTP(response, request)
	for _, cookie := range response.Result().Cookies() {
		if cookie.MaxAge < 0 {
			delete(b.cookies, cookie.Name)
		} else {
			b.cookies[cookie.Name] = cookie.Value
		}
	}
	return response
}

func (b *browser) signIn(provider *fakeProvider, next string) *httptest.ResponseRecorder {
	b.t.Helper()
	response := b.do(http.MethodGet, next, nil)
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/auth/login?next=") {
		b.t.Fatalf("signed-out page: %d %v", response.Code, response.Header())
	}
	response = b.do(http.MethodGet, response.Header().Get("Location"), nil)
	if response.Code != http.StatusFound {
		b.t.Fatalf("login: %d %s", response.Code, response.Body.String())
	}
	return b.do(http.MethodGet, provider.authorize(b.t, response.Header().Get("Location")), nil)
}

func oidcEnv(t *testing.T, issuer string) {
	t.Setenv("OVERLOAD_BASE_URL", "https://overload.example.com")
	t.Setenv("OVERLOAD_OIDC_ISSUER", issuer)
	t.Setenv("OVERLOAD_OIDC_CLIENT_ID", "overload")
	t.Setenv("OVERLOAD_OIDC_CLIENT_SECRET", "secret")
	t.Setenv("OVERLOAD_OIDC_ADMIN_GROUPS", "platform")
	t.Setenv("OVERLOAD_OIDC_MEMBER_GROUPS", "engineering")
	t.Setenv("OVERLOAD_OIDC_VIEWER_GROUPS", "everyone")
	t.Setenv("OVERLOAD_UI_USER", "admin")
	t.Setenv("OVERLOAD_UI_PASSWORD", "break-glass")
	t.Setenv("OVERLOAD_UI_INSECURE", "")
}

func TestOIDCSignInRolesAndSessions(t *testing.T) {
	provider := newFakeProvider(t)
	oidcEnv(t, provider.server.URL)
	if err := ValidateAuth(); err != nil {
		t.Fatal(err)
	}
	store := &fakeSessionStore{sessions: map[string]int64{}, logins: map[string]postgres.OIDCLogin{}}
	handler := Handler(store)

	viewer := &browser{t: t, handler: handler, cookies: map[string]string{}}
	provider.subject, provider.groups = "vera", []string{"everyone"}
	response := viewer.signIn(provider, "/runs?page=2")
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/runs?page=2" || viewer.cookies[sessionCookie] == "" {
		t.Fatalf("callback: %d %v", response.Code, response.Header())
	}
	page := viewer.do(http.MethodGet, "/settings", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Signed in as vera@example.com (viewer)") || strings.Contains(page.Body.String(), `href="/users"`) {
		t.Fatalf("viewer page: %d", page.Code)
	}
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())[1]
	if denied := viewer.do(http.MethodPost, "/settings", url.Values{"csrf": {csrf}, "name": {"x"}}); denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "viewer") {
		t.Fatalf("viewer changed settings: %d", denied.Code)
	}
	if denied := viewer.do(http.MethodGet, "/users", nil); denied.Code != http.StatusForbidden {
		t.Fatalf("viewer listed users: %d", denied.Code)
	}

	member := &browser{t: t, handler: handler, cookies: map[string]string{}}
	provider.subject, provider.groups = "mo", []string{"everyone", "engineering"}
	member.signIn(provider, "/")
	if denied := member.do(http.MethodPost, "/settings", url.Values{"csrf": {csrf}}); denied.Code != http.StatusForbidden {
		t.Fatalf("member changed settings: %d", denied.Code)
	}
	if !allowed(overload.RoleMember, httptest.NewRequest(http.MethodPost, "/configure/repositories/3/review", nil)) || allowed(overload.RoleViewer, httptest.NewRequest(http.MethodPost, "/configure/schedules/daily/run", nil)) {
		t.Fatal("member actions are wrong")
	}

	admin := &browser{t: t, handler: handler, cookies: map[string]string{}}
	provider.subject, provider.groups = "ada", []string{"platform"}
	admin.signIn(provider, "/")
	users := admin.do(http.MethodGet, "/users", nil)
	if users.Code != http.StatusOK || !strings.Contains(users.Body.String(), "vera@example.com") {
		t.Fatalf("users page: %d", users.Code)
	}
	if self := admin.do(http.MethodPost, "/users/3/disable", url.Values{"csrf": {csrf}}); self.Code != http.StatusConflict {
		t.Fatalf("admin disabled themselves: %d", self.Code)
	}
	if done := admin.do(http.MethodPost, "/users/1/disable", url.Values{"csrf": {csrf}}); done.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d %s", done.Code, done.Body.String())
	}
	if after := viewer.do(http.MethodGet, "/settings", nil); after.Code != http.StatusSeeOther {
		t.Fatalf("disabled user kept their session: %d", after.Code)
	}
	provider.subject, provider.groups = "vera", []string{"everyone"}
	if again := viewer.signIn(provider, "/"); again.Code != http.StatusForbidden || !strings.Contains(again.Body.String(), "disabled") {
		t.Fatalf("disabled user signed in: %d", again.Code)
	}

	if out := admin.do(http.MethodPost, "/auth/logout", url.Values{"csrf": {csrf}}); out.Code != http.StatusSeeOther || out.Header().Get("Location") != "/auth/signed-out" || admin.cookies[sessionCookie] != "" {
		t.Fatalf("logout: %d %v", out.Code, out.Header())
	}
	if after := admin.do(http.MethodGet, "/settings", nil); after.Code != http.StatusSeeOther {
		t.Fatalf("session survived logout: %d", after.Code)
	}
}

func TestOIDCRejectsBadSignIns(t *testing.T) {
	provider := newFakeProvider(t)
	oidcEnv(t, provider.server.URL)
	store := &fakeSessionStore{sessions: map[string]int64{}, logins: map[string]postgres.OIDCLogin{}}
	handler := Handler(store)

	nobody := &browser{t: t, handler: handler, cookies: map[string]string{}}
	provider.subject, provider.groups = "nobody", []string{"contractors"}
	if response := nobody.signIn(provider, "/"); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "not in a group") {
		t.Fatalf("user without a role: %d", response.Code)
	}

	replay := &browser{t: t, handler: handler, cookies: map[string]string{}}
	provider.subject, provider.groups, provider.nonce = "eve", []string{"platform"}, "forged"
	if response := replay.signIn(provider, "/"); response.Code != http.StatusForbidden {
		t.Fatalf("wrong nonce accepted: %d", response.Code)
	}
	provider.nonce = ""

	other := &browser{t: t, handler: handler, cookies: map[string]string{}}
	login := other.do(http.MethodGet, "/auth/login", nil)
	callback := provider.authorize(t, login.Header().Get("Location"))
	stranger := &browser{t: t, handler: handler, cookies: map[string]string{}}
	if response := stranger.do(http.MethodGet, callback, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("callback without the login cookie: %d", response.Code)
	}

	for _, next := range []string{"https://attacker.test/", "//attacker.test", "/auth/logout", "/\\attacker"} {
		if got := localPath(next); got != "/" {
			t.Errorf("localPath(%q) = %q", next, got)
		}
	}

	hx := httptest.NewRequest(http.MethodGet, "https://overload.example.com/runs", nil)
	hx.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hx)
	if response.Code != http.StatusUnauthorized || response.Header().Get("HX-Redirect") != "/auth/login" {
		t.Fatalf("HTMX request without a session: %d %v", response.Code, response.Header())
	}
	breakGlass := httptest.NewRequest(http.MethodGet, "https://overload.example.com/?break-glass=1", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, breakGlass)
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("break-glass challenge: %d", response.Code)
	}
	breakGlass.SetBasicAuth("admin", "break-glass")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, breakGlass)
	if response.Code != http.StatusOK {
		t.Fatalf("break-glass access: %d", response.Code)
	}
}

func TestOIDCSettings(t *testing.T) {
	t.Setenv("OVERLOAD_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("OVERLOAD_BASE_URL", "https://overload.example.com")
	t.Setenv("OVERLOAD_OIDC_CLIENT_ID", "")
	if _, err := oidcFromEnv(); err == nil {
		t.Fatal("accepted a missing client ID")
	}
	t.Setenv("OVERLOAD_OIDC_CLIENT_ID", "overload")
	if _, err := oidcFromEnv(); err == nil {
		t.Fatal("accepted no group mapping")
	}
	t.Setenv("OVERLOAD_OIDC_ADMIN_GROUPS", "a")
	t.Setenv("OVERLOAD_OIDC_VIEWER_GROUPS", "v, a2")
	settings, err := oidcFromEnv()
	if err != nil || settings.roleFor([]string{"v", "a"}) != overload.RoleAdmin || settings.roleFor([]string{"a2"}) != overload.RoleViewer || settings.roleFor([]string{"x"}) != "" {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
	t.Setenv("OVERLOAD_BASE_URL", "http://overload.example.com")
	if _, err := oidcFromEnv(); err == nil {
		t.Fatal("accepted a plain-http public base URL")
	}
	t.Setenv("OVERLOAD_BASE_URL", "https://overload.example.com")
	t.Setenv("OVERLOAD_ACCESS_TEAM_DOMAIN", "team.cloudflareaccess.com")
	t.Setenv("OVERLOAD_ACCESS_AUD", "aud")
	if err := ValidateAuth(); err == nil {
		t.Fatal("accepted both Cloudflare Access and OIDC")
	}
	if got := groupsClaim(json.RawMessage(`"solo"`)); len(got) != 1 || got[0] != "solo" {
		t.Fatalf("single group = %v", got)
	}
}
