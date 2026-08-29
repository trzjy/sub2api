# Vendored xianyu-auto-reply worker

This directory is the production source used to build the reviewed
`XIANYU_WORKER_IMAGE_DIGEST` artifact. It is vendored because the current
integration relies on internal account identity, final send receipt, and result
callback behavior beyond the upstream release.

The three service Dockerfiles and shared runtime directories are vendored;
non-runtime promotion assets are intentionally excluded. Build the backend image
with `backend-web/Dockerfile`. The WebSocket process listens on port 8090 and is
reachable through the backend service. Keep `XIANYU_WORKER_IMAGE_DIGEST`
synchronized with every rebuild and review the resulting image before deployment.
