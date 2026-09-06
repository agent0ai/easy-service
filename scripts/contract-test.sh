#!/bin/sh
set -eu
vars='GIT_URL GIT_TOKEN GIT_BRANCH UPDATE_METHOD UPDATE_PATTERN POLL_INTERVAL RUNTIME_IMAGE SETUP_COMMAND RUN_COMMAND SERVICE_PORT HEALTH_PATH STARTUP_TIMEOUT HEALTH_INTERVAL HEALTH_FAILURES DATA_DIR SERVICE_MEMORY_LIMIT'
for var in $vars; do grep -q "$var" README.md || { echo "README missing $var"; exit 1; }; done
for forbidden in LISTEN_PORT LOG_LEVEL HEALTH_TIMEOUT; do ! grep -q "$forbidden" internal/config/config.go || { echo "unexpected config $forbidden"; exit 1; }; done
grep -q 'restart: unless-stopped' compose.yaml
grep -q '8080:80' compose.yaml
grep -q '/data' compose.yaml
! grep -q '/var/run/docker.sock' compose.yaml Dockerfile
