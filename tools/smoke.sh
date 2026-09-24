#!/bin/sh
set -eu
smoke_prefix="orpheus-mm-smoke-$$"
smoke_tmp=$(mktemp -d)
cleanup() {
  docker rm -f "$smoke_prefix-app" "$smoke_prefix-deps" >/dev/null 2>&1 || true
  docker network rm "$smoke_prefix" >/dev/null 2>&1 || true
  rm -rf "$smoke_tmp"
}
trap cleanup EXIT INT TERM
cp .env.dist "$smoke_tmp/app.env"
# Isolated local fixtures; the CI job never connects to external services.
cat >> "$smoke_tmp/app.env" <<'ENV'
ORPHEUS_BASE_URL=http://deps:8080
ORPHEUS_API_KEY=test
MATTERMOST_BOT_TOKEN=test
ENV
mkdir "$smoke_tmp/workflows"
sed 's|https://chat.example.com|http://deps:8080|' workflows/assistant.md > "$smoke_tmp/workflows/assistant.md"
chmod 755 "$smoke_tmp" "$smoke_tmp/workflows"
chmod 644 "$smoke_tmp/workflows/assistant.md"
docker run --rm --env-file "$smoke_tmp/app.env" -v "$smoke_tmp/workflows:/app/workflows:ro" orpheus-mattermost:local validate
if docker run --rm --env-file "$smoke_tmp/app.env" -e WORKFLOWS_DIR=/missing orpheus-mattermost:local validate >/dev/null 2>&1; then
  echo 'Missing workflows unexpectedly accepted' >&2
  exit 1
fi
docker network create "$smoke_prefix" >/dev/null
docker run -d --name "$smoke_prefix-deps" --network "$smoke_prefix" --network-alias deps --entrypoint /smoke-server -v "$PWD/bin/smoke-server:/smoke-server:ro" orpheus-mattermost:local >/dev/null
docker run -d --name "$smoke_prefix-app" --network "$smoke_prefix" --env-file "$smoke_tmp/app.env" -v "$smoke_tmp/workflows:/app/workflows:ro" orpheus-mattermost:local >/dev/null
smoke_attempt=0
until docker exec "$smoke_prefix-app" /orpheus-mattermost healthcheck --url http://127.0.0.1:8080/readyz >/dev/null 2>&1; do
  smoke_attempt=$((smoke_attempt+1))
  if [ "$smoke_attempt" -ge 20 ]; then docker logs "$smoke_prefix-app"; exit 1; fi
  sleep 1
done
docker stop -t 10 "$smoke_prefix-app" >/dev/null
smoke_exit=$(docker inspect -f '{{.State.ExitCode}}' "$smoke_prefix-app")
[ "$smoke_exit" = 0 ] || { echo "Unexpected SIGTERM exit: $smoke_exit" >&2; exit 1; }
echo 'Production image: config validation, readiness and graceful SIGTERM passed.'
