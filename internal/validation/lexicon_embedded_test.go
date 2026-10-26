package validation

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The server validates post records at runtime, and a deployed binary has no
// lexicon directory next to it: the schemas must be compiled in. The embedded
// validator therefore has to give the same verdicts as the directory-loaded
// one, built here from a cwd that holds no schemas at all.
//
// Records are fed the way the post service holds them: JSON decoded into
// map[string]any, so a blob reference arrives as a plain map
// ({"$type":"blob","ref":{"$link":...},...}) and both validators must still
// accept it as a blob.
func TestEmbeddedLexiconValidatorMatchesDirectoryValidator(t *testing.T) {
	schemaPath, err := filepath.Abs("../../internal/atproto/lexicon")
	require.NoError(t, err)
	pathValidator, err := NewLexiconValidator(schemaPath, false)
	require.NoError(t, err)

	t.Chdir(t.TempDir())
	embeddedValidator, err := NewEmbeddedLexiconValidator(false)
	require.NoError(t, err)
	require.NotNil(t, embeddedValidator)

	validators := map[string]*LexiconValidator{
		"path":     pathValidator,
		"embedded": embeddedValidator,
	}

	tests := []struct {
		name      string
		nsid      string
		record    string
		wantValid bool
	}{
		{
			name: "postv2 with images embed carrying a blob, alt and aspectRatio",
			nsid: "social.coves.community.postv2",
			record: `{
				"$type": "social.coves.community.postv2",
				"community": "did:plc:communityexample123",
				"title": "A photo",
				"embed": {
					"$type": "social.coves.embed.images",
					"images": [{
						"image": {
							"$type": "blob",
							"ref": {"$link": "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},
							"mimeType": "image/jpeg",
							"size": 123456
						},
						"alt": "A lighthouse at dusk",
						"aspectRatio": {"width": 4, "height": 3}
					}]
				},
				"createdAt": "2026-10-13T12:00:00.000Z"
			}`,
			wantValid: true,
		},
		{
			name: "postv2 with images[0] missing its image blob",
			nsid: "social.coves.community.postv2",
			record: `{
				"$type": "social.coves.community.postv2",
				"community": "did:plc:communityexample123",
				"title": "A photo",
				"embed": {
					"$type": "social.coves.embed.images",
					"images": [{
						"alt": "A lighthouse at dusk",
						"aspectRatio": {"width": 4, "height": 3}
					}]
				},
				"createdAt": "2026-10-13T12:00:00.000Z"
			}`,
			wantValid: false,
		},
		{
			name: "comment replying to a post",
			nsid: "social.coves.community.comment",
			record: `{
				"$type": "social.coves.community.comment",
				"reply": {
					"root": {
						"uri": "at://did:plc:authorexample123/social.coves.community.postv2/3m2abcdefgh2a",
						"cid": "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
					},
					"parent": {
						"uri": "at://did:plc:authorexample123/social.coves.community.postv2/3m2abcdefgh2a",
						"cid": "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
					}
				},
				"content": "Great shot",
				"createdAt": "2026-10-13T12:05:00.000Z"
			}`,
			wantValid: true,
		},
		{
			name: "comment missing its required reply",
			nsid: "social.coves.community.comment",
			record: `{
				"$type": "social.coves.community.comment",
				"content": "Great shot",
				"createdAt": "2026-10-13T12:05:00.000Z"
			}`,
			wantValid: false,
		},
	}

	for _, tt := range tests {
		for validatorName, validator := range validators {
			t.Run(tt.name+"/"+validatorName, func(t *testing.T) {
				var record map[string]any
				require.NoError(t, json.Unmarshal([]byte(tt.record), &record))

				err := validator.ValidateRecord(record, tt.nsid)
				if tt.wantValid {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}
