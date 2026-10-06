"""Exercise release planning against real Git tags, commits and GitHub outputs."""

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from docker_release import highest_tag

SCRIPT = Path(__file__).with_name("docker_release.py").resolve()


class DockerReleaseTest(unittest.TestCase):
    def test_publication_policy(self):
        with tempfile.TemporaryDirectory() as directory:
            def git(*args):
                return subprocess.check_output(["git", "-C", directory, *args], text=True, stderr=subprocess.PIPE).strip()

            def run(mode, ref, event="workflow_dispatch", sha="", success=True):
                output = Path(directory, "github-output")
                output.write_text("")
                result = subprocess.run(
                    [sys.executable, str(SCRIPT), mode], cwd=directory, capture_output=True, text=True,
                    env={**os.environ, "TARGET_REF": ref, "EVENT_NAME": event, "BUILT_SHA": sha, "GITHUB_OUTPUT": str(output)},
                )
                self.assertEqual(result.returncode, 0 if success else 1, result.stdout + result.stderr)
                return dict(line.split("=", 1) for line in output.read_text().splitlines())

            git("init", "-b", "main")
            git("config", "user.name", "Release Test")
            git("config", "user.email", "release@example.invalid")
            git("commit", "--allow-empty", "-m", "first")
            first_sha = git("rev-parse", "HEAD")
            git("tag", "v0.1.9")
            git("commit", "--allow-empty", "-m", "second")
            second_sha = git("rev-parse", "HEAD")
            git("tag", "-a", "v0.1.10", "-m", "release")
            git("tag", "v1.0.0-rc.1")

            self.assertEqual(highest_tag(["v1.9", "v1.10", "v1.10.1", "v2.0-rc.1", "latest"]), "v1.10.1")
            self.assertEqual(highest_tag(["1.9.0", "1.10.0"]), "1.10.0")
            self.assertIsNone(highest_tag(["latest", "v1.0.0-rc.1"]))
            self.assertEqual(run("plan", "v0.1.9", "push"), {"build": "false"})
            self.assertEqual(run("plan", "v1.0.0-rc.1", "push"), {"build": "false"})
            self.assertEqual(run("plan", "other/tag", "push"), {"build": "false"})
            self.assertEqual(run("plan", "v0.1.10", "push"), {"build": "true", "tag": "v0.1.10", "sha": second_sha})
            self.assertEqual(run("plan", "v0.1.9")["sha"], first_sha)
            self.assertEqual(run("latest", "v0.1.9", sha=first_sha), {"latest": "false"})
            self.assertEqual(run("latest", "v0.1.10", sha=second_sha), {"latest": "true"})
            for ref in (first_sha, second_sha[:8], second_sha.upper()):
                self.assertTrue(run("plan", ref)["sha"].lower().startswith(ref.lower()))
                self.assertEqual(run("latest", ref, sha=second_sha), {"latest": "false"})
            for ref in ("main", "v9.9.9", "latest", "missing/tag", "a" * 129, "a" * 40, "x\nlatest=true", "$(touch injected)"):
                self.assertEqual(run("plan", ref, success=False), {})
            git("tag", "latest")
            self.assertEqual(run("plan", "latest", success=False), {})

            # A newer tag arriving during a build must prevent promotion.
            git("tag", "v0.2.0", first_sha)
            self.assertEqual(run("latest", "v0.1.10", sha=second_sha), {"latest": "false"})
            self.assertEqual(run("plan", "v0.1.10", "push"), {"build": "false"})
            git("tag", "-d", "v0.2.0")
            git("tag", "-f", "v0.1.10", first_sha)
            self.assertEqual(run("latest", "v0.1.10", sha=second_sha), {"latest": "false"})
            git("tag", "-d", "v0.1.10")
            self.assertEqual(run("latest", "v0.1.10", sha=second_sha), {"latest": "false"})

            # Match the workflow's tag refresh, including remote deletions.
            mirror = Path(directory, "automation")
            git("clone", "--quiet", "--no-hardlinks", directory, str(mirror))
            git("tag", "v0.2.0")
            refresh = ["git", "-C", str(mirror), "fetch", "--force", "--prune", "origin", "+refs/tags/*:refs/tags/*"]
            subprocess.run(refresh, check=True, capture_output=True)
            self.assertIn("v0.2.0", subprocess.check_output(["git", "-C", str(mirror), "tag"], text=True))
            git("tag", "-d", "v0.2.0")
            subprocess.run(refresh, check=True, capture_output=True)
            self.assertNotIn("v0.2.0", subprocess.check_output(["git", "-C", str(mirror), "tag"], text=True))


if __name__ == "__main__":
    unittest.main()
