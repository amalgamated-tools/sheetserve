package service

import (
	"slices"
	"testing"
)

func TestCompareMixed(t *testing.T) {
	got := []string{"10", "1x", "", "2"}
	slices.SortStableFunc(got, compare)
	if want := []string{"2", "10", "", "1x"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
