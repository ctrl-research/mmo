package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Sessions.
//
// A short-lived signed access token proves who a request comes from without a
// database round trip. A longer-lived opaque refresh token, stored server-side
// and rotated on every use, is what makes revocation possible -- a signed
// token alone cannot be withdrawn before it expires, which is precisely why
// the access token's life is measured in minutes.

// Session lifetimes.
const (
	// AccessTTL bounds how long a compromised or revoked session stays usable.
	// Short, because a signed token cannot be recalled once issued.
	AccessTTL = 15 * time.Minute

	// RefreshTTL is how long a player stays signed in without re-authenticating.
	RefreshTTL = 30 * 24 * time.Hour

	// TicketTTL is the window between requesting a game ticket and redeeming
	// it on a WebSocket. It exists only to carry an already-authenticated
	// identity across that gap, which takes milliseconds; anything longer
	// widens the window a leaked ticket is useful in, for no benefit.
	TicketTTL = 30 * time.Second

	// StateTTL bounds how long a login may sit half-finished at the provider.
	StateTTL = 10 * time.Minute
)

// Cookie names.
const (
	AccessCookie  = "mmo_session"
	RefreshCookie = "mmo_refresh"
)

// Session errors.
var (
	ErrNoSession      = errors.New("auth: no session")
	ErrSessionExpired = errors.New("auth: session expired")
	ErrRefreshInvalid = errors.New("auth: refresh token invalid or already used")
)

// Claims is what an access token asserts.
type Claims struct {
	AccountID uuid.UUID `json:"aid"`
	jwt.RegisteredClaims
}

// Ticket authorises one WebSocket connection.
//
// It names the character, so the identity and the character are decided over
// authenticated HTTP rather than inside the socket handshake -- the socket
// only proves the ticket was held.
type Ticket struct {
	AccountID   uuid.UUID `json:"account_id"`
	CharacterID uuid.UUID `json:"character_id"`
	Name        string    `json:"name"`
}

// refreshRecord is what a refresh token resolves to.
type refreshRecord struct {
	AccountID uuid.UUID `json:"account_id"`
}

// stateRecord carries a login across the redirect to the provider and back.
type stateRecord struct {
	Provider string `json:"provider"`

	// Verifier is the PKCE code verifier. It never leaves the server, so an
	// intercepted authorization code cannot be exchanged by whoever
	// intercepted it.
	Verifier string `json:"verifier"`

	// Return is where to send the browser once login completes.
	Return string `json:"return"`
}

// Sessions issues and validates tokens.
type Sessions struct {
	secret []byte
	store  Ephemeral

	// secureCookies marks cookies Secure, which browsers require for
	// SameSite=None and which must be off for plain-HTTP local development.
	secureCookies bool

	now func() time.Time
}

// NewSessions returns a session issuer.
//
// The secret signs access tokens; changing it invalidates every existing
// session, which is the intended way to log everyone out.
func NewSessions(secret []byte, store Ephemeral, secureCookies bool) (*Sessions, error) {
	// Short keys make HMAC forgery meaningfully easier, and a signing key is
	// the one secret that must not be weak.
	if len(secret) < 32 {
		return nil, errors.New("auth: session secret must be at least 32 bytes")
	}
	if store == nil {
		return nil, errors.New("auth: an ephemeral store is required")
	}
	return &Sessions{secret: secret, store: store, secureCookies: secureCookies, now: time.Now}, nil
}

// IssueAccess mints a signed access token.
func (s *Sessions) IssueAccess(accountID uuid.UUID) (string, time.Time, error) {
	now := s.now()
	expires := now.Add(AccessTTL)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		AccountID: accountID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   accountID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	})

	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: signing access token: %w", err)
	}
	return signed, expires, nil
}

// ParseAccess validates a token and returns its claims.
func (s *Sessions) ParseAccess(raw string) (Claims, error) {
	var claims Claims

	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		// Pinning the algorithm is what closes the "alg: none" family of
		// attacks, where a forged token names a signing method the verifier
		// then obligingly accepts.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return s.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Claims{}, ErrSessionExpired
		}
		return Claims{}, ErrNoSession
	}
	if claims.AccountID == uuid.Nil {
		return Claims{}, ErrNoSession
	}
	return claims, nil
}

// IssueRefresh mints an opaque refresh token bound to an account.
func (s *Sessions) IssueRefresh(ctx context.Context, accountID uuid.UUID) (string, error) {
	token, err := NewKey()
	if err != nil {
		return "", err
	}
	if err := s.store.Put(ctx, "refresh:"+token, refreshRecord{AccountID: accountID}, RefreshTTL); err != nil {
		return "", err
	}
	return token, nil
}

// RedeemRefresh consumes a refresh token and returns the account it belongs to.
//
// Single-use: the token is deleted as it is read, and the caller issues a new
// one. Rotation means a stolen refresh token is usable at most once, and its
// use is detectable when the legitimate holder's next refresh fails.
func (s *Sessions) RedeemRefresh(ctx context.Context, token string) (uuid.UUID, error) {
	if token == "" {
		return uuid.Nil, ErrRefreshInvalid
	}

	var rec refreshRecord
	ok, err := s.store.Take(ctx, "refresh:"+token, &rec)
	if err != nil {
		return uuid.Nil, err
	}
	if !ok {
		return uuid.Nil, ErrRefreshInvalid
	}
	return rec.AccountID, nil
}

// RevokeRefresh invalidates a refresh token, for logout.
func (s *Sessions) RevokeRefresh(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	var rec refreshRecord
	_, err := s.store.Take(ctx, "refresh:"+token, &rec)
	return err
}

// IssueTicket mints a single-use ticket for a WebSocket connection.
func (s *Sessions) IssueTicket(ctx context.Context, t Ticket) (string, error) {
	id, err := NewKey()
	if err != nil {
		return "", err
	}
	if err := s.store.Put(ctx, "ticket:"+id, t, TicketTTL); err != nil {
		return "", err
	}
	return id, nil
}

// RedeemTicket consumes a ticket. A replayed ticket always fails, because the
// store deletes as it reads.
func (s *Sessions) RedeemTicket(ctx context.Context, id string) (Ticket, bool, error) {
	if id == "" {
		return Ticket{}, false, nil
	}
	var t Ticket
	ok, err := s.store.Take(ctx, "ticket:"+id, &t)
	return t, ok, err
}

// PutState stores the PKCE verifier and destination for a login in progress.
func (s *Sessions) PutState(ctx context.Context, state, provider, verifier, returnTo string) error {
	return s.store.Put(ctx, "state:"+state, stateRecord{
		Provider: provider,
		Verifier: verifier,
		Return:   returnTo,
	}, StateTTL)
}

// TakeState consumes a login's state, returning the provider, PKCE verifier,
// and destination.
//
// Single-use, which is what makes the state parameter an effective CSRF
// defence: a replayed callback finds nothing.
func (s *Sessions) TakeState(ctx context.Context, state string) (provider, verifier, returnTo string, ok bool, err error) {
	if state == "" {
		return "", "", "", false, nil
	}
	var rec stateRecord
	ok, err = s.store.Take(ctx, "state:"+state, &rec)
	if err != nil || !ok {
		return "", "", "", false, err
	}
	return rec.Provider, rec.Verifier, rec.Return, true, nil
}

// SetSessionCookies writes the access and refresh cookies.
//
// Both are HttpOnly, so script cannot read them and an XSS bug cannot exfil a
// session. SameSite=Lax allows the top-level redirect back from the identity
// provider to carry them, while still refusing cross-site subrequests.
func (s *Sessions) SetSessionCookies(w http.ResponseWriter, access, refresh string, accessExpires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     AccessCookie,
		Value:    access,
		Path:     "/",
		Expires:  accessExpires,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:  RefreshCookie,
		Value: refresh,
		// Scoped to the refresh endpoint, so the long-lived credential is not
		// attached to every request it has no business being on.
		Path:     "/auth",
		Expires:  s.now().Add(RefreshTTL),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookies expires both cookies.
func (s *Sessions) ClearSessionCookies(w http.ResponseWriter) {
	for _, c := range []struct{ name, path string }{
		{AccessCookie, "/"},
		{RefreshCookie, "/auth"},
	} {
		http.SetCookie(w, &http.Cookie{
			Name:     c.name,
			Value:    "",
			Path:     c.path,
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   s.secureCookies,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// AccountFrom extracts and validates the account behind a request.
func (s *Sessions) AccountFrom(r *http.Request) (uuid.UUID, error) {
	c, err := r.Cookie(AccessCookie)
	if err != nil || c.Value == "" {
		return uuid.Nil, ErrNoSession
	}
	claims, err := s.ParseAccess(c.Value)
	if err != nil {
		return uuid.Nil, err
	}
	return claims.AccountID, nil
}
