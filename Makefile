.PHONY: build install clean sync-hooks

# Copy canonical hook files to embedded/ before building (single source of truth: hooks/)
sync-hooks:
	cp hooks/hooks.json cmd/claude-pool/embedded/hooks.json
	cp hooks/hook-runner.sh cmd/claude-pool/embedded/hook-runner.sh
	cp hooks/pid-registry.sh cmd/claude-pool/embedded/pid-registry.sh

build: sync-hooks
	go build -o bin/claude-pool ./cmd/claude-pool/
	go build -o bin/claude-pool-cli ./cmd/claude-pool-cli/

install: build
	ln -sf "$(CURDIR)/bin/claude-pool" ~/.local/bin/claude-pool
	ln -sf "$(CURDIR)/bin/claude-pool-cli" ~/.local/bin/claude-pool-cli

clean:
	rm -f bin/claude-pool bin/claude-pool-cli
