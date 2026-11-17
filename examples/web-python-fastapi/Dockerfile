FROM python:3.12-slim AS build
RUN useradd --uid 1000 --create-home app
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY main.py ./
# 平台要求非 root 运行；uvicorn 监听 8080（平台默认端口）。
USER 1000:1000
EXPOSE 8080
CMD ["python", "-m", "uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8080"]
