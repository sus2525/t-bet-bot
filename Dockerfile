FROM golang:1.27.2-alpine AS build
WORKDIR /src
RUN apk add --no-cache build-base sqlite-dev
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/football-bot .

FROM alpine:3.20
RUN apk add --no-cache sqlite-libs su-exec
RUN addgroup -S bot && adduser -S bot -G bot
WORKDIR /app
COPY --from=build /out/football-bot /usr/local/bin/football-bot
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN mkdir -p /data && chown bot:bot /data && chmod 0755 /usr/local/bin/docker-entrypoint.sh
ENV DATA_FILE=/data/bot.db
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
