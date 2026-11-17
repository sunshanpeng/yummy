# 最小 FastAPI 服务：/ 返回应用信息，/health 供平台探针。
import os
import socket
from datetime import datetime, timezone

from fastapi import FastAPI

app = FastAPI()


@app.get("/health")
def health():
    return {"status": "ok"}


@app.get("/")
def root():
    return {
        "service": "web-python-fastapi",
        "host": socket.gethostname(),
        "time": datetime.now(timezone.utc).isoformat(),
        "message": "Hello from Yummy PaaS",
    }
