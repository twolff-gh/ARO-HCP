"""Prow CI data acquisition for triage.

Handles GCS HTML parsing, parallel HTTP, junit XML extraction, and
GitHub API access.

Commands:
    list-jobs       — recent jobs with status (parallel fetching)
    list-dir        — GCS directory listing
    resolve-url     — presubmit GCS path indirection
    fetch-failures  — per-test failures from junit.xml
    fetch-step-failures — CI step-level failures from junit_operator.xml
    fetch-build-log — test/provision step build-log.txt (tail + errors)
    pr-checks       — PR check status from GitHub
    env-health      — pass/fail summary with failure samples
    all-periodic    — latest status of all periodic jobs for an env
    lookup-job      — find a job by ID across all envs and types

CLI usage:
    prow.py list-jobs ENV TYPE [--failed] [--limit N] [--since DATETIME]
    prow.py list-dir URL
    prow.py resolve-url JOB_ID ENV
    prow.py fetch-failures BASE_URL ENV
    prow.py fetch-step-failures BASE_URL
    prow.py fetch-build-log BASE_URL ENV [--step provision] [--lines N]
    prow.py pr-checks PR_NUMBER [--repo OWNER/REPO]
    prow.py env-health ENV TYPE [--since DATETIME] [--limit N] [--sample N]
    prow.py all-periodic ENV
    prow.py lookup-job JOB_ID
"""

import argparse
import json
import re
import subprocess
import sys
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET
from concurrent.futures import ThreadPoolExecutor, as_completed

GCSWEB_BASE = "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/test-platform-results"

_ANSI_RE = re.compile(r"\x1b\[[0-9;]*m|\ufffd\[[0-9;]*m")

# gh pr checks states that count as failures
_GH_FAIL_STATES = frozenset({
    "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE",
})

# GitHub check-run conclusions that count as failures
_GHA_FAIL_CONCLUSIONS = frozenset({
    "failure", "timed_out", "action_required", "startup_failure",
})

PERIODIC_JOBS = {
    "int": "periodic-ci-Azure-ARO-HCP-main-periodic-integration-e2e-parallel",
    "stg": "periodic-ci-Azure-ARO-HCP-main-periodic-stage-e2e-parallel",
    "prod": "periodic-ci-Azure-ARO-HCP-main-periodic-prod-e2e-parallel",
}

# All periodic jobs by environment. Includes e2e, cleanup, and global jobs.
ALL_PERIODIC_JOBS = {
    "dev": [
        "periodic-ci-Azure-ARO-HCP-main-periodic-delete-expired-development-resource-groups",
    ],
    "int": [
        "periodic-ci-Azure-ARO-HCP-main-periodic-integration-e2e-parallel",
        "periodic-ci-Azure-ARO-HCP-main-periodic-delete-expired-integration-resource-groups",
    ],
    "stg": [
        "periodic-ci-Azure-ARO-HCP-main-periodic-stage-e2e-parallel",
        "periodic-ci-Azure-ARO-HCP-main-periodic-delete-expired-stage-resource-groups",
    ],
    "prod": [
        "periodic-ci-Azure-ARO-HCP-main-periodic-prod-e2e-parallel",
        "periodic-ci-Azure-ARO-HCP-main-periodic-delete-expired-prod-resource-groups",
    ],
    "global": [
        "periodic-ci-Azure-ARO-HCP-main-periodic-delete-expired-kusto-role-assignments",
        "periodic-ci-Azure-ARO-HCP-main-image-updater-image-updater-tooling",
    ],
}

PRESUBMIT_JOBS = {
    "dev": "pull-ci-Azure-ARO-HCP-main-e2e-parallel",
    "int": "pull-ci-Azure-ARO-HCP-main-integration-e2e-parallel",
    "stg": "pull-ci-Azure-ARO-HCP-main-stage-e2e-parallel",
    "prod": "pull-ci-Azure-ARO-HCP-main-prod-e2e-parallel",
}

TEST_STEPS = {
    "dev": "e2e-parallel",
    "int": "integration-e2e-parallel",
    "stg": "stage-e2e-parallel",
    "prod": "prod-e2e-parallel",
}

# Container names within test steps
TEST_CONTAINERS = {
    "dev": "aro-hcp-test-local",
    "int": "aro-hcp-test-persistent",
    "stg": "aro-hcp-test-persistent",
    "prod": "aro-hcp-test-persistent",
}
PROVISION_CONTAINER = "aro-hcp-provision-environment"

# Max characters to keep from failure messages. Takes the tail so that
# stack traces preserve the most recent (most relevant) frames.
MAX_MESSAGE_CHARS = 2000


_PROW_DASHBOARD = "https://prow.ci.openshift.org/view/gs/"

_GITHUB_API = "https://api.github.com"

# Map env-specific substrings in job URLs to their env
_ENV_URL_MARKERS = {
    "integration-e2e": "int",
    "stage-e2e": "stg",
    "prod-e2e": "prod",
    "development-": "dev",
}


def _warn_env_mismatch(base_url, env):
    """Print a warning if base_url contains a job name for a different env."""
    for marker, marker_env in _ENV_URL_MARKERS.items():
        if marker in base_url and marker_env != env:
            print(
                json.dumps({"warning": f"URL contains '{marker}' "
                            f"(env '{marker_env}') but env '{env}' "
                            f"was specified. Results may be wrong."}),
                file=sys.stderr)
            return


def _normalize_base_url(url):
    """Convert a Prow dashboard URL or raw URL to a GCSWEB base URL.

    Handles:
    - Prow dashboard URLs: prow.ci.openshift.org/view/gs/test-platform-results/...
    - Query params and fragments: stripped
    - Trailing slashes: stripped
    """
    if _PROW_DASHBOARD in url:
        gcs_path = url.split("/view/gs/", 1)[1]
    elif "/view/gs/" in url:
        gcs_path = url.split("/view/gs/", 1)[1]
    else:
        return url.split("?")[0].split("#")[0].rstrip("/")

    gcs_path = gcs_path.split("?")[0].split("#")[0].rstrip("/")
    return f"https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/{gcs_path}"


def _run_gh(*args):
    """Run a gh CLI command and return parsed JSON.

    Returns parsed JSON, or None if stdout is empty.
    Raises RuntimeError on failure.
    """
    cmd = ["gh"] + list(args)
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    except FileNotFoundError:
        raise RuntimeError("gh CLI not found. Install: https://cli.github.com/")
    if r.returncode != 0:
        stderr = r.stderr.strip()
        if "auth" in stderr.lower() or "login" in stderr.lower():
            raise RuntimeError("gh CLI not authenticated. Run: gh auth login")
        raise RuntimeError(f"gh failed: {stderr}")
    return json.loads(r.stdout) if r.stdout.strip() else None


def _gh_available():
    """Check if gh CLI is installed and authenticated."""
    try:
        r = subprocess.run(["gh", "auth", "status"],
                           capture_output=True, text=True, timeout=10)
        return r.returncode == 0
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        return False


def _github_api_request(url, timeout=15):
    """GET a GitHub API URL. Returns (parsed_json, headers_dict).

    Works without auth for public repos. Raises RuntimeError on failure.
    """
    req = urllib.request.Request(url, headers={
        "Accept": "application/vnd.github+json",
        "User-Agent": "aro-hcp-ci-triage",
    })
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            data = json.loads(r.read())
            headers = {"Link": r.headers.get("Link", "")}
            return data, headers
    except urllib.error.HTTPError as e:
        if e.code == 403:
            raise RuntimeError(
                "GitHub API rate limit exceeded. Authenticate with: "
                "gh auth login, or set GH_TOKEN env var.")
        raise RuntimeError(f"GitHub API error {e.code}: {e.reason}")
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        raise RuntimeError(f"GitHub API request failed: {e}")


def _github_api_fetch(url, timeout=15):
    return _github_api_request(url, timeout)[0]


def _github_api_get_all(path, array_key=None, timeout=15):
    """GET all pages from a GitHub REST API endpoint.

    Follows Link: rel="next" headers for pagination.
    If array_key is set, extracts that key from each page and
    concatenates the arrays. Otherwise concatenates top-level arrays.
    """
    url = f"{_GITHUB_API}{path}"
    sep = "&" if "?" in path else "?"
    url = f"{url}{sep}per_page=100"
    all_items = []

    while url:
        data, headers = _github_api_request(url, timeout)
        if array_key:
            all_items.extend(data.get(array_key, []))
        else:
            all_items.extend(data if isinstance(data, list) else [])
        url = _parse_next_link(headers.get("Link", ""))

    return all_items


def _parse_next_link(link_header):
    """Extract the 'next' URL from a GitHub Link header."""
    if not link_header:
        return None
    for part in link_header.split(","):
        if 'rel="next"' in part:
            match = re.search(r'<([^>]+)>', part)
            if match:
                return match.group(1)
    return None


_BASE_SHA_RE = re.compile(r"BaseSHA:([0-9a-f]+)")


def _classify_prow_statuses(statuses):
    """Classify Prow commit statuses into failures and in-progress.

    Statuses are newest-first from the API. Groups by context, detects
    flakes (both success and failure exist) and resolved failures
    (latest terminal is success).

    Returns (failed_list, in_progress_list, prow_names_set).
    """
    failed = []
    in_progress = []

    groups = {}
    for s in statuses:
        ctx = s.get("context", "")
        groups.setdefault(ctx, []).append(s)

    prow_names = set()
    for ctx, runs in sorted(groups.items()):
        prow_names.add(ctx)
        latest_state = runs[0].get("state", "")

        terminal = [r for r in runs
                    if r.get("state") in ("success", "failure", "error")]
        if not terminal:
            if latest_state == "pending":
                in_progress.append(ctx)
            continue

        latest_terminal = terminal[0]
        has_success = any(r.get("state") == "success" for r in terminal)
        has_failure = any(
            r.get("state") in ("failure", "error") for r in terminal
        )

        if not has_failure:
            continue
        if latest_terminal.get("state") == "success":
            continue  # Resolved

        failed_run = next(
            (r for r in runs
             if r.get("state") in ("failure", "error")
             and r.get("target_url")), None)
        link = failed_run.get("target_url", "") if failed_run else ""

        base_sha = None
        if failed_run:
            desc = failed_run.get("description", "") or ""
            m = _BASE_SHA_RE.search(desc)
            if m:
                base_sha = m.group(1)

        failed.append({
            "name": ctx,
            "link": link,
            "status": "fail",
            "source": "prow" if ctx.startswith("ci/prow/") else "github-actions",
            "flake": has_success,
            "in_progress": latest_state == "pending",
            "resolved": False,
            "base_sha": base_sha,
        })

    return failed, in_progress, prow_names


def _classify_gha_check_runs(check_runs, exclude_names):
    """Classify GitHub Actions check-runs into failures and in-progress.

    Excludes names already processed as Prow statuses. Detects resolved
    failures, cancelled runs, and flakes.

    Returns (failed_list, in_progress_list).
    """
    failed = []
    in_progress = []

    groups = {}
    for cr in check_runs:
        name = cr.get("name", "")
        if name in exclude_names:
            continue
        groups.setdefault(name, []).append(cr)

    for name, runs in sorted(groups.items()):
        completed = [r for r in runs if r.get("status") == "completed"]
        not_completed = [r for r in runs if r.get("status") != "completed"]

        if not_completed:
            in_progress.append(name)

        if not completed:
            continue

        has_failure = any(
            r.get("conclusion") in _GHA_FAIL_CONCLUSIONS for r in completed
        )
        has_cancelled = any(
            r.get("conclusion") == "cancelled" for r in completed
        )
        has_success = any(
            r.get("conclusion") == "success" for r in completed
        )

        sorted_completed = sorted(
            completed,
            key=lambda r: r.get("completed_at") or "",
            reverse=True)
        latest_conclusion = sorted_completed[0].get("conclusion", "")
        resolved = latest_conclusion == "success"

        if has_failure:
            if resolved:
                continue
            failed_run = next(
                (r for r in completed
                 if r.get("conclusion") in _GHA_FAIL_CONCLUSIONS), None)
            link = ""
            if failed_run:
                link = (failed_run.get("details_url")
                        or failed_run.get("html_url", ""))
            failed.append({
                "name": name,
                "link": link,
                "status": "fail",
                "source": "github-actions",
                "flake": has_success,
                "in_progress": bool(not_completed),
                "resolved": False,
                "base_sha": None,
            })
        elif has_cancelled and not has_success:
            cancelled_run = next(
                (r for r in completed
                 if r.get("conclusion") == "cancelled"), None)
            link = ""
            if cancelled_run:
                link = (cancelled_run.get("details_url")
                        or cancelled_run.get("html_url", ""))
            failed.append({
                "name": name,
                "link": link,
                "status": "cancelled",
                "source": "github-actions",
                "flake": False,
                "in_progress": bool(not_completed),
                "resolved": False,
                "base_sha": None,
            })

    return failed, in_progress


def _process_checks_api(head_sha, check_runs, statuses):
    """Process raw GitHub API check-runs and statuses into classified failures.

    Orchestrates Prow status and GHA check-run classification with
    flake detection, resolved-failure detection, and source attribution.

    Returns {head_sha, failed: [...], in_progress: [...]}.
    """
    prow_failed, prow_ip, prow_names = _classify_prow_statuses(statuses)
    gha_failed, gha_ip = _classify_gha_check_runs(check_runs, prow_names)

    return {
        "head_sha": head_sha,
        "failed": sorted(prow_failed + gha_failed, key=lambda f: f["name"]),
        "in_progress": sorted(set(prow_ip + gha_ip)),
    }


def _parse_junit_failures(data, name_field="name"):
    """Parse junit XML bytes into a list of failure dicts.

    Each dict has {name_field: str, message: str}. Returns None if data
    is None or unparseable, empty list if no failures found.
    """
    if not data:
        return None
    try:
        root = ET.fromstring(data)
    except ET.ParseError:
        return None
    failures = []
    for tc in root.iter("testcase"):
        fail = tc.find("failure")
        if fail is not None:
            msg = fail.get("message") or fail.text or ""
            failures.append({
                name_field: tc.get("name", ""),
                "message": msg[-MAX_MESSAGE_CHARS:],
            })
    return failures


class Fetcher:
    """HTTP fetcher for GCS and JSON endpoints."""

    def fetch(self, url, timeout=15):
        """Fetch URL, return bytes or None on any error."""
        try:
            with urllib.request.urlopen(url, timeout=timeout) as r:
                return r.read()
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError, OSError):
            return None

    def fetch_text(self, url, timeout=15):
        """Fetch URL as text, rejecting HTML responses.

        GCS returns 200 with HTML directory listings for paths that
        don't exist as files. This method detects and rejects those.
        Returns decoded string or None.
        """
        try:
            with urllib.request.urlopen(url, timeout=timeout) as r:
                if "text/html" in r.headers.get("Content-Type", ""):
                    return None
                return r.read().decode("utf-8", errors="replace")
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError, OSError):
            return None

    def fetch_json(self, url, timeout=15):
        """Fetch URL as JSON. Rejects HTML responses (GCS 404 quirk)."""
        try:
            with urllib.request.urlopen(url, timeout=timeout) as r:
                if "text/html" in r.headers.get("Content-Type", ""):
                    return None
                return json.loads(r.read())
        except (urllib.error.URLError, urllib.error.HTTPError,
                TimeoutError, OSError, json.JSONDecodeError):
            return None


class ProwClient:
    """Client for Prow CI data acquisition.

    Accepts a Fetcher instance for testability.
    """

    def __init__(self, fetcher=None):
        self.fetcher = fetcher or Fetcher()

    def list_dir(self, url):
        """List filenames from a GCS web directory listing.

        Handles GCS HTML quirks: absolute-path hrefs, navigation links,
        style refs. Returns sorted list of clean filenames.
        """
        normalized = url.rstrip("/") + "/"
        html = self.fetcher.fetch(normalized)
        if not html:
            return []
        # Extract the path prefix from the URL to match absolute hrefs
        # e.g. "https://host/gcs/bucket/path/" -> "/gcs/bucket/path/"
        from urllib.parse import urlparse
        url_path = urlparse(normalized).path
        hrefs = re.findall(r'href="([^"]*)"', html.decode("utf-8", errors="replace"))
        names = []
        for h in hrefs:
            if h.startswith("?") or h.startswith("http"):
                continue
            # Handle absolute-path hrefs (GCS uses these)
            if h.startswith("/"):
                if not h.startswith(url_path):
                    continue
                # Strip the parent path prefix to get the child name
                h = h[len(url_path):]
                if not h:
                    continue
            name = h.rstrip("/").rsplit("/", 1)[-1]
            if name and name != "..":
                names.append(name)
        return sorted(set(names))

    def resolve(self, job_id, env):
        """Resolve presubmit base URL via GCS indirection.

        Returns base URL string. Raises ValueError on failure.
        """
        job_name = PRESUBMIT_JOBS.get(env)
        if not job_name:
            raise ValueError(f"Unknown env: {env}. Valid: {', '.join(PRESUBMIT_JOBS)}")

        txt_url = f"{GCSWEB_BASE}/pr-logs/directory/{job_name}/{job_id}.txt"
        text = self.fetcher.fetch_text(txt_url, timeout=10)
        if not text:
            raise ValueError(f"Could not fetch {txt_url}")

        gs_path = text.strip()
        if not gs_path.startswith("gs://"):
            raise ValueError(f"Expected gs:// path, got: {gs_path[:100]}")

        relative = gs_path.replace("gs://test-platform-results/", "", 1)
        return f"{GCSWEB_BASE}/{relative}"

    def fetch_failures(self, base_url, env):
        """Fetch per-test failures from junit.xml.

        Returns list of {test, message}, empty list if no failures,
        or None if junit.xml not found.
        """
        step = TEST_STEPS.get(env)
        if not step:
            raise ValueError(f"Unknown env: {env}. Valid: {', '.join(TEST_STEPS)}")

        container = TEST_CONTAINERS[env]
        url = f"{base_url}/artifacts/{step}/{container}/artifacts/junit.xml"
        data = self.fetcher.fetch(url, timeout=20)
        return _parse_junit_failures(data, name_field="test")

    def fetch_steps(self, base_url):
        """Fetch step-level failures from junit_operator.xml.

        Returns list of {step, message}, empty list if no failures,
        or None if junit_operator.xml not found.
        """
        url = f"{base_url}/artifacts/junit_operator.xml"
        data = self.fetcher.fetch(url, timeout=20)
        return _parse_junit_failures(data, name_field="step")

    def fetch_build_log(self, base_url, env, step="test", lines=80):
        """Fetch build-log.txt from test or provision step.

        step: "test" for test runner output, "provision" for ARM provisioning.
        lines: number of tail lines to return.

        Returns {step, container, lines: [str], total_lines: int},
        or None if not found.
        """
        test_step = TEST_STEPS.get(env)
        if not test_step:
            raise ValueError(f"Unknown env: {env}. Valid: {', '.join(TEST_STEPS)}")

        container = PROVISION_CONTAINER if step == "provision" else TEST_CONTAINERS[env]
        url = f"{base_url}/artifacts/{test_step}/{container}/build-log.txt"
        text = self.fetcher.fetch_text(url, timeout=20)
        if not text:
            return None

        text = _ANSI_RE.sub("", text)
        all_lines = text.splitlines()
        total = len(all_lines)
        tail = all_lines[-lines:]

        return {
            "step": test_step,
            "container": container,
            "lines": tail,
            "total_lines": total,
        }

    def _fetch_status(self, job_id, env, job_type):
        """Fetch status for one job. Returns dict or None."""
        try:
            if job_type == "periodic":
                name = PERIODIC_JOBS.get(env)
                base = f"{GCSWEB_BASE}/logs/{name}/{job_id}" if name else None
            else:
                base = self.resolve(job_id, env)
        except ValueError:
            return None
        if not base:
            return None

        pj = self.fetcher.fetch_json(f"{base}/prowjob.json", timeout=10)
        if not pj:
            return None

        status = pj.get("status", {})
        entry = {
            "state": status.get("state", "unknown"),
            "started": (status.get("startTime") or "?")[:19],
            "completed": (status.get("completionTime") or "running")[:19],
            "env": env,
            "job_id": job_id,
            "url": status.get("url", ""),
            "base_url": base,
        }

        if job_type == "presubmit":
            pulls = pj.get("spec", {}).get("refs", {}).get("pulls", [])
            if pulls:
                entry["pr"] = pulls[0].get("number")
                entry["pr_title"] = pulls[0].get("title", "")
                entry["pr_author"] = pulls[0].get("author", "")

        return entry

    def list_jobs(self, env, job_type, limit=20, failed_only=False, since=None):
        """List recent jobs with status via parallel fetching.

        Returns list of dicts sorted newest-first.
        Raises ValueError if env/job_type combo is invalid.
        """
        if since and not re.match(r"\d{4}-\d{2}-\d{2}", since):
            raise ValueError(f"--since must be ISO format (YYYY-MM-DD or YYYY-MM-DDTHH:MM), got: {since}")

        name = PERIODIC_JOBS.get(env) if job_type == "periodic" else PRESUBMIT_JOBS.get(env)
        if not name:
            valid = "periodic: int, stg, prod" if job_type == "periodic" else "presubmit: dev, int, stg, prod"
            raise ValueError(f"No {job_type} job for env '{env}'. Valid: {valid}.")

        if job_type == "periodic":
            listing_url = f"{GCSWEB_BASE}/logs/{name}/"
        else:
            listing_url = f"{GCSWEB_BASE}/pr-logs/directory/{name}/"

        html = self.fetcher.fetch(listing_url)
        if not html:
            return []

        # Prow job IDs are exactly 19 digits. Word boundaries prevent matching
        # substrings of longer numbers (timestamps, hashes).
        ids = re.findall(r"\b\d{19}\b", html.decode("utf-8", errors="replace"))
        unique = list(dict.fromkeys(ids))
        fetch_limit = max(limit, 100) if since else limit
        job_ids = sorted(unique, reverse=True)[:fetch_limit]

        results = []
        with ThreadPoolExecutor(max_workers=8) as pool:
            futures = {pool.submit(self._fetch_status, jid, env, job_type): jid for jid in job_ids}
            for f in as_completed(futures):
                try:
                    entry = f.result()
                except Exception:
                    continue
                if entry is None:
                    continue
                if failed_only and entry["state"] not in ("failure", "error"):
                    continue
                if since and entry["started"] < since:
                    continue
                results.append(entry)

        results.sort(key=lambda e: e["job_id"], reverse=True)
        return results[:limit]

    def _sample_failure(self, job, env):
        """Fetch failure summary from a job's artifacts.

        Tries junit.xml first (test-level), then junit_operator.xml (step-level).
        Returns a dict with the failure data, never None.
        """
        base_url = job["base_url"]
        base = {
            "job_id": job["job_id"],
            "base_url": base_url,
            "pr": job.get("pr"),
            "pr_title": job.get("pr_title"),
            "pr_author": job.get("pr_author"),
        }

        failures = self.fetch_failures(base_url, env)
        if failures:
            return {
                **base,
                "source": "junit.xml",
                "test": failures[0]["test"],
                "message": failures[0]["message"],
                "failed_tests": [f["test"] for f in failures],
                "num_failures": len(failures),
            }

        steps = self.fetch_steps(base_url)
        if not steps:
            return {**base, "source": None, "step": None, "message": None}

        # Prefer non-gather steps (gather failures are secondary effects)
        primary = [s for s in steps if "gather" not in s["step"].lower()]
        best = primary[0] if primary else steps[0]
        msg = _ANSI_RE.sub("", best["message"])

        return {
            **base,
            "source": "junit_operator.xml",
            "step": best["step"],
            "message": msg[-MAX_MESSAGE_CHARS:],
        }

    def env_health(self, env, job_type, since=None, sample=5, limit=20):
        """Environment health: pass/fail ratio with failure samples.

        Returns pass rate, time window, last success, and failure
        text from a sample of failed jobs.
        """
        all_jobs = self.list_jobs(env, job_type, limit=limit, since=since)
        if not all_jobs:
            return {
                "env": env, "type": job_type, "window": None,
                "total": 0, "passed": 0, "failed": 0, "pass_rate": 1.0,
                "last_success": None, "samples": [],
                "jobs_checked": 0, "unchecked_failures": 0,
            }

        passed = [j for j in all_jobs if j["state"] == "success"]
        failed = [j for j in all_jobs if j["state"] in ("failure", "error")]

        last_success = None
        if passed:
            ls = passed[0]
            last_success = {"job_id": ls["job_id"], "completed": ls["completed"]}
            if "pr" in ls:
                last_success["pr"] = ls["pr"]

        window = {
            "earliest": all_jobs[-1]["started"],
            "latest": all_jobs[0]["started"],
        }

        to_check = failed[:sample]
        samples = []
        if to_check:
            with ThreadPoolExecutor(max_workers=min(len(to_check), 8)) as pool:
                futures = {pool.submit(self._sample_failure, job, env): job
                           for job in to_check}
                for f in as_completed(futures):
                    try:
                        result = f.result()
                    except Exception:
                        continue
                    if result is not None:
                        samples.append(result)

        samples.sort(key=lambda s: s["job_id"], reverse=True)

        return {
            "env": env,
            "type": job_type,
            "window": window,
            "total": len(all_jobs),
            "passed": len(passed),
            "failed": len(failed),
            "pass_rate": round(len(passed) / len(all_jobs), 2),
            "last_success": last_success,
            "samples": samples,
            "jobs_checked": len(samples),
            "unchecked_failures": max(0, len(failed) - len(samples)),
        }

    def _latest_periodic_status(self, job_name):
        """Fetch the most recent run status for a periodic job.

        Returns {job_name, state, started, completed, job_id, base_url}
        or {job_name, state: "no_data"} if no runs found.
        """
        listing_url = f"{GCSWEB_BASE}/logs/{job_name}/"
        html = self.fetcher.fetch(listing_url)
        if not html:
            return {"job_name": job_name, "state": "no_data"}

        ids = re.findall(r"\b\d{19}\b", html.decode("utf-8", errors="replace"))
        if not ids:
            return {"job_name": job_name, "state": "no_data"}

        latest_id = sorted(set(ids), reverse=True)[0]
        base = f"{GCSWEB_BASE}/logs/{job_name}/{latest_id}"
        pj = self.fetcher.fetch_json(f"{base}/prowjob.json", timeout=10)
        if not pj:
            return {"job_name": job_name, "state": "no_data", "job_id": latest_id}

        status = pj.get("status", {})
        return {
            "job_name": job_name,
            "state": status.get("state", "unknown"),
            "started": (status.get("startTime") or "?")[:19],
            "completed": (status.get("completionTime") or "running")[:19],
            "job_id": latest_id,
            "base_url": base,
        }

    def lookup_job(self, job_id):
        """Find a job by ID across all environments and job types.

        Tries all periodic and presubmit paths in parallel.
        Returns {base_url, env, type, state, started, completed, job_id}
        or None if not found.
        """
        candidates = []
        # All periodic jobs across all envs (including global)
        for env, jobs in ALL_PERIODIC_JOBS.items():
            if env == "global":
                continue
            for job_name in jobs:
                candidates.append(("periodic", env, job_name))
        for job_name in ALL_PERIODIC_JOBS.get("global", []):
            candidates.append(("periodic", "global", job_name))
        # All presubmit jobs
        for env in PRESUBMIT_JOBS:
            candidates.append(("presubmit", env, None))

        def try_candidate(candidate):
            job_type, env, job_name = candidate
            if job_type == "periodic":
                base = f"{GCSWEB_BASE}/logs/{job_name}/{job_id}"
                pj = self.fetcher.fetch_json(
                    f"{base}/prowjob.json", timeout=10)
                if not pj:
                    return None
            else:
                try:
                    base = self.resolve(job_id, env)
                except ValueError:
                    return None
                pj = self.fetcher.fetch_json(
                    f"{base}/prowjob.json", timeout=10)
                if not pj:
                    return None

            status = pj.get("status", {})
            result = {
                "job_id": job_id,
                "base_url": base,
                "env": env,
                "type": job_type,
                "job_name": job_name or PRESUBMIT_JOBS.get(env, ""),
                "state": status.get("state", "unknown"),
                "started": (status.get("startTime") or "?")[:19],
                "completed": (
                    status.get("completionTime") or "running")[:19],
            }
            pulls = pj.get("spec", {}).get("refs", {}).get("pulls", [])
            if pulls:
                result["pr"] = pulls[0].get("number")
                result["pr_title"] = pulls[0].get("title", "")
                result["pr_author"] = pulls[0].get("author", "")
            return result

        with ThreadPoolExecutor(max_workers=12) as pool:
            futures = {pool.submit(try_candidate, c): c
                       for c in candidates}
            for f in as_completed(futures):
                try:
                    result = f.result()
                except Exception:
                    continue
                if result is not None:
                    # Cancel remaining futures
                    for remaining in futures:
                        remaining.cancel()
                    return result
        return None

    def all_periodic(self, env):
        """Latest run status for all periodic jobs in an environment.

        Includes env-specific jobs plus global jobs.
        Returns list of dicts with job_name, state, timestamps.
        """
        if env not in ALL_PERIODIC_JOBS:
            valid = [e for e in ALL_PERIODIC_JOBS if e != "global"]
            raise ValueError(f"Unknown env: {env}. Valid: {', '.join(valid)}")

        job_names = list(ALL_PERIODIC_JOBS[env])
        if env != "global":
            job_names += ALL_PERIODIC_JOBS.get("global", [])

        results = []
        with ThreadPoolExecutor(max_workers=min(len(job_names), 8)) as pool:
            futures = {pool.submit(self._latest_periodic_status, name): name
                       for name in job_names}
            for f in as_completed(futures):
                try:
                    results.append(f.result())
                except Exception:
                    continue

        results.sort(key=lambda r: r["job_name"])
        return results

    def pr_checks(self, pr_number, repo=None):
        """Get CI check status for a PR.

        Tries gh CLI first (handles auth, SSO, private repos). Falls
        back to GitHub REST API for public repos when gh is unavailable.
        """
        repo = repo or "Azure/ARO-HCP"

        if _gh_available():
            return self._pr_checks_gh(pr_number, repo)
        return self._pr_checks_api(pr_number, repo)

    def _pr_checks_gh(self, pr_number, repo):
        """PR checks via gh CLI."""
        pr_data = _run_gh("pr", "view", str(pr_number), "--repo", repo,
                          "--json", "state,mergedAt,headRefOid")
        if not pr_data:
            raise RuntimeError(f"Could not fetch PR #{pr_number}")

        checks = _run_gh("pr", "checks", str(pr_number), "--repo", repo,
                         "--json", "name,state,link") or []

        return self._process_checks(
            pr_state=pr_data["state"],
            pr_merged_at=pr_data.get("mergedAt"),
            head_sha=pr_data["headRefOid"],
            checks=checks,
        )

    @staticmethod
    def _pr_checks_api(pr_number, repo):
        """PR checks via GitHub REST API (no auth needed for public repos).

        Fetches full history to detect flakes, resolved failures, and
        source attribution (prow vs github-actions).
        """
        pr_data = _github_api_fetch(f"{_GITHUB_API}/repos/{repo}/pulls/{pr_number}")

        head_sha = pr_data["head"]["sha"]
        pr_state = "MERGED" if pr_data.get("merged_at") else pr_data["state"].upper()

        # Fetch both check runs and commit statuses with pagination.
        # Statuses are returned newest-first by GitHub.
        check_runs = _github_api_get_all(
            f"/repos/{repo}/commits/{head_sha}/check-runs",
            array_key="check_runs"
        )
        statuses = _github_api_get_all(
            f"/repos/{repo}/commits/{head_sha}/statuses"
        )

        result = _process_checks_api(head_sha, check_runs, statuses)
        result["pr_state"] = pr_state
        result["pr_merged_at"] = pr_data.get("merged_at")
        return result

    @staticmethod
    def _process_checks(pr_state, pr_merged_at, head_sha, checks):
        """Deduplicate and classify check results from gh CLI output.

        The gh CLI returns one entry per check (latest run only), so
        flake/resolved detection is not possible. Fields default to False.
        """
        by_name = {}
        for c in checks:
            name = c.get("name", "")
            prev = by_name.get(name)
            if not prev or prev.get("state") == "PENDING":
                by_name[name] = c

        failed = []
        in_progress = []
        for name in sorted(by_name):
            entry = by_name[name]
            state = entry.get("state", "")
            if state in _GH_FAIL_STATES:
                failed.append({
                    "name": name,
                    "link": entry.get("link", ""),
                    "status": "fail",
                    "source": "prow" if name.startswith("ci/prow/") else "github-actions",
                    "flake": False,
                    "in_progress": False,
                    "resolved": False,
                })
            elif state == "PENDING":
                in_progress.append(name)

        return {
            "pr_state": pr_state,
            "pr_merged_at": pr_merged_at,
            "head_sha": head_sha,
            "failed": failed,
            "in_progress": in_progress,
        }


def _build_parser():
    parser = argparse.ArgumentParser(
        prog="prow.py",
        description="Prow CI data acquisition for triage.",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    # list-jobs
    lj = sub.add_parser("list-jobs", help="List recent jobs with status")
    lj.add_argument("env", help="Environment (dev, int, stg, prod)")
    lj.add_argument("type", help="Job type (periodic, presubmit)")
    lj.add_argument("--failed", action="store_true", help="Only show failed jobs")
    lj.add_argument("--limit", type=int, default=20, help="Max jobs to return (default: 20)")
    lj.add_argument("--since", help="ISO date/datetime — only jobs started at/after this time")

    # list-dir
    ld = sub.add_parser("list-dir", help="List files in a GCS directory")
    ld.add_argument("url", help="GCS web directory URL")

    # resolve-url
    rs = sub.add_parser("resolve-url", help="Resolve presubmit job ID to base URL")
    rs.add_argument("job_id", help="19-digit Prow job ID")
    rs.add_argument("env", help="Environment (dev, int, stg, prod)")

    # fetch-failures
    ff = sub.add_parser("fetch-failures", help="Per-test failures from junit.xml")
    ff.add_argument("base_url", help="Job base URL (from list-jobs or resolve-url)")
    ff.add_argument("env", help="Environment (dev, int, stg, prod)")

    # fetch-step-failures
    fs = sub.add_parser(
        "fetch-step-failures",
        help="CI step-level failures from junit_operator.xml")
    fs.add_argument("base_url", help="Job base URL (from list-jobs or resolve-url)")

    # fetch-build-log
    fb = sub.add_parser("fetch-build-log", help="Test or provision step build-log.txt")
    fb.add_argument("base_url", help="Job base URL (from list-jobs or resolve-url)")
    fb.add_argument("env", help="Environment (dev, int, stg, prod)")
    fb.add_argument("--step", choices=["test", "provision"], default="test",
                    help="Step type: test (default) or provision")
    fb.add_argument("--lines", type=int, default=80, help="Tail lines to return (default: 80)")

    # pr-checks
    pc = sub.add_parser("pr-checks", help="PR check status from GitHub")
    pc.add_argument("pr_number", type=int, help="GitHub PR number")
    pc.add_argument("--repo", help="GitHub repo (default: Azure/ARO-HCP)", default=None)

    # env-health
    eh = sub.add_parser("env-health", help="Environment pass/fail summary with failure samples")
    eh.add_argument("env", help="Environment (dev, int, stg, prod)")
    eh.add_argument("type", help="Job type (periodic, presubmit)")
    eh.add_argument("--since", help="ISO date/datetime — only jobs started at/after this time")
    eh.add_argument("--limit", type=int, default=20, help="Max jobs to consider for pass rate (default: 20)")
    eh.add_argument("--sample", type=int, default=5, help="Number of failures to sample (default: 5)")

    # all-periodic
    ap = sub.add_parser(
        "all-periodic",
        help="Latest status of all periodic jobs for an environment")
    ap.add_argument("env", help="Environment (dev, int, stg, prod)")

    # lookup-job
    lk = sub.add_parser(
        "lookup-job",
        help="Find a job by ID across all envs and types")
    lk.add_argument("job_id", help="19-digit Prow job ID")

    return parser


def _die(msg):
    print(json.dumps({"error": msg}), file=sys.stderr)
    sys.exit(1)


def main(argv=None):
    parser = _build_parser()
    args = parser.parse_args(argv)
    client = ProwClient()

    try:
        if args.command == "list-jobs":
            result = client.list_jobs(args.env, args.type, limit=args.limit,
                                      failed_only=args.failed, since=args.since)
            print(json.dumps(result, indent=2))

        elif args.command == "list-dir":
            print(json.dumps(client.list_dir(args.url)))

        elif args.command == "resolve-url":
            print(client.resolve(args.job_id, args.env))

        elif args.command == "fetch-failures":
            base_url = _normalize_base_url(args.base_url)
            _warn_env_mismatch(base_url, args.env)
            result = client.fetch_failures(base_url, args.env)
            if result is None:
                print(json.dumps({"status": "no_junit", "message": "junit.xml not found"}))
            else:
                print(json.dumps(result, indent=2))

        elif args.command == "fetch-step-failures":
            base_url = _normalize_base_url(args.base_url)
            result = client.fetch_steps(base_url)
            if result is None:
                print(json.dumps({"status": "no_junit_operator", "message": "junit_operator.xml not found"}))
            else:
                print(json.dumps(result, indent=2))

        elif args.command == "fetch-build-log":
            base_url = _normalize_base_url(args.base_url)
            _warn_env_mismatch(base_url, args.env)
            result = client.fetch_build_log(base_url, args.env,
                                            step=args.step, lines=args.lines)
            if result is None:
                print(json.dumps({"status": "not_found",
                                  "message": f"build-log.txt not found for {args.step} step"}))
            else:
                print(json.dumps(result, indent=2))

        elif args.command == "pr-checks":
            print(json.dumps(client.pr_checks(args.pr_number, repo=args.repo), indent=2))

        elif args.command == "env-health":
            result = client.env_health(args.env, args.type, since=args.since,
                                       sample=args.sample, limit=args.limit)
            print(json.dumps(result, indent=2))

        elif args.command == "all-periodic":
            print(json.dumps(client.all_periodic(args.env), indent=2))

        elif args.command == "lookup-job":
            result = client.lookup_job(args.job_id)
            if result is None:
                _die(f"Job {args.job_id} not found in any env/type")
            print(json.dumps(result, indent=2))

    except (ValueError, RuntimeError) as e:
        _die(str(e))


if __name__ == "__main__":
    main()
