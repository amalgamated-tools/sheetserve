package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/time/rate"
	"google.golang.org/api/googleapi"

	"github.com/amalgamated-tools/sheetserve/internal/otel"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
)

// Quota is a Sheets API per-minute quota for reads and for writes. 0 means no limit.
type Quota struct{ ReadsPerMinute, WritesPerMinute int }

// maxWait is how long a call waits for the limiter before it's refused instead.
const maxWait = 2 * time.Second

// retries is how many times a call Google refused with 429 is tried again, after backoff, then
// twice that.
const retries = 2

var backoff = time.Second

// Limiter keeps Google calls under the Sheets API's per-minute quotas, so Google never has to
// refuse one: a read and a write bucket for the whole project, one of each per user, and one of
// each per API key, so one busy key can't spend its user's whole quota. Calls wait for room
// briefly, then are refused with a RateLimitError.
type Limiter struct {
	project         buckets
	perUser, perKey Quota

	mu    sync.Mutex
	users map[int64]buckets
	keys  map[string]buckets
}

type buckets struct{ read, write *rate.Limiter }

// NewLimiter returns a Limiter for a project's quota, each user's and each API key's.
func NewLimiter(project, perUser, perKey Quota) *Limiter {
	return &Limiter{project: newBuckets(project), perUser: perUser, perKey: perKey,
		users: map[int64]buckets{}, keys: map[string]buckets{}}
}

type callerKey struct{}

// WithCaller marks ctx's Google calls as made for API key, identified by its hash, so they count
// against its own quota as well.
func WithCaller(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, callerKey{}, key)
}

func newBuckets(q Quota) buckets { return buckets{bucket(q.ReadsPerMinute), bucket(q.WritesPerMinute)} }

// bucket returns a token bucket that allows at most 90% of perMinute calls in any minute, burst
// included, or nil for no limit.
func bucket(perMinute int) *rate.Limiter {
	if perMinute <= 0 {
		return nil
	}
	burst := max(1, perMinute/10)
	perSecond := math.Max(float64(perMinute)*0.9-float64(burst), 1) / 60
	return rate.NewLimiter(rate.Limit(perSecond), burst)
}

// RateLimitError is a call the Limiter refused because its quota is used up.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("Google Sheets quota reached; retry in %ds", e.Seconds())
}

// Seconds is RetryAfter in whole seconds, rounded up, as a Retry-After header gives it.
func (e *RateLimitError) Seconds() int { return int(math.Ceil(e.RetryAfter.Seconds())) }

// wait takes a token for a call of kind ("read" or "write") by user from the project's bucket, the
// user's, and the API key's on ctx if any, waiting up to maxWait for all of them.
func (l *Limiter) wait(ctx context.Context, user int64, kind string) error {
	var reserved []*rate.Reservation
	delay, scope := time.Duration(0), ""
	for s, b := range l.scopes(ctx, user) {
		lim := b.of(kind)
		if lim == nil {
			continue
		}
		r := lim.Reserve()
		reserved = append(reserved, r)
		if d := r.Delay(); d > delay {
			delay, scope = d, s
		}
	}
	cancel := func() {
		for _, r := range reserved {
			r.Cancel()
		}
	}
	attrs := []attribute.KeyValue{attribute.String(otelkeys.Kind, kind), attribute.String(otelkeys.Scope, scope)}
	if delay == 0 {
		return nil
	}
	if delay > maxWait {
		cancel()
		otel.Incr(ctx, otelkeys.GoogleLimiterRefused, attrs...)
		slog.WarnContext(ctx, "google quota reached", slog.String(otelkeys.Kind, kind), slog.String(otelkeys.Scope, scope),
			slog.Int64(otelkeys.RetryAfterMs, delay.Milliseconds()))
		return &RateLimitError{RetryAfter: delay}
	}
	otel.Incr(ctx, otelkeys.GoogleLimiterWaited, attrs...)
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}

// scopes returns the buckets a call by user counts against: the project's, the user's, and the API
// key's on ctx if any.
func (l *Limiter) scopes(ctx context.Context, user int64) map[string]buckets {
	scopes := map[string]buckets{"project": l.project}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.users[user]; !ok {
		l.users[user] = newBuckets(l.perUser)
	}
	scopes["user"] = l.users[user]
	if key, ok := ctx.Value(callerKey{}).(string); ok && key != "" {
		if _, ok := l.keys[key]; !ok {
			l.keys[key] = newBuckets(l.perKey)
		}
		scopes["key"] = l.keys[key]
	}
	return scopes
}

// of returns the bucket for calls of kind ("read" or "write"), or nil for no limit.
func (b buckets) of(kind string) *rate.Limiter {
	if kind == "write" {
		return b.write
	}
	return b.read
}

// Room is how much of a quota a caller has: of Limit calls a minute, Remaining can be made over
// the next minute, at most a tenth of Limit of them at once, and Reset is when it's back to Limit.
type Room struct {
	Kind             string // "read" or "write"
	Limit, Remaining int
	Reset            time.Duration
}

// Room returns user's room for reads and for writes, made with the API key on ctx if any, from
// whichever of its budgets has the least. A kind with no limit is left out.
func (l *Limiter) Room(ctx context.Context, user int64) []Room {
	scopes := l.scopes(ctx, user)
	now := time.Now()
	rooms := make([]Room, 0, 2)
	for _, kind := range []string{"read", "write"} {
		var tightest *Room
		for _, b := range scopes {
			lim := b.of(kind)
			if lim == nil {
				continue
			}
			perSecond, burst, tokens := float64(lim.Limit()), float64(lim.Burst()), lim.TokensAt(now)
			limit := int(math.Floor(burst + perSecond*60)) // 90% of the quota; see bucket
			r := Room{Kind: kind, Limit: limit,
				Remaining: min(limit, max(0, int(math.Floor(tokens+perSecond*60)))),
				Reset:     time.Duration(math.Ceil(max(0, burst-tokens)/perSecond)) * time.Second}
			if tightest == nil || r.Remaining < tightest.Remaining {
				tightest = &r
			}
		}
		if tightest != nil {
			rooms = append(rooms, *tightest)
		}
	}
	return rooms
}

// Roomer is a Store that can say how much quota its caller has left.
type Roomer interface {
	Room(ctx context.Context) []Room
}

// Google is a Store with every optional ability the Google store has.
type Google interface {
	Store
	Creator
	SerialReader
	CellReader
}

// Limit returns g with each call waiting its turn in l as user, and tried again after a short
// backoff if Google still refuses it with 429. Google refuses a call over quota before running
// it, so trying a write again can't apply it twice.
func Limit(g Google, l *Limiter, user int64) Google {
	return &limited{g, l, user}
}

type limited struct {
	g    Google
	l    *Limiter
	user int64
}

// call runs fn as a call of kind, within the limits.
func (s *limited) call(ctx context.Context, kind string, fn func() error) error {
	for try := 0; ; try++ {
		if err := s.l.wait(ctx, s.user, kind); err != nil {
			return err
		}
		err := fn()
		var gerr *googleapi.Error
		if !errors.As(err, &gerr) || gerr.Code != http.StatusTooManyRequests {
			return err
		}
		otel.Incr(ctx, otelkeys.GoogleRateLimited, attribute.String(otelkeys.Kind, kind))
		if try == retries {
			return err
		}
		// Truncated exponential backoff with jitter, as Google recommends: about 1s, then 2s.
		t := time.NewTimer(time.Duration(1<<try)*backoff + rand.N(backoff/2))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return err
		}
	}
}

func (s *limited) Room(ctx context.Context) []Room { return s.l.Room(ctx, s.user) }

func (s *limited) Read(ctx context.Context, id, tab string) (vals [][]any, err error) {
	err = s.call(ctx, "read", func() error { vals, err = s.g.Read(ctx, id, tab); return err })
	return vals, err
}

func (s *limited) ReadSerial(ctx context.Context, id, tab string, cols []int) (vals [][]any, err error) {
	err = s.call(ctx, "read", func() error { vals, err = s.g.ReadSerial(ctx, id, tab, cols); return err })
	return vals, err
}

func (s *limited) ReadCell(ctx context.Context, id, tab string, row, col int) (v any, err error) {
	err = s.call(ctx, "read", func() error { v, err = s.g.ReadCell(ctx, id, tab, row, col); return err })
	return v, err
}

func (s *limited) Tabs(ctx context.Context, id string) (tabs []string, err error) {
	err = s.call(ctx, "read", func() error { tabs, err = s.g.Tabs(ctx, id); return err })
	return tabs, err
}

func (s *limited) Append(ctx context.Context, id, tab string, row []any) error {
	return s.call(ctx, "write", func() error { return s.g.Append(ctx, id, tab, row) })
}

func (s *limited) SetCells(ctx context.Context, id, tab string, row int, cells map[int]any) error {
	return s.call(ctx, "write", func() error { return s.g.SetCells(ctx, id, tab, row, cells) })
}

// DeleteRow can look up the tab's sheet ID first, a read; it counts as one write.
func (s *limited) DeleteRow(ctx context.Context, id, tab string, row int) error {
	return s.call(ctx, "write", func() error { return s.g.DeleteRow(ctx, id, tab, row) })
}

func (s *limited) Create(ctx context.Context, title string, tabs []Tab) (id string, err error) {
	err = s.call(ctx, "write", func() error { id, err = s.g.Create(ctx, title, tabs); return err })
	return id, err
}
