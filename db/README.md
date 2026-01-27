# Database Schema Management

This directory contains the database schema and initialization scripts for
ldap-sync.

## Files

- `schema.sql` - SQL script that creates the searches table and indexes
- `init-schema.sh` - Shell script that waits for PostgreSQL and applies
  the schema

## Architecture

The database schema is managed **separately** from the application code.
This separation provides:

- **Clean Separation of Concerns**: Application code doesn't handle DDL
- **Explicit Schema Management**: Schema changes are explicit and versioned
- **Init Container Pattern**: Schema creation happens before app startup
- **Idempotent Operations**: Can be run multiple times safely

## How It Works

### Kubernetes Deployment

When deployed via Helm chart with `postgres.enabled: true`:

1. **Init Container Starts**: Runs before the main ldap-sync container
2. **Wait for PostgreSQL**: Uses `pg_isready` to wait up to 60 seconds
3. **Apply Schema**: Executes `schema.sql` using `psql`
4. **Main Container Starts**: Application connects to ready database

The init container uses the `postgres:15` image which includes the psql
client and pg_isready tools.

### Manual Deployment

For manual deployments or development:

```bash
# Set environment variables
export PGHOST=localhost
export PGPORT=5432
export PGUSER=ldapsync
export PGDATABASE=ldapsync
export PGPASSWORD=your-password

# Run the initialization script
bash init-schema.sh
```

Or run the SQL directly:
```bash
psql -h localhost -U ldapsync -d ldapsync -f schema.sql
```

## Schema Details

### Searches Table

Stores LDAP search specifications created via the API.

```sql
CREATE TABLE IF NOT EXISTS searches (
    id TEXT PRIMARY KEY,
    filter TEXT NOT NULL,
    refresh INTEGER NOT NULL,
    base_dn TEXT NOT NULL,
    oneshot BOOLEAN NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);
```

**Columns:**
- `id`: Unique identifier for the search
- `filter`: LDAP filter expression (e.g., "(objectClass=person)")
- `refresh`: Refresh interval in seconds
- `base_dn`: Base DN for the search
- `oneshot`: Whether this is a one-time search
- `created_at`: Timestamp when search was created
- `updated_at`: Timestamp when search was last updated

### Indexes

```sql
CREATE INDEX IF NOT EXISTS idx_searches_created_at
  ON searches(created_at);

CREATE INDEX IF NOT EXISTS idx_searches_updated_at
  ON searches(updated_at);
```

These indexes improve query performance when filtering or sorting by
timestamps.

## Modifying the Schema

To add or modify tables:

1. **Update schema.sql**: Add your CREATE TABLE or ALTER TABLE statements
2. **Use IF NOT EXISTS**: Make statements idempotent
3. **Test Locally**: Run the script against a test database
4. **Update Helm Chart**: The ConfigMap will automatically include changes
5. **Deploy**: Restart pods to apply the new schema

Example migration:
```sql
-- Add new column (use ALTER TABLE if adding to existing table)
ALTER TABLE searches
ADD COLUMN IF NOT EXISTS priority INTEGER DEFAULT 5;

-- Add new index
CREATE INDEX IF NOT EXISTS idx_searches_priority
  ON searches(priority);
```

**Note**: For complex migrations, consider using a proper migration tool
like Flyway or Liquibase.

## Troubleshooting

### Schema Not Created

Check init container logs:
```bash
kubectl logs <pod-name> -c init-db-schema
```

Common issues:
- PostgreSQL not ready within 60 seconds
- Wrong credentials in postgres-credentials secret
- Network connectivity issues

### Schema Out of Date

To update the schema in a running deployment:

1. Update `schema.sql` in the codebase
2. Update the Helm release (updates ConfigMap)
3. Delete pods to trigger init container restart
4. Init container applies updated schema

### Testing Schema Changes

Test schema changes locally:
```bash
# Start test PostgreSQL
docker run -d --name test-postgres \
  -e POSTGRES_USER=ldapsync \
  -e POSTGRES_PASSWORD=test \
  -e POSTGRES_DB=ldapsync \
  -p 5432:5432 \
  postgres:15

# Run schema
export PGHOST=localhost PGPORT=5432 PGUSER=ldapsync \
  PGDATABASE=ldapsync PGPASSWORD=test
bash init-schema.sh

# Verify
psql -h localhost -U ldapsync -d ldapsync -c "\dt"
psql -h localhost -U ldapsync -d ldapsync -c "\d searches"

# Cleanup
docker rm -f test-postgres
```

## Best Practices

1. **Idempotency**: Always use `IF NOT EXISTS` and `IF EXISTS`
2. **Backward Compatibility**: Don't remove columns that old versions use
3. **Test First**: Test schema changes in development before production
4. **Version Control**: Commit schema changes with code changes
5. **Documentation**: Update this README when adding tables or columns
6. **Indexes**: Add indexes for frequently queried columns
7. **Constraints**: Use appropriate constraints (NOT NULL, PRIMARY KEY)

## Future Enhancements

Consider these improvements for production use:

- **Migration Tool**: Use Flyway, Liquibase, or golang-migrate
- **Version Tracking**: Track schema version in the database
- **Rollback Support**: Create rollback scripts for migrations
- **Schema Validation**: Validate schema matches expected structure
- **Seed Data**: Include default data in initialization
