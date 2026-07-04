VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)
BIN      = sysmon

# Static, CGO-free build: the binary runs on any Linux distro (glibc,
# musl/Alpine, whatever) with no runtime dependencies.
export CGO_ENABLED = 0

.PHONY: build test vet install uninstall cross clean

build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/sysmon

test:
	go test ./...

vet:
	go vet ./...

# User-level install: binary + systemd user service, no root needed.
install: build
	install -Dm755 $(BIN) $(HOME)/.local/bin/$(BIN)
	install -Dm644 systemd/sysmon.service $(HOME)/.config/systemd/user/sysmon.service
	@if command -v systemctl >/dev/null 2>&1; then \
		systemctl --user daemon-reload; \
		echo "Run: systemctl --user enable --now sysmon.service"; \
	else \
		echo "No systemd — run: sysmon daemon --detach"; \
	fi

uninstall:
	-systemctl --user disable --now sysmon.service 2>/dev/null
	rm -f $(HOME)/.local/bin/$(BIN) $(HOME)/.config/systemd/user/sysmon.service

# Cross-compile for every common Linux architecture.
cross:
	GOOS=linux GOARCH=amd64             go build -ldflags '$(LDFLAGS)' -o dist/$(BIN)-linux-amd64   ./cmd/sysmon
	GOOS=linux GOARCH=arm64             go build -ldflags '$(LDFLAGS)' -o dist/$(BIN)-linux-arm64   ./cmd/sysmon
	GOOS=linux GOARCH=arm GOARM=7       go build -ldflags '$(LDFLAGS)' -o dist/$(BIN)-linux-armv7   ./cmd/sysmon
	GOOS=linux GOARCH=riscv64           go build -ldflags '$(LDFLAGS)' -o dist/$(BIN)-linux-riscv64 ./cmd/sysmon

clean:
	rm -rf $(BIN) dist
