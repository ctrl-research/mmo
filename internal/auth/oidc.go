package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC relying party.
//
// The game never sees a password. It redirects to a provider, receives an
// authorization code, and exchanges it for an identity -- so the one credential
// worth stealing is never in this system's custody at all.
//
// Authorization Code with PKCE, not the implicit flow: the code alone is
// useless without the verifier, which never leaves the server, so intercepting
// the redirect achieves nothing.

// ProviderConfig describes one identity provider.
type ProviderConfig struct {
	// ID appears in URLs and is stored against every identity, so changing it
	// orphans existing accounts.
	ID string

	// DisplayName is shown on the login screen.
	DisplayName string

	// Issuer is the OIDC issuer URL. Discovery finds the endpoints from it, so
	// providers need no per-endpoint configuration.
	Issuer string

	ClientID     string
	ClientSecret string

	// Scopes beyond openid. Defaults request the profile and email needed for
	// display and for allowlisting by address.
	Scopes []string
}

// Provider is a configured, discovered identity provider.
type Provider struct {
	ID          string
	DisplayName string

	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// UserInfo is the identity a provider asserts.
type UserInfo struct {
	Subject string
	Email   string
	Name    string
}

// NewProvider performs OIDC discovery and returns a ready provider.
//
// Discovery happens at startup rather than lazily, so a misconfigured issuer
// fails the boot instead of the first login attempt -- which would otherwise
// be discovered by a player.
func NewProvider(ctx context.Context, cfg ProviderConfig, redirectURL string) (*Provider, error) {
	switch {
	case cfg.ID == "":
		return nil, errors.New("auth: provider ID is required")
	case cfg.Issuer == "":
		return nil, fmt.Errorf("auth: provider %q has no issuer", cfg.ID)
	case cfg.ClientID == "":
		return nil, fmt.Errorf("auth: provider %q has no client ID", cfg.ID)
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	issuer, err := oidc.NewProvider(discoveryCtx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: discovering provider %q at %s: %w", cfg.ID, cfg.Issuer, err)
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}

	name := cfg.DisplayName
	if name == "" {
		name = cfg.ID
	}

	return &Provider{
		ID:          cfg.ID,
		DisplayName: name,
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     issuer.Endpoint(),
			RedirectURL:  redirectURL,
			Scopes:       scopes,
		},
		verifier: issuer.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

// AuthCodeURL builds the redirect that starts a login.
func (p *Provider) AuthCodeURL(state, verifier string) string {
	return p.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", pkceChallenge(verifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

// Exchange turns an authorization code into a verified identity.
func (p *Provider) Exchange(ctx context.Context, code, verifier string) (UserInfo, error) {
	token, err := p.oauth.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		return UserInfo{}, fmt.Errorf("auth: exchanging code: %w", err)
	}

	rawID, ok := token.Extra("id_token").(string)
	if !ok || rawID == "" {
		return UserInfo{}, errors.New("auth: provider returned no id_token")
	}

	// Verifying the ID token is the whole point: it checks the signature
	// against the provider's published keys, the issuer, the audience, and the
	// expiry. Reading the claims without this would accept anything.
	idToken, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return UserInfo{}, fmt.Errorf("auth: verifying id_token: %w", err)
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Preferred     string `json:"preferred_username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return UserInfo{}, fmt.Errorf("auth: reading claims: %w", err)
	}

	info := UserInfo{Subject: idToken.Subject, Name: claims.Name}
	if info.Name == "" {
		info.Name = claims.Preferred
	}

	// An unverified email must not be trusted for allowlisting: on providers
	// that allow arbitrary addresses, accepting one would let anybody claim to
	// be an allowlisted person.
	if claims.EmailVerified {
		info.Email = claims.Email
	}

	if info.Subject == "" {
		return UserInfo{}, errors.New("auth: provider returned no subject")
	}
	return info, nil
}

// newVerifier returns a PKCE code verifier.
func newVerifier() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generating PKCE verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// pkceChallenge is the S256 challenge for a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Registry holds every configured provider.
type Registry struct {
	byID  map[string]*Provider
	order []string
}

// NewRegistry discovers every provider. It fails if any cannot be reached, so
// a broken configuration is found at startup rather than by a player.
func NewRegistry(ctx context.Context, configs []ProviderConfig, redirectURL string) (*Registry, error) {
	r := &Registry{byID: make(map[string]*Provider)}

	for _, cfg := range configs {
		if _, dup := r.byID[cfg.ID]; dup {
			return nil, fmt.Errorf("auth: provider %q is configured twice", cfg.ID)
		}
		p, err := NewProvider(ctx, cfg, redirectURL)
		if err != nil {
			return nil, err
		}
		r.byID[p.ID] = p
		r.order = append(r.order, p.ID)
	}
	return r, nil
}

// Get returns a provider by ID.
func (r *Registry) Get(id string) (*Provider, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// List returns every provider in configuration order, which is the order the
// login screen shows them in.
func (r *Registry) List() []*Provider {
	out := make([]*Provider, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// Len reports how many providers are configured.
func (r *Registry) Len() int { return len(r.byID) }

// StartLogin generates the state and PKCE verifier and returns the redirect.
func StartLogin(ctx context.Context, s *Sessions, p *Provider, returnTo string) (string, error) {
	state, err := NewKey()
	if err != nil {
		return "", err
	}
	verifier, err := newVerifier()
	if err != nil {
		return "", err
	}
	if err := s.PutState(ctx, state, p.ID, verifier, returnTo); err != nil {
		return "", err
	}
	return p.AuthCodeURL(state, verifier), nil
}

// SafeReturnTo sanitises a post-login destination.
//
// Only same-origin paths are allowed. Reflecting an arbitrary URL here would
// make the login endpoint an open redirect, which is a phishing primitive:
// a link that genuinely starts at this server and lands somewhere else.
func SafeReturnTo(raw string) string {
	if raw == "" || raw[0] != '/' {
		return "/"
	}
	// "//host" and "/\host" are parsed as protocol-relative URLs by browsers,
	// so they escape the origin despite the leading slash.
	if len(raw) > 1 && (raw[1] == '/' || raw[1] == '\\') {
		return "/"
	}
	return raw
}

// requestOrigin reconstructs the externally visible origin of a request, for
// building the redirect URL when one is not configured explicitly.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Honouring X-Forwarded-Proto only matters behind a proxy that sets it;
	// trusting it unconditionally would let a client claim HTTPS.
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
