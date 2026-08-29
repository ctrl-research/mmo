package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ctrl-research/mmo/internal/store"
	"github.com/google/uuid"
)

// Local sign-in.
//
// Two properties matter more than the rest of this file, and both are easy to
// lose by accident:
//
//   - An unknown username and a wrong password must be indistinguishable, in
//     both the response and the time taken. Otherwise the endpoint is a
//     username oracle, and password guessing becomes a much smaller problem.
//   - Failures must be throttled per account, not only per address. A
//     distributed attack changes address freely but still has to target one
//     account.

// genericLoginFailure is the only message a failed sign-in ever produces.
//
// Not "no such user", not "wrong password", not "this account is locked" --
// each of those confirms something about which usernames exist.
const genericLoginFailure = "incorrect username or password"

func (s *Service) handleLocalRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	if err := ValidateUsername(req.Username); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := ValidatePassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	normalised := NormaliseUsername(req.Username)

	// The allowlist gates registration exactly as it gates every other
	// provider. An empty allowlist admits nobody, so a fresh server is not
	// open to whoever finds it first.
	allowed, err := s.store.Allowed(ctx, "local", normalised, "")
	if err != nil {
		s.log.Error("checking allowlist", "err", err)
		writeError(w, http.StatusInternalServerError, "could not register")
		return
	}
	if !allowed {
		s.log.Info("registration refused, not on the allowlist", "username", normalised)
		writeError(w, http.StatusForbidden,
			"this username is not on the allowlist; ask the server owner to add it")
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	account, err := s.store.CreateLocalAccount(ctx, req.Username, normalised, hash)
	switch {
	case errors.Is(err, store.ErrUsernameTaken):
		writeError(w, http.StatusConflict, "that username is taken")
		return
	case err != nil:
		s.log.Error("creating local account", "err", err)
		writeError(w, http.StatusInternalServerError, "could not register")
		return
	}

	if err := s.establishSession(w, r, account.ID); err != nil {
		return
	}

	s.log.Info("local account registered", "account", account.ID, "username", normalised)
	writeJSON(w, http.StatusCreated, map[string]any{"accountId": account.ID})
}

func (s *Service) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	// Bound the work before doing any: an oversized password would otherwise
	// make the server hash a megabyte on request.
	if len(req.Password) > MaxPasswordLen*4 {
		writeError(w, http.StatusUnauthorized, genericLoginFailure)
		return
	}

	ctx := r.Context()
	normalised := NormaliseUsername(req.Username)

	// Address-level throttling, on top of the per-account lockout below. This
	// one blunts a broad sweep across many usernames from one source, which a
	// per-account counter would never see.
	if !s.loginLimiter.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts; wait a moment")
		return
	}

	cred, err := s.store.LocalCredentialByUsername(ctx, normalised)
	if errors.Is(err, store.ErrNotFound) {
		// Burn the same work a real verification would, so timing does not
		// reveal that the username is unknown.
		DummyVerify(req.Password)
		writeError(w, http.StatusUnauthorized, genericLoginFailure)
		return
	}
	if err != nil {
		s.log.Error("reading credential", "err", err)
		writeError(w, http.StatusInternalServerError, "could not sign in")
		return
	}

	now := time.Now()
	if cred.Locked(now) {
		// Deliberately the same message as a wrong password: saying "locked"
		// confirms the account exists.
		DummyVerify(req.Password)
		s.log.Warn("sign-in attempt on a locked account", "username", normalised)
		writeError(w, http.StatusUnauthorized, genericLoginFailure)
		return
	}

	ok, needsUpgrade, err := VerifyPassword(cred.PasswordHash, req.Password)
	if err != nil {
		// A malformed stored hash is a data problem, not a credential problem.
		s.log.Error("verifying password", "account", cred.AccountID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not sign in")
		return
	}

	if !ok {
		attempts, err := s.store.RecordLoginFailure(ctx, cred.AccountID)
		if err != nil {
			s.log.Error("recording login failure", "err", err)
		} else if attempts >= store.MaxFailedAttempts {
			s.log.Warn("account locked after repeated failures",
				"username", normalised, "attempts", attempts)
		}
		writeError(w, http.StatusUnauthorized, genericLoginFailure)
		return
	}

	// The allowlist is re-checked on every sign-in, not only at registration,
	// so removing someone actually revokes their access.
	allowed, err := s.store.Allowed(ctx, "local", normalised, "")
	if err != nil {
		s.log.Error("checking allowlist", "err", err)
		writeError(w, http.StatusInternalServerError, "could not sign in")
		return
	}
	if !allowed {
		s.log.Info("sign-in refused, no longer on the allowlist", "username", normalised)
		writeError(w, http.StatusForbidden, "this account is no longer on the allowlist")
		return
	}

	if err := s.store.ClearLoginFailures(ctx, cred.AccountID); err != nil {
		s.log.Error("clearing login failures", "err", err)
	}

	// A successful sign-in is the only moment the plaintext exists, and so the
	// only chance to rehash under stronger parameters.
	if needsUpgrade {
		if hash, err := HashPassword(req.Password); err == nil {
			if err := s.store.UpdatePasswordHash(ctx, cred.AccountID, hash); err != nil {
				s.log.Error("upgrading password hash", "err", err)
			} else {
				s.log.Info("password hash upgraded", "account", cred.AccountID)
			}
		}
	}

	account, _, err := s.store.UpsertIdentity(ctx, "local", normalised, "")
	if err != nil {
		s.log.Error("upserting identity", "err", err)
		writeError(w, http.StatusInternalServerError, "could not sign in")
		return
	}
	if account.Banned(now) {
		writeError(w, http.StatusForbidden, "this account is suspended")
		return
	}

	if err := s.establishSession(w, r, account.ID); err != nil {
		return
	}

	s.loginLimiter.reset(clientIP(r))
	s.log.Info("player signed in", "account", account.ID, "provider", "local")
	writeJSON(w, http.StatusOK, map[string]any{"accountId": account.ID})
}

// handleChangePassword updates a signed-in player's password.
func (s *Service) handleChangePassword(w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	var req struct {
		Current string `json:"currentPassword"`
		New     string `json:"newPassword"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := ValidatePassword(req.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	cred, err := s.store.LocalCredentialForAccount(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "this account does not use a password")
		return
	}
	if err != nil {
		s.log.Error("reading credential", "err", err)
		writeError(w, http.StatusInternalServerError, "could not change password")
		return
	}

	// The current password is required even though the session is already
	// authenticated: a session left open on a shared machine should not be
	// enough to take the account over permanently.
	ok, _, err := VerifyPassword(cred.PasswordHash, req.Current)
	if err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}

	hash, err := HashPassword(req.New)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdatePasswordHash(ctx, accountID, hash); err != nil {
		s.log.Error("updating password", "err", err)
		writeError(w, http.StatusInternalServerError, "could not change password")
		return
	}

	s.log.Info("password changed", "account", accountID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// clientIP extracts the address to rate-limit by.
//
// X-Forwarded-For is honoured only when the server is told it sits behind a
// proxy. Trusting it unconditionally would let anyone defeat rate limiting by
// inventing a header.
func clientIP(r *http.Request) string {
	if r.RemoteAddr == "" {
		return "unknown"
	}
	// Strip the port, so repeated connections from one host share a bucket.
	addr := r.RemoteAddr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}
