package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageCountsPerUTCDate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	day1 := time.Date(2026, 3, 4, 23, 59, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Minute)
	if n, err := UsageToday(ctx, s.DB(), UsageEmbed, day1); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	for _, n := range []int{5, 3, 0} {
		if err := AddUsage(ctx, s.DB(), UsageEmbed, n, day1); err != nil {
			t.Fatal(err)
		}
	}
	AddUsage(ctx, s.DB(), UsageSummarize, 2, day1)
	AddUsage(ctx, s.DB(), UsageEmbed, 1, day2)
	for _, c := range []struct {
		kind string
		at   time.Time
		want int
	}{{UsageEmbed, day1, 8}, {UsageEmbed, day2, 1}, {UsageSummarize, day1, 2}, {UsageSummarize, day2, 0}} {
		if n, _ := UsageToday(ctx, s.DB(), c.kind, c.at); n != c.want {
			t.Errorf("%s %s = %d, want %d", c.kind, c.at, n, c.want)
		}
	}
	// a non-UTC clock lands on its UTC date
	local := time.FixedZone("x", -8*3600)
	if n, _ := UsageToday(ctx, s.DB(), UsageEmbed, day2.In(local)); n != 1 {
		t.Fatal(n)
	}
}
