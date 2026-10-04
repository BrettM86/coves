package tests

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const publishedLexiconPrefix = "internal/atproto/lexicon/social/coves/"

// shellBlocks returns the contents of every Markdown fence explicitly marked as
// shell, keeping publication checks scoped to commands operators can execute.
func shellBlocks(markdown string) []string {
	var blocks []string
	var blockLines []string
	inShellBlock := false
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inShellBlock {
			if trimmed == "```sh" {
				inShellBlock = true
				blockLines = nil
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			blocks = append(blocks, strings.Join(blockLines, "\n"))
			inShellBlock = false
			continue
		}
		blockLines = append(blockLines, line)
	}
	return blocks
}

// joinedShellLines folds backslash-continued shell lines into one command while
// preserving command order across each fenced block.
func joinedShellLines(block string) []string {
	var commands []string
	var current string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		continued := strings.HasSuffix(line, "\\")
		if continued {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
		}
		if line != "" {
			if current == "" {
				current = line
			} else {
				current += " " + line
			}
		}
		if !continued && current != "" {
			commands = append(commands, current)
			current = ""
		}
	}
	if current != "" {
		commands = append(commands, current)
	}
	return commands
}

// lexiconPublishArguments extracts explicit non-flag operands from every goat
// lex publish command in shell fences, in the order an operator would publish.
func lexiconPublishArguments(markdown string) []string {
	var arguments []string
	for _, block := range shellBlocks(markdown) {
		for _, command := range joinedShellLines(block) {
			fields := strings.Fields(command)
			if len(fields) < 3 || fields[0] != "goat" || fields[1] != "lex" || fields[2] != "publish" {
				continue
			}
			for _, field := range fields[3:] {
				if !strings.HasPrefix(field, "-") {
					arguments = append(arguments, field)
				}
			}
		}
	}
	return arguments
}

// lexiconRelativePath normalizes a repository-relative publish operand to the
// path vocabulary used by the reviewed holdback policy.
func lexiconRelativePath(argument string) string {
	normalized := filepath.ToSlash(filepath.Clean(argument))
	return strings.TrimPrefix(normalized, publishedLexiconPrefix)
}

// argumentIndex returns the document-order position of an exact publish path.
func argumentIndex(arguments []string, wanted string) int {
	for index, argument := range arguments {
		if lexiconRelativePath(argument) == wanted {
			return index
		}
	}
	return -1
}

// markdownSection returns one level-two section without allowing matching text
// from unrelated runbook sections to satisfy a scoped documentation contract.
func markdownSection(markdown, heading string) string {
	startMarker := "## " + heading
	start := strings.Index(markdown, startMarker)
	if start == -1 {
		return ""
	}
	section := markdown[start+len(startMarker):]
	if next := strings.Index(section, "\n## "); next != -1 {
		section = section[:next]
	}
	return section
}

// TestLexiconPublishingRunbookSafety pins the executable release procedure so
// a future documentation edit cannot publish held-back governance schemas,
// omit a new public schema, or publish dependants before their definitions.
func TestLexiconPublishingRunbookSafety(t *testing.T) {
	raw, err := os.ReadFile("../docs/LEXICON_PUBLISHING.md")
	require.NoError(t, err, "the publishing safety contract requires the checked-in runbook")
	runbook := string(raw)

	heldBackPaths := []string{
		"moderation/ban.json",
		"moderation/banUser.json",
		"moderation/getBanStatus.json",
		"moderation/listBans.json",
		"moderation/unbanUser.json",
		"moderation/ruleProposal.json",
		"moderation/tribunalVote.json",
		"moderation/vote.json",
		"community/rules.json",
		"community/moderator.json",
	}
	heldBackSet := make(map[string]struct{}, len(heldBackPaths))
	for _, path := range heldBackPaths {
		heldBackSet[path] = struct{}{}
	}

	t.Run("HeldBackFiles", func(t *testing.T) {
		for _, path := range heldBackPaths {
			assert.Truef(t, strings.Contains(runbook, path),
				"the runbook must name held-back file %s verbatim so operators do not infer safety from a broad namespace", path)
		}
	})

	arguments := lexiconPublishArguments(runbook)
	t.Run("ExplicitPublishFiles", func(t *testing.T) {
		assert.NotEmpty(t, arguments,
			"the runbook must contain at least one goat lex publish command or no schema has a reviewed release path")
		for _, argument := range arguments {
			assert.Truef(t, strings.HasSuffix(argument, ".json"),
				"publish operand %q must be an explicit JSON file; directory publication can sweep in held-back schemas", argument)
			assert.NotContainsf(t, argument, "*",
				"publish operand %q must not use a wildcard that can absorb a newly held-back schema", argument)
			_, statErr := os.Stat(filepath.Join("..", filepath.FromSlash(argument)))
			assert.NoErrorf(t, statErr,
				"publish operand %q must resolve to a checked-in schema file from the repository root", argument)
			_, heldBack := heldBackSet[lexiconRelativePath(argument)]
			assert.Falsef(t, heldBack,
				"publish operand %q is held back and must never appear in an executable publish command", argument)
		}

		publishedSet := make(map[string]struct{}, len(arguments))
		for _, argument := range arguments {
			publishedSet[filepath.ToSlash(filepath.Clean(argument))] = struct{}{}
		}
		var missing []string
		walkErr := filepath.WalkDir(filepath.Join("..", filepath.FromSlash(publishedLexiconPrefix)), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".json" {
				return nil
			}
			relativeToLexicons, relErr := filepath.Rel(filepath.Join("..", filepath.FromSlash(publishedLexiconPrefix)), path)
			if relErr != nil {
				return relErr
			}
			policyPath := filepath.ToSlash(relativeToLexicons)
			if _, heldBack := heldBackSet[policyPath]; heldBack {
				return nil
			}
			repositoryPath := publishedLexiconPrefix + policyPath
			if _, published := publishedSet[repositoryPath]; !published {
				missing = append(missing, repositoryPath)
			}
			return nil
		})
		require.NoError(t, walkErr, "the runbook coverage check must walk the complete Coves lexicon tree")
		sort.Strings(missing)
		assert.Empty(t, missing,
			"every non-held-back lexicon JSON file must appear in a publish command; otherwise new schemas can remain unpublished silently")
	})

	t.Run("DependencyOrder", func(t *testing.T) {
		moderationDefinitionsIndex := argumentIndex(arguments, "moderation/defs.json")
		commentDefinitionsIndex := argumentIndex(arguments, "community/comment/defs.json")
		postDefinitionsIndex := argumentIndex(arguments, "community/post/defs.json")
		postGetIndex := argumentIndex(arguments, "community/post/get.json")
		firstModerationEndpointIndex := -1
		for index, argument := range arguments {
			relative := lexiconRelativePath(argument)
			if strings.HasPrefix(relative, "moderation/") && relative != "moderation/defs.json" {
				firstModerationEndpointIndex = index
				break
			}
		}

		assert.NotEqual(t, -1, moderationDefinitionsIndex,
			"moderation/defs.json must be published explicitly before schemas that reference moderationView")
		assert.NotEqual(t, -1, commentDefinitionsIndex,
			"community/comment/defs.json must be published explicitly before moderation endpoints consume comment views")
		assert.NotEqual(t, -1, postDefinitionsIndex,
			"community/post/defs.json must be published explicitly before moderation endpoints consume post views")
		assert.NotEqual(t, -1, postGetIndex,
			"community/post/get.json must be published explicitly; its output union references post.defs#moderatedPost")
		assert.NotEqual(t, -1, firstModerationEndpointIndex,
			"at least one releasable moderation endpoint must follow the shared and content-view definitions")
		if postDefinitionsIndex >= 0 && postGetIndex >= 0 {
			assert.Greater(t, postGetIndex, postDefinitionsIndex,
				"community/post/get.json must publish after post definitions because its output union references post.defs#moderatedPost")
		}
		if moderationDefinitionsIndex >= 0 && commentDefinitionsIndex >= 0 && postDefinitionsIndex >= 0 && firstModerationEndpointIndex >= 0 {
			assert.Less(t, moderationDefinitionsIndex, commentDefinitionsIndex,
				"moderation definitions must publish before comment definitions that reference moderationView")
			assert.Less(t, moderationDefinitionsIndex, postDefinitionsIndex,
				"moderation definitions must publish before post definitions that reference moderationView")
			assert.Less(t, commentDefinitionsIndex, firstModerationEndpointIndex,
				"comment definitions must publish before the first moderation endpoint")
			assert.Less(t, postDefinitionsIndex, firstModerationEndpointIndex,
				"post definitions must publish before the first moderation endpoint")
		}
	})

	t.Run("ReleasePolicyText", func(t *testing.T) {
		lowerRunbook := strings.ToLower(runbook)
		acceptedExceptions := strings.ToLower(markdownSection(runbook, "Accepted evolution-rule exceptions"))
		assert.True(t, strings.Contains(acceptedExceptions, "commentview"),
			"the accepted-exceptions section must identify commentView's reviewed compatibility exception")
		assert.True(t, strings.Contains(acceptedExceptions, "output-loosening"),
			"the accepted-exceptions section must classify commentView as output-loosening rather than silently treating it as additive")
		assert.True(t, strings.Contains(lowerRunbook, "banview") &&
			(strings.Contains(lowerRunbook, "published as it stands") || strings.Contains(lowerRunbook, "as-is")),
			"the runbook must state that banView is published as it stands so operators do not rewrite its existing wire shape")
		assert.True(t, strings.Contains(lowerRunbook, "verb\u2013noun") || strings.Contains(lowerRunbook, "verb-noun"),
			"the runbook must preserve the reviewed verb-noun endpoint naming decision")

		for _, gate := range []string{
			"goat lex lint",
			"goat lex breaking",
			"goat lex diff",
			"dns delegation",
			"dependency closure",
			"live-record",
			"older-decoder",
		} {
			assert.Truef(t, strings.Contains(lowerRunbook, gate),
				"the runbook must name the %s release gate so publication cannot bypass it", gate)
		}
	})
}
