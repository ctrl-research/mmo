package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	ok, upgrade, err := VerifyPassword(hash, password)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Error("the correct password did not verify")
	}
	if upgrade {
		t.Error("a freshly made hash was reported as needing an upgrade")
	}

	ok, _, err = VerifyPassword(hash, "wrong horse battery staple")
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Error("an incorrect password verified")
	}
}

// Every hash carries its own random salt, so two people choosing the same
// password do not share a digest -- which is what makes a stolen database
// resistant to a single precomputed table.
func TestSamePasswordHashesDifferently(t *testing.T) {
	const password = "a shared and unimaginative password"

	first, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if first == second {
		t.Fatal("identical passwords produced identical hashes; the salt is not random")
	}

	// Both must still verify.
	for i, h := range []string{first, second} {
		if ok, _, _ := VerifyPassword(h, password); !ok {
			t.Errorf("hash %d did not verify", i)
		}
	}
}

func TestHashIsPHCFormatted(t *testing.T) {
	hash, err := HashPassword("a perfectly ordinary password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	// The format carries the algorithm and its parameters, which is what lets
	// them be raised later without invalidating existing hashes.
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=") {
		t.Errorf("hash is not PHC-formatted argon2id: %q", hash)
	}
	if strings.Count(hash, "$") != 5 {
		t.Errorf("hash has the wrong number of fields: %q", hash)
	}
}

// A hash made with weaker parameters must still verify, and be flagged for
// upgrade -- otherwise raising the parameters locks everyone out.
func TestWeakerParametersVerifyAndAreFlagged(t *testing.T) {
	const password = "an older and weaker stored password"

	// A hash as an earlier, cheaper configuration would have produced.
	salt := []byte("0123456789abcdef")
	weak := computeHash(password, salt, 1, 8*1024, 1)
	encoded := "$argon2id$v=19$m=8192,t=1,p=1$" +
		b64(salt) + "$" + b64(weak)

	ok, upgrade, err := VerifyPassword(encoded, password)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("a hash made with weaker parameters did not verify; raising them would lock everyone out")
	}
	if !upgrade {
		t.Error("a weaker hash was not flagged for upgrade")
	}
}

func TestMalformedHashesAreRejected(t *testing.T) {
	good, _ := HashPassword("a valid password for mangling")

	cases := map[string]string{
		"empty":           "",
		"not a hash":      "hunter2",
		"wrong algorithm": strings.Replace(good, "argon2id", "argon2i", 1),
		"missing fields":  "$argon2id$v=19$m=19456,t=2,p=1$onlysalt",
		"bad base64 salt": "$argon2id$v=19$m=19456,t=2,p=1$!!!$" + b64([]byte("digest")),
		"zero memory":     strings.Replace(good, "m=19456", "m=0", 1),
		"unknown version": strings.Replace(good, "v=19", "v=16", 1),
		"garbage params":  "$argon2id$v=19$nonsense$" + b64([]byte("salt")) + "$" + b64([]byte("digest")),
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, _, err := VerifyPassword(encoded, "anything")
			if err == nil {
				t.Errorf("a malformed hash was accepted without error (ok=%v)", ok)
			}
			if ok {
				t.Error("a malformed hash verified")
			}
		})
	}
}

func TestPasswordLengthRules(t *testing.T) {
	if err := ValidatePassword(strings.Repeat("a", MinPasswordLen-1)); err != ErrPasswordTooShort {
		t.Errorf("short password returned %v, want ErrPasswordTooShort", err)
	}
	if err := ValidatePassword(strings.Repeat("a", MinPasswordLen)); err != nil {
		t.Errorf("a password at the minimum length was rejected: %v", err)
	}
	// The upper bound exists because Argon2 will happily hash a megabyte,
	// which is a cheap way to make the server do expensive work.
	if err := ValidatePassword(strings.Repeat("a", MaxPasswordLen+1)); err != ErrPasswordTooLong {
		t.Errorf("overlong password returned %v, want ErrPasswordTooLong", err)
	}
}

// Length is counted in runes, so a passphrase in a non-Latin script is not
// held to a stricter standard than an English one.
func TestPasswordLengthCountsRunesNotBytes(t *testing.T) {
	// Ten characters, but thirty bytes in UTF-8.
	if err := ValidatePassword(strings.Repeat("あ", 10)); err != nil {
		t.Errorf("a ten-character passphrase was rejected: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("あ", 9)); err == nil {
		t.Error("a nine-character passphrase was accepted")
	}
}

// No composition rules: they measurably reduce entropy in practice by pushing
// people toward "Password1!".
func TestPasswordHasNoCompositionRules(t *testing.T) {
	for _, p := range []string{
		"all lower case words here",
		"aaaaaaaaaaaaaaaaaaaa",
		"1234567890123456",
		"correct horse battery staple",
	} {
		if err := ValidatePassword(p); err != nil {
			t.Errorf("ValidatePassword(%q) = %v, want nil", p, err)
		}
	}
}

func TestValidateUsername(t *testing.T) {
	valid := []string{"abc", "jonathan", "Player_1", "a-b.c", "x2"}
	for _, n := range valid {
		if len(n) < MinUsernameLen {
			continue
		}
		if err := ValidateUsername(n); err != nil {
			t.Errorf("ValidateUsername(%q) = %v, want nil", n, err)
		}
	}

	invalid := map[string]string{
		"ab":                    "too short",
		"":                      "empty",
		strings.Repeat("a", 33): "too long",
		"1abc":                  "starts with a digit",
		"_abc":                  "starts with a separator",
		"has space":             "contains a space",
		"semi;colon":            "contains punctuation",
		"trailing_":             "ends with a separator",
		"trailing-":             "ends with a separator",
		"trailing.":             "ends with a separator",
		"emoji\U0001F600name":   "contains an emoji",
	}
	for n, why := range invalid {
		if err := ValidateUsername(n); err == nil {
			t.Errorf("ValidateUsername(%q) accepted a username that is %s", n, why)
		}
	}
}

// Usernames are case-insensitive, so nobody can register "Admin" alongside
// "admin".
func TestNormaliseUsername(t *testing.T) {
	tests := map[string]string{
		"Jonathan":   "jonathan",
		"ADMIN":      "admin",
		"  spaced  ": "spaced",
		"MiXeD":      "mixed",
	}
	for in, want := range tests {
		if got := NormaliseUsername(in); got != want {
			t.Errorf("NormaliseUsername(%q) = %q, want %q", in, got, want)
		}
	}
}

// DummyVerify exists so an unknown username costs the same work as a wrong
// password. It must not panic and must actually do the work.
func TestDummyVerifyRuns(t *testing.T) {
	DummyVerify("some password")
}

func b64(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		for j := 0; j < n+1; j++ {
			out.WriteByte(alphabet[(v>>uint(18-6*j))&0x3F])
		}
	}
	return out.String()
}

func BenchmarkHashPassword(b *testing.B) {
	for i := 0; i < b.N; i++ {
		HashPassword("a representative password for benchmarking")
	}
}
