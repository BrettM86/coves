-- +goose Up
-- Create main users table for Coves (all users are atProto users)
CREATE TABLE users (
    did TEXT PRIMARY KEY,
    handle TEXT UNIQUE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- Indexes for efficient lookups
CREATE INDEX idx_users_handle ON users(handle);
CREATE INDEX idx_users_created_at ON users(created_at);

-- +goose Down
DROP TABLE users;
