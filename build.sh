#!/bin/bash
set -e

# 清理旧产物
echo "Cleaning up..."
rm -rf build/
rm -f backend/minivisor
rm -f backend/tinyvisor
mkdir -p build/daemons/scripts

echo "Building Frontend..."
cd frontend
npm install
npm run build
cd ..

echo "Building Backend..."
cd backend
go mod tidy
CGO_ENABLED=0 go build -ldflags="-s -w" -o ../build/tinyvisor .
cd ..

echo "Building Remote Deploy Tool..."
cd remote_deploy
go mod tidy
CGO_ENABLED=0 go build -ldflags="-s -w" -o ../build/tinyvisor-deploy .
cd ..

# 准备运行时环境：只放无密钥模板，绝不复制真实 config.json（含 apiKey）
if [ -f config.json.example ]; then
    cp config.json.example build/config.json.example
fi

echo "---------------------------------------"
echo "Build complete! Artifacts are in ./build"
echo "  - tinyvisor (Main binary)"
echo "  - tinyvisor-deploy (Remote deployment tool)"
echo "  - daemons/ (Data directory structure)"
echo "---------------------------------------"
