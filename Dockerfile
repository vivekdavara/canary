# The image of one of this repo's binaries: the operator (BINARY=canary, the default) or the end-to-end tests' demo app
# (BINARY=demoapp). make images builds the binary first, for Linux with CGO_ENABLED=0 so it needs no libc, and this only
# copies it onto distroless's static image: no shell, no package manager, and the nonroot tag runs it as uid 65532.
# The base is pinned by digest, so a rebuild gets the same base.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
ARG BINARY=canary
COPY bin/linux/${BINARY} /app
ENTRYPOINT ["/app"]
