# ---- build stage ----
FROM golang:1.21-alpine AS builder

WORKDIR /build

# Download dependencies and generate go.sum
COPY go.mod ./
RUN GONOSUMCHECK=* GOFLAGS=-mod=mod go mod tidy

# Build the binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o feishu-agent .

# ---- runtime stage ----
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /build/feishu-agent .

CMD ["/app/feishu-agent"]
