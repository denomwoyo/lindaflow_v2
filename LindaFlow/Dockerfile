FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/lindaflow ./cmd/lindaflow
FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/lindaflow /app/lindaflow
COPY web /app/web
RUN adduser -D -u 10001 app && mkdir -p /app/data && chown -R app:app /app
USER app
EXPOSE 8080
ENTRYPOINT ["/app/lindaflow"]
