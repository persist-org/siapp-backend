FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binaries run on distroless/static, which has no libc. ./cmd/... builds every main
# package into /out, named after its folder (api, worker, migrate, seed, personel-stub).
RUN mkdir -p /out && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
EXPOSE 8080
USER nonroot:nonroot
# The image defaults to the API. Run the others with --entrypoint /worker, /migrate, /seed.
ENTRYPOINT ["/api"]
