.DEFAULT_GOAL := help

.PHONY: help dev dev-build

help:
	@echo 'make dev        Build and replace ~/Applications/Offload Monitor.app (App + bundled CLI)'
	@echo 'make dev-build  Build a dev bundle without installing it'
	@echo 'Override destination: make dev OFFLOAD_DEV_INSTALL_DIR=/path/to/Applications'

# Pass command-line Make variables to the build/install script, including paths with spaces.
export OFFLOAD_DEV_BASE
export OFFLOAD_DEV_INSTALL_DIR

dev:
	bash scripts/dev-macos-app.sh --install

dev-build:
	bash scripts/dev-macos-app.sh
