package postgres

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestViewerBlockFilters_SurfaceAndParameterContract pins the helper in the
// sub-minute T0 loop; the tagged repository suites prove each SQL splice.
func TestViewerBlockFilters_SurfaceAndParameterContract(t *testing.T) {
	placeholderPattern := regexp.MustCompile(`\$\d+`)

	for _, surface := range []struct {
		name          string
		value         feedSurface
		wantCommunity bool
	}{
		{name: "explicit surface", value: explicitSurface, wantCommunity: false},
		{name: "aggregate surface", value: aggregateSurface, wantCommunity: true},
	} {
		for _, viewerParam := range []int{1, 7} {
			t.Run(fmt.Sprintf("%s viewer parameter %d", surface.name, viewerParam), func(t *testing.T) {
				filter := viewerBlockFilters(viewerParam, surface.value)
				placeholder := fmt.Sprintf("$%d", viewerParam)

				assert.True(t, strings.HasPrefix(filter, "AND "),
					"the fragment must splice directly into an existing WHERE clause")
				assert.Contains(t, filter, "user_blocks")
				assert.Contains(t, filter, placeholder)
				if surface.wantCommunity {
					assert.Contains(t, filter, "community_blocks")
				} else {
					assert.NotContains(t, filter, "community_blocks",
						"an explicit community read must not inherit the aggregate mute")
				}

				matches := placeholderPattern.FindAllString(filter, -1)
				require.NotEmpty(t, matches)
				for _, match := range matches {
					assert.Equal(t, placeholder, match,
						"every predicate must bind the same viewer parameter")
				}
			})
		}
	}
}
