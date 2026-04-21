FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN go mod tidy
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /out/llama-cpp-router-monitor ./main.go

FROM alpine:3.21
RUN adduser -D -H -u 10001 app && mkdir -p /app/data && chown -R app:app /app
USER app
WORKDIR /app
COPY --from=build /out/llama-cpp-router-monitor /app/llama-cpp-router-monitor
COPY --from=build /src/web /app/web
EXPOSE 9091
ENTRYPOINT ["/app/llama-cpp-router-monitor"]
