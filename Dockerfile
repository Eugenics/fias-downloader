FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY . .
RUN CGO_ENABLED=0 go build -mod=vendor -o /out/fias-downloader ./cmd/fias-downloader

FROM alpine:3.20
RUN adduser -D -u 10001 app
USER app
WORKDIR /app
COPY --from=build /out/fias-downloader /app/fias-downloader
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["/app/fias-downloader"]
