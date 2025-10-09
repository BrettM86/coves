-- +goose Up
-- Add performance indexes for OAuth tables
-- Migration: 004_add_oauth_indexes.sql
-- Created: 2025-10-06

-- Index for querying sessions by expiration (used in token refresh logic)
CREATE INDEX IF NOT EXISTS idx_oauth_sessions_did_expires
ON oauth_sessions(did, expires_at);

-- Index for active sessions expiration (removed WHERE clause due to NOW() immutability requirement)
-- This speeds up queries for non-expired sessions
CREATE INDEX IF NOT EXISTS idx_oauth_sessions_active
ON oauth_sessions(expires_at);

-- Index on oauth_requests expiration for faster cleanup
-- (Already exists via migration 003, but documenting here for completeness)
-- CREATE INDEX IF NOT EXISTS idx_oauth_requests_expires ON oauth_requests(expires_at);

-- +goose Down
DROP INDEX IF EXISTS idx_oauth_sessions_active;
DROP INDEX IF EXISTS idx_oauth_sessions_did_expires;
