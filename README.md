# ldap-sync

A Go-based service for synchronizing and transforming LDAP entries between
two LDAP servers with support for hook-based transformations, dependency
tracking, and persistent search management.

## Features

- **Bidirectional LDAP Sync**: Query source LDAP and write to target LDAP
- **Hook-Based Transformations**: Send entries to external services for
  custom transformation logic
- **Dependency Tracking**: Ensures entries are written in the correct order
  to maintain referential integrity
- **Derived Searches**: Hooks can dynamically create new searches based on
  processed entries
- **Persistent Searches**: PostgreSQL-backed persistence for searches
  created via API
- **REST API**: Full CRUD operations for managing searches
- **Merge Attributes**: Intelligent merging of multi-valued attributes
- **Real-time Monitoring**: Continuous polling with configurable refresh
  intervals
- **Swagger Documentation**: Interactive API documentation at `/swagger`

## Architecture

### Core Components

1. **Main Service**: REST API server (port 5500) managing LDAP
   synchronization
2. **Hook Services**: External transformation services that process LDAP
   entries
3. **PostgreSQL Database**: Optional persistence layer for search
   configurations
4. **Helm Chart**: Kubernetes deployment with integrated PostgreSQL

### How It Works

```
Source LDAP ──▶ ldap-sync + Hooks ──▶ Target LDAP
                      │
                      ▼
                 PostgreSQL
                 (Searches)
```

1. **Query**: Service performs LDAP searches on source server
2. **Transform**: Entries are sent to configured hooks via HTTP POST
3. **Process**: Hooks return transformed entries with optional
   dependencies
4. **Sync**: Entries are written to target LDAP respecting dependencies
5. **Persist**: Search configurations are saved to PostgreSQL

### Dependency Tracking

When a hook returns dependencies for an entry, that entry is held in a
pending state until all dependencies are synced. This prevents referential
integrity errors (e.g., ensures a parent group exists before adding
members).

### Derived Searches

Hooks can return new search specifications dynamically. For example, when
processing a group entry, a hook might return a derived search to find all
member users.

## Quick Start

### Local Development

1. **Start PostgreSQL** (optional):
   ```bash
   docker run -d --name postgres \
     -e POSTGRES_USER=ldapsync \
     -e POSTGRES_PASSWORD=mypassword \
     -e POSTGRES_DB=ldapsync \
     -p 5432:5432 \
     postgres:15
   ```

2. **Create configuration file** at `/etc/ldap-sync/config.yaml`:
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

   database:
     enabled: true
     host: "localhost"
     port: 5432
     username: "ldapsync"
     database: "ldapsync"
     password_file: "/etc/ldap-sync/secrets/postgres-password"
     sslmode: "disable"
   ```

3. **Run ldap-sync**:
   ```bash
   ./ldap-sync --loglevel debug
   ```

4. **Access Swagger UI**: http://localhost:5500/swagger

### Kubernetes Deployment

1. **Update Helm dependencies**:
   ```bash
   cd chart
   helm dependency update
   ```

2. **Install with PostgreSQL persistence**:
   ```bash
   helm upgrade --install ldap-sync ./chart \
     --set config.source.url="ldap://source:389" \
     --set config.source.bindPassword="password" \
     --set config.target.url="ldap://target:389" \
     --set config.target.bindPassword="password" \
     --namespace ldap-sync --create-namespace
   ```

   **Note**: If using a custom release name, you must set the postgres
   secret:
   ```bash
   helm upgrade --install my-release ./chart \
     --set postgres.auth.existingSecret=my-release-postgres \
     --set config.source.url="ldap://source:389" \
     [other settings...]
   ```

## Building

### Build Binary

```bash
# Generate Swagger documentation
make docs

# Build locally (requires Go 1.23+)
CGO_ENABLED=0 GOOS=linux go build -o ldap-sync .
```

### Build Docker Image

```bash
make build REPOSITORY=your-registry/ldap-sync TAG=v3.1.0
make push
```

## Configuration

### LDAP Configuration

Configure source and target LDAP servers in `/etc/ldap-sync/config.yaml`:

```yaml
source:
  url: "ldap://source:389"          # LDAP URL
  bind_dn: "cn=admin,dc=example,dc=org"  # Bind DN
  bind_password: "password"         # Bind password
  base_dn: "dc=example,dc=org"      # Search base DN

target:
  url: "ldap://target:389"
  bind_dn: "cn=admin,dc=example,dc=org"
  bind_password: "password"
  base_dn: "dc=example,dc=org"
```

### Hook Configuration

Hooks are HTTP services that receive LDAP entries and return
transformations:

```yaml
hooks:
  - "http://hook-service-1:5001/hook"
  - "http://hook-service-2:5002/hook"

# Hook retry configuration with exponential backoff
hook_retry:
  max_retries: 10           # Maximum retry attempts (default: 10)
  initial_delay_ms: 100     # Initial delay in ms (default: 100)
  max_delay_ms: 30000       # Maximum delay cap in ms (default: 30000)
```

**Hook Retry Behavior:**

Hooks are called with automatic retry and exponential backoff to handle
startup delays (e.g., when hook sidecars are still initializing):

- Retries up to `max_retries` times (default: 10)
- Starts with `initial_delay_ms` delay (default: 100ms)
- Doubles the delay on each retry (exponential backoff)
- Caps delay at `max_delay_ms` (default: 30 seconds)
- Adds ±10% jitter to prevent thundering herd

This ensures hooks have time to start before the main application
begins processing entries.

### Database Persistence

Enable PostgreSQL persistence for searches:

```yaml
database:
  enabled: true                     # Enable database persistence
  host: "postgres-host"             # PostgreSQL hostname
  port: 5432                        # PostgreSQL port
  username: "ldapsync"              # Database username
  database: "ldapsync"              # Database name
  password_file: "/path/to/pass"   # Password file path
  sslmode: "disable"                # SSL mode (disable/require)
```

#### How It Works

The Helm chart deploys PostgreSQL using the CloudPirates postgres chart
and manages search persistence through:

1. **PostgreSQL Database**: Deployed as part of the Helm chart
2. **Init Container**: Creates schema before main application starts
3. **Automatic Persistence**: All API-created searches saved to database
4. **Automatic Restoration**: Searches restored and resumed on startup

#### Init Container

An init container runs before the main ldap-sync container:

- Uses `postgres:15` image (includes psql and pg_isready)
- Waits up to 60 seconds for PostgreSQL to be ready
- Executes `db/schema.sql` to create tables and indexes
- Fails pod startup if PostgreSQL unavailable or schema creation fails

For manual deployments:
```bash
export PGHOST=postgres-host PGPORT=5432 PGUSER=ldapsync \
  PGDATABASE=ldapsync PGPASSWORD=password
bash db/init-schema.sh
```

#### Secret Management

The PostgreSQL password is auto-generated and preserved using:

1. **Helm Lookup**: Detects existing secrets from previous installations
2. **Keep Annotation**: `helm.sh/resource-policy: keep` prevents
   deletion during `helm uninstall`
3. **Auto-Generation**: Random 32-character password on first install

The secret is named `<release-name>-postgres` with key
`postgres-password`.

**Important**: When using a custom release name (other than "ldap-sync"),
you must set `postgres.auth.existingSecret`:

```bash
helm install my-release ./chart \
  --set postgres.auth.existingSecret=my-release-postgres \
  [other settings...]
```

**Behavior:**
- **Helm Upgrade**: Existing password reused, no data loss
- **Helm Uninstall + Reinstall**: Secret preserved, data persists if
  using persistent volumes
- **Complete Cleanup**: Manual deletion required

To completely remove everything:
```bash
helm uninstall ldap-sync
kubectl delete secret ldap-sync-postgres -n <namespace>
kubectl delete pvc -l app.kubernetes.io/instance=ldap-sync -n <namespace>
```

#### Using a Custom Password

To use a custom password instead of auto-generated:

1. Create secret before installing:
   ```bash
   kubectl create secret generic my-custom-secret \
     --from-literal=password='my-secure-password' \
     -n <namespace>
   ```

2. Set in values.yaml:
   ```yaml
   postgres:
     auth:
       existingSecret: "my-custom-secret"
   ```

#### Database Schema

The `searches` table structure:

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

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_searches_created_at
  ON searches(created_at);
CREATE INDEX IF NOT EXISTS idx_searches_updated_at
  ON searches(updated_at);
```

Schema is idempotent - init container can run multiple times safely.

#### Helm Configuration

```yaml
postgres:
  enabled: true
  auth:
    username: ldapsync
    database: ldapsync
    existingSecret: ""  # Optional: use custom secret
  primary:
    persistence:
      enabled: true
      size: 8Gi
```

### Disable Persistence

Set `postgres.enabled: false` in Helm values or omit database section in
config file.

## API Usage

### Create a Search

```bash
curl -X POST http://localhost:5500/search \
  -d "id=users" \
  -d "filter=(objectClass=person)" \
  -d "refresh=60" \
  -d "baseDN=ou=users,dc=example,dc=org"
```

### List All Searches

```bash
curl http://localhost:5500/search
```

### Get Search Results

```bash
# Simple (DN only)
curl http://localhost:5500/results/users

# Full (DN + content)
curl http://localhost:5500/results/users?full=true
```

### Update Search

```bash
curl -X PUT http://localhost:5500/search/users \
  -d "filter=(objectClass=inetOrgPerson)" \
  -d "refresh=120" \
  -d "baseDN=ou=people,dc=example,dc=org"
```

### Delete Search

```bash
curl -X DELETE http://localhost:5500/search/users
```

### Update Log Level

```bash
curl -X PUT http://localhost:5500/loglevel \
  -H "Content-Type: application/json" \
  -d '{"level": "debug"}'
```

## Hook Development

Hooks are independent services that transform LDAP entries. They receive
entries via HTTP POST and return transformations.

### Hook Request Format

```json
{
  "dn": "uid=user1,ou=users,dc=example,dc=org",
  "content": {
    "uid": "user1",
    "cn": "User One",
    "objectClass": ["person", "inetOrgPerson"]
  }
}
```

### Hook Response Format

```json
{
  "transformed": [
    {
      "dn": "uid=user1,ou=people,dc=example,dc=org",
      "content": {
        "uid": "user1",
        "cn": "User One",
        "displayName": "User, One",
        "objectClass": ["person", "inetOrgPerson"]
      }
    }
  ],
  "derived": [
    {
      "id": "user1-groups",
      "filter": "(member=uid=user1,ou=users,dc=example,dc=org)",
      "refresh": 60,
      "baseDN": "ou=groups,dc=example,dc=org",
      "oneshot": false
    }
  ],
  "dependencies": [
    "ou=people,dc=example,dc=org"
  ],
  "reset": false
}
```

**Fields:**
- `transformed`: Array of transformed entries to write to target LDAP
- `derived`: Array of new search specifications to create
- `dependencies`: Array of DNs that must exist before writing entry
- `reset`: Legacy field to clear internal search results

### Example Hooks

Two example hooks are included:

- `hooks/ordrd-group-x/`: Processes ORDRD groups, UNC users, and POSIX
  groups with pid-to-uid mapping
- `hooks/unc-group-x/`: Similar with template variable support for
  dependency resolution

## Database Backup & Restore

### Backup Searches

```bash
kubectl exec -it <postgres-pod> -n <namespace> -- \
  pg_dump -U ldapsync ldapsync > searches-backup.sql
```

### Restore Searches

```bash
kubectl exec -i <postgres-pod> -n <namespace> -- \
  psql -U ldapsync ldapsync < searches-backup.sql
```

## Monitoring

### Health Probes

- **Liveness**: `GET /healthz` - Returns OK if application is running
- **Readiness**: `GET /readyz` - Returns OK if ready to serve traffic

### Logs

Log levels: `debug`, `info`, `warn`, `error`

Set at startup:
```bash
./ldap-sync --loglevel debug
```

Or at runtime via API:
```bash
curl -X PUT http://localhost:5500/loglevel \
  -H "Content-Type: application/json" \
  -d '{"level": "debug"}'
```

## Troubleshooting

### Init Container Fails

If the init container fails (pod stuck in Init:0/1):

1. Check PostgreSQL is running:
   ```bash
   kubectl get pods -l app.kubernetes.io/name=postgres
   ```

2. Check init container logs:
   ```bash
   kubectl logs <pod-name> -c init-db-schema
   ```

3. Common issues:
   - PostgreSQL not ready within 60 seconds (check postgres pod status)
   - Connection refused (verify postgres service exists)
   - Authentication failed (check postgres-credentials secret)

### Searches Not Persisting

1. Check PostgreSQL is enabled: `postgres.enabled: true`
2. Verify database config in ConfigMap:
   ```bash
   kubectl get configmap <release>-ldap-sync-config -o yaml
   ```
3. Verify secret is mounted:
   ```bash
   kubectl exec <pod> -- ls -la /etc/ldap-sync/secrets/
   ```
4. Verify init container completed:
   ```bash
   kubectl describe pod <pod-name> | grep -A 10 "Init Containers"
   ```
5. Check application logs for database connection errors

### Password Issues After Reinstall

If you encounter authentication errors after reinstalling:

1. Check if secret exists:
   ```bash
   kubectl get secret <release>-postgres-credentials -n <namespace>
   ```

2. If secret was deleted, you'll need to either:
   - Restore from a database backup
   - Delete the PVC and start fresh (data loss)

### Migration from Non-Persistent Setup

1. Export existing searches via API before upgrading
2. Upgrade Helm chart with `postgres.enabled: true`
3. Recreate searches via API (they will now be persisted)

## Development

### Prerequisites

- Go 1.23+
- Docker
- Helm 3
- kubectl
- Access to LDAP servers

### Running Tests

```bash
go test ./...
```

### Generating Swagger Docs

```bash
swag init -g main.go --output ./docs
```

## Helm Chart

### Values

Key configuration options in `chart/values.yaml`:

```yaml
# Replica count
replicaCount: 1

# Image configuration
image:
  repository: containers.renci.org/helxplatform/ldap-sync
  tag: "latest"
  pullPolicy: IfNotPresent

# Log level
loglevel: "info"

# LDAP configuration
config:
  source:
    url: ""
    bindDN: "cn=admin,dc=example,dc=org"
    bindPassword: ""
    baseDN: "dc=example,dc=org"
  target:
    url: ""
    bindDN: "cn=admin,dc=example,dc=org"
    bindPassword: ""
    baseDN: "dc=example,dc=org"
  hooks: []

# PostgreSQL configuration
postgres:
  enabled: true
  auth:
    username: ldapsync
    database: ldapsync
  primary:
    persistence:
      enabled: true
      size: 8Gi
  sslmode: disable
```

## License

[Add your license here]

## Contributing

[Add contribution guidelines here]

## Support

For issues and questions:
- GitHub Issues: [Add your issues URL]
- Documentation: See `CLAUDE.md` and `db/README.md`
