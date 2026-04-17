BINARY_DIR := bin
BINARY_NAME := claude-bridge

.PHONY: all cli install-hooks install-local clean

all: cli

GOFLAGS := CGO_ENABLED=1

cli:
	@mkdir -p $(BINARY_DIR)
	$(GOFLAGS) go build -ldflags="-linkmode external" -o $(BINARY_DIR)/$(BINARY_NAME) ./cmd

# Install the hook script into both legacy ~/.claude/ and current ~/.Codex/
install-hooks:
	@mkdir -p ~/.claude/hooks
	@cp hooks/push_output.py ~/.claude/hooks/push_output.py
	@chmod +x ~/.claude/hooks/push_output.py
	@echo "Hook installed to ~/.claude/hooks/push_output.py"
	@echo ""
	@echo "Merge into your project's .claude/settings.json:"
	@cat hooks/settings.json

install-local:
	@mkdir -p ~/.local/bin
	@cp scripts/claude-bridge-local.sh ~/.local/bin/claude-bridge-local
	@chmod +x ~/.local/bin/claude-bridge-local scripts/claude-bridge-local.sh scripts/claude_bridge_mux.py
	@echo "Installed local launcher to ~/.local/bin/claude-bridge-local"

clean:
	rm -rf $(BINARY_DIR)
