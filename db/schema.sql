-- ldap-sync database schema
-- This script creates the necessary tables for persisting search configurations

-- Searches table stores LDAP search specifications
CREATE TABLE IF NOT EXISTS searches (
    id TEXT PRIMARY KEY,
    filter TEXT NOT NULL,
    refresh INTEGER NOT NULL,
    base_dn TEXT NOT NULL,
    oneshot BOOLEAN NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Create indexes for common queries
CREATE INDEX IF NOT EXISTS idx_searches_created_at ON searches(created_at);
CREATE INDEX IF NOT EXISTS idx_searches_updated_at ON searches(updated_at);
