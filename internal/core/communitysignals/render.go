package communitysignals

import (
	"fmt"
	"sort"
	"strings"
)

// telegramMessageLimit is the Bot API's maximum message length; a rendered
// digest stays strictly below it.
const telegramMessageLimit = 4096

const digestHeader = "Weekly NSFW digest: communities whose share of nsfw-labelled posts rose (last 7 days vs the 28 before)"

// RenderAlert renders the operator text for one flag change event. It names
// the community by handle, never by DID.
func RenderAlert(e Event) string {
	return fmt.Sprintf("Community nsfw flag %s: %s (%s)", changeWord(e.OldNSFW, e.NewNSFW), e.CommunityHandle, originLabel(e.OriginHost))
}

func changeWord(oldNSFW, newNSFW bool) string {
	switch {
	case !oldNSFW && newNSFW:
		return "added"
	case oldNSFW && !newNSFW:
		return "removed"
	default:
		return "unchanged"
	}
}

func originLabel(originHost string) string {
	if originHost == "" {
		return "native"
	}
	return originHost
}

// RenderDigest renders the weekly digest: one community per line by rise
// descending, cut to stay under Telegram's message limit. When rows are left
// out, the last line carries their count.
func RenderDigest(rows []DigestRow) string {
	sorted := append([]DigestRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := sorted[i].RisePoints(), sorted[j].RisePoints()
		if ri != rj {
			return ri > rj
		}
		return sorted[i].CommunityHandle < sorted[j].CommunityHandle
	})

	var b strings.Builder
	b.WriteString(digestHeader)
	shown := 0
	for _, row := range sorted {
		line := digestLine(row)
		size := b.Len() + 1 + len(line)
		if remaining := len(sorted) - shown - 1; remaining > 0 {
			size += 1 + len(leftOutLine(remaining))
		}
		if size >= telegramMessageLimit {
			break
		}
		b.WriteString("\n")
		b.WriteString(line)
		shown++
	}
	if leftOut := len(sorted) - shown; leftOut > 0 {
		b.WriteString("\n")
		b.WriteString(leftOutLine(leftOut))
	}
	return b.String()
}

func digestLine(r DigestRow) string {
	return fmt.Sprintf("%s (%s): %.1f%% -> %.1f%% (+%.1f points, %d posts)",
		r.CommunityHandle, originLabel(r.OriginHost), r.PriorShare*100, r.RecentShare*100, r.RisePoints(), r.RecentPosts)
}

func leftOutLine(n int) string {
	return fmt.Sprintf("%d more communities not shown", n)
}
