# rkttl Makefile —— 在 Linux 上交叉编译出全部产物
#
#   make            # 构建 bin/ 下全部产物 + 测试
#   make windows    # 仅 Windows（CLI + GUI）
#   make linux      # 仅 Linux CLI
#   make test       # 单元测试
#   make clean

VERSION ?= 0.1.0
LDFLAGS := -s -w
OUT := bin

.PHONY: all windows linux gui test vet fmt clean icon syso force

all: test linux windows

linux: $(OUT)/rkttl-linux-amd64

windows: $(OUT)/rkttl.exe $(OUT)/rkttl-gui.exe

# 目标加 force 前置依赖：源码变更后总会重编（避免增量判断漏掉 .go 文件）
$(OUT)/rkttl-linux-amd64: force
	@mkdir -p $(OUT)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/rkttl

# Windows 命令行版（控制台程序）
$(OUT)/rkttl.exe: force
	@mkdir -p $(OUT)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/rkttl

# Windows GUI 版（-H windowsgui 隐藏控制台；.syso 内嵌清单与图标）
$(OUT)/rkttl-gui.exe: cmd/rkttl-gui/rsrc_windows_amd64.syso force
	@mkdir -p $(OUT)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS) -H windowsgui" -o $@ ./cmd/rkttl-gui

force:

# 图标与资源
icon: rkttl.ico
rkttl.ico: tools/mkicon.py
	python3 tools/mkicon.py

syso: cmd/rkttl-gui/rsrc_windows_amd64.syso
cmd/rkttl-gui/rsrc_windows_amd64.syso: rkttl.ico cmd/rkttl-gui/app.manifest
	go run github.com/akavel/rsrc@v0.10.2 \
		-manifest cmd/rkttl-gui/app.manifest -ico rkttl.ico -arch amd64 \
		-o cmd/rkttl-gui/rsrc_windows_amd64.syso

test:
	go test ./internal/... ./cmd/rkttl-gui -count=1

vet:
	go vet ./cmd/rkttl ./internal/...
	GOOS=windows GOARCH=amd64 go vet ./cmd/rkttl ./cmd/rkttl-gui ./internal/...

fmt:
	gofmt -l -w .

clean:
	rm -rf $(OUT)
	rm -f rkttl.ico cmd/rkttl-gui/rsrc_windows_amd64.syso
