package store

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

// A bucket allows at most 90% of its quota in any minute, its burst included.
func TestBucketStaysUnderQuota(t *testing.T) {
	for _, perMinute := range []int{60, 300} {
		b := bucket(perMinute)
		start, allowed := time.Now(), 0
		for i := range 6000 { // every 10ms for a minute
			if b.AllowN(start.Add(time.Duration(i)*10*time.Millisecond), 1) {
				allowed++
			}
		}
		if limit := perMinute * 9 / 10; allowed > limit || allowed < limit*8/10 {
			t.Errorf("%d/min: allowed %d in a minute, want at most %d and close to it", perMinute, allowed, limit)
		}
	}
	if bucket(0) != nil {
		t.Error("a quota of 0 should mean no limit")
	}
}

// fakeGoogle refuses calls with Google's 429 until refusals runs out.
type fakeGoogle struct {
	Google
	refusals, calls int
}

func (f *fakeGoogle) Read(context.Context, string, string) ([][]any, error) {
	f.calls++
	if f.refusals > 0 {
		f.refusals--
		return nil, &googleapi.Error{Code: http.StatusTooManyRequests}
	}
	return [][]any{{"id"}}, nil
}

func (f *fakeGoogle) Append(context.Context, string, string, []any) error {
	f.calls++
	return nil
}

func TestLimit(t *testing.T) {
	old := backoff
	backoff = time.Millisecond
	t.Cleanup(func() { backoff = old })
	ctx := context.Background()
	refused := func(err error) bool {
		var rl *RateLimitError
		return errors.As(err, &rl) && rl.RetryAfter > maxWait
	}

	// A burst of 1 refills in 7.5s, longer than maxWait, so the second call is refused, not waited.
	g := &fakeGoogle{}
	s := Limit(g, NewLimiter(Quota{}, Quota{ReadsPerMinute: 10}, Quota{}), 1)
	if _, err := s.Read(ctx, "SS", "T"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, "SS", "T"); !refused(err) || g.calls != 1 {
		t.Errorf("over the user's quota: %v, %d calls", err, g.calls)
	}
	if err := s.Append(ctx, "SS", "T", nil); err != nil || g.calls != 2 {
		t.Errorf("writes have their own bucket: %v", err)
	}
	if _, err := Limit(g, NewLimiter(Quota{}, Quota{ReadsPerMinute: 10}, Quota{}), 2).Read(ctx, "SS", "T"); err != nil {
		t.Errorf("another user has their own bucket: %v", err)
	}

	// Each API key has its own bucket, and the project's is shared by everyone.
	l := NewLimiter(Quota{ReadsPerMinute: 100}, Quota{}, Quota{ReadsPerMinute: 10})
	keyA, keyB := WithCaller(ctx, "a"), WithCaller(ctx, "b")
	s = Limit(g, l, 1)
	if _, err := s.Read(keyA, "SS", "T"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(keyA, "SS", "T"); !refused(err) {
		t.Errorf("over key a's quota: %v", err)
	}
	if _, err := s.Read(keyB, "SS", "T"); err != nil {
		t.Errorf("key b: %v", err)
	}
	if _, err := s.Read(ctx, "SS", "T"); err != nil {
		t.Errorf("a call without a key: %v", err)
	}

	// Google's own 429 is tried again after a backoff, up to retries times.
	g = &fakeGoogle{refusals: 2}
	if _, err := Limit(g, NewLimiter(Quota{}, Quota{}, Quota{}), 1).Read(ctx, "SS", "T"); err != nil || g.calls != 3 {
		t.Errorf("after two 429s: %v, %d calls", err, g.calls)
	}
	g = &fakeGoogle{refusals: 5}
	var gerr *googleapi.Error
	if _, err := Limit(g, NewLimiter(Quota{}, Quota{}, Quota{}), 1).Read(ctx, "SS", "T"); !errors.As(err, &gerr) || g.calls != 1+retries {
		t.Errorf("after too many 429s: %v, %d calls", err, g.calls)
	}
}
