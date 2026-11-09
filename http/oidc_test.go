package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	sessions map[string]fakeSession
	logins   map[string]postgres.OIDCLogin
	started  int
	reviews  []int
}

type fakeSession struct {
	userID  int64
	idToken string
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: map[string]fakeSession{}, logins: map[string]postgres.OIDCLogin{}}
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
func (store *fakeSessionStore) CreateSession(_ context.Context, userID int64, _ time.Duration, idToken string) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	token := randomString()
	store.sessions[token] = fakeSession{userID: userID, idToken: idToken}
	return token, nil
}
func (store *fakeSessionStore) SessionUser(_ context.Context, token string, _ time.Duration) (overload.User, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	session, ok := store.sessions[token]
	if !ok || store.users[session.userID-1].Disabled {
		return overload.User{}, postgres.ErrNoSession
	}
	return store.users[session.userID-1], nil
}
func (store *fakeSessionStore) EndSession(_ context.Context, token string) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	session := store.sessions[token]
	delete(store.sessions, token)
	return session.idToken, nil
}
func (store *fakeSessionStore) StartLogin(_ context.Context, state string, login postgres.OIDCLogin, _ time.Duration) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.started++
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
	if id < 1 || int(id) > len(store.users) {
		return postgres.ErrUserNotFound
	}
	store.users[id-1].Disabled = disabled
	if disabled {
		for token, session := range store.sessions {
			if session.userID == id {
				delete(store.sessions, token)
			}
		}
	}
	return nil
}
func (store *fakeSessionStore) RequestReview(_ context.Context, _ int64, number int, _ bool) (int64, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.reviews = append(store.reviews, number)
	return 42, nil
}
func (store *fakeSessionStore) sessionCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.sessions)
}
func (store *fakeSessionStore) loginsStarted() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.started
}
func (store *fakeSessionStore) disabled(id int64) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.users[id-1].Disabled
}

// fakeProvider is an OpenID provider that issues codes for whoever the test
// says signs in next.
type fakeProvider struct {
	server    *httptest.Server
	key       *rsa.PrivateKey
	mu        sync.Mutex
	codes     map[string]providerCode
	subject   string
	groups    []string
	nonce     string
	audience  string
	noIDToken bool
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
	// oidctest's discovery document has no end_session_endpoint, so serve
	// one that adds it.
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		issuer := provider.server.URL
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "end_session_endpoint": issuer + "/logout", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		provider.mu.Lock()
		defer provider.mu.Unlock()
		code, ok := provider.codes[r.PostForm.Get("code")]
		delete(provider.codes, r.PostForm.Get("code"))
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != code.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		nonce := code.nonce
		if provider.nonce != "" {
			nonce = provider.nonce
		}
		audience := "overload"
		if provider.audience != "" {
			audience = provider.audience
		}
		groups, _ := json.Marshal(provider.groups)
		claims := fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":%q,"email":"%s@example.com","name":"Test","groups":%s,"nonce":%q,"iat":%d,"exp":%d}`, provider.server.URL, audience, provider.subject, provider.subject, groups, nonce, time.Now().Unix(), time.Now().Add(time.Hour).Unix())
		answer := map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3600}
		if !provider.noIDToken {
			answer["id_token"] = oidctest.SignIDToken(provider.key, "test", "RS256", claims)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	})
	provider.server = httptest.NewServer(mux)
	discovery.SetIssuer(provider.server.URL)
	t.Cleanup(provider.server.Close)
	return provider
}

// signsIn sets who the provider signs in next, with a well-formed answer.
func (provider *fakeProvider) signsIn(subject string, groups ...string) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.subject, provider.groups, provider.nonce, provider.audience, provider.noIDToken = subject, groups, "", "", false
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
	cookies map[string]*http.Cookie
	headers http.Header
}

func newBrowser(t *testing.T, handler http.Handler) *browser {
	return &browser{t: t, handler: handler, cookies: map[string]*http.Cookie{}, headers: http.Header{}}
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
	for name, values := range b.headers {
		request.Header[name] = values
	}
	for _, cookie := range b.cookies {
		request.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	response := httptest.NewRecorder()
	b.handler.ServeHTTP(response, request)
	for _, cookie := range response.Result().Cookies() {
		if cookie.MaxAge < 0 {
			delete(b.cookies, cookie.Name)
		} else {
			b.cookies[cookie.Name] = cookie
		}
	}
	return response
}

func (b *browser) session() string {
	if cookie := b.cookies[secureSessionCookie]; cookie != nil {
		return cookie.Value
	}
	return ""
}

// formToken reads the form token from a page the browser loads.
func (b *browser) formToken(path string) string {
	b.t.Helper()
	page := b.do(http.MethodGet, path, nil)
	match := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())
	if page.Code != http.StatusOK || match == nil {
		b.t.Fatalf("no form token on %s: %d", path, page.Code)
	}
	return match[1]
}

// startSignIn follows a signed-out page load to the provider and returns
// the callback URL the provider sends the browser back to.
func (b *browser) startSignIn(provider *fakeProvider, next string) string {
	b.t.Helper()
	response := b.do(http.MethodGet, next, nil)
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/auth/login?next=") {
		b.t.Fatalf("signed-out page: %d %v", response.Code, response.Header())
	}
	return b.login(provider, response.Header().Get("Location"))
}

// login starts a sign-in at the sign-in page and returns the provider's
// callback URL.
func (b *browser) login(provider *fakeProvider, path string) string {
	b.t.Helper()
	response := b.do(http.MethodGet, path, nil)
	if response.Code != http.StatusFound {
		b.t.Fatalf("login: %d %s", response.Code, response.Body.String())
	}
	return provider.authorize(b.t, response.Header().Get("Location"))
}

func (b *browser) signIn(provider *fakeProvider, next string) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.do(http.MethodGet, b.startSignIn(provider, next), nil)
}

func oidcEnv(t *testing.T, issuer string) {
	t.Setenv("OVERLOAD_BASE_URL", "https://overload.example.com")
	t.Setenv("OVERLOAD_OIDC_ISSUER", issuer)
	t.Setenv("OVERLOAD_OIDC_CLIENT_ID", "overload")
	t.Setenv("OVERLOAD_OIDC_CLIENT_SECRET", "secret")
	t.Setenv("OVERLOAD_OIDC_ADMIN_GROUPS", "platform")
	t.Setenv("OVERLOAD_OIDC_MEMBER_GROUPS", "engineering")
	t.Setenv("OVERLOAD_OIDC_VIEWER_GROUPS", "everyone")
	t.Setenv("OVERLOAD_OIDC_GROUPS_CLAIM", "")
	t.Setenv("OVERLOAD_OIDC_SCOPES", "")
	t.Setenv("OVERLOAD_UI_USER", "admin")
	t.Setenv("OVERLOAD_UI_PASSWORD", "break-glass")
	t.Setenv("OVERLOAD_UI_INSECURE", "")
	t.Setenv("OVERLOAD_ACCESS_TEAM_DOMAIN", "")
	t.Setenv("OVERLOAD_ACCESS_AUD", "")
}

func TestOIDCSignInRolesAndSessions(t *testing.T) {
	provider := newFakeProvider(t)
	oidcEnv(t, provider.server.URL)
	if err := ValidateAuth(); err != nil {
		t.Fatal(err)
	}
	store := newFakeSessionStore()
	handler := Handler(store)

	viewer := newBrowser(t, handler)
	provider.signsIn("vera", "everyone")
	response := viewer.signIn(provider, "/runs?page=2")
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/runs?page=2" || viewer.session() == "" {
		t.Fatalf("callback: %d %v", response.Code, response.Header())
	}
	session := viewer.cookies[secureSessionCookie]
	if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode || session.Path != "/" || session.Domain != "" {
		t.Fatalf("session cookie = %+v", session)
	}
	page := viewer.do(http.MethodGet, "/settings", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Signed in as vera@example.com (viewer)") || strings.Contains(page.Body.String(), `href="/users"`) {
		t.Fatalf("viewer page: %d", page.Code)
	}
	viewerToken := viewer.formToken("/settings")
	if denied := viewer.do(http.MethodPost, "/settings", url.Values{"csrf": {viewerToken}, "name": {"x"}}); denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "viewer") {
		t.Fatalf("viewer changed settings: %d", denied.Code)
	}
	if denied := viewer.do(http.MethodGet, "/users", nil); denied.Code != http.StatusForbidden {
		t.Fatalf("viewer listed users: %d", denied.Code)
	}
	if denied := viewer.do(http.MethodPost, "/configure/repositories/3/review", url.Values{"csrf": {viewerToken}, "pr": {"7"}}); denied.Code != http.StatusForbidden {
		t.Fatalf("viewer requested a review: %d", denied.Code)
	}

	member := newBrowser(t, handler)
	provider.signsIn("mo", "everyone", "engineering")
	member.signIn(provider, "/")
	memberToken := member.formToken("/")
	if memberToken == viewerToken {
		t.Fatal("two people share a form token")
	}
	if denied := member.do(http.MethodPost, "/settings", url.Values{"csrf": {memberToken}}); denied.Code != http.StatusForbidden {
		t.Fatalf("member changed settings: %d", denied.Code)
	}
	if queued := member.do(http.MethodPost, "/configure/repositories/3/review", url.Values{"csrf": {memberToken}, "pr": {"7"}}); queued.Code != http.StatusSeeOther || queued.Header().Get("Location") != "/runs/42?saved=true" || !slices.Equal(store.reviews, []int{7}) {
		t.Fatalf("member review request: %d %v %v", queued.Code, queued.Header(), store.reviews)
	}
	if stolen := member.do(http.MethodPost, "/configure/repositories/3/review", url.Values{"csrf": {viewerToken}, "pr": {"8"}}); stolen.Code != http.StatusForbidden {
		t.Fatalf("another person's form token was accepted: %d", stolen.Code)
	}

	admin := newBrowser(t, handler)
	provider.signsIn("ada", "platform")
	admin.signIn(provider, "/")
	users := admin.do(http.MethodGet, "/users", nil)
	if users.Code != http.StatusOK || !strings.Contains(users.Body.String(), "vera@example.com") {
		t.Fatalf("users page: %d", users.Code)
	}
	adminToken := admin.formToken("/users")
	if self := admin.do(http.MethodPost, "/users/3/disable", url.Values{"csrf": {adminToken}, "confirmed": {"true"}}); self.Code != http.StatusConflict {
		t.Fatalf("admin disabled themselves: %d", self.Code)
	}
	if prompt := admin.do(http.MethodPost, "/users/1/disable", url.Values{"csrf": {adminToken}}); prompt.Code != http.StatusOK || !strings.Contains(prompt.Body.String(), "Disable this person?") || store.disabled(1) {
		t.Fatalf("disable without confirmation: %d", prompt.Code)
	}
	if done := admin.do(http.MethodPost, "/users/1/disable", url.Values{"csrf": {adminToken}, "confirmed": {"true"}}); done.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d %s", done.Code, done.Body.String())
	}
	if missing := admin.do(http.MethodPost, "/users/99/enable", url.Values{"csrf": {adminToken}}); missing.Code != http.StatusNotFound {
		t.Fatalf("enabling a missing user: %d", missing.Code)
	}
	if after := viewer.do(http.MethodGet, "/settings", nil); after.Code != http.StatusSeeOther {
		t.Fatalf("disabled user kept their session: %d", after.Code)
	}
	provider.signsIn("vera", "everyone")
	if again := viewer.signIn(provider, "/"); again.Code != http.StatusForbidden || !strings.Contains(again.Body.String(), "disabled") || !strings.Contains(again.Body.String(), `href="/auth/login"`) {
		t.Fatalf("disabled user signed in: %d", again.Code)
	}
	if enabled := admin.do(http.MethodPost, "/users/1/enable", url.Values{"csrf": {adminToken}}); enabled.Code != http.StatusSeeOther || store.disabled(1) {
		t.Fatalf("enable: %d", enabled.Code)
	}

	// Signing in again ends the session the browser already had.
	before := admin.session()
	provider.signsIn("ada", "platform")
	if response := admin.do(http.MethodGet, admin.login(provider, "/auth/login"), nil); response.Code != http.StatusSeeOther || admin.session() == before {
		t.Fatalf("second sign-in: %d", response.Code)
	}
	if _, err := store.SessionUser(context.Background(), before, time.Hour); err == nil {
		t.Fatal("the previous session survived a new sign-in")
	}

	sessions := store.sessionCount()
	out := admin.do(http.MethodPost, "/auth/logout", url.Values{"csrf": {admin.formToken("/")}})
	if out.Code != http.StatusOK || admin.session() != "" || store.sessionCount() != sessions-1 || strings.Contains(out.Body.String(), "Signed in as") {
		t.Fatalf("logout: %d %v", out.Code, out.Header())
	}
	hint := regexp.MustCompile(`href="([^"]*id_token_hint=[^"&]+[^"]*)"`).FindStringSubmatch(out.Body.String())
	if hint == nil || !strings.Contains(hint[1], "post_logout_redirect_uri=https%3a%2f%2foverload.example.com%2f") && !strings.Contains(hint[1], "post_logout_redirect_uri=https%3A%2F%2Foverload.example.com%2F") {
		t.Fatalf("signed-out page has no provider sign-out link with a hint: %s", out.Body.String())
	}
	if after := admin.do(http.MethodGet, "/settings", nil); after.Code != http.StatusSeeOther {
		t.Fatalf("session survived logout: %d", after.Code)
	}
}

func TestOIDCRejectsBadSignIns(t *testing.T) {
	provider := newFakeProvider(t)
	oidcEnv(t, provider.server.URL)
	store := newFakeSessionStore()
	handler := Handler(store)

	for _, test := range []struct {
		name  string
		setup func()
		body  string
	}{
		{"no role", func() { provider.signsIn("nobody", "contractors") }, "not in a group"},
		{"wrong nonce", func() { provider.signsIn("eve", "platform"); provider.nonce = "forged" }, "could not be verified"},
		{"wrong audience", func() { provider.signsIn("eve", "platform"); provider.audience = "another-app" }, "could not be verified"},
		{"no ID token", func() { provider.signsIn("eve", "platform"); provider.noIDToken = true }, "could not be verified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.setup()
			response := newBrowser(t, handler).signIn(provider, "/")
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), test.body) || !strings.Contains(response.Body.String(), "Sign in again") {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
		})
	}
	provider.signsIn("eve", "platform")

	for _, test := range []struct {
		name     string
		callback func(b *browser, state string) string
		status   int
	}{
		{"provider error", func(b *browser, state string) string {
			return "/auth/callback?error=access_denied&state=" + url.QueryEscape(state)
		}, http.StatusForbidden},
		{"no sign-in cookie", func(b *browser, state string) string {
			delete(b.cookies, loginCookie)
			return "/auth/callback?code=x&state=" + url.QueryEscape(state)
		}, http.StatusBadRequest},
		{"cookie for another sign-in", func(b *browser, state string) string {
			b.cookies[loginCookie] = &http.Cookie{Name: loginCookie, Value: "other"}
			return "/auth/callback?code=x&state=" + url.QueryEscape(state)
		}, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := newBrowser(t, handler)
			login := b.do(http.MethodGet, "/auth/login", nil)
			location, _ := url.Parse(login.Header().Get("Location"))
			if response := b.do(http.MethodGet, test.callback(b, location.Query().Get("state")), nil); response.Code != test.status || !strings.Contains(response.Body.String(), "Sign in again") {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
		})
	}

	// A provider answer is accepted once.
	reuse := newBrowser(t, handler)
	callback := reuse.startSignIn(provider, "/")
	cookie := reuse.cookies[loginCookie]
	if !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/auth/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("sign-in cookie = %+v", cookie)
	}
	if response := reuse.do(http.MethodGet, callback, nil); response.Code != http.StatusSeeOther {
		t.Fatalf("first callback: %d", response.Code)
	}
	reuse.cookies[loginCookie] = cookie
	if response := reuse.do(http.MethodGet, callback, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("reused callback: %d", response.Code)
	}

	for _, next := range []string{"https://attacker.test/", "//attacker.test", "/auth/logout", "/\\attacker"} {
		if got := localPath(next); got != "/" {
			t.Errorf("localPath(%q) = %q", next, got)
		}
	}
}

func TestOIDCSignedOutRequests(t *testing.T) {
	provider := newFakeProvider(t)
	oidcEnv(t, provider.server.URL)
	store := newFakeSessionStore()
	handler := Handler(store)
	request := func(host, path string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
		for name, value := range headers {
			r.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		return response
	}
	for _, test := range []struct {
		name     string
		path     string
		headers  map[string]string
		status   int
		redirect string
	}{
		{"stylesheet", "/assets/ui.css", map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "style"}, http.StatusOK, ""},
		{"script", "/assets/ui.js", nil, http.StatusOK, ""},
		{"signed-out page", "/auth/signed-out", nil, http.StatusOK, ""},
		{"page load", "/runs", map[string]string{"Sec-Fetch-Mode": "navigate"}, http.StatusSeeOther, "/auth/login?next=%2Fruns"},
		{"page load without fetch metadata", "/runs", nil, http.StatusSeeOther, "/auth/login?next=%2Fruns"},
		{"event stream", "/runs/1/events", map[string]string{"Sec-Fetch-Mode": "cors", "Accept": "text/event-stream"}, http.StatusUnauthorized, ""},
		{"event stream without fetch metadata", "/runs/1/events", map[string]string{"Accept": "text/event-stream"}, http.StatusUnauthorized, ""},
		{"HTMX", "/runs", map[string]string{"HX-Request": "true"}, http.StatusUnauthorized, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := request("overload.example.com", test.path, test.headers)
			if response.Code != test.status || response.Header().Get("Location") != test.redirect {
				t.Fatalf("%d %v", response.Code, response.Header())
			}
		})
	}
	if started := store.loginsStarted(); started != 0 {
		t.Fatalf("signed-out requests started %d sign-ins", started)
	}
	if response := request("overload.example.com", "/runs", map[string]string{"HX-Request": "true"}); response.Header().Get("HX-Redirect") != "/auth/login" {
		t.Fatalf("HTMX redirect: %v", response.Header())
	}

	post := httptest.NewRequest(http.MethodPost, "https://overload.example.com/settings", strings.NewReader("name=x"))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, post)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "Sign in again") {
		t.Fatalf("signed-out form post: %d %s", response.Code, response.Body.String())
	}

	// Sign-ins start on the base URL's host, where the callback is.
	if response := request("127.0.0.1:8082", "/auth/login?next=%2Fruns", nil); response.Code != http.StatusFound || response.Header().Get("Location") != "https://overload.example.com/auth/login?next=%2Fruns" {
		t.Fatalf("login on another host: %d %v", response.Code, response.Header())
	}
}

func TestOIDCBreakGlass(t *testing.T) {
	provider := newFakeProvider(t)
	oidcEnv(t, provider.server.URL)
	store := newFakeSessionStore()
	handler := Handler(store)
	provider.signsIn("vera", "everyone")
	newBrowser(t, handler).signIn(provider, "/")

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	operator := newBrowser(t, handler)
	if response := operator.do(http.MethodGet, "/?break-glass=1", nil); response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("break-glass challenge: %d", response.Code)
	}
	operator.headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:break-glass")))
	operator.headers.Set("User-Agent", "incident-browser")
	token := operator.formToken("/users")
	if response := operator.do(http.MethodPost, "/users/1/disable", url.Values{"csrf": {token}, "confirmed": {"true"}}); response.Code != http.StatusSeeOther || !store.disabled(1) {
		t.Fatalf("break-glass change: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(logs.String(), "break-glass change") || !strings.Contains(logs.String(), "incident-browser") || !strings.Contains(logs.String(), "remote_addr=") {
		t.Fatalf("break-glass change was not logged with its source: %s", logs.String())
	}

	// A session wins over the break-glass login sent with it.
	both := newBrowser(t, handler)
	provider.signsIn("mo", "everyone")
	both.signIn(provider, "/")
	viewerToken := both.formToken("/")
	both.headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:break-glass")))
	if response := both.do(http.MethodPost, "/users/1/enable", url.Values{"csrf": {viewerToken}}); response.Code != http.StatusForbidden {
		t.Fatalf("break-glass login overrode a viewer's session: %d", response.Code)
	}
}

func TestOIDCDiscoveryAndLimits(t *testing.T) {
	var fetches atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		time.Sleep(20 * time.Millisecond)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	oidcEnv(t, down.URL)
	settings, err := oidcFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	auth := newOIDCAuth(settings, newFakeSessionStore())
	var clock sync.Mutex
	now := time.Now()
	auth.now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		return now
	}
	var wait sync.WaitGroup
	for range 5 {
		wait.Go(func() {
			if _, err := auth.discover(context.Background()); err == nil {
				t.Error("discovery succeeded against a provider that is down")
			}
		})
	}
	wait.Wait()
	if _, err := auth.discover(context.Background()); err == nil {
		t.Fatal("discovery succeeded against a provider that is down")
	}
	if count := fetches.Load(); count != 1 {
		t.Fatalf("concurrent and repeated sign-ins fetched discovery %d times", count)
	}
	if auth.endSessionURL("hint") != "" {
		t.Fatal("the signed-out page used a provider that was never discovered")
	}
	clock.Lock()
	now = now.Add(discoveryRetry)
	clock.Unlock()
	if _, err := auth.discover(context.Background()); err == nil || fetches.Load() != 2 {
		t.Fatalf("discovery was not retried after %s: %v", discoveryRetry, err)
	}

	limiter := newRateLimiter(2, time.Minute)
	start := time.Now()
	limiter.now = func() time.Time { return start }
	var allowed []bool
	for _, address := range []string{"a", "a", "a", "b"} {
		allowed = append(allowed, limiter.allow(address))
	}
	if !slices.Equal(allowed, []bool{true, true, false, true}) {
		t.Fatalf("the limit is not per address: %v", allowed)
	}
	start = start.Add(time.Minute)
	if !limiter.allow("a") {
		t.Fatal("the limit did not reset")
	}

	provider := newFakeProvider(t)
	oidcEnv(t, provider.server.URL)
	b := newBrowser(t, Handler(newFakeSessionStore()))
	for index := range loginsPerMinute + 1 {
		want := http.StatusFound
		if index == loginsPerMinute {
			want = http.StatusTooManyRequests
		}
		if response := b.do(http.MethodGet, "/auth/login", nil); response.Code != want {
			t.Fatalf("sign-in %d: %d", index+1, response.Code)
		}
	}
}

func TestOIDCSettings(t *testing.T) {
	oidcEnv(t, "https://idp.example.com")
	t.Setenv("OVERLOAD_OIDC_CLIENT_ID", "")
	if _, err := oidcFromEnv(); err == nil {
		t.Fatal("accepted a missing client ID")
	}
	t.Setenv("OVERLOAD_OIDC_CLIENT_ID", "overload")
	t.Setenv("OVERLOAD_OIDC_ADMIN_GROUPS", "")
	t.Setenv("OVERLOAD_OIDC_MEMBER_GROUPS", "")
	t.Setenv("OVERLOAD_OIDC_VIEWER_GROUPS", "")
	if _, err := oidcFromEnv(); err == nil {
		t.Fatal("accepted no group mapping")
	}
	t.Setenv("OVERLOAD_OIDC_ADMIN_GROUPS", "a")
	t.Setenv("OVERLOAD_OIDC_VIEWER_GROUPS", "v, a2")
	t.Setenv("OVERLOAD_OIDC_SCOPES", "openid groups, email offline_access")
	settings, err := oidcFromEnv()
	if err != nil || settings.roleFor([]string{"v", "a"}) != overload.RoleAdmin || settings.roleFor([]string{"a2"}) != overload.RoleViewer || settings.roleFor([]string{"x"}) != "" {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
	if !slices.Equal(settings.scopes, []string{"openid", "email", "profile", "groups", "offline_access"}) {
		t.Fatalf("scopes = %v", settings.scopes)
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

	claims := map[string]json.RawMessage{
		"groups":         json.RawMessage(`"solo"`),
		"realm_access":   json.RawMessage(`{"roles":["platform","offline"]}`),
		"cognito.groups": json.RawMessage(`["dotted"]`),
	}
	for _, test := range []struct {
		name string
		want []string
	}{
		{"groups", []string{"solo"}},
		{"realm_access.roles", []string{"platform", "offline"}},
		{"cognito.groups", []string{"dotted"}},
		{"realm_access.missing", nil},
		{"missing", nil},
	} {
		if got := groupsClaim(claims, test.name); !slices.Equal(got, test.want) {
			t.Errorf("groupsClaim(%q) = %v, want %v", test.name, got, test.want)
		}
	}

	for _, test := range []struct {
		user overload.User
		want string
	}{
		{overload.User{Email: "a@example.com", Name: "A", Subject: "s"}, "a@example.com"},
		{overload.User{Name: "A", Subject: "s"}, "A"},
		{overload.User{Subject: "s"}, "s"},
	} {
		if got := displayName(test.user); got != test.want {
			t.Errorf("displayName(%+v) = %q", test.user, got)
		}
	}
}
