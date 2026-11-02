FROM ghcr.io/org/base:1.2 AS builder
RUN make

FROM builder
CMD ["/run"]
