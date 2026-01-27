#!/bin/bash
set -e

# ldap-sync database schema initialization script
# This script waits for PostgreSQL to be ready and then creates the schema

echo "Waiting for PostgreSQL to be ready..."

# Read database configuration from environment variables
: "${PGHOST:?PGHOST must be set}"
: "${PGPORT:=5432}"
: "${PGUSER:?PGUSER must be set}"
: "${PGDATABASE:?PGDATABASE must be set}"

# Wait for PostgreSQL to be ready (max 60 seconds)
RETRIES=60
until pg_isready -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -q || [ $RETRIES -eq 0 ]; do
  echo "Waiting for PostgreSQL at $PGHOST:$PGPORT... ($RETRIES retries left)"
  RETRIES=$((RETRIES - 1))
  sleep 1
done

if [ $RETRIES -eq 0 ]; then
  echo "ERROR: PostgreSQL did not become ready in time"
  exit 1
fi

echo "PostgreSQL is ready, creating schema..."

# Run the schema creation script
psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -f /db/schema.sql

echo "Schema initialization completed successfully"
