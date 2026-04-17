# syntax=docker/dockerfile:1.7

FROM nvidia/cuda:12.6.3-cudnn-runtime-ubuntu22.04

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    LANG=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv \
      libgl1 libglib2.0-0 libgomp1 libsm6 libxext6 libxrender1 \
      ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf /usr/bin/python3 /usr/bin/python

RUN python -m pip install --upgrade pip

RUN pip install paddlepaddle-gpu==3.0.0 \
    -i https://www.paddlepaddle.org.cn/packages/stable/cu126/

WORKDIR /app

COPY requirements.txt ./
RUN pip install -r requirements.txt

COPY app.py ./

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8000/health || exit 1

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1", "--log-level", "info"]
