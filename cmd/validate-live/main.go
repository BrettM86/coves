// Command validate-live fetches all social.coves.* records from a live PDS via
// public XRPC endpoints and validates them against the local lexicon schemas.
// Use it before publishing lexicons or tightening schema constraints to confirm
// no live record would be invalidated.
//
// Usage: go run ./cmd/validate-live [-pds https://pds.example.com] [-schemas internal/atproto/lexicon]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
)

type repoEntry struct {
	Did string `json:"did"`
}

type listReposResponse struct {
	Cursor string      `json:"cursor"`
	Repos  []repoEntry `json:"repos"`
}

type describeRepoResponse struct {
	Collections []string `json:"collections"`
}

type recordEntry struct {
	URI   string          `json:"uri"`
	Value json.RawMessage `json:"value"`
}

type listRecordsResponse struct {
	Cursor  string        `json:"cursor"`
	Records []recordEntry `json:"records"`
}

func main() {
	var (
		pdsURL     = flag.String("pds", "https://pds.bretton.dev", "Base URL of the PDS to scan")
		schemaPath = flag.String("schemas", "internal/atproto/lexicon", "Path to lexicon schemas directory")
	)
	flag.Parse()

	catalog := lexicon.NewBaseCatalog()
	if err := catalog.LoadDirectory(*schemaPath); err != nil {
		log.Fatalf("Failed to load lexicon schemas: %v", err)
	}

	client := &http.Client{Timeout: 30 * time.Second} // coves:allow-bare-client: operator-run CLI whose -pds flag the operator types; there is no attacker in this path

	repos, err := listAllRepos(client, *pdsURL)
	if err != nil {
		log.Fatalf("Failed to list repos: %v", err)
	}
	fmt.Printf("Found %d repos on %s\n", len(repos), *pdsURL)

	totalByCollection := map[string]int{}
	failures := 0
	skippedRepos := 0
	skippedCollections := 0

	for _, repo := range repos {
		collections, err := describeRepo(client, *pdsURL, repo.Did)
		if err != nil {
			log.Printf("WARN: describeRepo %s: %v (repo skipped — sweep is incomplete)", repo.Did, err)
			skippedRepos++
			continue
		}
		for _, collection := range collections {
			if !strings.HasPrefix(collection, "social.coves.") {
				continue
			}
			records, err := listAllRecords(client, *pdsURL, repo.Did, collection)
			if err != nil {
				log.Printf("WARN: listRecords %s %s: %v (collection skipped — sweep is incomplete)", repo.Did, collection, err)
				skippedCollections++
				continue
			}
			for _, record := range records {
				totalByCollection[collection]++
				parsed, err := atdata.UnmarshalJSON(record.Value)
				if err != nil {
					failures++
					fmt.Printf("UNPARSEABLE %s: %v\n", record.URI, err)
					continue
				}
				if err := lexicon.ValidateRecord(&catalog, parsed, collection, lexicon.AllowLenientDatetime); err != nil {
					failures++
					fmt.Printf("INVALID %s: %v\n", record.URI, err)
				}
			}
		}
	}

	fmt.Println("\nRecords validated per collection:")
	collections := make([]string, 0, len(totalByCollection))
	for collection := range totalByCollection {
		collections = append(collections, collection)
	}
	sort.Strings(collections)
	for _, collection := range collections {
		fmt.Printf("  %-50s %d\n", collection, totalByCollection[collection])
	}
	if failures > 0 {
		fmt.Printf("\nFAIL: %d live records fail validation against current schemas\n", failures)
		os.Exit(1)
	}
	if skippedRepos > 0 || skippedCollections > 0 {
		fmt.Printf("\nINCOMPLETE SWEEP: %d repos and %d collections could not be fetched; no verdict on unvalidated records\n",
			skippedRepos, skippedCollections)
		os.Exit(2)
	}
	fmt.Println("\nOK: all live records validate against current schemas")
}

func getJSON(client *http.Client, rawURL string, out interface{}) error {
	resp, err := client.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func listAllRepos(client *http.Client, pdsURL string) ([]repoEntry, error) {
	var repos []repoEntry
	cursor := ""
	for {
		u := fmt.Sprintf("%s/xrpc/com.atproto.sync.listRepos?limit=500", pdsURL)
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		var page listReposResponse
		if err := getJSON(client, u, &page); err != nil {
			return nil, err
		}
		repos = append(repos, page.Repos...)
		if page.Cursor == "" || len(page.Repos) == 0 {
			return repos, nil
		}
		if page.Cursor == cursor {
			return nil, fmt.Errorf("listRepos: server returned non-advancing cursor %q", cursor)
		}
		cursor = page.Cursor
	}
}

func describeRepo(client *http.Client, pdsURL, did string) ([]string, error) {
	u := fmt.Sprintf("%s/xrpc/com.atproto.repo.describeRepo?repo=%s", pdsURL, url.QueryEscape(did))
	var resp describeRepoResponse
	if err := getJSON(client, u, &resp); err != nil {
		return nil, err
	}
	return resp.Collections, nil
}

func listAllRecords(client *http.Client, pdsURL, did, collection string) ([]recordEntry, error) {
	var records []recordEntry
	cursor := ""
	for {
		u := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=%s&limit=100",
			pdsURL, url.QueryEscape(did), url.QueryEscape(collection))
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		var page listRecordsResponse
		if err := getJSON(client, u, &page); err != nil {
			return nil, err
		}
		records = append(records, page.Records...)
		if page.Cursor == "" || len(page.Records) == 0 {
			return records, nil
		}
		if page.Cursor == cursor {
			return nil, fmt.Errorf("listRecords %s %s: server returned non-advancing cursor %q", did, collection, cursor)
		}
		cursor = page.Cursor
	}
}
