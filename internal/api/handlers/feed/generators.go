package feed

import (
	"fmt"
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/feedgen"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// maxFeedGeneratorURIs is the getFeedGenerators lexicon's maxLength for feeds.
const maxFeedGeneratorURIs = 25

// FeedGeneratorsHandler serves describeFeedGenerator and getFeedGenerators.
type FeedGeneratorsHandler struct {
	registry *feedgen.Registry
}

// NewFeedGeneratorsHandler builds the handler over the builtin registry.
func NewFeedGeneratorsHandler(registry *feedgen.Registry) *FeedGeneratorsHandler {
	return &FeedGeneratorsHandler{registry: registry}
}

type describedFeed struct {
	URI string `json:"uri"`
}

type describeFeedGeneratorOutput struct {
	DID   string          `json:"did"`
	Feeds []describedFeed `json:"feeds"`
}

type generatorView struct {
	URI                 string   `json:"uri"`
	DID                 string   `json:"did"`
	DisplayName         string   `json:"displayName"`
	Description         string   `json:"description,omitempty"`
	Kind                string   `json:"kind"`
	Scopes              []string `json:"scopes"`
	AcceptsInteractions bool     `json:"acceptsInteractions"`
}

type getFeedGeneratorsOutput struct {
	Feeds []generatorView `json:"feeds"`
}

// HandleDescribeFeedGenerator handles GET social.coves.feed.describeFeedGenerator.
func (h *FeedGeneratorsHandler) HandleDescribeFeedGenerator(w http.ResponseWriter, r *http.Request) {
	builtins := h.registry.Builtins()
	output := describeFeedGeneratorOutput{DID: h.registry.DID(), Feeds: make([]describedFeed, 0, len(builtins))}
	for _, b := range builtins {
		output.Feeds = append(output.Feeds, describedFeed{URI: h.registry.URI(b.RKey)})
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}

// HandleGetFeedGenerators handles GET social.coves.feed.getFeedGenerators.
func (h *FeedGeneratorsHandler) HandleGetFeedGenerators(w http.ResponseWriter, r *http.Request) {
	uris := r.URL.Query()["feeds"]
	if len(uris) == 0 {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "feeds is required")
		return
	}
	if len(uris) > maxFeedGeneratorURIs {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			fmt.Sprintf("feeds accepts at most %d at-uris", maxFeedGeneratorURIs))
		return
	}
	for _, uri := range uris {
		if _, err := syntax.ParseATURI(uri); err != nil {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "feeds must be at-uris")
			return
		}
	}
	output := getFeedGeneratorsOutput{Feeds: make([]generatorView, 0, len(uris))}
	for _, uri := range uris {
		view, ok := h.registry.View(uri)
		if !ok {
			continue
		}
		output.Feeds = append(output.Feeds, generatorView{
			URI:                 view.URI,
			DID:                 view.DID,
			DisplayName:         view.DisplayName,
			Description:         view.Description,
			Kind:                string(view.Kind),
			Scopes:              view.Scopes,
			AcceptsInteractions: view.AcceptsInteractions,
		})
	}
	xrpc.WriteJSON(w, http.StatusOK, output)
}
