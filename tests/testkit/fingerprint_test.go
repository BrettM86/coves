package testkit

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"Coves/internal/atproto/embedblobs"
	"Coves/internal/db/migrations"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fingerprintFixtures() (fstest.MapFS, fstest.MapFS, fstest.MapFS) {
	sqlFS := fstest.MapFS{
		"001_users.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (did text);\n")},
		"002_posts.sql": &fstest.MapFile{Data: []byte("CREATE TABLE posts (uri text);\n")},
	}
	goFS := fstest.MapFS{
		"embed.go":             &fstest.MapFile{Data: []byte("package migrations\n")},
		"057_backfill.go":      &fstest.MapFile{Data: []byte("package migrations\n\nfunc backfill() {}\n")},
		"057_backfill_test.go": &fstest.MapFile{Data: []byte("package migrations\n\nfunc TestBackfill() {}\n")},
	}
	extFS := fstest.MapFS{
		"embedblobs.go":      &fstest.MapFile{Data: []byte("package embedblobs\n\nfunc Extract() {}\n")},
		"embedblobs_test.go": &fstest.MapFile{Data: []byte("package embedblobs\n\nfunc TestExtract() {}\n")},
	}
	return sqlFS, goFS, extFS
}

func TestHashTemplateInputs_FingerprintsGoSources(t *testing.T) {
	sqlFS, goFS, extFS := fingerprintFixtures()
	base, err := HashTemplateInputs(sqlFS, goFS, extFS)
	require.NoError(t, err)

	tests := []struct {
		name    string
		changed bool
		modify  func(*fstest.MapFS, *fstest.MapFS, *fstest.MapFS)
	}{
		{
			name:    "edited numbered migration Go source",
			changed: true,
			modify: func(_, goFS, _ *fstest.MapFS) {
				(*goFS)["057_backfill.go"] = &fstest.MapFile{Data: []byte("package migrations\n\nfunc backfill() { migrate() }\n")}
			},
		},
		{
			name:    "edited unnumbered migration Go source",
			changed: true,
			modify: func(_, goFS, _ *fstest.MapFS) {
				(*goFS)["embed.go"] = &fstest.MapFile{Data: []byte("package migrations\n\nvar version = 2\n")}
			},
		},
		{
			name:    "added migration Go source",
			changed: true,
			modify: func(_, goFS, _ *fstest.MapFS) {
				(*goFS)["058_backfill.go"] = &fstest.MapFile{Data: []byte("package migrations\n\nfunc backfillNext() {}\n")}
			},
		},
		{
			name: "GUARD added migration Go test source",
			modify: func(_, goFS, _ *fstest.MapFS) {
				(*goFS)["058_backfill_test.go"] = &fstest.MapFile{Data: []byte("package migrations\n\nfunc TestBackfillNext() {}\n")}
			},
		},
		{
			name: "GUARD edited migration Go test source",
			modify: func(_, goFS, _ *fstest.MapFS) {
				(*goFS)["057_backfill_test.go"] = &fstest.MapFile{Data: []byte("package migrations\n\nfunc TestBackfill() { check() }\n")}
			},
		},
		{
			name:    "edited extractor Go source",
			changed: true,
			modify: func(_, _, extFS *fstest.MapFS) {
				(*extFS)["embedblobs.go"] = &fstest.MapFile{Data: []byte("package embedblobs\n\nfunc Extract() { parse() }\n")}
			},
		},
		{
			name: "GUARD edited extractor Go test source",
			modify: func(_, _, extFS *fstest.MapFS) {
				(*extFS)["embedblobs_test.go"] = &fstest.MapFile{Data: []byte("package embedblobs\n\nfunc TestExtract() { check() }\n")}
			},
		},
		{
			name:    "edited SQL migration",
			changed: true,
			modify: func(sqlFS, _, _ *fstest.MapFS) {
				(*sqlFS)["001_users.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE users (did text NOT NULL);\n")}
			},
		},
		{
			name:    "renamed migration Go source with same content",
			changed: true,
			modify: func(_, goFS, _ *fstest.MapFS) {
				(*goFS)["058_backfill.go"] = (*goFS)["057_backfill.go"]
				delete(*goFS, "057_backfill.go")
			},
		},
		{
			name:    "moved same-name same-content source from migrations to extractor",
			changed: true,
			modify: func(_, goFS, extFS *fstest.MapFS) {
				(*extFS)["embed.go"] = (*goFS)["embed.go"]
				delete(*goFS, "embed.go")
			},
		},
		{
			name: "GUARD MapFS literal order",
			modify: func(sqlFS, goFS, extFS *fstest.MapFS) {
				*sqlFS = fstest.MapFS{
					"002_posts.sql": (*sqlFS)["002_posts.sql"],
					"001_users.sql": (*sqlFS)["001_users.sql"],
				}
				*goFS = fstest.MapFS{
					"057_backfill_test.go": (*goFS)["057_backfill_test.go"],
					"057_backfill.go":      (*goFS)["057_backfill.go"],
					"embed.go":             (*goFS)["embed.go"],
				}
				*extFS = fstest.MapFS{
					"embedblobs_test.go": (*extFS)["embedblobs_test.go"],
					"embedblobs.go":      (*extFS)["embedblobs.go"],
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sqlFS, goFS, extFS := fingerprintFixtures()
			tt.modify(&sqlFS, &goFS, &extFS)
			got, err := HashTemplateInputs(sqlFS, goFS, extFS)
			require.NoError(t, err)
			if tt.changed {
				assert.NotEqual(t, base, got)
			} else {
				assert.Equal(t, base, got)
			}
		})
	}
}

func TestMigrationsHash_CoversEmbeddedGoSources(t *testing.T) {
	got, err := MigrationsHash()
	require.NoError(t, err)
	want, err := HashTemplateInputs(migrations.FS, migrations.GoSourceFS, embedblobs.SourceFS)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	_, err = fs.Stat(migrations.GoSourceFS, "embed.go")
	assert.NoError(t, err)
	_, err = fs.Stat(migrations.GoSourceFS, "062_backfill_content_blob_references.go")
	assert.NoError(t, err)
	_, err = fs.Stat(migrations.GoSourceFS, "063_backfill_user_blob_references.go")
	assert.NoError(t, err)
	_, err = fs.Stat(migrations.GoSourceFS, "064_backfill_community_blob_references.go")
	assert.NoError(t, err)
	_, err = fs.Stat(migrations.GoSourceFS, "065_backfill_nested_blob_references.go")
	assert.NoError(t, err)
	_, err = fs.Stat(embedblobs.SourceFS, "embedblobs.go")
	assert.NoError(t, err)
}
