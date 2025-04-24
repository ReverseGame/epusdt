FROM debian:11-slim

# 设置工作目录
WORKDIR /app

# 安装必要依赖
RUN apt-get update && \
    apt-get install -y wget ca-certificates && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

COPY epusdt epusdt
COPY static static

RUN chmod +x epusdt

# 创建运行所需目录
RUN mkdir -p /logs /runtime /static

# 暴露服务端口
EXPOSE 8000

# 设置容器启动命令
CMD ["./epusdt", "http", "start"]