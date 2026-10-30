package store

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/oauth2"
)

func TestAccessLost(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&oauth2.RetrieveError{ErrorCode: "invalid_grant"}, true},
		// As the Google client wraps it: the code unparsed, in a url.Error around an auth.Error.
		{fmt.Errorf("get: %w", fmt.Errorf("auth: %w", &oauth2.RetrieveError{Body: []byte(`{"error": "invalid_grant"}`)})), true},
		{&oauth2.RetrieveError{ErrorCode: "invalid_client"}, false},
		{errors.New("invalid_grant"), false},
		{nil, false},
	} {
		if got := AccessLost(tc.err); got != tc.want {
			t.Errorf("AccessLost(%v) = %v", tc.err, got)
		}
	}
}
