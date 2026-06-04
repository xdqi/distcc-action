#!/usr/bin/env bash
# Usage: TS_AUTHKEY=<key> bash run.sh
set -euo pipefail
: "${TS_AUTHKEY:?set TS_AUTHKEY}"
PFX="tsx$$"
IMG=spike-tsnet:latest
cd "$(dirname "$0")/../.."   # repo root for docker build context
docker build -t $IMG -f spike/tsnet-transport/Dockerfile .

docker network create ${PFX}-net-s >/dev/null
docker network create ${PFX}-net-c >/dev/null
cleanup(){ docker rm -f ${PFX}-s ${PFX}-c >/dev/null 2>&1||true; docker network rm ${PFX}-net-s ${PFX}-net-c >/dev/null 2>&1||true; }
trap cleanup EXIT

docker run -d --name ${PFX}-s --network ${PFX}-net-s \
  -e TS_AUTHKEY="$TS_AUTHKEY" -e TS_HOSTNAME=${PFX}-server -e ROLE=server \
  --entrypoint bash $IMG -c \
  'distccd --daemon --no-detach --allow 0.0.0.0/0 --listen 127.0.0.1 --port 3632 --jobs 4 --log-file /var/log/distccd.log & sleep 1; ROLE=server TS_HOSTNAME='"${PFX}"'-server spike-tsnet'
sleep 12

docker run --name ${PFX}-c --network ${PFX}-net-c \
  -e TS_AUTHKEY="$TS_AUTHKEY" -e TS_HOSTNAME=${PFX}-client -e ROLE=client \
  -e SERVER_HOST=${PFX}-server -e LOCAL_PORT=3700 \
  --entrypoint bash $IMG -c '
    spike-tsnet & sleep 12
    echo "int spikefn(int a){return a+1;}" > /tmp/u.c
    DISTCC_HOSTS="127.0.0.1:3700,lzo" DISTCC_VERBOSE=1 distcc gcc -c /tmp/u.c -o /tmp/u.o 2>&1 | tail -20
    if [ -f /tmp/u.o ]; then echo "SPIKE-RESULT tsnet-distcc = PASS"; else echo "SPIKE-RESULT tsnet-distcc = FAIL"; fi
  '
