FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/quickproxy ./cmd/quickproxy

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/quickproxy /app/quickproxy
COPY auth/quickauth.py /app/auth/quickauth.py
RUN mkdir -p /app/data
EXPOSE 8080
ENTRYPOINT ["/app/quickproxy"]
CMD ["-addr", "0.0.0.0:8080"]
