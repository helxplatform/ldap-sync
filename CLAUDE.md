# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ldap-sync is a Go-based service for synchronizing LDAP entries between two LDAP servers with transformation capabilities. The service uses a hook-based architecture to transform entries during synchronization, supports dependency tracking for ordered writes, and provides a REST API for managing searches.

## Build Commands

```bash
# Generate Swagger documentation (must be done before building)
make docs

# Build Docker image
make build REPOSITORY=containers.renci.org/helxplatform/ldap-sync TAG=v3.1.0

# Push Docker image
make push

# Build locally (requires Go 1.23+)
CGO_ENABLED=0 GOOS=linux go build -o ldap-sync .

# For Helm chart - update dependencies before installing
cd chart && helm dependency update
```

## Database Persistence

Searches created via the API can be persisted to a PostgreSQL database. Database configuration is specified in the config file at `/etc/ldap-sync/config.yaml`:

```yaml
database:
  enabled: true
  host: "postgres-host"
  port: 5432
  username: "ldapsync"
  database: "ldapsync"
  password_file: "/etc/ldap-sync/secrets/postgres-password"
  sslmode: "disable"
```

When database persistence is enabled, the application will:
- Connect to the PostgreSQL database
- Save all searches created via POST/PUT endpoints
- Load and restore all saved searches on startup
- Delete searches from the database when DELETE is called

The password is read from a file (not environment variable) for better security. The Helm chart includes PostgreSQL persistence by default using the CloudPirates postgres chart.

**Schema Management**: The database schema is created by an init container that runs before the main application starts. The init container:
- Waits for PostgreSQL to be ready (up to 60 seconds)
- Runs the schema creation script (`db/schema.sql`)
- Creates the `searches` table with appropriate indexes
- Ensures the application can connect immediately on startup

For manual deployments, run `db/init-schema.sh` to create the schema. See the README.md Database Persistence section for details on secret management and backup/restore procedures.

## Running the Application

The application requires a configuration file at `/etc/ldap-sync/config.yaml`:

```yaml
source:
  url: "ldap://source-server:389"
  bind_dn: "cn=admin,dc=example,dc=org"
  bind_password: "password"
  base_dn: "dc=example,dc=org"
target:
  url: "ldap://target-server:389"
  bind_dn: "cn=admin,dc=example,dc=org"
  bind_password: "password"
  base_dn: "dc=example,dc=org"
hooks:
  - "http://hook-service:5001/hook"
```

Run with:
```bash
./ldap-sync --loglevel debug
```

The service starts on port 5500 with Swagger documentation at http://localhost:5500/swagger/

## Architecture

### Core Components

1. **Main Service (main.go)**: The primary LDAP sync service exposing REST API on port 5500
   - Manages searches between source and target LDAP servers
   - Sends LDAP entries to configured hooks for transformation
   - Handles dependency tracking to ensure entries are written in correct order
   - Provides REST endpoints for managing searches and viewing results

2. **Hook Services**: External services that transform LDAP entries
   - Located in `hooks/ordrd-group-x/` and `hooks/unc-group-x/`
   - Each hook service listens on port 5001 by default
   - Process incoming LDAP entries and return transformed entries with optional derived searches

3. **Helm Chart**: Kubernetes deployment configuration in `chart/`

### Key Architectural Patterns

**Hook-Based Transformation**: The main service queries the source LDAP, sends entries to registered hooks via HTTP POST, and processes the hook responses to write transformed entries to the target LDAP.

**Dependency Tracking**: The `dependencyState` system ensures entries are written to target LDAP in the correct order. When a hook returns dependencies for an entry, that entry is held in pending state until all dependencies are synced. This prevents referential integrity errors (e.g., ensures a parent group exists before adding members).

**Derived Searches**: Hooks can return derived search specifications that create new dynamic searches. For example, when processing a group entry, a hook might return a derived search to find all member users.

**Search Management**: Searches run continuously on a refresh interval, detecting new or changed entries. Searches support:
- Custom base DNs (defaults to config if not specified)
- One-shot mode (runs once without engaging hooks)
- Dynamic refresh intervals

**Merge Attributes**: Certain attributes (like `memberuid`) are merged rather than replaced when updating existing entries. This allows multiple searches to contribute values to the same attribute.

**Per-DN Locking**: Uses `sync.Map` to store per-DN mutexes, preventing race conditions when multiple goroutines attempt to write to the same DN simultaneously.

### Hook Response Format

Hooks receive LDAP entries as JSON and return:
```json
{
  "transformed": [{"dn": "...", "content": {...}}],
  "derived": [{"id": "search-id", "filter": "...", "refresh": 60, "baseDN": "...", "oneshot": false}],
  "dependencies": ["dn1", "dn2"],
  "reset": false
}
```

The `transformed` array can contain multiple entries, allowing a single input entry to generate multiple output entries.

## Hook Development

Hooks are independent Go services that implement the transformation logic:

- `hooks/ordrd-group-x/`: Processes ORDRD groups, UNC users, and posix groups with pid-to-uid mapping
- `hooks/unc-group-x/`: Similar to ordrd-group-x but uses template variables and bindings for dependency resolution

Each hook has its own Makefile with `docs`, `build`, and `push` targets. Build hooks using the same pattern as the main service.

## REST API Endpoints

- `POST /search` - Create a new search (params: id, filter, refresh, baseDN, oneShot)
- `GET /search?id=<id>` - Get search by id, or all searches if id omitted
- `PUT /search/:id` - Update existing search
- `DELETE /search/:id` - Delete search
- `GET /results/:id?full=true` - Get results for search (full=true includes content)
- `PUT /loglevel` - Update log level at runtime (body: {"level": "debug"})
- `GET /loglevel` - Get current log level
- `GET /healthz` - Liveness probe
- `GET /readyz` - Readiness probe
- `GET /swagger` - Swagger documentation UI

## Configuration Notes

- Configuration is loaded from `/etc/ldap-sync/config.yaml` at startup
- Log level can be set via `--loglevel` flag or `LOG_LEVEL` environment variable
- Default log level is "info"; valid levels are debug, info, warn, error
- The service expects hooks to be HTTP endpoints that accept POST requests

### Hook Retry Configuration

Hook requests use exponential backoff retry logic to handle startup delays (e.g., when hook sidecars are still starting):

```yaml
hook_retry:
  max_retries: 10           # Maximum retry attempts (default: 10)
  initial_delay_ms: 100     # Initial delay in ms (default: 100)
  max_delay_ms: 30000       # Maximum delay cap in ms (default: 30000)
```

Implementation details:
- Uses exponential backoff with 2x multiplier
- Adds ±10% jitter to prevent thundering herd
- Logs warning on each retry attempt
- Returns error after max retries exceeded
- Configurable via config file with sensible defaults

## Key Implementation Details

**Search Results Storage**: Each search maintains a map of DN to `LDAPResult` in `searchResults`. This allows the service to detect when entries are new, updated, or unchanged.

**Concurrent Search Execution**: Each search runs in its own goroutine with a dedicated stop channel for cancellation.

**LDAP Operations**: The service performs distinct operations for add vs modify based on whether the entry exists in the target LDAP. For existing entries with merge attributes, it fetches current values and merges them with new values.

**Error Handling**: LDAP error code 32 (No Such Object) during entry lookup is treated as "entry does not exist" rather than a fatal error, allowing the service to proceed with an add operation.

## Deploying with Helm

The Helm chart includes PostgreSQL for search persistence:

```bash
# Update dependencies first
cd chart
helm dependency update

# Install or upgrade
helm upgrade --install ldap-sync . \
  --set config.source.url="ldap://source:389" \
  --set config.source.bindPassword="password" \
  --set config.target.url="ldap://target:389" \
  --set config.target.bindPassword="password" \
  --namespace ldap-sync --create-namespace
```

**Secret Persistence**: The PostgreSQL password is automatically generated on first install and preserved across `helm upgrade` and even `helm uninstall` + `helm install` (using the same release name). This is achieved through:
- Helm's `lookup` function to detect existing secrets
- The `helm.sh/resource-policy: keep` annotation to prevent deletion
- Auto-generation of a random 32-character password on first install

To completely remove everything including persisted data:
```bash
helm uninstall ldap-sync
kubectl delete secret ldap-sync-postgres-credentials -n ldap-sync
kubectl delete pvc -l app.kubernetes.io/instance=ldap-sync -n ldap-sync
```
