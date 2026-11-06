FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
# No separate "go mod download": it would also fetch the generators and the test
# dependencies, which the binary does not need.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /booking ./cmd/booking

FROM alpine:3.22
RUN adduser -D -H -u 10001 booking
COPY --from=build /booking /usr/local/bin/booking
USER booking
ENV HTTP_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/booking"]
