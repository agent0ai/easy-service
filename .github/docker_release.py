"""Resolve Docker publication targets and guard promotion of the latest tag."""

import argparse
import os
import re
import subprocess
import sys

DOCKER_TAG = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}")
VERSION_TAG = re.compile(r"v?([0-9]+)\.([0-9]+)(?:\.([0-9]+))?")
COMMIT_HASH = re.compile(r"[a-fA-F0-9]{7,40}")


def git(*args):
    return subprocess.check_output(["git", *args], text=True, stderr=subprocess.PIPE).strip()


def highest_tag(tags):
    versions = []
    for tag in tags:
        match = VERSION_TAG.fullmatch(tag)
        if match and DOCKER_TAG.fullmatch(tag):
            versions.append((tuple(int(part or 0) for part in match.groups()), tag))
    return max(versions)[1] if versions else None


def plan(event, ref):
    tags = git("tag", "--list").splitlines()
    highest = highest_tag(tags)
    if event == "push" and ref != highest:
        print(f"Skipping {ref}: highest stable version tag is {highest or 'absent'}.")
        return {"build": "false"}
    if not DOCKER_TAG.fullmatch(ref) or ref == "latest":
        raise ValueError("Use a Docker-compatible Git tag or commit hash; 'latest' is reserved.")
    if ref in tags:
        sha = git("rev-parse", "--verify", f"refs/tags/{ref}^{{commit}}")
    elif event == "workflow_dispatch" and COMMIT_HASH.fullmatch(ref):
        sha = git("rev-parse", "--verify", f"{ref.lower()}^{{commit}}")
        if not sha.startswith(ref.lower()):
            raise ValueError("The commit hash must identify a commit, not a named Git ref.")
    else:
        raise ValueError("The requested Git tag does not exist; branches are not publication targets.")
    print(f"Publishing {ref} from commit {sha} for linux/amd64 and linux/arm64.")
    return {"build": "true", "tag": ref, "sha": sha}


def can_publish_latest(ref, built_sha):
    tags = git("tag", "--list").splitlines()
    return ref == highest_tag(tags) and git("rev-parse", "--verify", f"refs/tags/{ref}^{{commit}}") == built_sha


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("plan", "latest"))
    mode = parser.parse_args().mode
    ref = os.environ["TARGET_REF"].strip()
    if mode == "plan":
        outputs = plan(os.environ["EVENT_NAME"], ref)
    else:
        latest = can_publish_latest(ref, os.environ["BUILT_SHA"])
        outputs = {"latest": str(latest).lower()}
        print(f"Publish latest for {ref}: {outputs['latest']}.")
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        for name, value in outputs.items():
            output.write(f"{name}={value}\n")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, subprocess.CalledProcessError) as error:
        message = str(error) if isinstance(error, ValueError) else "Cannot resolve the requested Git revision."
        print(f"::error::{message}", file=sys.stderr)
        sys.exit(1)
