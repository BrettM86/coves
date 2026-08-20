// cmd/reindex-votes/main.go
// Quick tool to reindex votes from PDS to AppView database
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"Coves/internal/core/posts"

	_ "github.com/lib/pq"
)

type ListRecordsResponse struct {
	Records []Record `json:"records"`
	Cursor  string   `json:"cursor"`
}

type Record struct {
	URI   string                 `json:"uri"`
	CID   string                 `json:"cid"`
	Value map[string]interface{} `json:"value"`
}

func main() {
	// Get config from env
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://dev_user:dev_password@localhost:5435/coves_dev?sslmode=disable"
	}
	pdsURL := os.Getenv("PDS_URL")
	if pdsURL == "" {
		pdsURL = "http://localhost:3001"
	}

	log.Printf("Connecting to database...")
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Get all accounts directly from the PDS
	log.Printf("Fetching accounts from PDS (%s)...", pdsURL)
	dids, err := fetchAllAccountsFromPDS(pdsURL)
	if err != nil {
		log.Fatalf("Failed to fetch accounts from PDS: %v", err)
	}
	log.Printf("Found %d accounts on PDS to check for votes", len(dids))

	// Reset vote counts first
	log.Printf("Resetting all vote counts...")
	if _, err := db.ExecContext(ctx, "DELETE FROM votes"); err != nil {
		log.Fatalf("Failed to clear votes table: %v", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE posts SET upvote_count = 0, downvote_count = 0, score = 0"); err != nil {
		log.Fatalf("Failed to reset post vote counts: %v", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE comments SET upvote_count = 0, downvote_count = 0, score = 0"); err != nil {
		log.Fatalf("Failed to reset comment vote counts: %v", err)
	}

	// For each user, fetch their votes from PDS
	totalVotes := 0
	for _, did := range dids {
		votes, err := fetchVotesFromPDS(pdsURL, did)
		if err != nil {
			log.Printf("Warning: failed to fetch votes for %s: %v", did, err)
			continue
		}

		if len(votes) == 0 {
			continue
		}

		log.Printf("Found %d votes for %s", len(votes), did)

		// Index each vote
		for _, vote := range votes {
			if err := indexVote(ctx, db, did, vote); err != nil {
				log.Printf("Warning: failed to index vote %s: %v", vote.URI, err)
				continue
			}
			totalVotes++
		}
	}

	log.Printf("✓ Reindexed %d votes from PDS", totalVotes)
}

// fetchAllAccountsFromPDS queries the PDS sync API to get all repo DIDs
//
// # WHY THIS FETCH IS NOT GUARDED, AND WHAT WOULD CHANGE THAT
//
// pdsURL is os.Getenv("PDS_URL"), defaulting to this instance's own local dev
// PDS — operator configuration, not data. That is the whole test: every guarded
// site in this remediation dials a host that arrived from a DID document, a
// federated record or a database column, and this one dials the host the person
// running the tool typed. There is no attacker in the path to the ADDRESS.
//
// Guarding it would also break the only invocation that needs no environment at
// all, since the default is loopback and loopback is what the guard refuses.
//
// If pdsURL is ever taken from a record, a database row or a flag whose value
// comes from indexed data, this marker is wrong and the call site needs
// oauth.NewSSRFSafeHTTPClient behind a dev gate, like cmd/backfill-profiles.
//
// # THE SEPARATE DEFECT, RECORDED RATHER THAN QUIETLY FIXED
//
// http.Get is http.DefaultClient, whose Timeout is ZERO — and zero in net/http
// means wait forever. Both loops here page until the cursor runs out, so a PDS
// that accepts the connection and then stops answering hangs this tool with no
// deadline of any kind, and an operator has to notice and kill it. That is a
// real weakness and it is not an SSRF one; it is left here deliberately rather
// than changed under a commit about audit tooling.
func fetchAllAccountsFromPDS(pdsURL string) ([]string, error) {
	// Use com.atproto.sync.listRepos to get all repos on this PDS
	var allDIDs []string
	cursor := ""

	for {
		reqURL := fmt.Sprintf("%s/xrpc/com.atproto.sync.listRepos?limit=100", pdsURL)
		if cursor != "" {
			reqURL += "&cursor=" + url.QueryEscape(cursor)
		}

		// coves:allow-bare-client: pdsURL is PDS_URL from the environment, so the host is operator config; the DIDs are query parameters only. NO TIMEOUT — see the doc comment.
		resp, err := http.Get(reqURL)
		if err != nil {
			return nil, fmt.Errorf("HTTP request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		var result struct {
			Repos []struct {
				DID string `json:"did"`
			} `json:"repos"`
			Cursor string `json:"cursor"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}

		for _, repo := range result.Repos {
			allDIDs = append(allDIDs, repo.DID)
		}

		if result.Cursor == "" {
			break
		}
		cursor = result.Cursor
	}

	return allDIDs, nil
}

// fetchVotesFromPDS reads one repo's vote records. Same host, same reasoning,
// same separate defect as fetchAllAccountsFromPDS — see its doc comment.
//
// `did` comes from the PDS's own listRepos answer and is url.QueryEscape'd into
// a QUERY PARAMETER. It never reaches the host, so it cannot redirect this
// request anywhere: the address is still PDS_URL and nothing else.
func fetchVotesFromPDS(pdsURL, did string) ([]Record, error) {
	var allRecords []Record
	cursor := ""
	collection := "social.coves.feed.vote"

	for {
		reqURL := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=%s&limit=100",
			pdsURL, url.QueryEscape(did), url.QueryEscape(collection))
		if cursor != "" {
			reqURL += "&cursor=" + url.QueryEscape(cursor)
		}

		// coves:allow-bare-client: the host is PDS_URL from the environment; the DID is an escaped query parameter. NO TIMEOUT — see fetchAllAccountsFromPDS.
		resp, err := http.Get(reqURL)
		if err != nil {
			return nil, fmt.Errorf("HTTP request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == 400 {
			// User doesn't exist on this PDS or has no records - that's OK
			return nil, nil
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		var result ListRecordsResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}

		allRecords = append(allRecords, result.Records...)

		if result.Cursor == "" {
			break
		}
		cursor = result.Cursor
	}

	return allRecords, nil
}

func indexVote(ctx context.Context, db *sql.DB, voterDID string, record Record) error {
	// Extract vote data from record
	subject, ok := record.Value["subject"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("missing subject")
	}
	subjectURI, _ := subject["uri"].(string)
	subjectCID, _ := subject["cid"].(string)
	direction, _ := record.Value["direction"].(string)
	createdAtStr, _ := record.Value["createdAt"].(string)

	if subjectURI == "" || direction == "" {
		return fmt.Errorf("invalid vote record: missing required fields")
	}

	// Parse created_at
	createdAt, err := time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		createdAt = time.Now()
	}

	// Extract rkey from URI (at://did/collection/rkey)
	parts := strings.Split(record.URI, "/")
	if len(parts) < 5 {
		return fmt.Errorf("invalid URI format: %s", record.URI)
	}
	rkey := parts[len(parts)-1]

	// Start transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert vote
	_, err = tx.ExecContext(ctx, `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (uri) DO NOTHING
	`, record.URI, record.CID, rkey, voterDID, subjectURI, subjectCID, direction, createdAt)
	if err != nil {
		return fmt.Errorf("failed to insert vote: %w", err)
	}

	// Update post/comment counts
	updateQuery := voteCountUpdateQuery(extractCollectionFromURI(subjectURI), direction)
	if updateQuery == "" {
		// Unknown collection, just index the vote
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, updateQuery, subjectURI); err != nil {
		return fmt.Errorf("failed to update vote counts: %w", err)
	}

	return tx.Commit()
}

// commentCollection is the collection a vote's subject names when the vote was
// cast on a comment.
const commentCollection = "social.coves.community.comment"

// voteCountUpdateQuery returns the statement that folds one vote into its
// subject's denormalized counters, or "" when the subject is something this tool
// does not count.
//
// It is a function rather than an inline switch so the routing can be tested
// without a database: this tool exists to REPAIR counts that drifted, so a
// subject it silently declines to route is a rerun that reports success and
// changes nothing — the least visible failure the tool can have.
func voteCountUpdateQuery(subjectCollection, direction string) string {
	switch {
	// Both post collections, because both are indexed into the `posts` table for
	// as long as the author-owned flip's dual-collection window is open
	// (posts.IsPostCollection).
	case posts.IsPostCollection(subjectCollection):
		if direction == "up" {
			return `UPDATE posts SET upvote_count = upvote_count + 1, score = upvote_count + 1 - downvote_count WHERE uri = $1 AND deleted_at IS NULL`
		}
		return `UPDATE posts SET downvote_count = downvote_count + 1, score = upvote_count - (downvote_count + 1) WHERE uri = $1 AND deleted_at IS NULL`
	case subjectCollection == commentCollection:
		if direction == "up" {
			return `UPDATE comments SET upvote_count = upvote_count + 1, score = upvote_count + 1 - downvote_count WHERE uri = $1 AND deleted_at IS NULL`
		}
		return `UPDATE comments SET downvote_count = downvote_count + 1, score = upvote_count - (downvote_count + 1) WHERE uri = $1 AND deleted_at IS NULL`
	default:
		return ""
	}
}

func extractCollectionFromURI(uri string) string {
	// at://did:plc:xxx/social.coves.community.post/rkey
	parts := strings.Split(uri, "/")
	if len(parts) >= 4 {
		return parts[3]
	}
	return ""
}
