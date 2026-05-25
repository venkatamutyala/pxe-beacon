# pxe-beacon container image — multi-stage, static binary on alpine.
#
# RUN REQUIREMENTS (proxyDHCP is broadcast-based and binds privileged
# UDP ports, so this is NOT a vanilla `docker run`):
#
#   docker run --network host \
#     --cap-add NET_BIND_SERVICE \
#     -v /etc/pxe-beacon:/etc/pxe-beacon \
#     -v pxe-beacon-data:/var/lib/pxe-beacon \
#     ghcr.io/venkatamutyala/pxe-beacon:latest \
#     -config /etc/pxe-beacon/fleet.yaml
#
#   --network host       : proxyDHCP DISCOVER is a broadcast; Docker's
#                          userland-proxy NAT drops it, so host net is
#                          mandatory (not just convenient).
#   --cap-add NET_BIND_SERVICE : UDP 67/69/4011 are privileged.
#   -v .../var/lib/pxe-beacon  : persists `pxe-beacon fetch` distro
#                          assets (~GBs) + template overrides across
#                          restarts (the image VOLUMEs this path).

# ---- build stage ----
FROM golang:1.23-alpine AS build
WORKDIR /src
# Cache modules first.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static binary (musl target in stage 2 has no glibc); version stamped
# the same way the Makefile does.
ARG VERSION=docker
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/pxe-beacon ./cmd/pxe-beacon

# ---- runtime stage ----
FROM alpine:3.20
# libcap provides setcap; tini for clean signal handling (graceful
# shutdown of the listeners on SIGTERM).
RUN apk add --no-cache libcap tini \
 && addgroup -S pxe && adduser -S -G pxe pxe \
 && mkdir -p /var/lib/pxe-beacon \
 && chown pxe:pxe /var/lib/pxe-beacon
COPY --from=build /out/pxe-beacon /usr/local/bin/pxe-beacon
# Grant the privileged-port capability to the binary, then drop to a
# non-root user. cap_net_bind_service lets UDP 67/69/4011 bind without
# full root.
RUN setcap cap_net_bind_service=+ep /usr/local/bin/pxe-beacon
USER pxe
VOLUME /var/lib/pxe-beacon
# Documented; host networking ignores published ports but EXPOSE
# records intent for readers/tooling.
EXPOSE 67/udp 69/udp 4011/udp 8080/tcp
# data-dir defaults to the VOLUME so fetched assets + template
# overrides persist without an explicit flag.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/pxe-beacon", "-data-dir", "/var/lib/pxe-beacon"]
