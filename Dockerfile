FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -ldflags="-s -w" -o ai-server .

FROM alpine:3.19

RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /app/ai-server .
COPY --from=builder /app/prompts ./prompts

# В контейнере /metrics слушает все интерфейсы: loopback контейнера с хоста
# недоступен. Наружу порт публикуется только на 127.0.0.1 (docker-compose.yml).
ENV METRICS_ADDR=:9091

EXPOSE 8091 9091
CMD ["./ai-server"]
