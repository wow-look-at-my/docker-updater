FROM node:22 AS frontend
RUN build-frontend

FROM golang:1.25 AS backend
RUN build-backend

FROM debian:bookworm-slim
COPY --from=backend /server /server
