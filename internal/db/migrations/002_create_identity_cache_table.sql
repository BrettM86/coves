-- +goose Up
-- +goose StatementBegin
CREATE TABLE identity_cache (
    -- Can lookup by either handle or DID
    identifier TEXT PRIMARY KEY,

    -- Cached resolution data
    did TEXT NOT NULL,
    handle TEXT,
    pds_url TEXT,

    -- Resolution metadata
    resolved_at TIMESTAMP WITH TIME ZONE NOT NULL,
    resolution_method TEXT NOT NULL, -- 'dns', 'https', 'cache'

    -- Cache management
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- Index for reverse lookup (DID → handle)
CREATE INDEX idx_identity_cache_did ON identity_cache(did);

-- Index for expiry cleanup
CREATE INDEX idx_identity_cache_expires ON identity_cache(expires_at);

-- Function to normalize handles to lowercase
CREATE OR REPLACE FUNCTION normalize_handle() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.handle IS NOT NULL THEN
        NEW.handle = LOWER(TRIM(NEW.handle));
    END IF;
    IF TG_OP = 'INSERT' OR TG_OP = 'UPDATE' THEN
        -- Normalize identifier if it looks like a handle (contains a dot)
        IF NEW.identifier LIKE '%.%' THEN
            NEW.identifier = LOWER(TRIM(NEW.identifier));
        END IF;
    END IF;
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Trigger to normalize handles automatically
CREATE TRIGGER normalize_handle_trigger
    BEFORE INSERT OR UPDATE ON identity_cache
    FOR EACH ROW
    EXECUTE FUNCTION normalize_handle();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS normalize_handle_trigger ON identity_cache;
DROP FUNCTION IF EXISTS normalize_handle();
DROP TABLE IF EXISTS identity_cache;
-- +goose StatementEnd
