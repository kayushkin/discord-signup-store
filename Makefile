# No build tags here, unlike quote-store: this schema uses no FTS5 virtual
# table, so the stock mattn/go-sqlite3 build is enough. If a full-text index is
# ever added to schema.sql, add -tags sqlite_fts5 to every target below or the
# binary will build cleanly and die at boot with "no such module: fts5".

.PHONY: build test vet fmt check

build:
	go build -o discord-signup-store ./cmd/discord-signup-store

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: fmt vet test build
