package feedgen

import (
	"fmt"
	"time"
)

// Builtin is a feed generator the AppView runs itself and declares with a
// record in the instance account's repo. A posts builtin has exactly one of
// Generator and HydratedGenerator; a comments builtin has CommentGenerator.
type Builtin struct {
	RKey, DisplayName, Description string
	Kind                           Kind
	Scopes                         []string
	AcceptsInteractions            bool
	Generator                      PostGenerator
	HydratedGenerator              HydratedPostGenerator
	CommentGenerator               CommentGenerator
}

// HotBuiltin is the instance's Hot feed for the home and community scopes.
func HotBuiltin(gen PostGenerator) Builtin {
	return Builtin{
		RKey:        "hot",
		DisplayName: "Hot",
		Description: "Popular posts ranked by score, with newer posts favored over older ones",
		Kind:        KindPosts,
		Scopes:      []string{ScopeHome, ScopeCommunity},
		Generator:   gen,
	}
}

// NewBuiltin is the instance's New feed for the home, community and all scopes.
func NewBuiltin(gen PostGenerator) Builtin {
	return Builtin{
		RKey:        "new",
		DisplayName: "New",
		Description: "The newest posts first",
		Kind:        KindPosts,
		Scopes:      []string{ScopeHome, ScopeCommunity, ScopeAll},
		Generator:   gen,
	}
}

// TopBuiltin is the instance's Top feed for the home, community and all scopes. It
// ranks within the request's timeframe.
func TopBuiltin(gen PostGenerator) Builtin {
	return Builtin{
		RKey:        "top",
		DisplayName: "Top",
		Description: "The highest-scoring posts within the chosen timeframe",
		Kind:        KindPosts,
		Scopes:      []string{ScopeHome, ScopeCommunity, ScopeAll},
		Generator:   gen,
	}
}

// DiscoverHotBuiltin is the instance's Discover feed for the all scope. Its
// generator hydrates its own pages.
func DiscoverHotBuiltin(gen HydratedPostGenerator) Builtin {
	return Builtin{
		RKey:              "discover-hot",
		DisplayName:       "Discover",
		Description:       "Popular posts from every community, ranked by score with newer posts favored and communities mixed",
		Kind:              KindPosts,
		Scopes:            []string{ScopeAll},
		HydratedGenerator: gen,
	}
}

// CommentsHotBuiltin is the instance's Hot comment order for threads.
func CommentsHotBuiltin(gen CommentGenerator) Builtin {
	return Builtin{
		RKey:             "comments-hot",
		DisplayName:      "Hot",
		Description:      "Popular comments ranked by score, with newer comments favored over older ones",
		Kind:             KindComments,
		Scopes:           []string{ScopeThread},
		CommentGenerator: gen,
	}
}

// CommentsTopBuiltin is the instance's Top comment order for threads. It ranks
// the top level within the request's timeframe.
func CommentsTopBuiltin(gen CommentGenerator) Builtin {
	return Builtin{
		RKey:             "comments-top",
		DisplayName:      "Top",
		Description:      "The highest-scoring comments within the chosen timeframe",
		Kind:             KindComments,
		Scopes:           []string{ScopeThread},
		CommentGenerator: gen,
	}
}

// CommentsNewBuiltin is the instance's New comment order for threads.
func CommentsNewBuiltin(gen CommentGenerator) Builtin {
	return Builtin{
		RKey:             "comments-new",
		DisplayName:      "New",
		Description:      "The newest comments first",
		Kind:             KindComments,
		Scopes:           []string{ScopeThread},
		CommentGenerator: gen,
	}
}

// instanceDefaultRKeys are the fixed instance default feeds per scope. A scope
// whose default is not registered is unsupported.
var instanceDefaultRKeys = map[string]string{
	ScopeHome:      "hot",
	ScopeCommunity: "hot",
	ScopeAll:       "discover-hot",
}

// Registry holds the built-in generators, addressed by at-uris under the
// instance DID.
type Registry struct {
	instanceDID string
	builtins    []Builtin
	byURI       map[string]int
}

// NewRegistry registers builtins under instanceDID. A duplicate rkey is a
// programming error and panics.
func NewRegistry(instanceDID string, builtins ...Builtin) *Registry {
	r := &Registry{instanceDID: instanceDID, byURI: make(map[string]int, len(builtins))}
	for _, b := range builtins {
		uri := r.URI(b.RKey)
		if _, exists := r.byURI[uri]; exists {
			panic(fmt.Sprintf("feedgen: builtin rkey %q registered twice", b.RKey))
		}
		r.byURI[uri] = len(r.builtins)
		r.builtins = append(r.builtins, b)
	}
	return r
}

// URI is the at-uri of the builtin generator record with rkey.
func (r *Registry) URI(rkey string) string {
	return "at://" + r.instanceDID + "/" + GeneratorCollection + "/" + rkey
}

// DID is the instance DID the registry was built with.
func (r *Registry) DID() string {
	return r.instanceDID
}

// GeneratorView is a registered builtin as feed pickers see it.
type GeneratorView struct {
	URI, DID, DisplayName, Description string
	Kind                               Kind
	Scopes                             []string
	AcceptsInteractions                bool
}

// View is the picker view of the builtin registered at uri.
func (r *Registry) View(uri string) (GeneratorView, bool) {
	b, ok := r.Lookup(uri)
	if !ok {
		return GeneratorView{}, false
	}
	return GeneratorView{
		URI:                 uri,
		DID:                 r.instanceDID,
		DisplayName:         b.DisplayName,
		Description:         b.Description,
		Kind:                b.Kind,
		Scopes:              append([]string(nil), b.Scopes...),
		AcceptsInteractions: b.AcceptsInteractions,
	}, true
}

// Lookup finds the builtin registered at uri.
func (r *Registry) Lookup(uri string) (Builtin, bool) {
	i, ok := r.byURI[uri]
	if !ok {
		return Builtin{}, false
	}
	return r.builtins[i], true
}

// Builtins returns the registered builtins in registration order.
func (r *Registry) Builtins() []Builtin {
	return append([]Builtin(nil), r.builtins...)
}

// Record is the social.coves.feed.generator record declaring b.
func (r *Registry) Record(b Builtin, createdAt time.Time) map[string]any {
	record := map[string]any{
		"$type":               GeneratorCollection,
		"did":                 r.instanceDID,
		"displayName":         b.DisplayName,
		"kind":                string(b.Kind),
		"scopes":              append([]string(nil), b.Scopes...),
		"acceptsInteractions": b.AcceptsInteractions,
		"createdAt":           createdAt.UTC().Format(time.RFC3339),
	}
	if b.Description != "" {
		record["description"] = b.Description
	}
	return record
}

// defaultURI is the instance default feed for a scope, if one is registered.
func (r *Registry) defaultURI(scope string) (string, bool) {
	rkey, ok := instanceDefaultRKeys[scope]
	if !ok {
		return "", false
	}
	uri := r.URI(rkey)
	if _, registered := r.byURI[uri]; !registered {
		return "", false
	}
	return uri, true
}
