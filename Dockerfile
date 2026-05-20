FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/db-backup-r2 .

FROM alpine:3.20

RUN apk add --no-cache \
    ca-certificates \
    postgresql-client \
    tzdata

WORKDIR /app

COPY --from=build /out/db-backup-r2 /app/db-backup-r2

ENTRYPOINT ["/app/db-backup-r2"]
CMD ["serve"]
