FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY src/go.mod src/go.sum ./
RUN go mod download
COPY src/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/epusdt .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/epusdt /app/epusdt
COPY src/static /app/static
RUN mkdir -p /app/runtime
EXPOSE 8000
ENTRYPOINT ["/app/epusdt"]
CMD ["http", "start"]
