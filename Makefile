# loom — build & install helpers
#
#   make gui-install   build the GUI and install it into /Applications (one command)
#   make gui           just build the app bundle (cmd/loom-gui/build/bin/loom-gui.app)
#   make gui-run       build and launch, without installing

GOBIN   := $(shell go env GOPATH)/bin
WAILS   := $(GOBIN)/wails
GUI_DIR := cmd/loom-gui
APP     := $(GUI_DIR)/build/bin/loom.app

.PHONY: gui gui-install gui-run

# Install the Wails CLI only if it isn't already present.
$(WAILS):
	go install github.com/wailsapp/wails/v2/cmd/wails@latest

# Build the loom GUI app bundle.
gui: $(WAILS)
	cd $(GUI_DIR) && PATH="$(GOBIN):$$PATH" $(WAILS) build

# Build, then install into /Applications so it's a normal Dock/Spotlight app.
gui-install: gui
	rm -rf /Applications/loom.app /Applications/loom-gui.app
	cp -R "$(APP)" /Applications/loom.app
	@echo "✓ Installed to /Applications/loom.app — launch it from Spotlight or the Dock."

# Build and launch straight from the build dir (no install).
gui-run: gui
	open "$(APP)"
