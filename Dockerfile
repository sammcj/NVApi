FROM golang:1.27 AS go

# Build stage
FROM nvcr.io/nvidia/cuda:13.3.0-devel-ubuntu24.04 AS builder

# git for module fetching
RUN apt update && apt install -y git && rm -rf /var/lib/apt/lists/*

# Ubuntu's packaged Go lags go.mod, so take the toolchain from the official image
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . /app

# cgo links against libnvidia-ml, so this is not a static binary; the runtime
# stage below provides a matching glibc and the driver supplies libnvidia-ml.
# go-nvml's cgo wrappers reference NVML functions NVIDIA has deprecated.
RUN CGO_CFLAGS="-O2 -g -Wno-deprecated-declarations" go build -ldflags="-s -w" -o /app/nvapi . && \
  chmod +x /app/nvapi

# Runtime stage
FROM nvcr.io/nvidia/cuda:13.3.0-runtime-ubuntu24.04

LABEL org.opencontainers.image.description="NVApi is a lightweight API that exposes NVIDIA GPU metrics"

# procps for process lookups when run with pid:host
RUN apt update && apt install -y procps && rm -rf /var/lib/apt/lists/*

ENV NVIDIA_VISIBLE_DEVICES=all

WORKDIR /app

COPY --from=builder /app/nvapi /app/nvapi

EXPOSE 9999

CMD ["/app/nvapi"]
