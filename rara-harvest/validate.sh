#!/bin/bash

# validate.sh
# Validates that the database schema is correctly set up
# Usage: ./validate.sh

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Helper functions
log_info() {
    echo -e "${GREEN}✓${NC} $1"
}

log_error() {
    echo -e "${RED}✗${NC} $1"
}

log_check() {
    echo -e "${BLUE}→${NC} Checking $1..."
}

# Check if DATABASE_URL is set
if [ -z "$DATABASE_URL" ]; then
    log_error "DATABASE_URL environment variable is not set"
    exit 1
fi

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Database Schema Validation"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Check 1: Database Connection
log_check "database connection"
if psql "$DATABASE_URL" -c "SELECT 1" > /dev/null 2>&1; then
    log_info "Connected to database"
else
    log_error "Cannot connect to database"
    exit 1
fi

# Check 2: target_channels table
log_check "target_channels table"
if psql "$DATABASE_URL" -c "\dt public.target_channels" | grep -q "target_channels"; then
    log_info "target_channels table exists"
else
    log_error "target_channels table not found"
    exit 1
fi

# Check 3: channel_videos table
log_check "channel_videos table"
if psql "$DATABASE_URL" -c "\dt public.channel_videos" | grep -q "channel_videos"; then
    log_info "channel_videos table exists"
else
    log_error "channel_videos table not found"
    exit 1
fi

# Check 4: Indexes
# Two explicit performance indexes are expected (idx_videos_published_at,
# idx_videos_channel_id). UNIQUE/PK-backed indexes are not named idx_*.
log_check "indexes"
INDEXES=$(psql "$DATABASE_URL" -c "\di" | grep -E "idx_" | wc -l)
if [ "$INDEXES" -ge 2 ]; then
    log_info "Found $INDEXES indexes (expected: 2+)"
else
    log_error "Missing indexes (found: $INDEXES, expected: 2+)"
    exit 1
fi

# Check 5: Foreign key constraint
log_check "foreign key constraints"
if psql "$DATABASE_URL" -c "\d public.channel_videos" | grep -q "target_channels"; then
    log_info "Foreign key constraint exists"
else
    log_error "Foreign key constraint not found"
    exit 1
fi

# Check 6: Table columns
log_check "column structure"
TARGET_COLS=$(psql "$DATABASE_URL" -c "\d public.target_channels" | grep -c "│")
VIDEOS_COLS=$(psql "$DATABASE_URL" -c "\d public.channel_videos" | grep -c "│")

if [ "$TARGET_COLS" -ge 4 ] && [ "$VIDEOS_COLS" -ge 5 ]; then
    log_info "Column structure is correct"
else
    log_error "Unexpected column structure"
    exit 1
fi

# Check 7: Data (if any exists)
log_check "data"
CHANNELS=$(psql "$DATABASE_URL" -t -c "SELECT COUNT(*) FROM target_channels")
VIDEOS=$(psql "$DATABASE_URL" -t -c "SELECT COUNT(*) FROM channel_videos")

if [ "$CHANNELS" -eq 0 ] && [ "$VIDEOS" -eq 0 ]; then
    log_info "No data (fresh database)"
else
    log_info "Found $CHANNELS channels and $VIDEOS videos"
fi

# Summary
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${GREEN}✅ All validation checks passed!${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Database is ready for rara-harvest"
echo ""
echo "Next steps:"
echo "  • Load test data: psql \$DATABASE_URL -f seed.sql"
echo "  • Start harvesting: cd rara-harvest && make test"
echo "  • Deploy: ./deploy.sh"
echo ""
