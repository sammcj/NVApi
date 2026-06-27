# Build stage
FROM nvcr.io/nvidia/cuda:13.3.0-devel-ubuntu24.04 AS builder

# install go + git (git for module fetching)
RUN apt update && apt install -y golang git && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY . /app

# cgo links against libnvidia-ml, so this is not a static binary; the runtime
# stage below provides a matching glibc and the driver supplies libnvidia-ml.
RUN go build -ldflags="-s -w" -o /app/nvapi . && \
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
