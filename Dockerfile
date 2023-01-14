FROM golang:1.18-alpine3.14 as build
RUN mkdir /app
WORKDIR /app
COPY . .
RUN go build -o /k3s-api

## Deploy
FROM alpine:3.17
WORKDIR /app
COPY --from=build /k3s-api k3s-api
EXPOSE 80

ENTRYPOINT ["/app/k3s-api"]