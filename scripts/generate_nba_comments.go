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

// Post URI for "Your son don't wanna be here..." NBACentral post
// at://did:plc:hcuo3qx2lr7h7dquusbeobht/social.coves.community.post/3m56mowhbuk22

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

var userNames = []string{
	"lakers_fan_23", "pistons_nation", "nba_historian", "hoops_enthusiast",
	"detroit_pride", "basketball_iq", "courtside_view", "rim_protector",
	"three_point_specialist", "paint_beast", "fast_break_fan", "clutch_time",
	"triple_double_king", "defense_wins", "small_ball_era", "old_school_hoops",
	"draft_expert", "salary_cap_guru", "trade_machine", "basketball_analytics",
	"box_score_reader", "eye_test_guy", "film_room_analyst", "player_development",
	"hometown_hero", "bandwagon_fan", "loyal_since_day_one", "casual_viewer",
	"die_hard_supporter", "armchair_coach", "nbatv_addict", "league_pass_subscriber",
}

var topLevelComments = []string{
	"Imagine having to explain to your mom at Thanksgiving that you got ejected for fighting your brother 💀",
	"Mrs. Duren watching this at home like 'I didn't raise y'all like this'",
	"Their mom is somewhere absolutely LIVID right now. Both of them getting the belt when they get home",
	"This is the most expensive sibling rivalry in history lmao",
	"Jalen really said 'I've been whooping your ass since we were kids, what makes you think tonight's different' 😂",
	"Ausar thought the NBA would protect him from his older brother. He thought wrong.",
	"The trash talk must have been PERSONAL. That's years of sibling beef coming out",
	"Family group chat is gonna be awkward after this one",
	"Their parents spent 18 years breaking up fights just for it to happen on national TV",
	"This is what happens when little bro thinks he's tough now that he's in the league",
	"Jalen's been dunking on Ausar in the driveway for years, this was just another Tuesday for him",
	"The fact that they're both in the league and THIS is how they settle it 💀💀💀",
	"Ausar: 'I'm in the NBA now, I'm not scared of you anymore' - Jalen: 'BET'",
	"Mom definitely called both of them after the game. Neither one answered lol",
	"This is the content I pay League Pass for. Brothers getting into it on the court is peak entertainment",
	"Thanksgiving dinner is about to be TENSE in the Duren/Thompson household",
	"Little brother energy vs Big brother authority. Tale as old as time",
	"The refs trying to break them up like 'Sir that's your BROTHER'",
	"Jalen been waiting for this moment since Ausar got drafted",
	"Both of them getting fined and their mom making them split the cost 😂",
	"This brings me back to fighting my brother over the last piece of pizza. Just at a much higher tax bracket",
	"The Pistons and Rockets staff trying to separate them: 'Guys we have practice tomorrow!'",
	"Ausar finally tall enough to talk back and chose violence",
	"Their dad watching like 'At least wait til you're both All-Stars before embarrassing the family'",
	"This is what decades of 'Mom said it's my turn on the Xbox' leads to",
}

var replyComments = []string{
	"LMAOOO facts, mom's not playing",
	"Bro I'm crying at this visual 😂😂😂",
	"This is the one right here 💀",
	"Thanksgiving about to be SILENT",
	"You know their dad had flashbacks to breaking up driveway fights",
	"The family group chat IS ON FIRE right now I guarantee it",
	"Little bro syndrome is real and Ausar has it BAD",
	"Big facts. Jalen been the big brother his whole life, NBA don't change that",
	"Mom's gonna make them hug it out before Christmas I'm calling it now",
	"This comment wins 😂😂😂",
	"I need the full footage of what was said because it had to be PERSONAL",
	"Years of sibling rivalry just exploded on NBA hardwood",
	"The refs were so confused trying to separate family members 💀",
	"Both of them getting the 'I'm disappointed' text from mom",
	"Ausar thought NBA money meant he was safe. Nope.",
	"Jalen's been waiting to humble him since draft night",
	"This is exactly what their parents warned them about lmao",
	"The fine money coming out of their allowance fr fr",
	"Peak sibling behavior. I respect it.",
	"Someone check on Mrs. Duren she's probably stress eating rn",
}

var deepReplyComments = []string{
	"And you KNOW mom's taking both their sides AND neither side at the same time",
	"Family dynamics don't stop just cause you're making millions. Big brother gonna big brother",
	"This thread has me in TEARS. Y'all are hilarious 😭",
	"The fact that NBA refs had to break up a family dispute is sending me",
	"Both of them are gonna act like nothing happened next family reunion",
	"I guarantee their teammates are ROASTING them in the group chats right now",
	"This is the most relatable NBA drama I've ever seen. We all fought our siblings",
	"Mom's calling BOTH coaches after this I just know it",
	"The league office trying to figure out how to fine siblings for fighting each other",
	"This is gonna be an amazing 30 for 30 one day: 'What if I told you family and basketball don't always mix'",
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
	log.Println("Starting to generate NBA test comments...")

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
	baseTime := time.Now().Add(-3 * time.Hour) // Comments from 3 hours ago
	topLevelCommentsCreated := make([]*Comment, 0)

	// Create 18-22 top-level comments
	numTopLevel := 18 + rand.Intn(5)
	for i := 0; i < numTopLevel && i < len(users) && i < len(topLevelComments); i++ {
		user := users[i]
		content := topLevelComments[i]
		createdAt := baseTime.Add(time.Duration(i*4+rand.Intn(3)) * time.Minute)

		comment, err := createComment(db, user, content, postURI, postCID, createdAt)
		if err != nil {
			log.Printf("Warning: Failed to create top-level comment: %v", err)
			continue
		}

		topLevelCommentsCreated = append(topLevelCommentsCreated, comment)

		// Update post comment count
		if err := updateCommentCount(db, postURI, true); err != nil {
			log.Printf("Warning: Failed to update post comment count: %v", err)
		}

		// Small delay to avoid timestamp collisions
		time.Sleep(10 * time.Millisecond)
	}

	log.Printf("Created %d top-level comments", len(topLevelCommentsCreated))

	// Create first-level replies (replies to top-level comments)
	log.Println("\n=== Creating First-Level Replies ===")
	firstLevelReplies := make([]*Comment, 0)

	for i, parentComment := range topLevelCommentsCreated {
		// 70% chance of having replies (NBA threads get lots of engagement)
		if rand.Float64() > 0.7 {
			continue
		}

		// 1-4 replies per comment
		numReplies := 1 + rand.Intn(4)
		for j := 0; j < numReplies && len(replyComments) > 0; j++ {
			userIdx := (i*3 + j + len(topLevelCommentsCreated)) % len(users)
			user := users[userIdx]
			content := replyComments[rand.Intn(len(replyComments))]
			createdAt := parentComment.CreatedAt.Add(time.Duration(3+rand.Intn(8)) * time.Minute)

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

	// Create second-level replies (replies to replies) - deep threads
	log.Println("\n=== Creating Second-Level Replies ===")
	secondLevelCount := 0

	for i, parentComment := range firstLevelReplies {
		// 50% chance of having deep replies (NBA drama threads go DEEP)
		if rand.Float64() > 0.5 {
			continue
		}

		// 1-2 deep replies
		numReplies := 1 + rand.Intn(2)
		for j := 0; j < numReplies && len(deepReplyComments) > 0; j++ {
			userIdx := (i*2 + j + len(topLevelCommentsCreated) + len(firstLevelReplies)) % len(users)
			user := users[userIdx]
			content := deepReplyComments[rand.Intn(len(deepReplyComments))]
			createdAt := parentComment.CreatedAt.Add(time.Duration(2+rand.Intn(5)) * time.Minute)

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
	totalComments := len(topLevelCommentsCreated) + len(firstLevelReplies) + secondLevelCount
	log.Println("\n=== Summary ===")
	log.Printf("Total users created: %d", len(users))
	log.Printf("Total comments created: %d", totalComments)
	log.Printf("  - Top-level comments: %d", len(topLevelCommentsCreated))
	log.Printf("  - First-level replies: %d", len(firstLevelReplies))
	log.Printf("  - Second-level replies: %d", secondLevelCount)
	log.Println("\nDone! Check the NBACentral post for the brothers drama comments.")
}
