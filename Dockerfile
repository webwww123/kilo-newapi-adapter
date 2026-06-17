FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

RUN go build -trimpath -ldflags="-s -w" -o /out/kilo-newapi-adapter ./cmd/kilo-newapi-adapter

FROM alpine:3.20

RUN apk add --no-cache ca-certificates
COPY --from=build /out/kilo-newapi-adapter /usr/local/bin/kilo-newapi-adapter

ENV ADDR=:18083
EXPOSE 18083

ENTRYPOINT ["/usr/local/bin/kilo-newapi-adapter"]

