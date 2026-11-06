FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /server ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
COPY --from=build /server /usr/local/bin/server
USER app
ENV HTTP_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["server"]
