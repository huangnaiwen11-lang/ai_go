#!/usr/bin/env bash
# Run Go integration tests against disposable Mongo, never the developer's data.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$script_dir/.."

command -v docker >/dev/null
command -v go >/dev/null
# Do not pull images or silently connect to an existing database.
docker image inspect mongo:7 >/dev/null
mongo_container_id=""
cleanup() {
  if [[ "$mongo_container_id" =~ ^[a-f0-9]{64}$ ]]; then
    docker stop --timeout 5 "$mongo_container_id" >/dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Existing test safety checks require rs0 on localhost:27017. If occupied,
# docker fails; do not stop that service or fall back to its database.
mongo_container_id="$(docker run --rm --pull=never -d \
  --name "cling-go-mongo-tests-$$" --label cling.test=isolated-go-suite \
  --tmpfs /data/db:rw,size=512m --tmpfs /data/configdb:rw,size=32m \
  -p 127.0.0.1:27017:27017 mongo:7 --replSet rs0 --bind_ip_all)"

ready=false
for ((attempt=0; attempt<30; attempt++)); do
  if docker exec "$mongo_container_id" mongosh --quiet --eval \
    'quit(db.adminCommand({ping:1}).ok === 1 ? 0 : 1)' >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo 'Disposable Mongo did not become ready.' >&2
  exit 1
fi
docker exec "$mongo_container_id" mongosh --quiet --eval \
  'const r = rs.initiate({_id:"rs0",members:[{_id:0,host:"127.0.0.1:27017"}]}); quit(r.ok === 1 ? 0 : 1)' >/dev/null

ready=false
for ((attempt=0; attempt<30; attempt++)); do
  if docker exec "$mongo_container_id" mongosh --quiet --eval \
    'quit(db.hello().isWritablePrimary === true ? 0 : 1)' >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo 'Disposable Mongo did not elect a primary.' >&2
  exit 1
fi

export CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true'
if (($# == 0)); then
  set -- ./... -count=1
fi
go test "$@"
