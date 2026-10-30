package store

import (
	"bytes"
	"errors"

	"golang.org/x/oauth2"
)

// AccessLost reports whether err is Google refusing a user's refresh token because it expired or
// was revoked. Only signing in again fixes it.
func AccessLost(err error) bool {
	re, ok := errors.AsType[*oauth2.RetrieveError](err)
	return ok && (re.ErrorCode == "invalid_grant" || bytes.Contains(re.Body, []byte(`"invalid_grant"`)))
}
