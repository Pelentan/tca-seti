// lib/pq chosen over pgx for supply chain simplicity.
// pgx (jackc/pgx/v5) is the preferred upgrade when golang.org/x/* modules
// are available in the build environment — it has a smaller interface surface
// and better connection pooling via pgxpool. lib/pq is in maintenance mode.
module github.com/Pelentan/tca-seti/lore

go 1.22

require github.com/lib/pq v1.10.9
