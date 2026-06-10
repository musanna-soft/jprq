FROM golang:1.22-alpine AS builder
WORKDIR /src

# Modullarni avval — kesh foydalanish uchun
COPY go.mod go.sum ./
RUN go mod download

# Manba va static fayllar
COPY server/ server/

# Bitta statik binar (server + website embedded)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/jprq ./server/

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Tashkent
RUN mkdir -p /etc/jprq && touch /etc/jprq/allowed-users.csv
COPY --from=builder /out/jprq /usr/local/bin/jprq

EXPOSE 80 443 4321 3300
ENTRYPOINT ["/usr/local/bin/jprq"]
