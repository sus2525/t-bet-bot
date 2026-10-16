FROM golang:1.27.2-alpine AS build
WORKDIR /src
RUN apk add --no-cache build-base sqlite-dev
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/football-bot .

FROM alpine:3.20
RUN apk add --no-cache sqlite-libs
RUN addgroup -S bot && adduser -S bot -G bot
WORKDIR /app
COPY --from=build /out/football-bot /usr/local/bin/football-bot
RUN mkdir -p /data && chown bot:bot /data
USER bot
ENV DATA_FILE=/data/bot.db
ENTRYPOINT ["/usr/local/bin/football-bot"]
