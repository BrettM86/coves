package moderation

// InitialVersion is the opaque state token of a subject with no moderation
// rows. Later chunks advance the encoding; readers treat it as opaque.
const InitialVersion = "v0"
