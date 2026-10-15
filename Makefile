# loom — build & install helpers
#
#   make gui-install   build the GUI and install it into /Applications (one command)
#   make gui           just build the app bundle (cmd/loom-gui/build/bin/loom-gui.app)
#   make gui-run       build and launch, without installing
#   make bundle-test   run the discipline bundle's manifest pin and python suites
#   make bundle-manifest  regenerate internal/discipline/bundle/manifest.json after a bundle edit

GOBIN   := $(shell go env GOPATH)/bin
WAILS   := $(GOBIN)/wails

# npm normally comes from nvm, which only loads in interactive shells, so
# `make gui` from a script or an agent found no npm. Resolve its bin dir the
# way nvm would: PATH if it is already there, else the `default` alias's
# version under ~/.nvm, else the newest installed node.
NPM_DIR := $(shell if command -v npm >/dev/null 2>&1; then dirname "$$(command -v npm)"; else \
	v=$$(cat "$$HOME/.nvm/alias/default" 2>/dev/null); \
	d=$$(ls -d "$$HOME"/.nvm/versions/node/v$${v\#v}*/bin 2>/dev/null | sort -V | tail -1); \
	[ -n "$$d" ] || d=$$(ls -d "$$HOME"/.nvm/versions/node/*/bin 2>/dev/null | sort -V | tail -1); \
	echo "$$d"; fi)
GUI_DIR := cmd/loom-gui
APP     := $(GUI_DIR)/build/bin/loom.app

.PHONY: gui gui-install gui-run bundle-test bundle-manifest

# Install the Wails CLI only if it isn't already present.
$(WAILS):
	go install github.com/wailsapp/wails/v2/cmd/wails@latest

# Build the loom GUI app bundle.
gui: $(WAILS)
	cd $(GUI_DIR) && PATH="$(GOBIN):$(NPM_DIR):$$PATH" $(WAILS) build

# Build, then install into /Applications so it's a normal Dock/Spotlight app.
gui-install: gui
	rm -rf /Applications/loom.app /Applications/loom-gui.app
	cp -R "$(APP)" /Applications/loom.app
	@echo "✓ Installed to /Applications/loom.app — launch it from Spotlight or the Dock."

# Build and launch straight from the build dir (no install).
gui-run: gui
	open "$(APP)"

# The Agent Discipline bundle (docs/DISCIPLINE.md): the manifest pin and the
# hooks' own python suites, run against the embedded copy in a temp dir.
bundle-test:
	go test ./internal/discipline/ -run 'TestBundleSuites|TestManifestCurrent' -count=1 -v

bundle-manifest:
	cd internal/discipline && go run ./gen
