package auth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ctrl-research/mmo/internal/store"
	"github.com/google/uuid"
)

// MaxCharactersPerAccount bounds how many characters one account may hold.
const MaxCharactersPerAccount = 6

// Service serves the identity and character-selection endpoints.
//
// Everything a player does before entering the world happens here, over
// authenticated HTTP: signing in, choosing a character, and obtaining the
// single-use ticket that opens a game socket. The socket itself then proves
// only that the ticket was held, which keeps identity out of the game
// protocol entirely.
type Service struct {
	store     *store.Store
	sessions  *Sessions
	providers *Registry
	log       *slog.Logger

	// devAuth signs players in with no identity check at all. It exists so the
	// game is playable without configuring an identity provider, and the
	// server refuses to enable it unless asked explicitly.
	devAuth bool

	// defaultMap is where a newly created character starts.
	defaultMap string

	// localAuth enables username and password accounts held by this server.
	localAuth bool

	// loginLimiter throttles sign-in attempts by source address, alongside the
	// per-account lockout in the database.
	loginLimiter *attemptLimiter
}

// ServiceConfig configures the Service.
type ServiceConfig struct {
	Store      *store.Store
	Sessions   *Sessions
	Providers  *Registry
	Logger     *slog.Logger
	DevAuth    bool
	DefaultMap string

	// LocalAuth enables username and password accounts held by this server.
	//
	// It makes this server the custodian of password hashes, which the OIDC
	// path deliberately avoids -- but requiring an external identity provider
	// is a real barrier for a self-hosted game.
	LocalAuth bool
}

// NewService builds the identity service.
func NewService(cfg ServiceConfig) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("auth: a store is required")
	case cfg.Sessions == nil:
		return nil, errors.New("auth: sessions are required")
	case cfg.DefaultMap == "":
		return nil, errors.New("auth: a default map is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Providers == nil {
		cfg.Providers = &Registry{byID: map[string]*Provider{}}
	}
	if cfg.Providers.Len() == 0 && !cfg.DevAuth && !cfg.LocalAuth {
		return nil, errors.New("auth: no way to sign in is configured; enable local accounts, configure an OIDC provider, or use development authentication")
	}

	return &Service{
		store:      cfg.Store,
		sessions:   cfg.Sessions,
		providers:  cfg.Providers,
		log:        cfg.Logger,
		devAuth:    cfg.DevAuth,
		defaultMap: cfg.DefaultMap,
		localAuth:  cfg.LocalAuth,

		loginLimiter: newAttemptLimiter(loginAttemptLimit, loginAttemptWindow),
	}, nil
}

// Routes registers the identity and character endpoints.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/providers", s.handleProviders)
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/refresh", s.handleRefresh)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)

	mux.HandleFunc("GET /api/me", s.requireSession(s.handleMe))
	mux.HandleFunc("GET /api/characters", s.requireSession(s.handleListCharacters))
	mux.HandleFunc("POST /api/characters", s.requireSession(s.handleCreateCharacter))
	mux.HandleFunc("DELETE /api/characters/{id}", s.requireSession(s.handleDeleteCharacter))
	mux.HandleFunc("POST /api/ticket", s.requireSession(s.handleTicket))

	if s.localAuth {
		mux.HandleFunc("POST /auth/local/register", s.handleLocalRegister)
		mux.HandleFunc("POST /auth/local/login", s.handleLocalLogin)
		mux.HandleFunc("POST /api/password", s.requireSession(s.handleChangePassword))
	}

	if s.devAuth {
		mux.HandleFunc("POST /auth/dev/login", s.handleDevLogin)
	}
}

// requireSession rejects unauthenticated requests.
type sessionHandler func(w http.ResponseWriter, r *http.Request, accountID uuid.UUID)

func (s *Service) requireSession(next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, err := s.sessions.AccountFrom(r)
		if err != nil {
			// 401 with a distinguishable body, so the client knows to refresh
			// rather than to show a login screen for what is only an expired
			// access token.
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r, accountID)
	}
}

// --- authentication ---------------------------------------------------------

func (s *Service) handleProviders(w http.ResponseWriter, _ *http.Request) {
	type providerView struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	}

	out := struct {
		Providers []providerView `json:"providers"`
		DevAuth   bool           `json:"devAuth"`
		LocalAuth bool           `json:"localAuth"`
	}{Providers: []providerView{}, DevAuth: s.devAuth, LocalAuth: s.localAuth}

	for _, p := range s.providers.List() {
		out.Providers = append(out.Providers, providerView{ID: p.ID, DisplayName: p.DisplayName})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.providers.Get(r.URL.Query().Get("provider"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}

	url, err := StartLogin(r.Context(), s.sessions, provider, SafeReturnTo(r.URL.Query().Get("return")))
	if err != nil {
		s.log.Error("starting login", "provider", provider.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not start login")
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

func (s *Service) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if errCode := q.Get("error"); errCode != "" {
		// The provider declined. Surface its reason rather than a generic
		// failure, since "access_denied" and "invalid_client" call for
		// completely different responses from whoever is debugging.
		s.log.Warn("provider returned an error", "error", errCode, "description", q.Get("error_description"))
		writeError(w, http.StatusUnauthorized, "sign-in was declined: "+errCode)
		return
	}

	providerID, verifier, returnTo, ok, err := s.sessions.TakeState(r.Context(), q.Get("state"))
	if err != nil {
		s.log.Error("reading login state", "err", err)
		writeError(w, http.StatusInternalServerError, "could not complete sign-in")
		return
	}
	if !ok {
		// Either a replayed callback, a forged one, or a login left so long
		// the state expired. All three are refused identically.
		writeError(w, http.StatusBadRequest, "this sign-in link has expired or was already used")
		return
	}

	provider, ok := s.providers.Get(providerID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}

	info, err := provider.Exchange(r.Context(), q.Get("code"), verifier)
	if err != nil {
		s.log.Warn("code exchange failed", "provider", providerID, "err", err)
		writeError(w, http.StatusUnauthorized, "sign-in failed")
		return
	}

	if err := s.completeLogin(w, r, providerID, info, returnTo); err != nil {
		return // completeLogin has already written a response
	}
}

// completeLogin enforces the allowlist and establishes a session.
func (s *Service) completeLogin(w http.ResponseWriter, r *http.Request, providerID string, info UserInfo, returnTo string) error {
	ctx := r.Context()

	// The allowlist is checked before the account is created, and again on
	// every subsequent login, so revoking access takes effect rather than
	// applying only to people who never signed in.
	allowed, err := s.store.Allowed(ctx, providerID, info.Subject, info.Email)
	if err != nil {
		s.log.Error("checking allowlist", "err", err)
		writeError(w, http.StatusInternalServerError, "could not complete sign-in")
		return err
	}
	if !allowed {
		s.log.Info("sign-in refused, not on the allowlist",
			"provider", providerID, "subject", info.Subject, "email", info.Email)
		writeError(w, http.StatusForbidden, "this account is not on the allowlist")
		return errors.New("not allowed")
	}

	account, _, err := s.store.UpsertIdentity(ctx, providerID, info.Subject, info.Email)
	if err != nil {
		s.log.Error("upserting identity", "err", err)
		writeError(w, http.StatusInternalServerError, "could not complete sign-in")
		return err
	}

	if account.Banned(nowUTC()) {
		writeError(w, http.StatusForbidden, "this account is suspended")
		return errors.New("banned")
	}

	if err := s.establishSession(w, r, account.ID); err != nil {
		return err
	}

	s.log.Info("player signed in", "account", account.ID, "provider", providerID)
	http.Redirect(w, r, returnTo, http.StatusFound)
	return nil
}

func (s *Service) establishSession(w http.ResponseWriter, r *http.Request, accountID uuid.UUID) error {
	access, expires, err := s.sessions.IssueAccess(accountID)
	if err != nil {
		s.log.Error("issuing access token", "err", err)
		writeError(w, http.StatusInternalServerError, "could not complete sign-in")
		return err
	}
	refresh, err := s.sessions.IssueRefresh(r.Context(), accountID)
	if err != nil {
		s.log.Error("issuing refresh token", "err", err)
		writeError(w, http.StatusInternalServerError, "could not complete sign-in")
		return err
	}
	s.sessions.SetSessionCookies(w, access, refresh, expires)
	return nil
}

func (s *Service) handleRefresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(RefreshCookie)
	if err != nil || c.Value == "" {
		writeError(w, http.StatusUnauthorized, "not signed in")
		return
	}

	accountID, err := s.sessions.RedeemRefresh(r.Context(), c.Value)
	if err != nil {
		// Rotation means a token works exactly once. A failure here is either
		// an expired session or a replayed token, and clearing the cookies is
		// right either way.
		s.sessions.ClearSessionCookies(w)
		writeError(w, http.StatusUnauthorized, "session expired")
		return
	}

	if err := s.establishSession(w, r, accountID); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(RefreshCookie); err == nil && c.Value != "" {
		// Revoke server-side as well as clearing the cookie, or a copied token
		// keeps working until it expires.
		s.sessions.RevokeRefresh(r.Context(), c.Value)
	}
	s.sessions.ClearSessionCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDevLogin signs a player in with no identity check.
//
// The M2 stand-in for configuring a real identity provider locally. It creates
// a genuine account with the provider "dev", so every path after sign-in --
// accounts, characters, leases, checkpoints -- is exercised exactly as it
// would be in production.
func (s *Service) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject string `json:"subject"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	subject := strings.TrimSpace(req.Subject)
	if subject == "" || len(subject) > 64 {
		writeError(w, http.StatusBadRequest, "subject must be 1 to 64 characters")
		return
	}

	ctx := r.Context()

	// Auto-allow, so the allowlist check still runs on the same code path
	// rather than being bypassed. Development auth is already unauthenticated;
	// adding an exception to the allowlist logic as well would mean the
	// production path is the one that never gets exercised.
	if err := s.store.AddAllowlistEntry(ctx, "dev", store.MatchSubject, subject, "development login"); err != nil {
		s.log.Error("allowlisting dev subject", "err", err)
		writeError(w, http.StatusInternalServerError, "could not sign in")
		return
	}

	if err := s.completeLoginJSON(w, r, "dev", UserInfo{Subject: subject, Name: subject}); err != nil {
		return
	}
}

// completeLoginJSON is completeLogin for callers that expect JSON rather than
// a redirect.
func (s *Service) completeLoginJSON(w http.ResponseWriter, r *http.Request, providerID string, info UserInfo) error {
	ctx := r.Context()

	allowed, err := s.store.Allowed(ctx, providerID, info.Subject, info.Email)
	if err != nil || !allowed {
		writeError(w, http.StatusForbidden, "this account is not on the allowlist")
		return errors.New("not allowed")
	}

	account, _, err := s.store.UpsertIdentity(ctx, providerID, info.Subject, info.Email)
	if err != nil {
		s.log.Error("upserting identity", "err", err)
		writeError(w, http.StatusInternalServerError, "could not sign in")
		return err
	}
	if err := s.establishSession(w, r, account.ID); err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, map[string]any{"accountId": account.ID})
	return nil
}

// --- characters -------------------------------------------------------------

func (s *Service) handleMe(w http.ResponseWriter, _ *http.Request, accountID uuid.UUID) {
	writeJSON(w, http.StatusOK, map[string]any{"accountId": accountID})
}

type characterView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Class string `json:"class"`
	Level int    `json:"level"`
	Exp   int64  `json:"exp"`
	Gold  int64  `json:"gold"`
	MapID string `json:"mapId"`
}

func viewOf(c store.Character) characterView {
	return characterView{
		ID: c.ID.String(), Name: c.Name, Class: c.ClassID,
		Level: c.Level, Exp: c.Exp, Gold: c.Gold, MapID: c.MapID,
	}
}

func (s *Service) handleListCharacters(w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	chars, err := s.store.ListCharacters(r.Context(), accountID)
	if err != nil {
		s.log.Error("listing characters", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list characters")
		return
	}

	out := make([]characterView, 0, len(chars))
	for _, c := range chars {
		out = append(out, viewOf(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"characters": out, "max": MaxCharactersPerAccount})
}

func (s *Service) handleCreateCharacter(w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	var req struct {
		Name  string `json:"name"`
		Class string `json:"class"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	name := strings.TrimSpace(req.Name)
	if err := store.ValidateName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	class := req.Class
	if class == "" {
		class = "warrior"
	}

	ctx := r.Context()
	count, err := s.store.CountCharacters(ctx, accountID)
	if err != nil {
		s.log.Error("counting characters", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create character")
		return
	}
	if count >= MaxCharactersPerAccount {
		writeError(w, http.StatusConflict, "this account already has the maximum number of characters")
		return
	}

	c, err := s.store.CreateCharacter(ctx, accountID, name, class, s.defaultMap)
	switch {
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "that name is taken")
		return
	case err != nil:
		s.log.Error("creating character", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create character")
		return
	}

	s.log.Info("character created", "account", accountID, "character", c.ID, "name", c.Name)
	writeJSON(w, http.StatusCreated, viewOf(c))
}

func (s *Service) handleDeleteCharacter(w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad character id")
		return
	}

	switch err := s.store.DeleteCharacter(r.Context(), accountID, id); {
	case errors.Is(err, store.ErrNotFound):
		// Deliberately the same response as "belongs to someone else", so the
		// endpoint cannot be used to discover which character IDs exist.
		writeError(w, http.StatusNotFound, "character not found")
	case err != nil:
		s.log.Error("deleting character", "err", err)
		writeError(w, http.StatusInternalServerError, "could not delete character")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleTicket issues the single-use credential that opens a game socket.
//
// The character is chosen here, over authenticated HTTP, rather than inside
// the socket handshake -- so the socket proves only that a ticket was held,
// and identity never enters the game protocol.
func (s *Service) handleTicket(w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	var req struct {
		CharacterID string `json:"characterId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	characterID, err := uuid.Parse(req.CharacterID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad character id")
		return
	}

	// Loading verifies ownership as part of the query, so a guessed ID cannot
	// produce a ticket for someone else's character.
	c, err := s.store.LoadCharacter(r.Context(), accountID, characterID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}
	if err != nil {
		s.log.Error("loading character for ticket", "err", err)
		writeError(w, http.StatusInternalServerError, "could not issue a ticket")
		return
	}

	ticket, err := s.sessions.IssueTicket(r.Context(), Ticket{
		AccountID:   accountID,
		CharacterID: c.ID,
		Name:        c.Name,
	})
	if err != nil {
		s.log.Error("issuing ticket", "err", err)
		writeError(w, http.StatusInternalServerError, "could not issue a ticket")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":    ticket,
		"expiresIn": int(TicketTTL.Seconds()),
		"character": viewOf(c),
	})
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
