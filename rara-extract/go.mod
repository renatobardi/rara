module rara-extract

go 1.26.4

require (
	github.com/jackc/pgx/v5 v5.10.0
	rara-addon v0.0.0
)

// rara-addon is the bridge-total SDK, a sibling module in this monorepo. A replace directive (not
// a go.work) keeps rara-extract self-contained: it builds standalone in CI (cd rara-extract && go test)
// and the P2 Docker build needs the replace anyway. A local go.work (gitignored) is optional
// ergonomics; it is never committed.
replace rara-addon => ../rara-addon

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
