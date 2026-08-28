package bus

import "strings"

// matchSubject reports whether a concrete subject matches a pattern.
//
// The rules follow NATS so that the in-process and NATS implementations route
// identically, and a subject that works on one node keeps working when roles
// are split across many:
//
//	"*" matches exactly one token
//	"&gt;" matches one or more remaining tokens, and is only valid last
//
// Matching is on whole tokens: "room.4" does not match pattern "room.42".
func matchSubject(pattern, subject string) bool {
	if pattern == subject {
		return true
	}

	p, s := pattern, subject
	for {
		pTok, pRest, pMore := nextToken(p)
		sTok, sRest, sMore := nextToken(s)

		// "&gt;" swallows every remaining token, but must match at least one.
		if pTok == ">" {
			return sTok != ""
		}

		if pTok == "" || sTok == "" {
			// Both exhausted at the same time is a match; one outlasting the
			// other is not.
			return pTok == "" && sTok == "" && !pMore && !sMore
		}
		if pTok != "*" && pTok != sTok {
			return false
		}

		p, s = pRest, sRest
	}
}

// nextToken splits off the leading dot-separated token.
func nextToken(s string) (tok, rest string, more bool) {
	if s == "" {
		return "", "", false
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", false
}

// validSubject reports whether s is usable as a publish subject: non-empty,
// no empty tokens, and no wildcards. Publishing to a wildcard is always a bug,
// and one that silently delivers nothing if not caught here.
func validSubject(s string) bool {
	if s == "" {
		return false
	}
	for {
		tok, rest, more := nextToken(s)
		if tok == "" || tok == "*" || tok == ">" {
			return false
		}
		if !more {
			return true
		}
		s = rest
	}
}
