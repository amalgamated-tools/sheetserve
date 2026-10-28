package version

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFormatUptime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"sub-second rounds down to zero", 400 * time.Millisecond, "0s"},
		{"seconds only", 45 * time.Second, "45s"},
		{"minutes drop the day and hour", 12*time.Minute + 5*time.Second, "12m 5s"},
		{"hours keep zero minutes", 2*time.Hour + 3*time.Second, "2h 0m 3s"},
		{"days keep zero hours", 3*24*time.Hour + 7*time.Second, "3d 0h 0m 7s"},
		{"full", 3*24*time.Hour + 4*time.Hour + 12*time.Minute + 5*time.Second, "3d 4h 12m 5s"},
		{"rounds to the nearest second", 90*time.Second + 600*time.Millisecond, "1m 31s"},
		{"negative clamps to zero", -5 * time.Second, "0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, FormatUptime(tc.d))
		})
	}
}

func TestStartedAndUptime(t *testing.T) {
	t.Parallel()
	require.False(t, Started.IsZero(), "Started must be captured at package init")
	require.WithinDuration(t, time.Now(), Started, time.Minute)

	first := Uptime()
	require.Positive(t, first)
	require.GreaterOrEqual(t, Uptime(), first, "uptime must not go backwards")
}
