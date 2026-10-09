FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN mkdir -p /out && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/api"]