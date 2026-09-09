# shellcheck shell=sh
# Shared by dctl, tool launchers, login shells, and the injected Debux shell.
# No activation hook: simply entering the target must never evaluate its mise
# configuration, .miserc.toml, .env, or version files.
export DEBUX_DATA_DIR=/var/lib/debux
export DEBUX_TOOL_BIN="$DEBUX_DATA_DIR/bin"
export PATH="$DEBUX_TOOL_BIN:/usr/local/bin:$PATH"
export MISE_DATA_DIR="$DEBUX_DATA_DIR/mise"
export MISE_CACHE_DIR="$DEBUX_DATA_DIR/cache"
export MISE_STATE_DIR="$DEBUX_DATA_DIR/state"
export MISE_CONFIG_DIR=/etc/debux/mise
export MISE_SYSTEM_CONFIG_DIR=/etc/debux/mise
export MISE_GLOBAL_CONFIG_FILE="$MISE_DATA_DIR/config.toml"
export MISE_OVERRIDE_CONFIG_FILENAMES="$MISE_GLOBAL_CONFIG_FILE"
export MISE_OVERRIDE_TOOL_VERSIONS_FILENAMES=none
export MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS=""
export MISE_CEILING_PATHS=/
export MISE_YES=1
# Explicit dctl installs may download tools, never bootstrap sudo/Homebrew/APK
# dependencies or trigger surprise downloads while merely invoking a command.
export MISE_SYSTEM_DEPS=warn
export MISE_SYSTEM_PACKAGES_SUDO=false
export MISE_AUTO_INSTALL=false
export MISE_EXEC_AUTO_INSTALL=false
