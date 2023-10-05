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

RUN go install -ldflags "-s -w" -trimpath github.com/golangci/golangci-lint/cmd/golangci-lint@latest \
    && go install -ldflags "-s -w" -trimpath honnef.co/go/tools/cmd/staticcheck@latest \
    && go install -ldflags "-s -w" -trimpath github.com/google/capslock/cmd/capslock@main

FROM alpine:3.18.4

# copy Go toolchain, dep-inspector and binaries it needs
COPY --from=builder /usr/local/go/ /usr/local/go/
COPY --from=builder /build/dep-inspector /bin/
COPY --from=builder /go/bin /bin/

ENV PATH /usr/local/go/bin:$GOPATH/bin:$PATH
ENV GOPATH /go
ENV GOTOOLCHAIN=local

WORKDIR /usr/local/go
RUN rm -r codereview.cfg CONTRIBUTING.md LICENSE PATENTS README.md SECURITY.md api/ doc/ misc/ test/

# add git so VCS info will be stamped in binary
# ignore warning that a specific version of git isn't pinned
# hadolint ignore=DL3018
RUN apk add --no-cache git

WORKDIR /work

ENTRYPOINT [ "/bin/dep-inspector" ]
