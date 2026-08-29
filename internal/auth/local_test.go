package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ctrl-research/mmo/internal/store"
	"github.com/ctrl-research/mmo/internal/store/storetest"
)

func testLocalService(t *testing.T) (*Service, *store.Store, *httptest.Server) {
	t.Helper()

	// A schema private to this test, so packages testing in parallel cannot
	// delete each other's rows.
	st := storetest.New(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc, err := NewService(ServiceConfig{
		Store:      st,
		Sessions:   newSessions(t),
		Logger:     log,
		LocalAuth:  true,
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

func post(t *testing.T, client *http.Client, url, body string) (int, string) {
	t.Helper()
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func allow(t *testing.T, st *store.Store, username string) {
	t.Helper()
	if err := st.AddAllowlistEntry(context.Background(), "local", store.MatchSubject,
		NormaliseUsername(username), "test"); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
}

func TestRegisterAndSignIn(t *testing.T) {
	_, st, srv := testLocalService(t)
	allow(t, st, "jonathan")

	client := &http.Client{Jar: &cookieJar{cookies: map[string][]*http.Cookie{}}}

	status, body := post(t, client, srv.URL+"/auth/local/register",
		`{"username":"jonathan","password":"correct horse battery"}`)
	if status != http.StatusCreated {
		t.Fatalf("register returned %d: %s", status, body)
	}

	// Registration signs the player straight in, rather than making them
	// immediately repeat the credentials they just chose.
	resp, err := client.Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("not signed in after registering: %d", resp.StatusCode)
	}

	// And signing in again works on a fresh client.
	fresh := &http.Client{Jar: &cookieJar{cookies: map[string][]*http.Cookie{}}}
	status, body = post(t, fresh, srv.URL+"/auth/local/login",
		`{"username":"jonathan","password":"correct horse battery"}`)
	if status != http.StatusOK {
		t.Fatalf("login returned %d: %s", status, body)
	}
}

// The allowlist gates registration exactly as it gates every other provider,
// so a fresh server is not open to whoever finds it first.
func TestRegistrationRequiresTheAllowlist(t *testing.T) {
	_, _, srv := testLocalService(t)

	status, body := post(t, http.DefaultClient, srv.URL+"/auth/local/register",
		`{"username":"stranger","password":"a perfectly fine password"}`)
	if status != http.StatusForbidden {
		t.Errorf("registration without an allowlist entry returned %d: %s", status, body)
	}
}

func TestUsernamesAreCaseInsensitive(t *testing.T) {
	_, st, srv := testLocalService(t)
	allow(t, st, "jonathan")

	status, _ := post(t, http.DefaultClient, srv.URL+"/auth/local/register",
		`{"username":"Jonathan","password":"correct horse battery"}`)
	if status != http.StatusCreated {
		t.Fatalf("register returned %d", status)
	}

	// Nobody may register a name that differs only by case.
	status, _ = post(t, http.DefaultClient, srv.URL+"/auth/local/register",
		`{"username":"JONATHAN","password":"a different password here"}`)
	if status != http.StatusConflict && status != http.StatusForbidden {
		t.Errorf("registering a case variant returned %d, want a rejection", status)
	}

	// And signing in works whatever case is typed.
	status, body := post(t, http.DefaultClient, srv.URL+"/auth/local/login",
		`{"username":"JoNaThAn","password":"correct horse battery"}`)
	if status != http.StatusOK {
		t.Errorf("case-varied sign-in returned %d: %s", status, body)
	}
}

// The property that matters most: a wrong password and an unknown username
// must be indistinguishable. Otherwise the endpoint is a username oracle and
// password guessing becomes a far smaller problem.
func TestUnknownUserAndWrongPasswordAreIndistinguishable(t *testing.T) {
	_, st, srv := testLocalService(t)
	allow(t, st, "realuser")

	post(t, http.DefaultClient, srv.URL+"/auth/local/register",
		`{"username":"realuser","password":"the actual password"}`)

	wrongPassStatus, wrongPassBody := post(t, http.DefaultClient, srv.URL+"/auth/local/login",
		`{"username":"realuser","password":"not the actual password"}`)
	unknownStatus, unknownBody := post(t, http.DefaultClient, srv.URL+"/auth/local/login",
		`{"username":"nosuchuser","password":"not the actual password"}`)

	if wrongPassStatus != unknownStatus {
		t.Errorf("status differs: wrong password %d, unknown user %d -- this reveals which usernames exist",
			wrongPassStatus, unknownStatus)
	}
	if wrongPassBody != unknownBody {
		t.Errorf("body differs:\n wrong password: %s\n unknown user:   %s\n"+
			"this reveals which usernames exist", wrongPassBody, unknownBody)
	}
	if !strings.Contains(wrongPassBody, "incorrect username or password") {
		t.Errorf("failure message is specific: %s", wrongPassBody)
	}
}

// Timing must not reveal it either, which is what DummyVerify is for.
func TestUnknownUserCostsSimilarTime(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison is slow")
	}

	_, st, srv := testLocalService(t)
	allow(t, st, "realuser")
	post(t, http.DefaultClient, srv.URL+"/auth/local/register",
		`{"username":"realuser","password":"the actual password"}`)

	measure := func(body string) time.Duration {
		// A few samples, taking the median, since a single one is noise.
		var samples []time.Duration
		for i := 0; i < 5; i++ {
			start := time.Now()
			post(t, http.DefaultClient, srv.URL+"/auth/local/login", body)
			samples = append(samples, time.Since(start))
		}
		for i := 1; i < len(samples); i++ {
			for j := i; j > 0 && samples[j] < samples[j-1]; j-- {
				samples[j], samples[j-1] = samples[j-1], samples[j]
			}
		}
		return samples[len(samples)/2]
	}

	known := measure(`{"username":"realuser","password":"wrong password here"}`)
	unknown := measure(`{"username":"nosuchuser","password":"wrong password here"}`)

	// Both do a full Argon2 hash, so they should be within the same order of
	// magnitude. A missing DummyVerify makes the unknown case near-instant.
	ratio := float64(known) / float64(unknown)
	if ratio > 4 || ratio < 0.25 {
		t.Errorf("timing differs by %.1fx (known %v, unknown %v); "+
			"an unknown username should cost the same as a wrong password",
			ratio, known, unknown)
	}
}

// Online guessing must become hopeless well before it succeeds.
func TestRepeatedFailuresLockTheAccount(t *testing.T) {
	_, st, srv := testLocalService(t)
	allow(t, st, "target")
	post(t, http.DefaultClient, srv.URL+"/auth/local/register",
		`{"username":"target","password":"the actual password"}`)

	for i := 0; i < store.MaxFailedAttempts; i++ {
		post(t, http.DefaultClient, srv.URL+"/auth/local/login",
			`{"username":"target","password":"guess number `+fmt.Sprint(i)+`"}`)
	}

	// Even the correct password is refused once locked.
	status, body := post(t, http.DefaultClient, srv.URL+"/auth/local/login",
		`{"username":"target","password":"the actual password"}`)
	if status == http.StatusOK {
		t.Fatal("the account was not locked after repeated failures")
	}

	// And the refusal must not say the account is locked, which would confirm
	// the username exists.
	if strings.Contains(strings.ToLower(body), "lock") {
		t.Errorf("the lockout is disclosed to the caller: %s", body)
	}
}

// A person mistyping their own password must not lock themselves out
// permanently: a success clears the counter.
func TestSuccessfulSignInClearsFailures(t *testing.T) {
	_, st, srv := testLocalService(t)
	allow(t, st, "clumsy")
	post(t, http.DefaultClient, srv.URL+"/auth/local/register",
		`{"username":"clumsy","password":"the actual password"}`)

	for i := 0; i < store.MaxFailedAttempts-2; i++ {
		post(t, http.DefaultClient, srv.URL+"/auth/local/login",
			`{"username":"clumsy","password":"wrong"}`)
	}

	status, _ := post(t, http.DefaultClient, srv.URL+"/auth/local/login",
		`{"username":"clumsy","password":"the actual password"}`)
	if status != http.StatusOK {
		t.Fatalf("correct password after some failures returned %d", status)
	}

	cred, err := st.LocalCredentialByUsername(context.Background(), "clumsy")
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if cred.FailedAttempts != 0 {
		t.Errorf("failure count is %d after a success, want 0", cred.FailedAttempts)
	}
}

// Removing someone from the allowlist must revoke access, not merely prevent
// future registrations.
func TestAllowlistRemovalBlocksLocalSignIn(t *testing.T) {
	_, st, srv := testLocalService(t)
	allow(t, st, "revokable")
	post(t, http.DefaultClient, srv.URL+"/auth/local/register",
		`{"username":"revokable","password":"the actual password"}`)

	if err := st.RemoveAllowlistEntry(context.Background(), "local", store.MatchSubject, "revokable"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	status, _ := post(t, http.DefaultClient, srv.URL+"/auth/local/login",
		`{"username":"revokable","password":"the actual password"}`)
	if status == http.StatusOK {
		t.Error("a revoked account signed in with the correct password")
	}
}

func TestWeakPasswordsAreRejected(t *testing.T) {
	_, st, srv := testLocalService(t)
	allow(t, st, "newbie")

	status, body := post(t, http.DefaultClient, srv.URL+"/auth/local/register",
		`{"username":"newbie","password":"short"}`)
	if status != http.StatusBadRequest {
		t.Errorf("a short password returned %d: %s", status, body)
	}
}

func TestChangePasswordRequiresTheCurrentOne(t *testing.T) {
	_, st, srv := testLocalService(t)
	allow(t, st, "changer")

	client := &http.Client{Jar: &cookieJar{cookies: map[string][]*http.Cookie{}}}
	post(t, client, srv.URL+"/auth/local/register",
		`{"username":"changer","password":"the original password"}`)

	// A session alone is not enough: one left open on a shared machine should
	// not be enough to take the account over permanently.
	status, _ := post(t, client, srv.URL+"/api/password",
		`{"currentPassword":"not it","newPassword":"a brand new password"}`)
	if status != http.StatusUnauthorized {
		t.Errorf("changing a password with the wrong current one returned %d", status)
	}

	status, body := post(t, client, srv.URL+"/api/password",
		`{"currentPassword":"the original password","newPassword":"a brand new password"}`)
	if status != http.StatusOK {
		t.Fatalf("password change returned %d: %s", status, body)
	}

	// The new password works and the old one does not.
	fresh := &http.Client{Jar: &cookieJar{cookies: map[string][]*http.Cookie{}}}
	if s, _ := post(t, fresh, srv.URL+"/auth/local/login",
		`{"username":"changer","password":"a brand new password"}`); s != http.StatusOK {
		t.Errorf("the new password does not work: %d", s)
	}
	if s, _ := post(t, fresh, srv.URL+"/auth/local/login",
		`{"username":"changer","password":"the original password"}`); s == http.StatusOK {
		t.Error("the old password still works after a change")
	}
}

// Two simultaneous registrations of one username must not both succeed: the
// unique index is the authority, not a prior availability check.
func TestConcurrentRegistrationsElectOneWinner(t *testing.T) {
	_, st, srv := testLocalService(t)
	allow(t, st, "contested")

	const contenders = 8
	results := make(chan int, contenders)
	for i := 0; i < contenders; i++ {
		go func() {
			resp, err := http.Post(srv.URL+"/auth/local/register", "application/json",
				strings.NewReader(`{"username":"contested","password":"a password for racing"}`))
			if err != nil {
				results <- 0
				return
			}
			resp.Body.Close()
			results <- resp.StatusCode
		}()
	}

	created := 0
	for i := 0; i < contenders; i++ {
		if <-results == http.StatusCreated {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d of %d registrations succeeded for one username, want exactly 1",
			created, contenders)
	}
}

func TestProvidersEndpointAnnouncesLocalAuth(t *testing.T) {
	_, _, srv := testLocalService(t)

	resp, err := http.Get(srv.URL + "/auth/providers")
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		LocalAuth bool `json:"localAuth"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if !body.LocalAuth {
		t.Error("localAuth is not announced, so the client cannot show the form")
	}
}

// --- rate limiter -----------------------------------------------------------

func TestAttemptLimiter(t *testing.T) {
	l := newAttemptLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("attempt %d was refused within the limit", i)
		}
	}
	if l.allow("1.2.3.4") {
		t.Error("an attempt past the limit was allowed")
	}

	// A different source is unaffected.
	if !l.allow("5.6.7.8") {
		t.Error("one source's limit blocked another")
	}
}

func TestAttemptLimiterWindowExpires(t *testing.T) {
	l := newAttemptLimiter(2, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }

	l.allow("1.2.3.4")
	l.allow("1.2.3.4")
	if l.allow("1.2.3.4") {
		t.Fatal("setup: the limit did not apply")
	}

	now = now.Add(2 * time.Minute)
	if !l.allow("1.2.3.4") {
		t.Error("the limit did not lapse after its window")
	}
}

// Success clears the count, so someone who mistyped a few times is not
// throttled once they get it right.
func TestAttemptLimiterReset(t *testing.T) {
	l := newAttemptLimiter(2, time.Minute)

	l.allow("1.2.3.4")
	l.allow("1.2.3.4")
	l.reset("1.2.3.4")

	if !l.allow("1.2.3.4") {
		t.Error("the count was not cleared by a reset")
	}
}

// Without sweeping, the map grows with every distinct source address -- which
// is to say unboundedly, under exactly the attack it exists to blunt.
func TestAttemptLimiterDoesNotGrowUnbounded(t *testing.T) {
	l := newAttemptLimiter(5, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 500; i++ {
		l.allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	if l.tracked() == 0 {
		t.Fatal("setup: nothing was tracked")
	}

	now = now.Add(2 * time.Minute)
	l.allow("1.1.1.1")

	if n := l.tracked(); n > 5 {
		t.Errorf("%d expired buckets remain after the window lapsed", n)
	}
}
