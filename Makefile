.PHONY: build install clean

build:
	go build -o bin/claude-pool ./cmd/claude-pool/
	go build -o bin/claude-pool-cli ./cmd/claude-pool-cli/

install: build
	ln -sf "$(CURDIR)/bin/claude-pool" ~/.local/bin/claude-pool
	ln -sf "$(CURDIR)/bin/claude-pool-cli" ~/.local/bin/claude-pool-cli

clean:
	rm -f bin/claude-pool bin/claude-pool-cli
