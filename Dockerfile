FROM golang:1.20.6-alpine AS builder

COPY . /build
WORKDIR /build

# add git so VCS info will be stamped in binary
# ignore warning that a specific version of git isn't pinned
# hadolint ignore=DL3018
RUN apk add --no-cache git

# build as PIE to take advantage of exploit mitigations
ARG CGO_ENABLED=0
ARG VERSION
RUN go build -buildmode=pie -buildvcs=true -ldflags "-s -w -X main.version=${VERSION}" -trimpath -o dep-inspector

# pie-loader is built and scanned daily, we want the most recent version
# hadolint ignore=DL3007
FROM ghcr.io/capnspacehook/pie-loader:latest
COPY --from=builder /build/dep-inspector /dep-inspector

USER 1000:1000

ENTRYPOINT [ "/dep-inspector" ]
