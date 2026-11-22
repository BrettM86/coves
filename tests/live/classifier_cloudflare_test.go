//go:build live

package live

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"Coves/internal/classifier/cloudflare"
	"Coves/internal/classifier/cloudflare/cloudflaretest"
	"Coves/internal/core/classification"
)

// ipv4Pattern matches a dotted-quad IPv4 address.
var ipv4Pattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// TestClassifierCloudflare_ClassifiesPostTextWithoutIdentifiers sends one
// benign post text to the real Workers AI Clef text model and checks the
// answer covers every policy key and the request carries no identifier (L1).
func TestClassifierCloudflare_ClassifiesPostTextWithoutIdentifiers(t *testing.T) {
	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	token := os.Getenv("CLOUDFLARE_WORKERS_AI_API_TOKEN")
	if accountID == "" {
		t.Fatalf("CLOUDFLARE_ACCOUNT_ID must be set in the environment")
	}
	if token == "" {
		t.Fatalf("CLOUDFLARE_WORKERS_AI_API_TOKEN must be set in the environment")
	}

	policy, err := classification.LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	transport := cloudflaretest.NewForwardingTransport(http.DefaultTransport)
	client := cloudflare.NewClient(cloudflare.Config{
		AccountID:   accountID,
		APIToken:    token,
		TextModelID: "@cf/cloudflare/clef-flash",
	}, &http.Client{Transport: transport, Timeout: 60 * time.Second})

	content := classification.NewPostContent(classification.PostFields{
		Title: "Weekend at the botanical garden",
		Body:  "The tulips were in full bloom and we had a picnic by the pond.",
	}, 8000)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := client.Classify(ctx, content, policy.Questions)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(policy.Questions) != 6 {
		t.Fatalf("policy has %d questions, want 6", len(policy.Questions))
	}
	for _, question := range policy.Questions {
		probability, ok := result.Probabilities[question.Key]
		if !ok {
			t.Errorf("no probability for %q", question.Key)
			continue
		}
		if probability < 0 || probability > 1 {
			t.Errorf("probability for %q is %v, want within [0,1]", question.Key, probability)
		}
	}

	requests := transport.Requests()
	if len(requests) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(requests))
	}
	request := requests[0]
	if !strings.HasSuffix(request.URL, "/@cf/cloudflare/clef-flash") {
		t.Errorf("request URL path does not end with the text model ID @cf/cloudflare/clef-flash")
	}

	var headers strings.Builder
	for name, values := range request.Header {
		headers.WriteString(name)
		headers.WriteString(": ")
		if strings.EqualFold(name, "Authorization") {
			headers.WriteString("<redacted>\n")
			continue
		}
		headers.WriteString(strings.Join(values, ","))
		headers.WriteString("\n")
	}
	for part, recorded := range map[string]string{
		"URL":     request.URL,
		"headers": headers.String(),
		"body":    string(request.Body),
	} {
		if strings.Contains(recorded, "did:") {
			t.Errorf("request %s contains a DID", part)
		}
		if strings.Contains(recorded, "at://") {
			t.Errorf("request %s contains an AT-URI", part)
		}
		if ipv4Pattern.MatchString(recorded) {
			t.Errorf("request %s contains an IPv4 address", part)
		}
	}
}

// TestClassifierCloudflare_ClassifiesCommentAndCommunityText sends one benign
// comment body and one community's name, display name and description to the
// real Workers AI Clef text model, and checks each answer covers every policy
// key and no request carries a DID, handle or URI (L1, chunk 03).
func TestClassifierCloudflare_ClassifiesCommentAndCommunityText(t *testing.T) {
	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	token := os.Getenv("CLOUDFLARE_WORKERS_AI_API_TOKEN")
	if accountID == "" {
		t.Fatalf("CLOUDFLARE_ACCOUNT_ID must be set in the environment")
	}
	if token == "" {
		t.Fatalf("CLOUDFLARE_WORKERS_AI_API_TOKEN must be set in the environment")
	}

	policy, err := classification.LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if len(policy.Questions) != 6 {
		t.Fatalf("policy has %d questions, want 6", len(policy.Questions))
	}
	transport := cloudflaretest.NewForwardingTransport(http.DefaultTransport)
	client := cloudflare.NewClient(cloudflare.Config{
		AccountID:   accountID,
		APIToken:    token,
		TextModelID: "@cf/cloudflare/clef-flash",
	}, &http.Client{Transport: transport, Timeout: 60 * time.Second})

	// The subjects' identifiers, none of which may reach the provider.
	identifiers := []string{
		"did:plc:livecommenter",
		"did:plc:livecommunity",
		"livecommenter.coves.social",
		"c-gardening.coves.social",
		"at://did:plc:livecommenter/social.coves.community.comment/3kcomment",
	}

	subjects := []struct {
		name    string
		content classification.Content
		want    string
	}{
		{
			name:    "comment",
			content: classification.NewCommentContent("Those tulips look wonderful, which variety are they?", 8000),
			want:    "Those tulips look wonderful, which variety are they?",
		},
		{
			name: "community",
			content: classification.NewCommunityContent(classification.CommunityFields{
				Name:        "gardening",
				DisplayName: "Home Gardening",
				Description: "Tips and photos from vegetable patches and flower beds.",
			}, 8000),
			want: "gardening\n\nHome Gardening\n\nTips and photos from vegetable patches and flower beds.",
		},
	}

	for _, subject := range subjects {
		if subject.content.Text != subject.want {
			t.Fatalf("%s content is %q, want %q", subject.name, subject.content.Text, subject.want)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		result, err := client.Classify(ctx, subject.content, policy.Questions)
		cancel()
		if err != nil {
			t.Fatalf("Classify %s: %v", subject.name, err)
		}
		for _, question := range policy.Questions {
			probability, ok := result.Probabilities[question.Key]
			if !ok {
				t.Errorf("%s: no probability for %q", subject.name, question.Key)
				continue
			}
			if probability < 0 || probability > 1 {
				t.Errorf("%s: probability for %q is %v, want within [0,1]", subject.name, question.Key, probability)
			}
		}
	}

	requests := transport.Requests()
	if len(requests) != len(subjects) {
		t.Fatalf("recorded %d requests, want %d", len(requests), len(subjects))
	}
	for i, request := range requests {
		var headers strings.Builder
		for name, values := range request.Header {
			headers.WriteString(name)
			headers.WriteString(": ")
			if strings.EqualFold(name, "Authorization") {
				headers.WriteString("<redacted>\n")
				continue
			}
			headers.WriteString(strings.Join(values, ","))
			headers.WriteString("\n")
		}
		for part, recorded := range map[string]string{
			"URL":     request.URL,
			"headers": headers.String(),
			"body":    string(request.Body),
		} {
			if strings.Contains(recorded, "did:") {
				t.Errorf("%s request %s contains a DID", subjects[i].name, part)
			}
			if strings.Contains(recorded, "at://") {
				t.Errorf("%s request %s contains an AT-URI", subjects[i].name, part)
			}
			for _, identifier := range identifiers {
				if strings.Contains(recorded, identifier) {
					t.Errorf("%s request %s contains the identifier %q", subjects[i].name, part, identifier)
				}
			}
		}
	}
}

// TestClassifierCloudflare_ClassifiesTextWithImagesInBatches sends one benign
// text with five committed benign fixture images to the real Workers AI Clef
// image model in batches of at most four, and checks every request carries
// the text, every answer covers every policy key, and no request carries an
// identifier (L1, chunk 04).
func TestClassifierCloudflare_ClassifiesTextWithImagesInBatches(t *testing.T) {
	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	token := os.Getenv("CLOUDFLARE_WORKERS_AI_API_TOKEN")
	if accountID == "" {
		t.Fatalf("CLOUDFLARE_ACCOUNT_ID must be set in the environment")
	}
	if token == "" {
		t.Fatalf("CLOUDFLARE_WORKERS_AI_API_TOKEN must be set in the environment")
	}

	policy, err := classification.LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if len(policy.Questions) != 6 {
		t.Fatalf("policy has %d questions, want 6", len(policy.Questions))
	}
	transport := cloudflaretest.NewForwardingTransport(http.DefaultTransport)
	client := cloudflare.NewClient(cloudflare.Config{
		AccountID:    accountID,
		APIToken:     token,
		TextModelID:  "@cf/cloudflare/clef-flash",
		ImageModelID: "@cf/cloudflare/clef",
	}, &http.Client{Transport: transport, Timeout: 60 * time.Second})

	// The owner identifier the references name, which must not reach the
	// provider.
	const ownerDID = "did:plc:liveimageowner"
	fixtures := []string{
		"benign-01-sun.png", "benign-02-leaf.png", "benign-03-sea.png", "benign-04-brick.png", "benign-05-sky.png",
	}
	images := map[string]classification.Image{}
	refs := make([]classification.ImageRef, 0, len(fixtures))
	for _, name := range fixtures {
		data, err := os.ReadFile(filepath.Join("..", "fixtures", "images", name))
		if err != nil {
			t.Fatalf("reading fixture image %s: %v", name, err)
		}
		images[name] = classification.Image{ContentType: "image/png", Data: data}
		refs = append(refs, classification.ImageRef{OwnerDID: ownerDID, CID: name})
	}

	const text = "Weekend at the botanical garden\n\nThe tulips were in full bloom and we had a picnic by the pond."
	content := classification.NewPostContent(classification.PostFields{
		Title: "Weekend at the botanical garden",
		Body:  "The tulips were in full bloom and we had a picnic by the pond.",
	}, 8000)
	if content.Text != text {
		t.Fatalf("post content is %q, want %q", content.Text, text)
	}

	batches := classification.ImageBatches(refs)
	if len(batches) != 2 || len(batches[0]) != 4 || len(batches[1]) != 1 {
		sizes := make([]int, 0, len(batches))
		for _, batch := range batches {
			sizes = append(sizes, len(batch))
		}
		t.Fatalf("five images split into batches of %v, want [4 1]", sizes)
	}
	for i, batch := range batches {
		batchContent := content
		batchContent.Images = nil
		for _, ref := range batch {
			batchContent.Images = append(batchContent.Images, images[ref.CID])
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		result, err := client.Classify(ctx, batchContent, policy.Questions)
		cancel()
		if err != nil {
			t.Fatalf("Classify batch %d: %v", i+1, err)
		}
		if result.ModelID != "@cf/cloudflare/clef" {
			t.Errorf("batch %d answered by model %q, want @cf/cloudflare/clef", i+1, result.ModelID)
		}
		for _, question := range policy.Questions {
			probability, ok := result.Probabilities[question.Key]
			if !ok {
				t.Errorf("batch %d: no probability for %q", i+1, question.Key)
				continue
			}
			if probability < 0 || probability > 1 {
				t.Errorf("batch %d: probability for %q is %v, want within [0,1]", i+1, question.Key, probability)
			}
		}
	}

	requests := transport.Requests()
	if len(requests) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(requests))
	}
	for i, request := range requests {
		if !strings.HasSuffix(request.URL, "/@cf/cloudflare/clef") {
			t.Errorf("request %d URL path does not end with the image model ID @cf/cloudflare/clef", i+1)
		}
		var body struct {
			State  string   `json:"state"`
			Images []string `json:"images"`
		}
		if err := json.Unmarshal(request.Body, &body); err != nil {
			t.Fatalf("request %d body does not decode: %v", i+1, err)
		}
		if body.State != text {
			t.Errorf("request %d state is %q, want the post text", i+1, body.State)
		}
		if want := []int{4, 1}[i]; len(body.Images) != want {
			t.Errorf("request %d carries %d images, want %d", i+1, len(body.Images), want)
		}
		for _, dataURL := range body.Images {
			if !strings.HasPrefix(dataURL, "data:image/png;base64,") {
				t.Errorf("request %d carries an image that is not a PNG data URL", i+1)
			}
		}

		var headers strings.Builder
		for name, values := range request.Header {
			headers.WriteString(name)
			headers.WriteString(": ")
			if strings.EqualFold(name, "Authorization") {
				headers.WriteString("<redacted>\n")
				continue
			}
			headers.WriteString(strings.Join(values, ","))
			headers.WriteString("\n")
		}
		for part, recorded := range map[string]string{
			"URL":     request.URL,
			"headers": headers.String(),
			"body":    string(request.Body),
		} {
			if strings.Contains(recorded, "did:") {
				t.Errorf("request %d %s contains a DID", i+1, part)
			}
			if strings.Contains(recorded, "at://") {
				t.Errorf("request %d %s contains an AT-URI", i+1, part)
			}
			if ipv4Pattern.MatchString(recorded) {
				t.Errorf("request %d %s contains an IPv4 address", i+1, part)
			}
			for _, name := range fixtures {
				if strings.Contains(recorded, name) {
					t.Errorf("request %d %s contains the image reference %q", i+1, part, name)
				}
			}
		}
	}
}
