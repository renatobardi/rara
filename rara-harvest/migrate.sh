#!/bin/bash

# migrate.sh
# Database migration script for rara-harvest
# Usage: ./migrate.sh [cleanup|migrate|reset]

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Helper functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if DATABASE_URL is set
if [ -z "$DATABASE_URL" ]; then
    log_error "DATABASE_URL environment variable is not set"
    echo "Please set: export DATABASE_URL='postgresql://user:password@host:port/database'"
    exit 1
fi

# Parse command
COMMAND="${1:-migrate}"

case "$COMMAND" in
    cleanup)
        log_warn "🗑️  CLEANING UP DATABASE - This will delete all data!"
        read -p "Are you sure? Type 'yes' to confirm: " -r CONFIRM
        if [ "$CONFIRM" != "yes" ]; then
            log_info "Cleanup cancelled"
            exit 0
        fi

        log_info "Executing cleanup.sql..."
        psql "$DATABASE_URL" -f cleanup.sql
        log_info "✅ Cleanup complete"
        ;;

    migrate)
        log_info "Running migrations..."

        # Check if migrations directory exists
        if [ ! -d "migrations" ]; then
            log_error "migrations directory not found"
            exit 1
        fi

        # Run all migrations in order
        for migration in migrations/*.sql; do
            if [ -f "$migration" ]; then
                log_info "Running: $(basename $migration)..."
                psql "$DATABASE_URL" -f "$migration"
                log_info "✅ $(basename $migration) completed"
            fi
        done

        log_info "✅ All migrations completed successfully"
        ;;

    reset)
        log_warn "🔄 RESETTING DATABASE - This will delete and recreate all tables!"
        read -p "Are you sure? Type 'yes' to confirm: " -r CONFIRM
        if [ "$CONFIRM" != "yes" ]; then
            log_info "Reset cancelled"
            exit 0
        fi

        log_info "Cleaning up..."
        psql "$DATABASE_URL" -f cleanup.sql

        log_info "Running migrations..."
        for migration in migrations/*.sql; do
            if [ -f "$migration" ]; then
                log_info "Running: $(basename $migration)..."
                psql "$DATABASE_URL" -f "$migration"
            fi
        done

        log_info "✅ Database reset complete"
        ;;

    *)
        echo "Usage: ./migrate.sh [command]"
        echo ""
        echo "Commands:"
        echo "  migrate  - Run all pending migrations (default)"
        echo "  cleanup  - Delete all tables and data (⚠️  DESTRUCTIVE)"
        echo "  reset    - Cleanup + Run migrations (⚠️  DESTRUCTIVE)"
        echo ""
        echo "Environment:"
        echo "  DATABASE_URL - PostgreSQL connection string (required)"
        echo ""
        echo "Example:"
        echo "  export DATABASE_URL='postgresql://user:pass@host:5432/dbname'"
        echo "  ./migrate.sh migrate"
        exit 1
        ;;
esac
