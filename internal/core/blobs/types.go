package blobs

// BlobRef represents a blob reference for atproto records
type BlobRef struct {
	Type     string            `json:"$type"`
	Ref      map[string]string `json:"ref"`
	MimeType string            `json:"mimeType"`
	Size     int               `json:"size"`
}
