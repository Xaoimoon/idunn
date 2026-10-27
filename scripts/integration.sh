#!/bin/sh
# Runs the integration tests against a disposable Swarm: a privileged
# docker:dind container with its own engine, a one-node Swarm and a local
# registry. Nothing touches the engine you run it from, except that
# container.
#
# Needs: docker, go. Ports 23750 (engine API) and 5000 (registry) must be free.
set -eu

NAME=idunn-it
ENGINE_PORT=23750
REGISTRY=127.0.0.1:5000

# DOCKER_HOST is pointed at the disposable engine below; the container itself
# lives on the outer engine.
OUTER_DOCKER_HOST=${DOCKER_HOST:-}
cleanup() { DOCKER_HOST=$OUTER_DOCKER_HOST docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "Starting a disposable engine..."
docker run -d --privileged --name "$NAME" \
  -e DOCKER_TLS_CERTDIR= \
  -p 127.0.0.1:$ENGINE_PORT:2375 -p 127.0.0.1:5000:5000 \
  docker:dind --host=tcp://0.0.0.0:2375 >/dev/null

export DOCKER_HOST=tcp://127.0.0.1:$ENGINE_PORT
i=0
until docker version >/dev/null 2>&1; do
  i=$((i + 1)); [ $i -lt 60 ] || { echo "engine did not start"; exit 1; }
  sleep 1
done

docker swarm init >/dev/null
docker run -d --name registry -p 5000:5000 registry:3 >/dev/null

echo "Building test images..."
build() {
  printf '%s\n' "$2" | docker build -q -t "$REGISTRY/it/app:$1" - >/dev/null
  docker push -q "$REGISTRY/it/app:$1" >/dev/null
}
build good1 'FROM busybox:1.37
LABEL idunn.test=good1
CMD ["sleep", "3600"]'
build good2 'FROM busybox:1.37
LABEL idunn.test=good2
CMD ["sleep", "3600"]'
build unhealthy 'FROM busybox:1.37
LABEL idunn.test=unhealthy
HEALTHCHECK --interval=2s --timeout=1s --start-period=0s --retries=1 CMD ["false"]
CMD ["sleep", "3600"]'

echo "Running tests..."
IDUNN_IT_REGISTRY=$REGISTRY IDUNN_IT_DISPOSABLE=1 \
  go test -tags integration -count=1 -v ./integration/...
