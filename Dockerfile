FROM golang:1.21.1-alpine AS builder

COPY . /build
WORKDIR /build

# add git so VCS info will be stamped in binary
# ignore warning that a specific version of git isn't pinned
# hadolint ignore=DL3018
RUN apk add --no-cache git

ARG CGO_ENABLED=0
ARG VERSION=devel
RUN go build -buildvcs=true -ldflags "-s -w -X main.version=${VERSION}" -trimpath -o dep-inspector

FROM alpine:3.18.4

COPY --from=builder /build/dep-inspector /dep-inspector
# ignore warning that a specific version of git isn't pinned
# hadolint ignore=DL3018
RUN apk add --no-cache git

ENTRYPOINT [ "/dep-inspector" ]
