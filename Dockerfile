FROM golang:1.23-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /build/bot ./cmd/bot/main.go

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /build/bot /usr/local/bin/bot

CMD ["bot"]
