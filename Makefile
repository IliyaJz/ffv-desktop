.PHONY: all build build-all clean install test

BIN_DIR := bin
BINARY := $(BIN_DIR)/ffv

all: build

build:
	@mkdir -p $(BIN_DIR)
	@rm -f $(BINARY)
	go build -ldflags="-s -w" -o $(BINARY) ./cmd/ffv
	@codesign -s - -f $(BINARY) 2>/dev/null || true
	@echo "Built $(BINARY) successfully."

build-all: build-darwin build-linux build-windows
	@echo "All binaries built in $(BIN_DIR)/"

build-darwin:
	@mkdir -p $(BIN_DIR)
	@rm -f $(BIN_DIR)/ffv-darwin-arm64 $(BIN_DIR)/ffv-darwin-amd64 $(BINARY)
	GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o $(BIN_DIR)/ffv-darwin-arm64 ./cmd/ffv
	GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o $(BIN_DIR)/ffv-darwin-amd64 ./cmd/ffv
	lipo -create -output $(BINARY) $(BIN_DIR)/ffv-darwin-arm64 $(BIN_DIR)/ffv-darwin-amd64 2>/dev/null || cp $(BIN_DIR)/ffv-darwin-arm64 $(BINARY)
	@codesign -s - -f $(BINARY) 2>/dev/null || true
	@echo "Built $(BINARY) successfully."

build-linux:
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BIN_DIR)/ffv-linux-amd64 ./cmd/ffv
	GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o $(BIN_DIR)/ffv-linux-arm64 ./cmd/ffv
	@echo "Built Linux binaries in $(BIN_DIR)/"

build-windows:
	@mkdir -p $(BIN_DIR)
	GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o $(BIN_DIR)/ffv.exe ./cmd/ffv
	@echo "Built Windows binary $(BIN_DIR)/ffv.exe successfully."

install: build
	install -m 755 $(BINARY) /usr/local/bin/ffv
	@echo "Installed ffv to /usr/local/bin/ffv"

clean:
	rm -rf $(BIN_DIR) ffv ffv-*
	@echo "Cleaned build artifacts."

test:
	go test -v ./...
