package comments

// CreateCommentRequest contains parameters for creating a comment
type CreateCommentRequest struct {
	Reply   ReplyRef      `json:"reply"`
	Content string        `json:"content"`
	Facets  []interface{} `json:"facets,omitempty"`
	Embed   interface{}   `json:"embed,omitempty"`
	Langs   []string      `json:"langs,omitempty"`
	Labels  *SelfLabels   `json:"labels,omitempty"`
}

// CreateCommentResponse contains the result of creating a comment
type CreateCommentResponse struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// UpdateCommentRequest contains parameters for updating a comment
type UpdateCommentRequest struct {
	URI     string        `json:"uri"`
	Content string        `json:"content"`
	Facets  []interface{} `json:"facets,omitempty"`
	Embed   interface{}   `json:"embed,omitempty"`
	Langs   []string      `json:"langs,omitempty"`
	Labels  *SelfLabels   `json:"labels,omitempty"`
}

// UpdateCommentResponse contains the result of updating a comment
type UpdateCommentResponse struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// DeleteCommentRequest contains parameters for deleting a comment
type DeleteCommentRequest struct {
	URI string `json:"uri"`
}
