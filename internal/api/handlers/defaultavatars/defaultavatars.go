// Package defaultavatars serves the built-in default avatar images.
//
// The images are embedded in the binary and published under a versioned path,
// /img/default-avatars/v1/<slug>.webp. A version's files never change and
// moderation never removes them, so responses are cacheable for a year and
// marked immutable.
package defaultavatars

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
)

// version is the only default avatar set this server publishes.
const version = "v1"

const (
	fileExtension         = ".webp"
	immutableCacheControl = "public, max-age=31536000, immutable"
)

//go:embed v1/*.webp
var embeddedImages embed.FS

// avatar is one embedded image and its content-derived ETag.
type avatar struct {
	data []byte
	etag string
}

var avatarsBySlug, sortedSlugs = loadAvatars()

// loadAvatars reads every embedded v1 image once. The files are compiled in,
// so a read failure means a broken build and panics at start-up.
func loadAvatars() (map[string]avatar, []string) {
	entries, err := embeddedImages.ReadDir(version)
	if err != nil {
		panic("defaultavatars: read embedded images: " + err.Error())
	}
	avatars := make(map[string]avatar, len(entries))
	slugs := make([]string, 0, len(entries))
	for _, entry := range entries {
		data, err := embeddedImages.ReadFile(path.Join(version, entry.Name()))
		if err != nil {
			panic("defaultavatars: read embedded image " + entry.Name() + ": " + err.Error())
		}
		digest := sha256.Sum256(data)
		slug := strings.TrimSuffix(entry.Name(), fileExtension)
		avatars[slug] = avatar{data: data, etag: `"` + hex.EncodeToString(digest[:16]) + `"`}
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return avatars, slugs
}

// Slugs returns the v1 default avatar slugs, sorted ascending.
func Slugs() []string {
	return append([]string(nil), sortedSlugs...)
}

// Image returns the embedded v1 bytes for a slug.
func Image(slug string) ([]byte, bool) {
	found, ok := avatarsBySlug[slug]
	return found.data, ok
}

// Handler serves GET and HEAD /img/default-avatars/{version}/{file}.
type Handler struct{}

// NewHandler creates a default avatar handler.
func NewHandler() *Handler { return &Handler{} }

// HandleImage serves one default avatar image. It reads only the version and
// file path parameters; query strings and Origin do not change the response.
func (h *Handler) HandleImage(w http.ResponseWriter, r *http.Request) {
	file := chi.URLParam(r, "file")
	slug, hasExtension := strings.CutSuffix(file, fileExtension)
	found, known := avatarsBySlug[slug]
	if chi.URLParam(r, "version") != version || !hasExtension || !known {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("ETag", found.etag)
	w.Header().Set("Cache-Control", immutableCacheControl)

	if etagMatches(r.Header.Get("If-None-Match"), found.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(found.data)
	}
}

// etagMatches reports whether an If-None-Match header names etag, either
// directly, as a weak validator, or through "*".
func etagMatches(ifNoneMatch, etag string) bool {
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
