package main

import (
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"time"

	_ "github.com/lib/pq"
)

const (
	postURI      = "at://did:plc:hcuo3qx2lr7h7dquusbeobht/social.coves.community.post/3m56mowhbuk22"
	postCID      = "bafyreibml4midgt7ojq7dnabnku5ikzro4erfvdux6mmiqeat7pci2gy4u"
	communityDID = "did:plc:hcuo3qx2lr7h7dquusbeobht"
)

type User struct {
	DID    string
	Handle string
	Name   string
}

type Comment struct {
	URI       string
	CID       string
	RKey      string
	DID       string
	RootURI   string
	RootCID   string
	ParentURI string
	ParentCID string
	Content   string
	CreatedAt time.Time
}

// Escalating conversation between two users
var deepThreadConversation = []string{
	"Wait, I just realized - if they both get suspended for this, their fantasy managers are SCREWED 😂",
	"Bro imagine being in a league where you have BOTH Duren brothers and they both get suspended for fighting EACH OTHER",
	"That's actually hilarious. 'Dear commissioner, my players got suspended for fighting... with each other'",
	"The fantasy implications are wild. Do you get negative points for your players fighting your other players? 🤔",
	"New fantasy category: Family Feuds. Duren brothers leading the league in FFD (Family Fight Disqualifications)",
	"I'm dying 💀 FFD should absolutely be a stat. The Morris twins would've been unstoppable in that category",
	"Don't forget the Plumlees! Those boys used to scrap in college practices. FFD Hall of Famers",
	"Okay but serious question: has there EVER been brothers fighting each other in an NBA game before this? This has to be a first",
	"I've been watching the NBA for 30 years and I can't think of a single time. This might genuinely be historic family beef",
	"So we're witnessing NBA history right now. Not the good kind, but history nonetheless. Their mom is SO proud 😂",
}

var userHandles = []string{
	"deep_thread_guy_1.bsky.social",
	"deep_thread_guy_2.bsky.social",
}

func generateTID() string {
	now := time.Now().UnixMicro()
	return fmt.Sprintf("%d%04d", now, rand.Intn(10000))
}

func createUser(db *sql.DB, handle string, idx int) (*User, error) {
	did := fmt.Sprintf("did:plc:deepthread%d%d", time.Now().Unix(), idx)
	user := &User{
		DID:    did,
		Handle: handle,
		Name:   handle,
	}

	query := `
		INSERT INTO users (did, handle, pds_url, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		ON CONFLICT (did) DO NOTHING
	`

	_, err := db.Exec(query, user.DID, user.Handle, "http://localhost:3001")
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	log.Printf("Created user: %s (%s)", user.Handle, user.DID)
	return user, nil
}

func createComment(db *sql.DB, user *User, content, parentURI, parentCID string, createdAt time.Time) (*Comment, error) {
	rkey := generateTID()
	uri := fmt.Sprintf("at://%s/social.coves.feed.comment/%s", user.DID, rkey)
	cid := fmt.Sprintf("bafy%s", rkey)

	comment := &Comment{
		URI:       uri,
		CID:       cid,
		RKey:      rkey,
		DID:       user.DID,
		RootURI:   postURI,
		RootCID:   postCID,
		ParentURI: parentURI,
		ParentCID: parentCID,
		Content:   content,
		CreatedAt: createdAt,
	}

	query := `
		INSERT INTO comments (
			uri, cid, rkey, commenter_did, root_uri, root_cid,
			parent_uri, parent_cid, content, created_at, indexed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		ON CONFLICT (uri) DO NOTHING
		RETURNING id
	`

	var id int64
	err := db.QueryRow(query,
		comment.URI, comment.CID, comment.RKey, comment.DID,
		comment.RootURI, comment.RootCID, comment.ParentURI, comment.ParentCID,
		comment.Content, comment.CreatedAt,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("failed to create comment: %w", err)
	}

	log.Printf("Level %d: %s", getCurrentLevel(parentURI), content)
	return comment, nil
}

func getCurrentLevel(parentURI string) int {
	if parentURI == postURI {
		return 1
	}
	// Count how many times we've nested (rough estimate)
	return 2 // Will be incremented as we go
}

func updateCommentCount(db *sql.DB, parentURI string, isPost bool) error {
	if isPost {
		_, err := db.Exec(`
			UPDATE posts
			SET comment_count = comment_count + 1
			WHERE uri = $1
		`, parentURI)
		return err
	}

	_, err := db.Exec(`
		UPDATE comments
		SET reply_count = reply_count + 1
		WHERE uri = $1
	`, parentURI)
	return err
}

func main() {
	dbURL := "postgres://dev_user:dev_password@localhost:5435/coves_dev?sslmode=disable"
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}

	log.Println("Connected to database successfully!")
	log.Println("Creating 10-level deep comment thread...")

	rand.Seed(time.Now().UnixNano())

	// Create two users who will have the back-and-forth
	user1, err := createUser(db, userHandles[0], 1)
	if err != nil {
		log.Fatalf("Failed to create user 1: %v", err)
	}

	user2, err := createUser(db, userHandles[1], 2)
	if err != nil {
		log.Fatalf("Failed to create user 2: %v", err)
	}

	baseTime := time.Now().Add(-30 * time.Minute)

	// Create the 10-level deep thread
	parentURI := postURI
	parentCID := postCID
	isPost := true

	for i, content := range deepThreadConversation {
		// Alternate between users
		user := user1
		if i%2 == 1 {
			user = user2
		}

		createdAt := baseTime.Add(time.Duration(i*2) * time.Minute)

		comment, err := createComment(db, user, content, parentURI, parentCID, createdAt)
		if err != nil {
			log.Fatalf("Failed to create comment at level %d: %v", i+1, err)
		}

		// Update parent's reply count
		if err := updateCommentCount(db, parentURI, isPost); err != nil {
			log.Printf("Warning: Failed to update comment count: %v", err)
		}

		// Set this comment as the parent for the next iteration
		parentURI = comment.URI
		parentCID = comment.CID
		isPost = false

		time.Sleep(10 * time.Millisecond)
	}

	log.Println("\n=== Summary ===")
	log.Printf("Created 10-level deep comment thread")
	log.Printf("Thread participants: %s and %s", user1.Handle, user2.Handle)
	log.Println("Done! Check the NBACentral post for the deep thread.")
}
