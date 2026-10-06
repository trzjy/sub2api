# Vendored xianyu-auto-reply worker

This directory is the production source used to build the reviewed
`XIANYU_WORKER_IMAGE_DIGEST` artifact. It is vendored because the current
integration relies on internal account identity, final send receipt, and result
callback behavior beyond the upstream release.

## Services & ports

Single container runs three processes via `launcher/entrypoint.py`:

| Service     | Port | Source dir    |
|-------------|------|---------------|
| backend-web | 8089 | `backend-web/` |
| websocket   | 8090 | `websocket/`  |
| scheduler   | 8091 | `scheduler/`  |

Build the image with `backend-web/Dockerfile`; it copies all three service
directories plus `common/` and `launcher/`, installs their dependencies, and
EXPOSE 8089/8090/8091. The main program reaches the Worker through
`http://xianyu-worker-backend:8089` (backend-web), which exposes the
`/api/v1/internal/*` service routes and forwards message sending to websocket.

## Main-program integration

- Service identity auth: `backend-web/app/api/deps.py` `get_service_or_user`
  validates `X-Worker-Token` against `SUB2API_INTERNAL_TOKEN` and binds to an
  existing admin user (no parallel account model).
- Internal routes: `backend-web/app/api/routes/internal_api.py` exposes account
  projection, enable/disable, cookie renew, clear-credentials, QR-login, item
  projection and message-send-with-receipt under `/api/v1/internal/*`.
- Delivery receipt: `common/services/sub2api_delivery_result_client.py` reports
  `POST {SUB2API_INTERNAL_BASE_URL}/api/v1/internal/xianyu/delivery-results`
  after the platform send receipt; skipped when not configured.

## Tests

Worker contract tests live in `backend-web/tests/`. To run them:

```bash
python -m venv /tmp/worker-venv
/tmp/worker-venv/bin/pip install -r backend-web/requirements-test.txt
PYTHONPATH=backend-web:. /tmp/worker-venv/bin/python -m pytest backend-web/tests -p asyncio
```

## Tag discipline

`XIANYU_WORKER_IMAGE_TAG` in `deploy-config/sub2api-ops.md` §11.2 must stay
synchronized with every rebuild: build from this directory, then update the
tag value and the corresponding built image ID. The legacy
`XIANYU_WORKER_IMAGE_DIGEST` field has been retired in favor of a pinned tag
(local builds produce no registry RepoDigest, so a fixed tag is the reliable
reference).
