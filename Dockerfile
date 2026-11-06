FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/booking ./cmd/booking

FROM alpine:3.21
RUN addgroup -S app && adduser -S -G app app
COPY --from=build /out/booking /usr/local/bin/booking
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/booking"]
