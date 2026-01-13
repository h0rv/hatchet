# Local Development Setup (Mac)

Run Hatchet locally without Docker using the `--local` flag.

## Prerequisites

- Go 1.21+
- PostgreSQL (via Homebrew: `brew install postgresql@17`)

## Quick Start

```bash
# 1. Start PostgreSQL (if not already running)
brew services start postgresql@17

# 2. Create the database and set timezone to UTC
createdb hatchet
psql hatchet -c "ALTER DATABASE hatchet SET TIMEZONE='UTC'"

# 3. Build all required binaries
go build -o bin/hatchet ./cmd/hatchet-cli && \
go build -o bin/hatchet-api ./cmd/hatchet-api && \
go build -o bin/hatchet-engine ./cmd/hatchet-engine && \
export PATH="$PWD/bin:$PATH"

# 4. Start the server
hatchet server start --local

# 5. (In another terminal) Start the TUI
hatchet tui
```

## Step-by-Step

### 1. PostgreSQL Setup

```bash
# Start PostgreSQL
brew services start postgresql@17

# Create database (default connection: localhost:5432, current user, no password)
createdb hatchet

# Set timezone to UTC (required by Hatchet)
psql hatchet -c "ALTER DATABASE hatchet SET TIMEZONE='UTC'"

# Verify it works
psql hatchet -c "SELECT 1"
```

If you need a specific user/password:
```bash
psql postgres -c "CREATE USER hatchet WITH PASSWORD 'hatchet';"
psql postgres -c "CREATE DATABASE hatchet OWNER hatchet;"
psql postgres -c "GRANT ALL PRIVILEGES ON DATABASE hatchet TO hatchet;"
psql hatchet -c "ALTER DATABASE hatchet SET TIMEZONE='UTC'"
```

### 2. Build Binaries

```bash
# Build all three required binaries
go build -o bin/hatchet ./cmd/hatchet-cli
go build -o bin/hatchet-api ./cmd/hatchet-api
go build -o bin/hatchet-engine ./cmd/hatchet-engine

# Add to PATH for current session
export PATH="$PWD/bin:$PATH"
```

### 3. Start the Server

```bash
# Default: connects to postgresql://localhost:5432/hatchet (works with Homebrew Postgres)
hatchet server start --local

# With explicit connection string (if you have a password)
hatchet server start --local --database-url "postgresql://user:pass@localhost:5432/hatchet"

# With custom ports (useful if running Docker Hatchet alongside)
hatchet server start --local --api-port 9080 --grpc-port 9077 --healthcheck-port 9733
```

**Default ports:**
- API: 8080
- gRPC: 7077
- Healthcheck: 8733

On first run, this will:
- Generate encryption keys (stored in `~/.hatchet/local/`)
- Run database migrations
- Seed admin user (`admin@example.com` / `Admin123!!`)
- Start `hatchet-api` on port 8080
- Start `hatchet-engine`
- Create a CLI profile named "local"

### 4. Verify It's Running

```bash
# Check API health
curl http://localhost:8080/api/ready

# Check gRPC port
nc -zv localhost 7077

# List profiles
./bin/hatchet profile list
```

### 5. Use the TUI

```bash
./bin/hatchet tui
```

### 6. Stop the Server

```bash
./bin/hatchet server stop
```

## Configuration

Config and state are stored in `~/.hatchet/local/`:
- `keys.json` - Encryption keys (generated once, reused)
- `state.json` - Running process PIDs
- `database.yaml` - Database config
- `server.yaml` - Server config

## Troubleshooting

### "postgres not accessible"
```bash
# Check if PostgreSQL is running
brew services list | grep postgresql

# Start it
brew services start postgresql@17

# Check connection
psql hatchet -c "SELECT 1"
```

### "timezone is set to 'America/New_York'" (or other non-UTC timezone)
```bash
psql hatchet -c "ALTER DATABASE hatchet SET TIMEZONE='UTC'"
```

### "hatchet-api: command not found"
```bash
# Ensure binaries are built and in PATH
export PATH="$PWD/bin:$PATH"
which hatchet-api
```

### Port already in use
```bash
# Use different ports (e.g., if Docker Hatchet is running on defaults)
hatchet server start --local --api-port 9080 --grpc-port 9077 --healthcheck-port 9733
```

### Reset everything
```bash
# Stop server
hatchet server stop

# Drop and recreate database
dropdb hatchet
createdb hatchet
psql hatchet -c "ALTER DATABASE hatchet SET TIMEZONE='UTC'"

# Remove config (will regenerate keys on next start)
rm -rf ~/.hatchet/local/

# Start fresh
hatchet server start --local
```

## Running a Worker

Once the server is running, you can run workers against it:

```bash
# Python example
cd examples/python/quickstart
poetry install
export HATCHET_CLIENT_TOKEN=$(./bin/hatchet profile show -n local | grep Token | awk '{print $2}')
export HATCHET_CLIENT_TLS_STRATEGY=none
poetry run python worker.py
```

## Differences from Docker Mode

| Feature | Docker Mode | Local Mode |
|---------|-------------|------------|
| Web UI | Yes (port 8888) | No (headless) |
| PostgreSQL | Managed container | Your local instance |
| Setup | Just Docker | Build binaries + Postgres |
| Ports | 8888 (dashboard), 7077 (gRPC) | 8080 (API), 7077 (gRPC) |
