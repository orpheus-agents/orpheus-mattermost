COMPOSE = docker compose
RUN = $(COMPOSE) --profile tools run --rm --no-deps tools
export ORPHEUS_UID ?= $(shell if [ "$$(uname -s)" = Linux ]; then id -u; else echo 1000; fi)
export ORPHEUS_GID ?= $(shell if [ "$$(uname -s)" = Linux ]; then id -g; else echo 1000; fi)
export ORPHEUS_TOOLS_IMAGE := orpheus-mattermost-tools:$(shell cksum < .docker/tools/Dockerfile | awk '{print $$1 "-" $$2}')-$(ORPHEUS_UID)-$(ORPHEUS_GID)
.PHONY: tools tools-build start stop fix gofix tidy-check gofix-check lint deadcode test test-go-race test-live vuln build check smoke

tools:
	@docker image inspect "$(ORPHEUS_TOOLS_IMAGE)" >/dev/null 2>&1 || $(COMPOSE) build tools
tools-build:
	$(COMPOSE) build tools
start:
	$(COMPOSE) up -d --build --wait app
stop:
	$(COMPOSE) --profile tools down
fix: tools
	$(RUN) sh -ec 'gofmt -w cmd internal tools; goimports -w cmd internal tools'
gofix: tools
	$(RUN) go fix ./...
tidy-check: tools
	$(RUN) go mod tidy -diff
gofix-check: tools
	$(RUN) sh -ec 'f=$$(mktemp); trap '\''rm -f "$$f"'\'' EXIT; go fix -diff ./... > "$$f"; if test -s "$$f"; then cat "$$f"; exit 1; fi'
lint: tools
	$(RUN) sh -ec 'files=$$(gofmt -l cmd internal tools); if test -n "$$files"; then printf "%s\n" "$$files"; exit 1; fi'
	$(RUN) go vet ./...
	$(RUN) golangci-lint run --build-tags live ./...
	$(RUN) hadolint .docker/app/dev/Dockerfile .docker/app/prod/Dockerfile .docker/tools/Dockerfile
deadcode: tools
	$(RUN) sh -ec 'f=$$(mktemp); trap '\''rm -f "$$f"'\'' EXIT; go tool deadcode -test -tags=live ./... > "$$f"; if test -s "$$f"; then cat "$$f"; exit 1; fi'
test: tools
	$(RUN) python3 -I -m unittest discover -s internal/sandbox -p "test_*.py"
	$(RUN) go test ./...
test-go-race: tools
	$(RUN) go test -race ./...
test-live: tools
	$(COMPOSE) --profile tools run --rm --no-deps --env-from-file .env.live tools go test -tags live -count=1 -timeout=20m ./...
vuln: tools
	$(RUN) govulncheck ./...
	$(RUN) trivy fs --no-progress --db-repository ghcr.io/aquasecurity/trivy-db:2 --db-repository mirror.gcr.io/aquasec/trivy-db:2 --scanners vuln,misconfig --exit-code 1 --severity HIGH,CRITICAL --skip-dirs .git --skip-dirs .live .
build: tools
	$(RUN) go build -trimpath -o /tmp/orpheus-mattermost ./cmd/orpheus-mattermost
check: tidy-check gofix-check lint deadcode test test-go-race vuln build
smoke: tools
	$(RUN) env CGO_ENABLED=0 go build -trimpath -o bin/smoke-server ./tools/smoke-server
	docker build -f .docker/app/prod/Dockerfile -t orpheus-mattermost:local .
	sh tools/smoke.sh
