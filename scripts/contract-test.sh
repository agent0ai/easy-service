#!/bin/sh
set -eu
vars='GIT_URL GIT_TOKEN GIT_BRANCH UPDATE_METHOD UPDATE_PATTERN POLL_INTERVAL RUNTIME_IMAGE SETUP_COMMAND RUN_COMMAND HEALTH_PATH STARTUP_TIMEOUT HEALTH_INTERVAL HEALTH_FAILURES DATA_DIR CONFIG_DIR LOG_DIR SERVICE_MEMORY_LIMIT DRAIN_TIMEOUT SETUP_TIMEOUT LOG_RETENTION_DAYS LOG_MAX_FILE_SIZE LOG_MAX_TOTAL_SIZE'
for var in $vars; do grep -q "$var" README.md || { echo "README missing $var"; exit 1; }; done
for forbidden in LISTEN_PORT LOG_LEVEL HEALTH_TIMEOUT SERVICE_PORT; do ! grep -q "$forbidden" internal/config/config.go || { echo "unexpected config $forbidden"; exit 1; }; done
grep -q 'restart: unless-stopped' compose.yaml
grep -q '8080:80' compose.yaml
grep -q 'mkdir /data /config /logs' Dockerfile
! grep -q '^VOLUME' Dockerfile
! grep -q '/var/run/docker.sock' compose.yaml Dockerfile
! grep -Eq 'rootlesskit|slirp4netns|uidmap|/dev/net/tun|/etc/subuid|/etc/subgid' compose.yaml Dockerfile
