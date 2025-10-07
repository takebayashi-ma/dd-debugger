# Build stage
FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dd-debugger main.go

# Run stage
FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=builder /app/dd-debugger /app/dd-debugger
EXPOSE 8126
EXPOSE 8125/udp
USER nonroot:nonroot
ENTRYPOINT ["/app/dd-debugger"]
