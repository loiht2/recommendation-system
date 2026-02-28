FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /web-ui ./main.go

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /web-ui /web-ui
EXPOSE 8090
ENTRYPOINT ["/web-ui"]
