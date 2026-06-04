#!/usr/bin/env bash
# TS_AUTHKEY=<key> bash run.sh  — 1 coordinator + 2 workers, isolated nets, build redis.
set -euo pipefail
: "${TS_AUTHKEY:?}"; PFX="int$$"; IMG=distcc-int:latest
cd "$(dirname "$0")/../.."
docker build -t $IMG -f spike/integration/Dockerfile .
nets=(${PFX}-c ${PFX}-w1 ${PFX}-w2)
for n in "${nets[@]}"; do docker network create $n >/dev/null; done
cleanup(){ docker rm -f ${PFX}-co ${PFX}-wo1 ${PFX}-wo2 >/dev/null 2>&1||true; for n in "${nets[@]}"; do docker network rm $n >/dev/null 2>&1||true; done; }
trap cleanup EXIT

common="-e INPUT_OAUTH_CLIENT_ID=x -e INPUT_OAUTH_SECRET=$TS_AUTHKEY -e INPUT_TAGS=tag:ci -e INPUT_RUN_PREFIX=$PFX -e GITHUB_RUN_ID=$PFX -e INPUT_POLL_INTERVAL=1s -e INPUT_TEARDOWN_THRESHOLD=5"
for i in 1 2; do
  docker run -d --name ${PFX}-wo$i --network ${PFX}-w$i $common \
    -e INPUT_MODE=worker -e INPUT_WORKER_INDEX=$i $IMG distcc-action
done
sleep 8
docker run --name ${PFX}-co --network ${PFX}-c $common \
  -e INPUT_MODE=coordinator -e INPUT_EXPECTED_WORKERS=2 -e INPUT_MIN_WORKERS=1 \
  -e GITHUB_ENV=/tmp/genv -e GITHUB_OUTPUT=/tmp/gout \
  --entrypoint bash $IMG -c '
    distcc-action            # main: detaches forwarder, exports /tmp/genv, returns
    # GITHUB_ENV format is "KEY=value\n" lines (not shell-sourceable due to colons/slashes in values);
    # parse manually to avoid "No such file or directory" on DISTCC_HOSTS value.
    while IFS='=' read -r key val; do
      [ -z "$key" ] && continue
      export "$key=$val"
    done < /tmp/genv
    echo "DISTCC_HOSTS=$DISTCC_HOSTS  DISTCC_J=$DISTCC_J"
    git clone --depth 1 https://github.com/redis/redis /tmp/redis 2>&1 | tail -1
    cd /tmp/redis && time make -j${DISTCC_J} CC="distcc gcc" 2>&1 | tail -15
    ls -la src/redis-server && echo "INTEGRATION-RESULT build = PASS" || echo "INTEGRATION-RESULT build = FAIL"
    distcc-action --teardown # explicit teardown to test that path too
    sleep 8
  '
echo "=== worker logs ==="; for i in 1 2; do echo "--w$i--"; docker logs --tail 20 ${PFX}-wo$i 2>&1; done
