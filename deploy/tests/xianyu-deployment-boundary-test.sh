#!/usr/bin/env bash
set -euo pipefail

for compose_file in \
  deploy-config/compose.yml \
  deploy/docker-compose.yml \
  deploy/docker-compose.local.yml \
  deploy/docker-compose.standalone.yml; do
  if grep -qF '${BIND_HOST:-0.0.0.0}' "$compose_file"; then
    printf '%s must not default to a public bind host\n' "$compose_file" >&2
    exit 1
  fi
  grep -qF '${BIND_HOST:-127.0.0.1}' "$compose_file" || {
    printf '%s must default the application port to loopback\n' "$compose_file" >&2
    exit 1
  }
done

grep -qF '@internal_api path /api/v1/internal/*' deploy/Caddyfile
grep -qF 'respond @internal_api 403' deploy/Caddyfile
grep -qF 'deny all' deploy-config/nginx/corealgos.conf

if command -v docker >/dev/null 2>&1; then
  workdir=$(mktemp -d)
  cleanup() {
    docker rm -f xianyu-caddy-smoke >/dev/null 2>&1 || true
    rm -rf "$workdir"
  }
  trap cleanup EXIT
  cat > "$workdir/Caddyfile" <<'CADDY'
{
  admin off
  auto_https off
}
http://127.0.0.1:6081 {
  @internal_api path /api/v1/internal/*
  respond @internal_api 403
  reverse_proxy 127.0.0.1:1
}
CADDY
  docker run --rm -d --name xianyu-caddy-smoke \
    -p 127.0.0.1:6081:6081 \
    -v "$workdir/Caddyfile:/etc/caddy/Caddyfile:ro" \
    caddy:2-alpine caddy run --config /etc/caddy/Caddyfile >/dev/null
  for _ in $(seq 1 50); do
    if curl -sS -o /dev/null http://127.0.0.1:6081/api/v1/internal/xianyu/redeem-codes/claim; then
      break
    fi
    sleep 0.1
  done
  status_code=$(curl -sS -o /dev/null -w '%{http_code}' \
    http://127.0.0.1:6081/api/v1/internal/xianyu/redeem-codes/claim)
  if [[ "$status_code" != 403 ]]; then
    printf 'expected internal route HTTP 403, got %s\n' "$status_code" >&2
    exit 1
  fi
else
  printf 'docker unavailable; skipped real HTTP Caddy boundary test\n' >&2
fi

printf 'xianyu deployment boundary test passed\n'
