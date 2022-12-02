FROM golang:1.19.3-alpine AS builder

COPY . /build
WORKDIR /build

# add git so VCS info will be stamped in binary
RUN apk add --no-cache git=2.36.3-r0

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
