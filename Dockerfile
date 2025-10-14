FROM golang:1.25.0-alpine AS builder

WORKDIR /git

# build statically compiled git
# hadolint ignore=DL3018
RUN apk add --no-cache curl autoconf gcc flex bison make bash cmake libtool musl-dev g++ \
    zlib-dev zlib-static \
    tcl tk \
    tcl-dev gettext

ARG GIT_VERSION=2.42.0

RUN curl -sL https://github.com/git/git/archive/v${GIT_VERSION}.tar.gz -o git_v${GIT_VERSION}.tar.gz && \
    tar zxf git_v${GIT_VERSION}.tar.gz

WORKDIR /git/git-${GIT_VERSION}

RUN make configure && \
    sed -i 's/qversion/-version/g' configure && \
    ./configure prefix=/git-dist LDFLAGS="--static" CFLAGS="${CFLAGS} -static" && \
    cat config.log && \
    make && make install && \
    mv /git-dist/bin/git /bin/

# build dep-inspector and install commands it needs
WORKDIR /
COPY . /build
WORKDIR /build

ARG CGO_ENABLED=0
ARG VERSION=devel
RUN go build -buildvcs=true -ldflags "-s -w -X main.version=${VERSION}" -trimpath -o dep-inspector

RUN go install -ldflags "-s -w" -trimpath github.com/golangci/golangci-lint/cmd/golangci-lint@latest && \
    go install -ldflags "-s -w" -trimpath honnef.co/go/tools/cmd/staticcheck@latest && \
    go install -ldflags "-s -w" -trimpath github.com/google/capslock/cmd/capslock@main

WORKDIR /usr/local/go

# remove unneeded files from Go toolchain
RUN  rm -r codereview.cfg CONTRIBUTING.md LICENSE PATENTS README.md SECURITY.md api/ doc/ misc/ test/

FROM gcr.io/distroless/static-debian12:latest

# copy git, Go toolchain, dep-inspector and commands it needs
COPY --from=builder /bin/git /bin/
COPY --from=builder /usr/local/go/ /usr/local/go/
COPY --from=builder /build/dep-inspector /bin/
COPY --from=builder /go/bin /bin/

ENV PATH=/usr/local/go/bin:${GOPATH}/bin:${PATH}
ENV GOPATH=/go
ENV GOTOOLCHAIN=local

WORKDIR /work

ENTRYPOINT [ "/bin/dep-inspector" ]
