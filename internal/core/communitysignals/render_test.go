package communitysignals_test

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"Coves/internal/core/communitysignals"
)

func TestRenderAlert(t *testing.T) {
	const did = "did:plc:alertcommunity0001"
	const handle = "c-alerts.example"
	cases := []struct {
		name             string
		origin           string
		oldNSFW, newNSFW bool
		wantOrigin       string
		wantChange       string
	}{
		{name: "added native", origin: "", oldNSFW: false, newNSFW: true, wantOrigin: "native", wantChange: "added"},
		{name: "added bridged", origin: "lemmy.world", oldNSFW: false, newNSFW: true, wantOrigin: "lemmy.world", wantChange: "added"},
		{name: "removed native", origin: "", oldNSFW: true, newNSFW: false, wantOrigin: "native", wantChange: "removed"},
		{name: "removed bridged", origin: "lemmy.world", oldNSFW: true, newNSFW: false, wantOrigin: "lemmy.world", wantChange: "removed"},
		{name: "unchanged bridged", origin: "lemmy.world", oldNSFW: true, newNSFW: true, wantOrigin: "lemmy.world", wantChange: "unchanged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := communitysignals.RenderAlert(communitysignals.Event{
				ID:              1,
				CommunityDID:    did,
				CommunityHandle: handle,
				OriginHost:      tc.origin,
				OldNSFW:         tc.oldNSFW,
				NewNSFW:         tc.newNSFW,
			})
			if !strings.Contains(got, handle) {
				t.Errorf("alert %q does not contain handle %q", got, handle)
			}
			if !strings.Contains(got, tc.wantOrigin) {
				t.Errorf("alert %q does not contain origin %q", got, tc.wantOrigin)
			}
			if !strings.Contains(got, tc.wantChange) {
				t.Errorf("alert %q does not contain change word %q", got, tc.wantChange)
			}
			if strings.Contains(got, did) {
				t.Errorf("alert %q contains the community DID", got)
			}
		})
	}
}

// digestHandle returns a fixed-width handle so no handle is a substring of another.
func digestHandle(i int) string { return fmt.Sprintf("c-%04d.example", i) }

// shownIndexes returns the fixture indexes whose handle appears in out, in
// order of first appearance, and fails on any handle appearing more than once.
func shownIndexes(t *testing.T, out string, n int) []int {
	t.Helper()
	type hit struct{ index, pos int }
	var hits []hit
	for i := 0; i < n; i++ {
		h := digestHandle(i)
		switch c := strings.Count(out, h); {
		case c == 1:
			hits = append(hits, hit{index: i, pos: strings.Index(out, h)})
		case c > 1:
			t.Errorf("handle %s appears %d times", h, c)
		}
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].pos < hits[b].pos })
	indexes := make([]int, len(hits))
	for k, h := range hits {
		indexes[k] = h.index
	}
	return indexes
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[len(lines)-1]
}

func TestRenderDigest(t *testing.T) {
	t.Run("overflow keeps the highest rises and counts the rest", func(t *testing.T) {
		const n = 400
		// Fixture row i has rise (400-i)/4 points, so rise-desc order is index order.
		rows := make([]communitysignals.DigestRow, n)
		for k := 0; k < n; k++ {
			// Fixed permutation: 7919 is coprime with 400, so k -> i is a bijection.
			i := (k * 7919) % n
			rows[k] = communitysignals.DigestRow{
				CommunityDID:    fmt.Sprintf("did:plc:digest%04d", i),
				CommunityHandle: digestHandle(i),
				OriginHost:      "lemmy.world",
				RecentPosts:     10,
				RecentShare:     float64(n-i) / 400,
				PriorShare:      0,
			}
		}

		out := communitysignals.RenderDigest(rows)

		if len(out) >= 4096 {
			t.Fatalf("digest is %d chars, want < 4096", len(out))
		}
		shown := shownIndexes(t, out, n)
		if len(shown) == 0 {
			t.Fatalf("digest shows no fixture handles; output %q", out)
		}
		for k, idx := range shown {
			if idx != k {
				t.Fatalf("handle at position %d is %s, want %s (shown handles must be the rise-desc prefix)", k, digestHandle(idx), digestHandle(k))
			}
		}
		leftOut := n - len(shown)
		if leftOut == 0 {
			t.Fatalf("all %d rows shown; fixture is meant to overflow", n)
		}
		last := lastLine(out)
		for _, idx := range shown {
			if strings.Contains(last, digestHandle(idx)) {
				t.Fatalf("last line %q is a community row, want the left-out count", last)
			}
		}
		if !strings.Contains(last, strconv.Itoa(leftOut)) {
			t.Errorf("last line %q does not contain left-out count %d", last, leftOut)
		}
	})

	t.Run("rows that fit all appear in rise order with no left-out line", func(t *testing.T) {
		rows := []communitysignals.DigestRow{
			{CommunityDID: "did:plc:digest0001", CommunityHandle: digestHandle(1), OriginHost: "", RecentPosts: 12, RecentShare: 0.5, PriorShare: 0.2},
			{CommunityDID: "did:plc:digest0002", CommunityHandle: digestHandle(2), OriginHost: "lemmy.world", RecentPosts: 10, RecentShare: 0.4, PriorShare: 0.15},
			{CommunityDID: "did:plc:digest0000", CommunityHandle: digestHandle(0), OriginHost: "lemmy.world", RecentPosts: 20, RecentShare: 0.9, PriorShare: 0.1},
		}
		// Rises: c-0000 80, c-0001 30, c-0002 25.
		out := communitysignals.RenderDigest(rows)

		shown := shownIndexes(t, out, 3)
		if want := []int{0, 1, 2}; fmt.Sprint(shown) != fmt.Sprint(want) {
			t.Fatalf("shown handle indexes %v, want %v; output %q", shown, want, out)
		}
		// One row per line: the final line is the lowest-rise row, not a left-out line.
		if last := lastLine(out); !strings.Contains(last, digestHandle(2)) {
			t.Errorf("last line %q is not the last row %s; a left-out line was added", last, digestHandle(2))
		}
	})
}

// TestRenderDigestLimitBoundary sweeps the first row's handle padding across a
// whole line-length period, for a few row widths and row counts around the
// cut, so some cases put the rendered size exactly at Telegram's limit and
// some need the left-out-count reservation to stay under it.
func TestRenderDigestLimitBoundary(t *testing.T) {
	const limit = 4096
	const rowSuffix = " (lemmy.world): 0.0% -> 50.0% (+50.0 points, 10 posts)"
	tail := func(leftOut int) int {
		if leftOut == 0 {
			return 0
		}
		return len("\n" + fmt.Sprintf("%d more communities not shown", leftOut))
	}
	exactHits := 0
	for _, pad := range []int{0, 17, 40} {
		lineLen := len(fmt.Sprintf("c-%04d.%s.example", 0, strings.Repeat("x", pad)) + rowSuffix)
		for extra := 0; extra <= lineLen; extra++ {
			// Every row has the same rise, so rows sort by handle: index order.
			// Row 0 alone carries extra padding, still sorting first.
			handle := func(i int) string {
				p := pad
				if i == 0 {
					p += extra
				}
				return fmt.Sprintf("c-%04d.%s.example", i, strings.Repeat("x", p))
			}
			maxRows := limit/(lineLen+1) + 3
			for n := maxRows - 6; n <= maxRows; n++ {
				if n < 1 {
					continue
				}
				rows := make([]communitysignals.DigestRow, n)
				for i := range rows {
					rows[i] = communitysignals.DigestRow{
						CommunityDID:    fmt.Sprintf("did:plc:boundary%04d", i),
						CommunityHandle: handle(i),
						OriginHost:      "lemmy.world",
						RecentPosts:     10,
						RecentShare:     0.5,
						PriorShare:      0,
					}
				}

				out := communitysignals.RenderDigest(rows)
				name := fmt.Sprintf("pad=%d extra=%d rows=%d", pad, extra, n)

				if len(out) >= limit {
					t.Errorf("%s: digest is %d chars, want < %d", name, len(out), limit)
					continue
				}
				lines := strings.Split(out, "\n")
				headerLen := len(lines[0])
				body := lines[1:]
				shown := 0
				for shown < len(body) && shown < n && body[shown] == handle(shown)+rowSuffix {
					shown++
				}
				leftOut := n - shown
				wantLines := shown
				if leftOut > 0 {
					wantLines++
					if want := fmt.Sprintf("%d more communities not shown", leftOut); body[len(body)-1] != want {
						t.Errorf("%s: last line %q, want %q", name, body[len(body)-1], want)
					}
				}
				if len(body) != wantLines {
					t.Errorf("%s: %d lines after the header, want %d whole row lines plus %d left-out line(s); output %q", name, len(body), shown, wantLines-shown, out)
				}

				// The largest k whose rendering stays under the limit is the expected cut.
				size := func(k int) int {
					s := headerLen + k*(lineLen+1) + tail(n-k)
					if k > 0 {
						s += extra
					}
					return s
				}
				wantShown := 0
				for k := 0; k <= n; k++ {
					if size(k) < limit {
						wantShown = k
					}
					if size(k) == limit {
						exactHits++
					}
				}
				if shown != wantShown {
					t.Errorf("%s: %d rows shown, want %d", name, shown, wantShown)
				}
			}
		}
	}
	if exactHits == 0 {
		t.Fatal("no swept case reaches exactly the limit; widen the sweep")
	}
}
