FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/codebuddy2api ./cmd/codebuddy2api

FROM debian:bookworm-slim

WORKDIR /app
COPY --from=build /out/codebuddy2api /app/codebuddy2api
EXPOSE 18182
CMD ["/app/codebuddy2api"]
