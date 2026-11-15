#!/bin/bash
echo "========================================================"
echo "   正在编译 QQMailDrive (Pure Go Standard Library)"
echo "   基于 Go 标准库纯净构建，零外部依赖，安全极速"
echo "========================================================"

if ! command -v go &> /dev/null; then
    echo "[错误] 未检测到 Go 编译器环境，请先安装 Go。"
    exit 1
fi

echo "正在编译 QQMailDrive 二进制程序..."
go build -ldflags="-s -w" -o QQMailDrive .

if [ $? -eq 0 ]; then
    echo "✔ 编译成功！运行 ./QQMailDrive 即可启动网盘。"
else
    echo "❌ 编译失败。"
fi
