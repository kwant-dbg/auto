# Local development. Tests that need Postgres read AOE_TEST_DATABASE_URL and
# are skipped without it; `make db` starts a disposable pgvector container.
TEST_DB ?= postgres://postgres:postgres@localhost:55432/postgres?sslmode=disable
export GOTOOLCHAIN := local

.PHONY: build test test-unit vet fmt db db-stop

build:
	go build -o bin/ ./cmd/...

test:
	AOE_TEST_DATABASE_URL=$(TEST_DB) go test -race ./...

test-unit:
	go test ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

fmt:
	gofmt -w .

db:
	docker run -d --rm --name aoe-test-db -e POSTGRES_PASSWORD=postgres -p 55432:5432 pgvector/pgvector:pg16

db-stop:
	docker stop aoe-test-db
