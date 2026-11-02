# syntax=docker/dockerfile:1
# this is a comment
FROM golang:1.25 AS builder
RUN go build

# final stage
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app /app
