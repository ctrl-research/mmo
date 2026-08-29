package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password storage.
//
// Adding local accounts makes this server the custodian of password hashes,
// which the OIDC path deliberately avoided. That is a real obligation and the
// reason this file is careful: a self-hosted game should not require an
// external identity provider, but taking passwords means taking them
// seriously.
//
// Argon2id, because it is memory-hard. bcrypt resists CPU-bound cracking but
// is cheap to parallelise on a GPU; Argon2id's memory cost is what makes a
// stolen database expensive to attack rather than merely slow.

// Argon2id parameters, following OWASP's recommendation.
//
// Memory dominates the cost, and it is also what bounds how many logins can be
// hashed at once -- hence the concurrency limit below. Raising these is safe:
// each stored hash records the parameters it was made with, so old hashes keep
// verifying and are upgraded on the owner's next successful login.
const (
	argonMemoryKiB = 19 * 1024 // 19 MiB
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// maxConcurrentHashes bounds simultaneous password hashing.
//
// Each hash holds 19 MiB for its duration, so an unbounded login flood is a
// memory exhaustion attack that needs no credentials to mount. Queuing instead
// makes it a latency problem, which is survivable.
const maxConcurrentHashes = 8

var hashSlots = make(chan struct{}, maxConcurrentHashes)

// Password rules.
//
// Length is the only requirement. Composition rules -- a digit, a symbol, a
// capital -- measurably reduce entropy in practice by pushing people toward
// "Password1!", and NIST has recommended against them for years. Long
// passphrases are what to encourage.
const (
	MinPasswordLen = 10
	MaxPasswordLen = 256
)

// Password errors.
var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d characters", MaxPasswordLen)
	ErrPasswordInvalid  = errors.New("auth: password is not valid")
	ErrHashMalformed    = errors.New("auth: stored password hash is malformed")
)

// ValidatePassword checks a candidate password.
func ValidatePassword(password string) error {
	// Counted in runes, not bytes, so a passphrase in a non-Latin script is
	// not held to a stricter standard than an English one.
	n := utf8.RuneCountInString(password)
	switch {
	case n < MinPasswordLen:
		return ErrPasswordTooShort
	case n > MaxPasswordLen:
		// An upper bound exists because Argon2 will happily hash a megabyte,
		// which is a cheap way to make the server do expensive work.
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword returns an encoded Argon2id hash.
//
// The result is a PHC-format string carrying the algorithm, its parameters,
// the salt, and the digest, so verification needs nothing else and parameters
// can change without invalidating existing hashes.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generating salt: %w", err)
	}

	digest := computeHash(password, salt, argonTime, argonMemoryKiB, argonThreads)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

// computeHash runs Argon2id under the concurrency limit.
func computeHash(password string, salt []byte, time, memory uint32, threads uint8) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()

	return argon2.IDKey([]byte(password), salt, time, memory, threads, argonKeyLen)
}

// VerifyPassword checks a password against an encoded hash.
//
// It also reports whether the hash was made with weaker parameters than the
// current ones, so a successful login can transparently upgrade it -- the only
// moment the plaintext is available to rehash with.
func VerifyPassword(encoded, password string) (ok bool, needsUpgrade bool, err error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, false, err
	}

	got := computeHash(password, salt, params.time, params.memory, params.threads)

	// Constant-time, so the comparison cannot be turned into an oracle that
	// reveals the stored digest a byte at a time.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}

	weaker := params.memory < argonMemoryKiB ||
		params.time < argonTime ||
		len(want) < argonKeyLen
	return true, weaker, nil
}

// DummyVerify performs a hash comparison that always fails.
//
// Called when no account matches, so an unknown username costs the same time
// as a wrong password. Without it, response timing tells an attacker exactly
// which usernames exist, which turns a password-guessing problem into a much
// smaller one.
func DummyVerify(password string) {
	// A fixed salt is fine: nothing is stored, and the only purpose is to burn
	// the same work a real verification would.
	var salt [argonSaltLen]byte
	computeHash(password, salt[:], argonTime, argonMemoryKiB, argonThreads)
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

// decodeHash parses a PHC-format Argon2id string.
func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, digest
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrHashMalformed
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argonParams{}, nil, nil, ErrHashMalformed
	}
	if version != argon2.Version {
		// A different Argon2 version produces different output for the same
		// input, so verifying against it would silently always fail.
		return argonParams{}, nil, nil, fmt.Errorf("%w: unsupported argon2 version %d", ErrHashMalformed, version)
	}

	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, ErrHashMalformed
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return argonParams{}, nil, nil, ErrHashMalformed
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return argonParams{}, nil, nil, ErrHashMalformed
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(digest) == 0 {
		return argonParams{}, nil, nil, ErrHashMalformed
	}

	return p, salt, digest, nil
}

// Username rules.
const (
	MinUsernameLen = 3
	MaxUsernameLen = 32
)

// ValidateUsername checks a username.
//
// Narrow on purpose. A username is an identifier, shown next to other players'
// names and used to allowlist people before they register; permitting
// lookalike characters invites impersonation, and tightening the rule later
// means renaming real accounts.
func ValidateUsername(name string) error {
	n := utf8.RuneCountInString(name)
	if n < MinUsernameLen || n > MaxUsernameLen {
		return fmt.Errorf("username must be %d to %d characters", MinUsernameLen, MaxUsernameLen)
	}

	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		case (r == '_' || r == '-' || r == '.') && i > 0:
		default:
			if i == 0 {
				return errors.New("username must start with a letter")
			}
			return errors.New("username may contain only letters, digits, and _ - .")
		}
	}

	// A trailing separator reads as a typo and makes two usernames that differ
	// only by punctuation easy to confuse.
	if last := name[len(name)-1]; last == '_' || last == '-' || last == '.' {
		return errors.New("username may not end with _ - or .")
	}
	return nil
}

// NormaliseUsername returns the form used for lookups and as the identity
// subject. Usernames are case-insensitive: nobody should be able to register
// "Admin" alongside "admin".
func NormaliseUsername(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
