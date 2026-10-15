package jetstream

// HasUncachedIdentityResolver reports whether the consumer was built with a
// resolver for fresh identity-event refresh. Without one, an identity event is
// answered from the cached resolver, so cmd/server asserts the production
// wiring through this.
func (c *UserEventConsumer) HasUncachedIdentityResolver() bool {
	return c.uncachedIdentityResolver != nil
}

// HasUncachedIdentityResolver reports whether the consumer was built with a
// resolver for the fresh resolution a profile carrying origin display fields
// needs. Without one, those fields are never stored, so cmd/server asserts the
// production wiring through this.
func (c *CommunityEventConsumer) HasUncachedIdentityResolver() bool {
	return c.uncachedIdentityResolver != nil
}
