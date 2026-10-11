#!/usr/bin/env bash
# 构建静态链接的 wetype-x11 单文件二进制(零动态依赖)
set -euo pipefail
cd "$(dirname "$0")"

# 1. 构建静态 libopus (已存在则跳过)
if [ ! -f vendor/opus/lib/libopus.a ]; then
    echo "==> 构建静态 libopus ..."
    mkdir -p vendor
    cd vendor
    if [ ! -f opus.tar.gz ]; then
        # 优先 GitHub mirror(CI 友好), 退回 xiph 官方
        curl -sfL -o opus.tar.gz https://github.com/xiph/opus/releases/download/v1.5.2/opus-1.5.2.tar.gz \
        || curl -sfL -o opus.tar.gz https://downloads.xiph.org/releases/opus/opus-1.5.2.tar.gz
    fi
    [ -d opus-src ] || { tar xzf opus.tar.gz && mv opus-1.5.2 opus-src; }
    cd opus-src
    ./configure --prefix="$PWD/../opus" --disable-shared --enable-static \
        --disable-doc --disable-extra-programs --with-pic
    make -j"$(nproc)"
    make install
    cd ../..
fi

# 2. 静态构建 (netgo: 纯 Go DNS 解析, 避免 glibc NSS 警告)
echo "==> 构建 wetype-x11 ..."
CGO_ENABLED=1 go build -tags netgo -trimpath \
    -ldflags '-linkmode external -extldflags "-static" -s -w' \
    -o wetype-x11 .

echo "==> 验证:"
file wetype-x11
ldd_out="$(LC_ALL=C ldd wetype-x11 2>&1 || true)"
if echo "$ldd_out" | grep -q "not a dynamic executable"; then
    echo "OK: 静态二进制, 零动态依赖"
else
    echo "警告: 不是完全静态!" >&2
    echo "$ldd_out" >&2
    exit 1
fi
