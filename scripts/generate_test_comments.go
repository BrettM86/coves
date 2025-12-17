//go:build ignore

package main

import (
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"time"

	_ "github.com/lib/pq"
)

// Post URI: at://did:plc:hcuo3qx2lr7h7dquusbeobht/social.coves.community.post/3m4yohkzbkc2b
// Community DID: did:plc:hcuo3qx2lr7h7dquusbeobht
// Community Handle: test-usnews.community.coves.social

const (
	postURI      = "at://did:plc:hcuo3qx2lr7h7dquusbeobht/social.coves.community.post/3m4yohkzbkc2b"
	postCID      = "bafyzohran123"
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

var userNames = []string{
	"sarah_jenkins", "michael_chen", "jessica_rodriguez", "david_nguyen",
	"emily_williams", "james_patel", "ashley_garcia", "robert_kim",
	"jennifer_lee", "william_martinez", "amanda_johnson", "daniel_brown",
	"melissa_davis", "christopher_wilson", "rebecca_anderson", "matthew_taylor",
	"laura_thomas", "anthony_moore", "stephanie_jackson", "joshua_white",
	"nicole_harris", "ryan_martin", "rachel_thompson", "kevin_garcia",
	"michelle_robinson", "brandon_clark", "samantha_lewis", "justin_walker",
	"kimberly_hall", "tyler_allen", "brittany_young", "andrew_king",
}

var positiveComments = []string{
	"This is such fantastic news! Zohran represents real progressive values and I couldn't be happier with this outcome!",
	"Finally! A mayor who actually understands the needs of working families. This is a historic moment for NYC!",
	"What an incredible victory! Zohran's grassroots campaign shows that people power still matters in politics.",
	"I'm so proud of our city today. This win gives me hope for the future of progressive politics!",
	"This is exactly what NYC needed. Zohran's policies on housing and healthcare are going to transform our city!",
	"Congratulations to Zohran! His commitment to affordable housing is going to make such a difference.",
	"I've been following his campaign since day one and I'm thrilled to see him win. He truly deserves this!",
	"This victory is proof that authentic progressive candidates can win. So excited for what's ahead!",
	"Zohran's dedication to public transit and climate action is exactly what we need. Great day for NYC!",
	"What a momentous occasion! His policies on education are going to help so many families.",
	"I'm emotional reading this! Zohran gives me so much hope for the direction of our city.",
	"This is the change we've been waiting for! Can't wait to see his vision become reality.",
	"His campaign was inspiring from start to finish. This win is well-deserved!",
	"Finally, a mayor who will prioritize working people over corporate interests!",
	"The grassroots organizing that made this happen was incredible to witness. Democracy in action!",
	"Zohran's focus on social justice is refreshing. This is a win for all New Yorkers!",
	"I volunteered for his campaign and this victory means everything. So proud!",
	"This gives me faith in our democratic process. People-powered campaigns can still win!",
	"His policies on criminal justice reform are exactly what NYC needs right now.",
	"What an amazing day for progressive politics! Zohran is going to do great things.",
}

var replyComments = []string{
	"Absolutely agree! This is going to be transformative.",
	"Couldn't have said it better myself!",
	"Yes! This is exactly right.",
	"100% this! So well said.",
	"This perfectly captures how I feel too!",
	"Exactly my thoughts! Great perspective.",
	"So true! I'm equally excited.",
	"Well put! I share your optimism.",
	"This! Absolutely this!",
	"I feel the same way! Great comment.",
	"You took the words right out of my mouth!",
	"Perfectly stated! I agree completely.",
	"Yes yes yes! This is it exactly.",
	"This is spot on! Thank you for sharing.",
	"I couldn't agree more with this take!",
	"Exactly what I was thinking! Well said.",
	"This captures it perfectly!",
	"So much this! Great comment.",
	"You nailed it! I feel exactly the same.",
	"This is the best take I've seen! Agreed!",
}

var deepReplyComments = []string{
	"And it's not just about the policies, it's about the movement he's building!",
	"This thread is giving me life! So glad to see so many people excited.",
	"I love seeing all this positive energy! We're going to change NYC together.",
	"Reading these comments makes me even more hopeful. We did this!",
	"The solidarity in this thread is beautiful. This is what democracy looks like!",
	"I'm so grateful to be part of this community right now. Historic moment!",
	"This conversation shows how ready people are for real change.",
	"Seeing this support gives me so much hope for what's possible.",
	"This is the kind of energy we need to keep up! Let's go!",
	"I'm saving this thread to look back on this incredible moment.",
}

func generateTID() string {
	// Simple TID generator for testing (timestamp in microseconds + random)
	now := time.Now().UnixMicro()
	return fmt.Sprintf("%d%04d", now, rand.Intn(10000))
}

func createUser(db *sql.DB, handle, name string, idx int) (*User, error) {
	did := fmt.Sprintf("did:plc:testuser%d%d", time.Now().Unix(), idx)
	user := &User{
		DID:    did,
		Handle: handle,
		Name:   name,
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
	uri := fmt.Sprintf("at://%s/social.coves.community.comment/%s", user.DID, rkey)
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

	log.Printf("Created comment by %s: %.50s...", user.Handle, content)
	return comment, nil
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
	// Connect to dev database
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
	log.Printf("Post URI: %s", postURI)
	log.Println("Starting to generate test data...")

	rand.Seed(time.Now().UnixNano())

	// Create users
	log.Println("\n=== Creating Users ===")
	users := make([]*User, 0, len(userNames))
	for i, name := range userNames {
		handle := fmt.Sprintf("%s.bsky.social", name)
		user, err := createUser(db, handle, name, i)
		if err != nil {
			log.Printf("Warning: Failed to create user %s: %v", name, err)
			continue
		}
		users = append(users, user)
	}

	log.Printf("\nCreated %d users", len(users))

	// Generate comments with varied timing
	log.Println("\n=== Creating Top-Level Comments ===")
	baseTime := time.Now().Add(-2 * time.Hour) // Comments from 2 hours ago
	topLevelComments := make([]*Comment, 0)

	// Create 15-20 top-level comments
	numTopLevel := 15 + rand.Intn(6)
	for i := 0; i < numTopLevel && i < len(users); i++ {
		user := users[i]
		content := positiveComments[i%len(positiveComments)]
		createdAt := baseTime.Add(time.Duration(i*5+rand.Intn(3)) * time.Minute)

		comment, err := createComment(db, user, content, postURI, postCID, createdAt)
		if err != nil {
			log.Printf("Warning: Failed to create top-level comment: %v", err)
			continue
		}

		topLevelComments = append(topLevelComments, comment)

		// Update post comment count
		if err := updateCommentCount(db, postURI, true); err != nil {
			log.Printf("Warning: Failed to update post comment count: %v", err)
		}

		// Small delay to avoid timestamp collisions
		time.Sleep(10 * time.Millisecond)
	}

	log.Printf("Created %d top-level comments", len(topLevelComments))

	// Create first-level replies (replies to top-level comments)
	log.Println("\n=== Creating First-Level Replies ===")
	firstLevelReplies := make([]*Comment, 0)

	for i, parentComment := range topLevelComments {
		// 60% chance of having replies
		if rand.Float64() > 0.6 {
			continue
		}

		// 1-3 replies per comment
		numReplies := 1 + rand.Intn(3)
		for j := 0; j < numReplies; j++ {
			userIdx := (i*3 + j + len(topLevelComments)) % len(users)
			user := users[userIdx]
			content := replyComments[rand.Intn(len(replyComments))]
			createdAt := parentComment.CreatedAt.Add(time.Duration(5+rand.Intn(10)) * time.Minute)

			comment, err := createComment(db, user, content, parentComment.URI, parentComment.CID, createdAt)
			if err != nil {
				log.Printf("Warning: Failed to create first-level reply: %v", err)
				continue
			}

			firstLevelReplies = append(firstLevelReplies, comment)

			// Update parent comment reply count
			if err := updateCommentCount(db, parentComment.URI, false); err != nil {
				log.Printf("Warning: Failed to update comment reply count: %v", err)
			}

			time.Sleep(10 * time.Millisecond)
		}
	}

	log.Printf("Created %d first-level replies", len(firstLevelReplies))

	// Create second-level replies (replies to replies) - testing nested threading
	log.Println("\n=== Creating Second-Level Replies ===")
	secondLevelCount := 0

	for i, parentComment := range firstLevelReplies {
		// 40% chance of having deep replies
		if rand.Float64() > 0.4 {
			continue
		}

		// 1-2 deep replies
		numReplies := 1 + rand.Intn(2)
		for j := 0; j < numReplies; j++ {
			userIdx := (i*2 + j + len(topLevelComments) + len(firstLevelReplies)) % len(users)
			user := users[userIdx]
			content := deepReplyComments[rand.Intn(len(deepReplyComments))]
			createdAt := parentComment.CreatedAt.Add(time.Duration(3+rand.Intn(7)) * time.Minute)

			_, err := createComment(db, user, content, parentComment.URI, parentComment.CID, createdAt)
			if err != nil {
				log.Printf("Warning: Failed to create second-level reply: %v", err)
				continue
			}

			secondLevelCount++

			// Update parent comment reply count
			if err := updateCommentCount(db, parentComment.URI, false); err != nil {
				log.Printf("Warning: Failed to update comment reply count: %v", err)
			}

			time.Sleep(10 * time.Millisecond)
		}
	}

	log.Printf("Created %d second-level replies", secondLevelCount)

	// Print summary
	totalComments := len(topLevelComments) + len(firstLevelReplies) + secondLevelCount
	log.Println("\n=== Summary ===")
	log.Printf("Total users created: %d", len(users))
	log.Printf("Total comments created: %d", totalComments)
	log.Printf("  - Top-level comments: %d", len(topLevelComments))
	log.Printf("  - First-level replies: %d", len(firstLevelReplies))
	log.Printf("  - Second-level replies: %d", secondLevelCount)
	log.Println("\nDone! Check the post at !test-usnews for the comments.")
}
