package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ctrl-research/mmo/internal/store"
	"github.com/google/uuid"
)

func testSecret() []byte { return []byte("a-test-signing-secret-at-least-32-bytes") }

func newSessions(t *testing.T) *Sessions {
	t.Helper()
	s, err := NewSessions(testSecret(), NewMemoryEphemeral(), false)
	if err != nil {
		t.Fatalf("new sessions: %v", err)
	}
	return s
}

// --- ephemeral store --------------------------------------------------------

func TestEphemeralIsSingleUse(t *testing.T) {
	e := NewMemoryEphemeral()
	ctx := context.Background()

	if err := e.Put(ctx, "k", map[string]string{"v": "secret"}, time.Minute); err != nil {
		t.Fatalf("put: %v", err)
	}

	var got map[string]string
	ok, err := e.Take(ctx, "k", &got)
	if err != nil || !ok {
		t.Fatalf("first take: ok=%v err=%v", ok, err)
	}
	if got["v"] != "secret" {
		t.Errorf("value = %v, want secret", got)
	}

	// Single-use is enforced by the store, not by callers: a caller that
	// forgets to delete turns a replay into a working login.
	ok, _ = e.Take(ctx, "k", &got)
	if ok {
		t.Error("a value was readable twice")
	}
}

func TestEphemeralExpires(t *testing.T) {
	e := NewMemoryEphemeral()
	ctx := context.Background()

	now := time.Now()
	e.now = func() time.Time { return now }

	e.Put(ctx, "k", map[string]string{"v": "x"}, time.Minute)
	now = now.Add(2 * time.Minute)

	var got map[string]string
	if ok, _ := e.Take(ctx, "k", &got); ok {
		t.Error("an expired value was returned")
	}
	if e.Len() != 0 {
		t.Errorf("%d values remain after expiry", e.Len())
	}
}

func TestNewKeyIsUnpredictable(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		k, err := NewKey()
		if err != nil {
			t.Fatalf("new key: %v", err)
		}
		if seen[k] {
			t.Fatal("NewKey produced a duplicate")
		}
		if len(k) < 40 {
			t.Fatalf("key is only %d characters; these are bearer secrets", len(k))
		}
		seen[k] = true
	}
}

// --- sessions ---------------------------------------------------------------

func TestAccessTokenRoundTrips(t *testing.T) {
	s := newSessions(t)
	account := uuid.New()

	token, expires, err := s.IssueAccess(account)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !expires.After(time.Now()) {
		t.Error("token expires in the past")
	}

	claims, err := s.ParseAccess(token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.AccountID != account {
		t.Errorf("account = %s, want %s", claims.AccountID, account)
	}
}

func TestAccessTokenExpires(t *testing.T) {
	s := newSessions(t)

	// Mint a token as if it had been issued long enough ago to have lapsed.
	// The JWT parser validates against the real clock, so the only way to
	// produce an expired token is to backdate its issuance.
	s.now = func() time.Time { return time.Now().Add(-AccessTTL - time.Hour) }
	expired, _, err := s.IssueAccess(uuid.New())
	if err != nil {
		t.Fatalf("issue backdated token: %v", err)
	}

	s.now = time.Now
	fresh, _, err := s.IssueAccess(uuid.New())
	if err != nil {
		t.Fatalf("issue fresh token: %v", err)
	}

	if _, err := s.ParseAccess(expired); err != ErrSessionExpired {
		t.Errorf("expired token returned %v, want ErrSessionExpired", err)
	}
	if _, err := s.ParseAccess(fresh); err != nil {
		t.Errorf("fresh token was rejected: %v", err)
	}
}

// A token signed with a different key must not validate, which is the entire
// basis of the session mechanism.
func TestAccessTokenRejectsForeignSignature(t *testing.T) {
	mine := newSessions(t)
	theirs, _ := NewSessions([]byte("a-completely-different-secret-32-bytes!"), NewMemoryEphemeral(), false)

	forged, _, _ := theirs.IssueAccess(uuid.New())
	if _, err := mine.ParseAccess(forged); err == nil {
		t.Error("a token signed with another key was accepted")
	}
}

// The "alg: none" family of attacks: a forged token names a signing method the
// verifier then obligingly accepts.
func TestAccessTokenRejectsUnsignedToken(t *testing.T) {
	s := newSessions(t)

	// {"alg":"none","typ":"JWT"} with a plausible payload and no signature.
	unsigned := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." +
		"eyJhaWQiOiIwMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDEifQ."

	if _, err := s.ParseAccess(unsigned); err == nil {
		t.Error("an unsigned token was accepted")
	}
}

func TestAccessTokenRejectsGarbage(t *testing.T) {
	s := newSessions(t)
	for _, bad := range []string{"", "not-a-token", "a.b.c", strings.Repeat("x", 500)} {
		if _, err := s.ParseAccess(bad); err == nil {
			t.Errorf("accepted malformed token %q", bad)
		}
	}
}

func TestSessionSecretMustBeLongEnough(t *testing.T) {
	if _, err := NewSessions([]byte("short"), NewMemoryEphemeral(), false); err == nil {
		t.Error("a short signing secret was accepted")
	}
}

// Rotation: a refresh token works exactly once, so a stolen one is usable at
// most once and its use is detectable when the real holder's next refresh
// fails.
func TestRefreshTokenRotates(t *testing.T) {
	s := newSessions(t)
	ctx := context.Background()
	account := uuid.New()

	token, err := s.IssueRefresh(ctx, account)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	got, err := s.RedeemRefresh(ctx, token)
	if err != nil || got != account {
		t.Fatalf("redeem: account=%s err=%v", got, err)
	}

	if _, err := s.RedeemRefresh(ctx, token); err != ErrRefreshInvalid {
		t.Errorf("replayed refresh token returned %v, want ErrRefreshInvalid", err)
	}
}

func TestRevokeRefreshInvalidates(t *testing.T) {
	s := newSessions(t)
	ctx := context.Background()

	token, _ := s.IssueRefresh(ctx, uuid.New())
	if err := s.RevokeRefresh(ctx, token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.RedeemRefresh(ctx, token); err != ErrRefreshInvalid {
		t.Error("a revoked refresh token still worked")
	}
}

func TestTicketIsSingleUse(t *testing.T) {
	s := newSessions(t)
	ctx := context.Background()

	want := Ticket{AccountID: uuid.New(), CharacterID: uuid.New(), Name: "Alice"}
	id, err := s.IssueTicket(ctx, want)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	got, ok, err := s.RedeemTicket(ctx, id)
	if err != nil || !ok {
		t.Fatalf("redeem: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Errorf("ticket = %+v, want %+v", got, want)
	}

	if _, ok, _ := s.RedeemTicket(ctx, id); ok {
		t.Error("a replayed ticket was accepted")
	}
}

// Single-use state is what makes the state parameter a CSRF defence: a
// replayed callback finds nothing.
func TestLoginStateIsSingleUse(t *testing.T) {
	s := newSessions(t)
	ctx := context.Background()

	if err := s.PutState(ctx, "state-1", "google", "verifier-abc", "/play"); err != nil {
		t.Fatalf("put state: %v", err)
	}

	provider, verifier, returnTo, ok, err := s.TakeState(ctx, "state-1")
	if err != nil || !ok {
		t.Fatalf("take state: ok=%v err=%v", ok, err)
	}
	if provider != "google" || verifier != "verifier-abc" || returnTo != "/play" {
		t.Errorf("state round-trip lost data: %s %s %s", provider, verifier, returnTo)
	}

	if _, _, _, ok, _ := s.TakeState(ctx, "state-1"); ok {
		t.Error("login state was usable twice")
	}
}

func TestCookiesAreHttpOnly(t *testing.T) {
	s := newSessions(t)
	rec := httptest.NewRecorder()

	s.SetSessionCookies(rec, "access-token", "refresh-token", time.Now().Add(time.Hour))

	cookies := rec.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("set %d cookies, want 2", len(cookies))
	}
	for _, c := range cookies {
		// Script must not be able to read a session, or an XSS bug becomes
		// session theft.
		if !c.HttpOnly {
			t.Errorf("cookie %s is not HttpOnly", c.Name)
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("cookie %s has SameSite %v, want Lax", c.Name, c.SameSite)
		}
	}

	// The long-lived credential should not ride along on every request.
	for _, c := range cookies {
		if c.Name == RefreshCookie && c.Path != "/auth" {
			t.Errorf("refresh cookie path is %q, want /auth", c.Path)
		}
	}
}

// --- PKCE and redirects -----------------------------------------------------

func TestPKCEChallengeIsStable(t *testing.T) {
	// RFC 7636 appendix B's worked example.
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

	if got := pkceChallenge(verifier); got != want {
		t.Errorf("pkceChallenge = %q, want %q", got, want)
	}
}

func TestVerifiersAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		v, err := newVerifier()
		if err != nil {
			t.Fatalf("verifier: %v", err)
		}
		if seen[v] {
			t.Fatal("newVerifier produced a duplicate")
		}
		seen[v] = true
	}
}

// Reflecting an arbitrary URL would make login an open redirect: a link that
// genuinely starts at this server and lands somewhere else.
func TestSafeReturnToRefusesOffsiteDestinations(t *testing.T) {
	tests := map[string]string{
		"/play":               "/play",
		"/":                   "/",
		"":                    "/",
		"//evil.com":          "/",
		"/\\evil.com":         "/",
		"https://evil.com":    "/",
		"http://evil.com/x":   "/",
		"javascript:alert(1)": "/",
		"//evil.com/path?a=b": "/",
		"/legit/path?query=1": "/legit/path?query=1",
	}
	for in, want := range tests {
		if got := SafeReturnTo(in); got != want {
			t.Errorf("SafeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- service, against a real database ---------------------------------------

func testService(t *testing.T) (*Service, *store.Store, *httptest.Server) {
	t.Helper()

	url := os.Getenv("MMO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MMO_TEST_DATABASE_URL is not set; skipping service tests")
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.Open(ctx, store.Config{URL: url, Logger: log})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	if _, err := st.Pool().Exec(ctx, `TRUNCATE accounts, allowlist CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	svc, err := NewService(ServiceConfig{
		Store:      st,
		Sessions:   newSessions(t),
		Logger:     log,
		DevAuth:    true,
		DefaultMap: "tutorial",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	mux := http.NewServeMux()
	svc.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return svc, st, srv
}

// signIn performs a dev login and returns a client holding the session.
func signIn(t *testing.T, srv *httptest.Server, subject string) *http.Client {
	t.Helper()

	jar := &cookieJar{cookies: map[string][]*http.Cookie{}}
	client := &http.Client{Jar: jar}

	resp, err := client.Post(srv.URL+"/auth/dev/login", "application/json",
		strings.NewReader(`{"subject":"`+subject+`"}`))
	if err != nil {
		t.Fatalf("dev login: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dev login returned %d: %s", resp.StatusCode, body)
	}
	return client
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	_, _, srv := testService(t)

	for _, path := range []string{"/api/me", "/api/characters", "/api/ticket"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s without a session returned %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestDevLoginEstablishesASession(t *testing.T) {
	_, _, srv := testService(t)
	client := signIn(t, srv, "tester")

	resp, err := client.Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me returned %d, want 200", resp.StatusCode)
	}
	var body struct {
		AccountID string `json:"accountId"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.AccountID == "" {
		t.Error("no account id in the session")
	}
}

func TestCharacterLifecycle(t *testing.T) {
	_, _, srv := testService(t)
	client := signIn(t, srv, "player")

	// Create.
	resp, err := client.Post(srv.URL+"/api/characters", "application/json",
		strings.NewReader(`{"name":"Alice","class":"warrior"}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create returned %d: %s", resp.StatusCode, body)
	}
	var created characterView
	json.NewDecoder(resp.Body).Decode(&created)

	if created.Name != "Alice" || created.Level != 1 {
		t.Errorf("created character = %+v", created)
	}

	// List.
	listResp, _ := client.Get(srv.URL + "/api/characters")
	defer listResp.Body.Close()
	var list struct {
		Characters []characterView `json:"characters"`
	}
	json.NewDecoder(listResp.Body).Decode(&list)
	if len(list.Characters) != 1 {
		t.Fatalf("listed %d characters, want 1", len(list.Characters))
	}

	// Ticket.
	tResp, _ := client.Post(srv.URL+"/api/ticket", "application/json",
		strings.NewReader(`{"characterId":"`+created.ID+`"}`))
	defer tResp.Body.Close()
	if tResp.StatusCode != http.StatusOK {
		t.Fatalf("ticket returned %d", tResp.StatusCode)
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	json.NewDecoder(tResp.Body).Decode(&ticket)
	if ticket.Ticket == "" {
		t.Error("no ticket issued")
	}

	// Delete.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/characters/"+created.ID, nil)
	dResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	dResp.Body.Close()
	if dResp.StatusCode != http.StatusOK {
		t.Fatalf("delete returned %d", dResp.StatusCode)
	}

	after, _ := client.Get(srv.URL + "/api/characters")
	defer after.Body.Close()
	var remaining struct {
		Characters []characterView `json:"characters"`
	}
	json.NewDecoder(after.Body).Decode(&remaining)
	if len(remaining.Characters) != 0 {
		t.Errorf("%d characters remain after deletion", len(remaining.Characters))
	}
}

// A ticket must not be obtainable for a character the requester does not own.
func TestCannotGetATicketForAnotherAccountsCharacter(t *testing.T) {
	_, _, srv := testService(t)

	owner := signIn(t, srv, "owner")
	resp, _ := owner.Post(srv.URL+"/api/characters", "application/json",
		strings.NewReader(`{"name":"Victim"}`))
	var created characterView
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	intruder := signIn(t, srv, "intruder")
	tResp, err := intruder.Post(srv.URL+"/api/ticket", "application/json",
		strings.NewReader(`{"characterId":"`+created.ID+`"}`))
	if err != nil {
		t.Fatalf("ticket: %v", err)
	}
	defer tResp.Body.Close()

	if tResp.StatusCode != http.StatusNotFound {
		t.Errorf("ticket for another account's character returned %d, want 404", tResp.StatusCode)
	}
}

func TestCharacterNameValidationIsEnforced(t *testing.T) {
	_, _, srv := testService(t)
	client := signIn(t, srv, "player")

	for _, name := range []string{"ab", "1abc", "has space", strings.Repeat("x", 30), ""} {
		resp, err := client.Post(srv.URL+"/api/characters", "application/json",
			strings.NewReader(`{"name":"`+name+`"}`))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("name %q returned %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestCharacterLimitIsEnforced(t *testing.T) {
	_, _, srv := testService(t)
	client := signIn(t, srv, "prolific")

	for i := 0; i < MaxCharactersPerAccount; i++ {
		resp, err := client.Post(srv.URL+"/api/characters", "application/json",
			strings.NewReader(`{"name":"Char`+string(rune('a'+i))+`"}`))
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %d returned %d", i, resp.StatusCode)
		}
	}

	resp, _ := client.Post(srv.URL+"/api/characters", "application/json",
		strings.NewReader(`{"name":"OneTooMany"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("exceeding the character limit returned %d, want 409", resp.StatusCode)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	_, _, srv := testService(t)
	client := signIn(t, srv, "player")

	resp, err := client.Post(srv.URL+"/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()

	after, _ := client.Get(srv.URL + "/api/me")
	defer after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Errorf("still signed in after logout: %d", after.StatusCode)
	}
}

func TestRefreshIssuesANewSession(t *testing.T) {
	_, _, srv := testService(t)
	client := signIn(t, srv, "player")

	resp, err := client.Post(srv.URL+"/auth/refresh", "application/json", nil)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh returned %d", resp.StatusCode)
	}

	after, _ := client.Get(srv.URL + "/api/me")
	defer after.Body.Close()
	if after.StatusCode != http.StatusOK {
		t.Errorf("session is not usable after refresh: %d", after.StatusCode)
	}
}

// Removing someone from the allowlist must actually revoke access, or the
// check is decorative for anyone who has signed in once.
func TestAllowlistRemovalBlocksTheNextLogin(t *testing.T) {
	_, st, srv := testService(t)
	ctx := context.Background()

	signIn(t, srv, "revokable")

	if err := st.RemoveAllowlistEntry(ctx, "dev", store.MatchSubject, "revokable"); err != nil {
		t.Fatalf("remove allowlist entry: %v", err)
	}

	// Sign in again without the auto-allow that dev login performs, by going
	// through the same allowlist check the production path uses.
	allowed, err := st.Allowed(ctx, "dev", "revokable", "")
	if err != nil {
		t.Fatalf("allowed: %v", err)
	}
	if allowed {
		t.Error("a revoked subject is still allowed")
	}
}

func TestServiceRefusesToStartWithNoWayToSignIn(t *testing.T) {
	url := os.Getenv("MMO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MMO_TEST_DATABASE_URL is not set")
	}

	st, err := store.Open(context.Background(), store.Config{
		URL: url, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// No providers and no dev auth means nobody could ever sign in, which is a
	// misconfiguration worth failing the boot for rather than discovering when
	// the first player tries.
	_, err = NewService(ServiceConfig{
		Store: st, Sessions: newSessions(t), DefaultMap: "tutorial", DevAuth: false,
	})
	if err == nil {
		t.Error("a server with no way to sign in started anyway")
	}
}

// --- helpers ----------------------------------------------------------------

// cookieJar is a minimal jar; net/http/cookiejar requires a PSL-aware host
// policy that rejects the bare "127.0.0.1" httptest uses.
type cookieJar struct {
	cookies map[string][]*http.Cookie
}

func (j *cookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	for _, c := range cookies {
		existing := j.cookies[c.Name]
		_ = existing
		if c.MaxAge < 0 {
			delete(j.cookies, c.Name)
			continue
		}
		j.cookies[c.Name] = []*http.Cookie{c}
	}
}

func (j *cookieJar) Cookies(u *url.URL) []*http.Cookie {
	var out []*http.Cookie
	for _, cs := range j.cookies {
		out = append(out, cs...)
	}
	return out
}
