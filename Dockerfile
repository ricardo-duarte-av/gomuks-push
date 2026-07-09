ARG DOCKER_HUB="docker.io"

# Pin the builder to the native platform and cross-compile from there, so that
# multi-arch builds don't run the Go toolchain under QEMU emulation.
FROM --platform=${BUILDPLATFORM:-linux/amd64} ${DOCKER_HUB}/golang:1-alpine AS builder

ARG VERSION="unknown"
ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache ca-certificates
WORKDIR /build/gomuks-push
COPY . /build/gomuks-push
ENV CGO_ENABLED=0
# -buildvcs=false because the build image has no git binary; the revision is
# stamped in explicitly instead so it shows up in gomuks_push_build_info.
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -buildvcs=false -ldflags="-s -w -X main.version=${VERSION}" -o /usr/bin/gomuks-push

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/bin/gomuks-push /usr/bin/gomuks-push

# 8080: push endpoint. 9090: Prometheus metrics (keep this one off the public internet).
EXPOSE 8080 9090

ENTRYPOINT ["/usr/bin/gomuks-push"]
