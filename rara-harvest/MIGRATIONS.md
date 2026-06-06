# Database Migrations - rara-harvest

Complete guide to managing the Neon PostgreSQL database for rara-harvest.

## Overview

The database schema is managed through migrations stored in the `migrations/` directory. Each migration is a SQL file that can be applied independently.

```
migrations/
└── 001_initial_schema.sql    # Creates target_channels and channel_videos tables
```

## Files

### Migration Files

- **migrations/001_initial_schema.sql**
  - Creates `target_channels` table
  - Creates `channel_videos` table with foreign key
  - Creates performance indexes
  - Adds documentation comments

### Utility Scripts

- **migrate.sh** - Main migration runner (interactive, safe)
- **cleanup.sql** - Deletes all tables and data ⚠️
- **seed.sql** - Populates with test data (sample YouTube channels and videos)
- **schema.sql** - Original schema reference (legacy)

## Setup

### 1. Get Database Credentials

From Neon Console:
1. Go to [console.neon.tech](https://console.neon.tech)
2. Select your project
3. Copy connection string under "Connection string"

### 2. Set Environment Variable

```bash
export DATABASE_URL='postgresql://user:password@host:port/database'

# Verify it's set
echo $DATABASE_URL
```

### 3. Run Migrations

```bash
# Run all migrations
./migrate.sh migrate

# Or without arguments (migrate is default)
./migrate.sh
```

Expected output:
```
[INFO] Running migrations...
[INFO] Running: 001_initial_schema.sql...
[INFO] ✅ 001_initial_schema.sql completed
[INFO] ✅ All migrations completed successfully
```

## Usage

### Apply Migrations

```bash
./migrate.sh migrate
```

Creates:
- `target_channels` table (500 MB free tier allows ~100,000 channels)
- `channel_videos` table with indexes (500 MB allows ~1 million videos)
- Optimized indexes for queries

**When to use**: First time setup or after cleanup

### Load Test Data

```bash
psql $DATABASE_URL -f seed.sql
```

Inserts:
- 5 sample YouTube channels (some active, some inactive)
- 5 sample videos from those channels

**When to use**: Local development, testing the pipeline

### Clean Up Everything

```bash
./migrate.sh cleanup
```

⚠️ **WARNING**: This **permanently deletes all tables and data**

Prompts for confirmation:
```
[WARN] 🗑️  CLEANING UP DATABASE - This will delete all data!
Are you sure? Type 'yes' to confirm: yes
```

**When to use**: 
- Starting fresh
- Removing test data before production
- Resetting after testing

### Reset (Cleanup + Migrate)

```bash
./migrate.sh reset
```

⚠️ **WARNING**: Same as cleanup, then re-creates schema from migrations

Combines cleanup and migration in one command with safety prompt.

**When to use**: Full database reset with fresh schema

## Database Schema

### target_channels

Stores YouTube channels to harvest from.

```sql
id              SERIAL PRIMARY KEY
youtube_channel_id   VARCHAR(255) UNIQUE NOT NULL  -- e.g., "UCkRfArvrzheW2E7b6SVV2vA"
channel_name    VARCHAR(255) NOT NULL
active          BOOLEAN DEFAULT TRUE               -- Controls harvest inclusion
created_at      TIMESTAMPTZ DEFAULT NOW()
updated_at      TIMESTAMPTZ DEFAULT NOW()
```

**Indexes**:
- `youtube_channel_id` - Fast channel lookups

### channel_videos

Stores harvested videos.

```sql
id              SERIAL PRIMARY KEY
channel_id      INT NOT NULL (FOREIGN KEY)        -- Links to target_channels
youtube_video_id    VARCHAR(50) UNIQUE NOT NULL  -- e.g., "dQw4w9WgXcQ"
title           TEXT NOT NULL
url             TEXT NOT NULL                      -- Full YouTube watch URL
published_at    TIMESTAMPTZ NOT NULL              -- When published on YouTube
collected_at    TIMESTAMPTZ DEFAULT NOW()         -- When we harvested it
```

**Indexes**:
- `youtube_video_id` - Prevents duplicates (UNIQUE)
- `published_at DESC` - Fast sorting by publish date
- `channel_id` - Fast queries by channel
- Foreign key on `channel_id` with CASCADE delete

## Idempotency

All migrations use `IF NOT EXISTS` clauses, making them idempotent:

```bash
./migrate.sh migrate  # Run multiple times safely
./migrate.sh migrate  # No errors, tables already exist
./migrate.sh migrate  # Still works fine
```

## Troubleshooting

### Connection Error

```
psql: error: could not translate host name to address
```

**Fix**: Check `DATABASE_URL` format and endpoint is correct:
```bash
# Should be: postgresql://user:password@host:port/database
echo $DATABASE_URL
```

### Permission Denied

```
ERROR: permission denied for schema public
```

**Fix**: Ensure your database user has CREATE permissions. In Neon, the default owner should have permissions. If not:
```bash
# Contact Neon support or recreate the role with proper permissions
```

### Table Already Exists

Migrations use `IF NOT EXISTS`, so this won't error:
```bash
./migrate.sh migrate
# Safe to run multiple times
```

### psql Command Not Found

**Fix**: Install PostgreSQL client tools:
```bash
# macOS with Homebrew
brew install libpq
brew link libpq --force

# Or use Docker
docker run -it postgres:latest psql $DATABASE_URL
```

## Neon Free Tier Limits

- **Storage**: 500 MB
- **Connections**: 20 concurrent
- **CPU**: Shared, fair-use

**Estimates**:
- ~100,000 YouTube channels (with video metadata)
- ~1 million videos (with minimal metadata)
- Full daily harvest with 50 channels × 5 videos = < 1% storage usage

## Verification

Check schema after migrations:

```bash
psql $DATABASE_URL -c "
SELECT tablename FROM pg_tables 
WHERE schemaname = 'public' 
ORDER BY tablename;
"
```

Expected output:
```
     tablename
------------------
 channel_videos
 target_channels
```

Check row counts:

```bash
psql $DATABASE_URL -c "
SELECT 
  (SELECT COUNT(*) FROM target_channels) as channels,
  (SELECT COUNT(*) FROM channel_videos) as videos;
"
```

## Next Steps

1. **Setup**: Run `./migrate.sh` once
2. **Test**: Load test data with `psql $DATABASE_URL -f seed.sql`
3. **Deploy**: Push changes to git
4. **Use**: rara-harvest pipeline automatically uses this schema

---

**Status**: ✅ Production ready
**Last Updated**: 2026-06-05
