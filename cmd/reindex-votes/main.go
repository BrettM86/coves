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
func fetchAllAccountsFromPDS(pdsURL string) ([]string, error) {
	// Use com.atproto.sync.listRepos to get all repos on this PDS
	var allDIDs []string
	cursor := ""

	for {
		reqURL := fmt.Sprintf("%s/xrpc/com.atproto.sync.listRepos?limit=100", pdsURL)
		if cursor != "" {
			reqURL += "&cursor=" + url.QueryEscape(cursor)
		}

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
	collection := extractCollectionFromURI(subjectURI)
	var updateQuery string

	switch collection {
	case "social.coves.community.post":
		if direction == "up" {
			updateQuery = `UPDATE posts SET upvote_count = upvote_count + 1, score = upvote_count + 1 - downvote_count WHERE uri = $1 AND deleted_at IS NULL`
		} else {
			updateQuery = `UPDATE posts SET downvote_count = downvote_count + 1, score = upvote_count - (downvote_count + 1) WHERE uri = $1 AND deleted_at IS NULL`
		}
	case "social.coves.community.comment":
		if direction == "up" {
			updateQuery = `UPDATE comments SET upvote_count = upvote_count + 1, score = upvote_count + 1 - downvote_count WHERE uri = $1 AND deleted_at IS NULL`
		} else {
			updateQuery = `UPDATE comments SET downvote_count = downvote_count + 1, score = upvote_count - (downvote_count + 1) WHERE uri = $1 AND deleted_at IS NULL`
		}
	default:
		// Unknown collection, just index the vote
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, updateQuery, subjectURI); err != nil {
		return fmt.Errorf("failed to update vote counts: %w", err)
	}

	return tx.Commit()
}

func extractCollectionFromURI(uri string) string {
	// at://did:plc:xxx/social.coves.community.post/rkey
	parts := strings.Split(uri, "/")
	if len(parts) >= 4 {
		return parts[3]
	}
	return ""
}
