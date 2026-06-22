// rara-extract — the already-text extractor, a bridge-total claim-worker on the rara-addon SDK.
//
// Some lanes arrive as text, not audio: an email body, a pasted LinkedIn post. They need no ASR —
// they need NORMALIZATION. `extrair` is that to-text capability: strip the noise the source carries
// (HTML markup, an email signature, quoted-reply history) and leave the human-written message the
// gates and distill should judge. The result lands in the SAME `transcripts` store the ASR worker
// (rara-transcribe) writes, keyed on (source_ref, source_type) — the universal "to-text" convention the
// gate_rico/distill lookups chain on. extrair is a peer of transcrever, not a special case.
//
// ONE app serves ALL the text providers purely by config (GLEAN_PROVIDER): `winnow-cloud` cleans an
// email body, `scrub-cloud` normalizes a post, `glean-cloud` cleans a feed article. The handler
// picks the cleaner + the to-text source_type by the item's lane, so a single codebase covers every
// text lane — codebases ≪ providers.
//
// Per claimed item: 1) read the raw body from the collector's domain table (emails.body keyed on
// message_id, linkedin_posts.body keyed on url, news_items.body|excerpt keyed on url — a cross-app
// SELECT, the 1.0 isolation convention);
// 2) run the PURE, deterministic cleaner; 3) upsert the cleaned text as a `transcripts` row and
// return its id as the step's output_ref. A body that cleans to nothing is benign no-content
// (`empty`): the step is done and the item is curated out rather than marched into a distill that
// must fail. A source row that has not landed yet is RETRYABLE: the SDK requeues up to the cap
// instead of failing a good item against a collector race.
//
// The cleaning (cleanEmailText / cleanPostText and the html helpers) is PURE — zero I/O — so the
// whole normalization policy is unit-tested. The I/O edge — the cross-app body read and the
// transcripts upsert — lives in appDB; tests use an in-memory mock. The SDK (addon.Run) owns the
// claim/heartbeat/result/requeue/poke around it; this process only supplies the extrair domain.
package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	addon "rara-addon"
)

// capExtrair is the logical task this app serves (the capability name in the schema; never renamed —
// only the app name "glean" is evocative). The reconciler routes extrair steps to this worker's
// provider; addon.Run claims them by (capability, assigned_provider).
const capExtrair = "extrair"

// The providers this one app serves, selected by GLEAN_PROVIDER. Each is pinned to a text lane
// by its seed `accepts` so the email extractor never grabs a post and vice-versa.
const (
	provExtrairEmail    = "winnow-cloud" // email HTML/quote/signature cleaner
	provExtrairLinkedIn = "scrub-cloud"  // LinkedIn post normalizer
	provExtrairNews     = "glean-cloud"  // feed-article HTML/boilerplate cleaner
)

// items.lane — the body read and the to-text source_type are lane-aware (a different domain table
// per lane), while the cleaning stays a pure function chosen by lane.
const (
	laneEmail    = "email"
	laneLinkedIn = "linkedin"
	laneNews     = "news"
)

// transcripts.engine labels for an extrair to-text row (a NOT NULL column), distinguishing extractor
// output from real ASR engines, and the lanes from each other in the audit trail.
const (
	emailEngine    = "rara-extract/winnow"
	linkedinEngine = "rara-extract/scrub"
	newsEngine     = "rara-extract/glean"
)

func isValidProvider(s string) bool {
	return s == provExtrairEmail || s == provExtrairLinkedIn || s == provExtrairNews
}

// ---------------------------------------------------------------------------
// The pure cleaners (zero I/O — the whole normalization policy, unit-tested)
// ---------------------------------------------------------------------------

var (
	// reScriptStyle drops <script>/<style> blocks whole (their content is never message text).
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	// reBlockTag turns block-level boundaries into newlines so stripped HTML stays readable.
	reBlockTag = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/h[1-6]|/li)\s*/?>`)
	// reAnyTag removes every remaining tag.
	reAnyTag = regexp.MustCompile(`(?s)<[^>]+>`)
	// reHTMLish detects whether a body is HTML (so plain-text bodies are left untouched).
	reHTMLish = regexp.MustCompile(`(?i)<(html|body|div|p|br|table|span|a|img|head)\b`)
	// reBlankRun collapses 3+ consecutive newlines to a single blank line.
	reBlankRun = regexp.MustCompile(`\n{3,}`)
	// reAttribution matches a reply attribution line ("On <date>, <X> wrote:" / pt "escreveu:")
	// after which the quoted original thread follows — everything from there is dropped.
	reAttribution = regexp.MustCompile(`(?i)^(on\b.*\bwrote:|.*\bescreveu:|-{2,}\s*original message\s*-{2,}|from:\s.+)$`)
)

// cleanerForLane resolves the lane's deterministic cleaner AND its transcripts.engine label in one
// place, so the two can never drift (a new lane is added to both at once). email needs
// signature/quoted-reply stripping; linkedin and news are the lighter normalize-only path (a post
// or article has neither). An unsupported lane never reaches here: appDB.ReadSource rejects it first.
func cleanerForLane(lane string) (clean func(string) string, engine string) {
	switch lane {
	case laneEmail:
		return cleanEmailText, emailEngine
	case laneNews:
		// News articles are already text; like a post they have no signature/quote to strip — just
		// strip HTML/boilerplate and collapse blanks (the cleanPostText path).
		return cleanPostText, newsEngine
	default:
		return cleanPostText, linkedinEngine
	}
}

// cleanEmailText returns the human-written body of an email: HTML stripped (if any), the
// signature (everything after the standard "-- " delimiter) removed, and quoted-reply history
// (lines beginning with ">" and everything after a reply attribution) dropped. Deterministic
// and cheap — the gates/distill then judge the message itself, not its decoration.
func cleanEmailText(raw string) string {
	var out []string
	for _, line := range splitClean(raw) {
		trimmed := strings.TrimSpace(line)
		// Signature delimiter ("-- ") or a reply attribution starts the noise — everything below
		// it (the signature / the quoted original thread) is dropped.
		if trimmed == "--" || reAttribution.MatchString(trimmed) {
			break
		}
		// Quoted lines ("> ...") are prior messages, not this one.
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		out = append(out, line)
	}
	return joinClean(out)
}

// cleanPostText normalizes a pasted LinkedIn post into the text the gates/distill judge. Posts
// are already text, so this is lighter than email's cleaner: strip HTML if the body carries any
// (so a future Bright Data collector that yields HTML needs no change), then collapse blank
// runs and trim. It does NOT strip signatures/quotes (a post has neither). Deterministic and
// cheap — the real "to-text" work the lane does.
func cleanPostText(raw string) string {
	return joinClean(splitClean(raw))
}

// splitClean is the shared head of both cleaners: strip HTML if the body carries any, then split
// into right-trimmed lines for the cleaner to filter.
func splitClean(raw string) []string {
	s := raw
	if reHTMLish.MatchString(s) {
		s = stripHTML(s)
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t\r")
	}
	return lines
}

// joinClean is the shared tail: rejoin, collapse blank runs to a single blank line, and trim.
func joinClean(lines []string) string {
	return strings.TrimSpace(reBlankRun.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// stripHTML reduces an HTML body to plain text: drop script/style, turn block boundaries into
// newlines, remove the remaining tags, and unescape entities.
func stripHTML(s string) string {
	s = reScriptStyle.ReplaceAllString(s, "")
	s = reBlockTag.ReplaceAllString(s, "\n")
	s = reAnyTag.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}

// ---------------------------------------------------------------------------
// The handler — the domain logic behind addon.Run
// ---------------------------------------------------------------------------

// GleanStore is the DOMAIN persistence seam the handler needs (distinct from the CONTRACT store,
// which is the SDK's addon.NewPgxStore over item_steps/providers/items). The real implementation
// (appDB) talks to Neon; tests use an in-memory mock.
//
//   - ReadSource fetches the raw body from the collector's domain table, lane-aware. ready=false
//     means the source row has not landed yet (a collector race) -> the handler requeues.
//   - WriteText upserts the cleaned text into the shared transcripts store and returns its id (the
//     OutputRef on the step) — the to-text convention gate_rico/distill consume.
type GleanStore interface {
	ReadSource(ctx context.Context, item addon.Item) (raw string, ready bool, err error)
	WriteText(ctx context.Context, sourceType, sourceRef, text, engine string) (int, error)
}

// gleanHandler is the domain logic behind addon.Run: turn ONE already-text item into a to-text
// artifact. The SDK owns the claim/heartbeat/result/requeue/poke around it; this only reads, cleans
// and writes.
//
//  1. read the item's raw body (lane-aware) — not landed yet -> ErrRetryable (the SDK requeues);
//  2. run the PURE, deterministic cleaner for the lane;
//  3. upsert the cleaned text as a transcripts row and report its id as the step OutputRef.
//
// A body that cleans to nothing is benign no-content: the step is done (Filtered) and the SDK
// curates the item out, rather than marching it into a distill that must fail. A read or write
// error is terminal (surfaced as-is); only the not-yet-landed source is retryable.
func gleanHandler(store GleanStore) addon.Handler {
	return func(ctx context.Context, item addon.Item, _ addon.Step) (addon.Result, error) {
		raw, ready, err := store.ReadSource(ctx, item)
		if err != nil {
			return addon.Result{}, fmt.Errorf("extrair %s: read source: %w", item.SourceRef, err)
		}
		if !ready {
			// The collector has not written the body yet (a race against ingest): requeue rather
			// than fail a good item for good.
			return addon.Result{}, fmt.Errorf("extrair %s: source not ready: %w", item.SourceRef, addon.ErrRetryable)
		}

		clean, engine := cleanerForLane(item.Lane)
		text := clean(raw)
		id, err := store.WriteText(ctx, item.Lane, item.SourceRef, text, engine)
		if err != nil {
			return addon.Result{}, fmt.Errorf("extrair %s: write to-text: %w", item.SourceRef, err)
		}

		filtered := strings.TrimSpace(text) == ""
		log.Printf("extrair %s (%s) -> transcript %d (filtered=%v)", item.SourceRef, item.Lane, id, filtered)
		return addon.Result{OutputRef: strconv.Itoa(id), Filtered: filtered}, nil
	}
}

// ---------------------------------------------------------------------------
// Real domain database: Neon PostgreSQL via pgxpool
//
// appDB is the DOMAIN store: the cross-app body read (emails / linkedin_posts — the collector's
// tables, SELECT only, the 1.0 isolation convention) and the shared transcripts upsert. The
// CONTRACT store (item_steps/providers/items) is the SDK's addon.NewPgxStore over the same pool. A
// pool (not a single conn) backs both because the SDK heartbeats from a background goroutine while
// the drain loop claims — and *pgxpool.Pool is safe for concurrent use.
// ---------------------------------------------------------------------------

type appDB struct{ pool *pgxpool.Pool }

var _ GleanStore = (*appDB)(nil)

// ReadSource fetches the raw body for the item, dispatching by lane. A missing row is NOT fatal: it
// means the collector has not written the body yet (a race against ingest), so ready=false asks the
// handler to requeue. An empty body on an existing row is ready (it cleans to nothing -> the item is
// curated out).
func (db *appDB) ReadSource(ctx context.Context, item addon.Item) (string, bool, error) {
	var q string
	switch item.Lane {
	case laneEmail:
		q = `SELECT COALESCE(body, '') FROM emails WHERE message_id = $1`
	case laneLinkedIn:
		q = `SELECT COALESCE(body, '') FROM linkedin_posts WHERE url = $1`
	case laneNews:
		// Feed articles: body is the captured full text (NULL if not captured); fall back to the
		// excerpt the feed always carries. Keyed on the article url (= items.source_ref).
		q = `SELECT COALESCE(body, excerpt, '') FROM news_items WHERE url = $1`
	default:
		return "", false, fmt.Errorf("extrair: unsupported lane %q", item.Lane)
	}
	var body string
	switch err := db.pool.QueryRow(ctx, q, item.SourceRef).Scan(&body); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil // source row not landed yet -> retryable
	case err != nil:
		return "", false, err
	}
	return body, true, nil
}

// WriteText upserts the cleaned text into the shared transcripts table keyed on (source_ref,
// source_type) — the contract gate_rico/distill chain on. transcripts has no unique key for a
// non-youtube source, so it upserts manually (UPDATE, else INSERT): a retry re-cleans in place
// rather than duplicating. An empty text is stored with status='empty' (benign no-content; the
// handler then curates the item out).
//
// (A partial unique index on transcripts(source_ref, source_type) WHERE youtube_video_id IS NULL
// would make this a one-statement ON CONFLICT; recommended but not required — it stays the to-text
// owner's, rara-transcribe's, schema decision.)
func (db *appDB) WriteText(ctx context.Context, sourceType, sourceRef, text, engine string) (int, error) {
	status := "done"
	if strings.TrimSpace(text) == "" {
		status = "empty"
	}
	const upd = `
		UPDATE transcripts SET transcript = $3, status = $4, engine = $5
		WHERE source_ref = $1 AND source_type = $2
		RETURNING id`
	var id int
	switch err := db.pool.QueryRow(ctx, upd, sourceRef, sourceType, text, status, engine).Scan(&id); {
	case errors.Is(err, pgx.ErrNoRows):
		const ins = `
			INSERT INTO transcripts (source_type, source_ref, engine, transcript, status)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id`
		if err := db.pool.QueryRow(ctx, ins, sourceType, sourceRef, engine, text, status).Scan(&id); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Config & entrypoint
// ---------------------------------------------------------------------------

// buildGleanPoolConfig parses the DSN and forces simple protocol so pgx never caches
// prepared statements — required when DATABASE_URL points to a PgBouncer pooler endpoint.
func buildGleanPoolConfig(dbURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	return cfg, nil
}

// main wires the bridge-total claim-worker: the SDK (addon.Run) owns the queue protocol; this
// process only supplies the extrair domain (gleanHandler). One app serves all text providers by
// config: GLEAN_PROVIDER picks the concrete provider it serves (winnow-cloud | scrub-cloud |
// glean-cloud) so it claims only the steps the reconciler routed to it. Default is on_demand (drain
// once and exit,
// the woken Cloud Run job); a resident deploy opts into the long-running loop + symmetric activation
// via WORK_POLL_INTERVAL and/or POKE_ADDR + POKE_TOKEN.
func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatalf("DATABASE_URL environment variable is required")
	}
	provider := os.Getenv("GLEAN_PROVIDER")
	if !isValidProvider(provider) {
		log.Fatalf("GLEAN_PROVIDER must be %q, %q, or %q, got %q", provExtrairEmail, provExtrairLinkedIn, provExtrairNews, provider)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolCfg, err := buildGleanPoolConfig(dbURL)
	if err != nil {
		log.Fatalf("Failed to parse DATABASE_URL") // don't echo err — may contain DSN credentials
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer pool.Close()
	log.Printf("rara-extract worker %s/%s ready", capExtrair, provider)

	ac := addon.Config{
		Capability:   capExtrair,
		Provider:     provider,
		Store:        addon.NewPgxStore(pool),
		MaxAttempts:  addon.DefaultMaxAttempts,
		PollInterval: addon.EnvDuration("WORK_POLL_INTERVAL", 0),
		PokeAddr:     os.Getenv("POKE_ADDR"),
		PokeToken:    os.Getenv("POKE_TOKEN"),
	}
	if err := addon.Run(ctx, ac, gleanHandler(&appDB{pool: pool})); err != nil {
		log.Fatalf("rara-extract worker %s/%s: %v", capExtrair, provider, err)
	}
	log.Printf("rara-extract worker %s/%s: queue drained", capExtrair, provider)
}
