FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binaries run on distroless/static, which has no libc. ./cmd/... builds every main
# package into /out, named after its folder (api, worker, migrate, seed).
RUN mkdir -p /out && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# The folder of the local storage driver (STORAGE_DRIVER=local). The runtime image has no shell,
# so it is made here and copied in owned by the image's non-root user (uid 65532). A named
# volume mounted over it for the first time inherits that owner, which is what lets the API
# and the worker write to it without running as root. On a server it is replaced by a bind
# mount of a host folder, which the operator must chown to 65532 themselves.
RUN mkdir -p /media-root/var/lib/siapp/media

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
COPY --from=build --chown=65532:65532 /media-root/var/lib/siapp/media /var/lib/siapp/media
ENV STORAGE_LOCAL_DIR=/var/lib/siapp/media
EXPOSE 8080
USER nonroot:nonroot
# The image defaults to the API. Run the others with --entrypoint /worker, /migrate, /seed.
ENTRYPOINT ["/api"]
