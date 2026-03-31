"""Tests for prow.py — Prow CI data acquisition."""

import json
import os
import subprocess
import sys
import unittest
import urllib.error
from unittest.mock import patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import prow  # noqa: E402


# --- Shared fixtures ---

JUNIT_ONE_FAILURE = b"""<?xml version="1.0" encoding="UTF-8"?>
<testsuite tests="3" failures="1">
    <testcase name="TestPassing" time="10.0"></testcase>
    <testcase name="TestFailing" time="5.0">
        <failure message="RESPONSE 503: ServiceUnavailable">fail details</failure>
    </testcase>
    <testcase name="TestSkipped" time="0"><skipped/></testcase>
</testsuite>"""

JUNIT_TWO_FAILURES = b"""<?xml version="1.0" encoding="UTF-8"?>
<testsuite tests="3" failures="2">
    <testcase name="TestPassing" time="10.0"></testcase>
    <testcase name="TestFailing" time="5.0">
        <failure message="RESPONSE 503: ServiceUnavailable">details</failure>
    </testcase>
    <testcase name="TestAlsoFailing" time="3.0">
        <failure message="timeout waiting for cluster">details</failure>
    </testcase>
</testsuite>"""

OPERATOR_ONE_FAILURE = b"""<?xml version="1.0" encoding="UTF-8"?>
<testsuite tests="2" failures="1">
    <testcase name="provision-step" time="100"></testcase>
    <testcase name="Run multi-stage test e2e-parallel - provision container" time="60">
        <failure>quota exceeded in WestUS3</failure>
    </testcase>
</testsuite>"""


class MockFetcher(prow.Fetcher):
    """Test fetcher that returns preconfigured responses."""

    def __init__(self, fetch_fn=None, fetch_json_fn=None):
        self._fetch_fn = fetch_fn
        self._fetch_json_fn = fetch_json_fn

    def fetch(self, url, timeout=15):
        if self._fetch_fn:
            return self._fetch_fn(url, timeout)
        return None

    def fetch_text(self, url, timeout=15):
        data = self.fetch(url, timeout)
        if data is None:
            return None
        text = data.decode("utf-8", errors="replace")
        # Simulate real Fetcher's HTML rejection
        if text.lstrip().startswith(("<!DOCTYPE", "<html", "<HTML")):
            return None
        return text

    def fetch_json(self, url, timeout=15):
        if self._fetch_json_fn:
            return self._fetch_json_fn(url, timeout)
        return None


def _periodic_fetcher(listing_html, prowjob_fn):
    """Build a MockFetcher for periodic list_jobs tests.

    listing_html: bytes — the GCS directory listing page.
    prowjob_fn: callable(job_id) -> dict — returns prowjob.json content per job ID.
    """
    def fetch(url, timeout):
        if "/logs/" in url and "prowjob" not in url:
            return listing_html
        return None

    def fetch_json(url, timeout):
        if "prowjob.json" in url:
            # Extract job ID from URL — it's the path segment before /prowjob.json
            segment = url.split("/prowjob.json")[0].rsplit("/", 1)[-1]
            return prowjob_fn(segment)
        return None

    return MockFetcher(fetch_fn=fetch, fetch_json_fn=fetch_json)


def _make_prowjob(state="success", started="2026-03-31T10:00:00Z",
                  completed="2026-03-31T11:00:00Z", pulls=None):
    """Build a prowjob.json dict for tests."""
    pj = {
        "status": {
            "state": state,
            "startTime": started,
            "completionTime": completed,
            "url": "",
        },
        "spec": {"refs": {}},
    }
    if pulls:
        pj["spec"]["refs"]["pulls"] = pulls
    return pj


# --- Tests ---


class TestConstants(unittest.TestCase):
    def test_periodic_jobs_exist(self):
        for env in ("int", "stg", "prod"):
            self.assertIn(env, prow.PERIODIC_JOBS)

    def test_presubmit_jobs_exist(self):
        for env in ("dev", "int", "stg", "prod"):
            self.assertIn(env, prow.PRESUBMIT_JOBS)

    def test_dev_has_no_periodic(self):
        self.assertNotIn("dev", prow.PERIODIC_JOBS)

    def test_all_periodic_jobs_covers_all_envs(self):
        for env in ("dev", "int", "stg", "prod"):
            self.assertIn(env, prow.ALL_PERIODIC_JOBS)
            self.assertTrue(len(prow.ALL_PERIODIC_JOBS[env]) >= 1)

    def test_all_periodic_jobs_has_global(self):
        self.assertIn("global", prow.ALL_PERIODIC_JOBS)
        self.assertTrue(len(prow.ALL_PERIODIC_JOBS["global"]) >= 1)

    def test_all_periodic_includes_e2e_jobs(self):
        for env in ("int", "stg", "prod"):
            self.assertIn(prow.PERIODIC_JOBS[env], prow.ALL_PERIODIC_JOBS[env])

    def test_test_steps_cover_all_envs(self):
        for env in ("dev", "int", "stg", "prod"):
            self.assertIn(env, prow.TEST_STEPS)

    def test_step_names_match_job_names(self):
        self.assertEqual(prow.TEST_STEPS["dev"], "e2e-parallel")
        self.assertEqual(prow.TEST_STEPS["int"], "integration-e2e-parallel")
        self.assertEqual(prow.TEST_STEPS["stg"], "stage-e2e-parallel")
        self.assertEqual(prow.TEST_STEPS["prod"], "prod-e2e-parallel")


class TestParseJunitFailures(unittest.TestCase):
    def test_returns_none_for_none_input(self):
        self.assertIsNone(prow._parse_junit_failures(None))

    def test_returns_none_for_bad_xml(self):
        self.assertIsNone(prow._parse_junit_failures(b"not xml"))

    def test_returns_none_for_unclosed_tags(self):
        self.assertIsNone(prow._parse_junit_failures(
            b"<testsuite><testcase name='T1'><failure>oops"))

    def test_returns_empty_list_for_no_failures(self):
        xml = b'<testsuite><testcase name="T1"></testcase></testsuite>'
        self.assertEqual(prow._parse_junit_failures(xml), [])

    def test_extracts_failures(self):
        result = prow._parse_junit_failures(JUNIT_ONE_FAILURE)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["name"], "TestFailing")
        self.assertEqual(result[0]["message"], "RESPONSE 503: ServiceUnavailable")

    def test_custom_name_field(self):
        xml = b'<testsuite><testcase name="step1"><failure>err</failure></testcase></testsuite>'
        result = prow._parse_junit_failures(xml, name_field="step")
        self.assertEqual(result[0]["step"], "step1")

    def test_falls_back_to_text_when_no_message_attr(self):
        xml = b'<testsuite><testcase name="T1"><failure>text only</failure></testcase></testsuite>'
        result = prow._parse_junit_failures(xml)
        self.assertEqual(result[0]["message"], "text only")

    def test_truncates_long_messages(self):
        msg = "x" * 5000
        xml = f'<testsuite><testcase name="T1"><failure>{msg}</failure></testcase></testsuite>'
        result = prow._parse_junit_failures(xml.encode())
        self.assertEqual(len(result[0]["message"]), 2000)

    def test_handles_nested_testsuites(self):
        xml = b"""<testsuites>
            <testsuite name="suite1">
                <testcase name="T1"><failure message="err1">d</failure></testcase>
            </testsuite>
            <testsuite name="suite2">
                <testcase name="T2"><failure message="err2">d</failure></testcase>
            </testsuite>
        </testsuites>"""
        result = prow._parse_junit_failures(xml)
        self.assertEqual(len(result), 2)
        names = [r["name"] for r in result]
        self.assertIn("T1", names)
        self.assertIn("T2", names)


class TestListDir(unittest.TestCase):
    def test_parses_gcs_html_with_relative_hrefs(self):
        html = """<html><body>
        <a href="?C=N;O=D">Name</a>
        <a href="https://cloud.google.com/terms">Terms</a>
        <a href="/styles/style.css">CSS</a>
        <a href="..">Parent</a>
        <a href="prowjob.json">prowjob.json</a>
        <a href="ci-operator.log">ci-operator.log</a>
        <a href="subdir/">subdir</a>
        </body></html>"""

        fetcher = MockFetcher(fetch_fn=lambda url, timeout: html.encode())
        client = prow.ProwClient(fetcher)
        result = client.list_dir("https://example.com/some/path")
        self.assertEqual(result, ["ci-operator.log", "prowjob.json", "subdir"])

    def test_parses_gcs_html_with_absolute_hrefs(self):
        """Real GCS uses absolute-path hrefs like /gcs/bucket/path/child/."""
        html = """<html><body>
        <a href="/styles/style.css">CSS</a>
        <a href="/gcs/bucket/logs/">Parent</a>
        <a href="/gcs/bucket/logs/job/1234567890123456789/">1234567890123456789</a>
        <a href="/gcs/bucket/logs/job/1234567890123456790/">1234567890123456790</a>
        <a href="https://cloud.google.com/terms">Terms</a>
        </body></html>"""

        fetcher = MockFetcher(fetch_fn=lambda url, timeout: html.encode())
        client = prow.ProwClient(fetcher)
        result = client.list_dir("https://example.com/gcs/bucket/logs/job")
        self.assertEqual(result, ["1234567890123456789", "1234567890123456790"])

    def test_filters_absolute_urls_and_query_params(self):
        html = """<html><body>
        <a href="?C=N;O=D">Name</a>
        <a href="https://cloud.google.com/terms">Terms</a>
        <a href="https://some-new-link.google.com/foo">New nav</a>
        <a href="/styles/style.css">CSS</a>
        <a href="/some/absolute/path">Abs</a>
        <a href="http://example.com/external">External</a>
        <a href="prowjob.json">prowjob.json</a>
        </body></html>"""

        fetcher = MockFetcher(fetch_fn=lambda url, timeout: html.encode())
        client = prow.ProwClient(fetcher)
        result = client.list_dir("https://example.com/path")
        self.assertEqual(result, ["prowjob.json"])

    def test_normalizes_trailing_slash(self):
        captured = {}

        def capture(url, timeout):
            captured["url"] = url
            return b"<html></html>"
        fetcher = MockFetcher(fetch_fn=capture)
        client = prow.ProwClient(fetcher)
        client.list_dir("https://example.com/path/")
        self.assertTrue(captured["url"].endswith("/"))
        self.assertFalse(captured["url"].endswith("//"))

    def test_empty_on_failure(self):
        client = prow.ProwClient(MockFetcher())
        self.assertEqual(client.list_dir("https://example.com/bad"), [])


class TestResolve(unittest.TestCase):
    def test_unknown_env_raises(self):
        client = prow.ProwClient(MockFetcher())
        with self.assertRaises(ValueError) as ctx:
            client.resolve("1234567890123456789", "xxx")
        self.assertIn("Unknown env", str(ctx.exception))

    def test_valid_env_constructs_url(self):
        fetcher = MockFetcher(
            fetch_fn=lambda url, timeout: b"gs://test-platform-results/pr-logs/pull/some/path/123"
        )
        client = prow.ProwClient(fetcher)
        result = client.resolve("1234567890123456789", "int")
        self.assertEqual(result, f"{prow.GCSWEB_BASE}/pr-logs/pull/some/path/123")

    def test_html_response_raises(self):
        """GCS returns HTML for missing .txt files — should raise cleanly."""
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: b"<html>not found</html>")
        client = prow.ProwClient(fetcher)
        with self.assertRaises(ValueError) as ctx:
            client.resolve("1234567890123456789", "int")
        self.assertIn("Could not fetch", str(ctx.exception))

    def test_non_gs_path_raises(self):
        """Non-HTML response that isn't a gs:// path — should raise."""
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: b"not a gs path")
        client = prow.ProwClient(fetcher)
        with self.assertRaises(ValueError) as ctx:
            client.resolve("1234567890123456789", "int")
        self.assertIn("Expected gs://", str(ctx.exception))

    def test_fetch_failure_raises(self):
        client = prow.ProwClient(MockFetcher())
        with self.assertRaises(ValueError):
            client.resolve("1234567890123456789", "int")


class TestFetchFailures(unittest.TestCase):
    def test_returns_failures(self):
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: JUNIT_ONE_FAILURE)
        client = prow.ProwClient(fetcher)
        result = client.fetch_failures("https://example.com/base", "stg")
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["test"], "TestFailing")
        self.assertIn("503", result[0]["message"])

    def test_returns_none_when_no_junit(self):
        client = prow.ProwClient(MockFetcher())
        result = client.fetch_failures("https://example.com/base", "stg")
        self.assertIsNone(result)

    def test_unknown_env_raises(self):
        client = prow.ProwClient(MockFetcher())
        with self.assertRaises(ValueError):
            client.fetch_failures("https://example.com/base", "xxx")

    def test_uses_correct_test_step(self):
        captured = {}

        def capture(url, timeout):
            captured["url"] = url
            return JUNIT_ONE_FAILURE
        fetcher = MockFetcher(fetch_fn=capture)
        client = prow.ProwClient(fetcher)

        client.fetch_failures("https://base", "int")
        self.assertIn("integration-e2e-parallel", captured["url"])
        self.assertIn("aro-hcp-test-persistent", captured["url"])
        client.fetch_failures("https://base", "prod")
        self.assertIn("prod-e2e-parallel", captured["url"])

    def test_dev_uses_local_container(self):
        captured = {}

        def capture(url, timeout):
            captured["url"] = url
            return JUNIT_ONE_FAILURE
        fetcher = MockFetcher(fetch_fn=capture)
        client = prow.ProwClient(fetcher)
        client.fetch_failures("https://base", "dev")
        self.assertIn("aro-hcp-test-local", captured["url"])

    def test_falls_back_to_text_when_no_message_attr(self):
        xml = b"""<testsuite><testcase name="T1">
            <failure>raw error text here</failure>
        </testcase></testsuite>"""
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: xml)
        client = prow.ProwClient(fetcher)
        result = client.fetch_failures("https://base", "stg")
        self.assertEqual(result[0]["message"], "raw error text here")


class TestFetchSteps(unittest.TestCase):
    def test_returns_step_failures(self):
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: OPERATOR_ONE_FAILURE)
        client = prow.ProwClient(fetcher)
        result = client.fetch_steps("https://example.com/base")
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["step"], "Run multi-stage test e2e-parallel - provision container")
        self.assertIn("quota exceeded", result[0]["message"])

    def test_returns_none_when_not_found(self):
        client = prow.ProwClient(MockFetcher())
        self.assertIsNone(client.fetch_steps("https://example.com/base"))

    def test_returns_empty_when_no_failures(self):
        xml = b'<testsuite><testcase name="step1"></testcase></testsuite>'
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: xml)
        client = prow.ProwClient(fetcher)
        self.assertEqual(client.fetch_steps("https://example.com/base"), [])

    def test_fetches_correct_url(self):
        captured = {}

        def capture(url, timeout):
            captured["url"] = url
            return None
        fetcher = MockFetcher(fetch_fn=capture)
        client = prow.ProwClient(fetcher)
        client.fetch_steps("https://example.com/base")
        self.assertEqual(captured["url"], "https://example.com/base/artifacts/junit_operator.xml")


class TestFetchBuildLog(unittest.TestCase):

    BUILD_LOG = "line1\nline2\nline3\n\x1b[31mERROR: something failed\x1b[0m\nline5\n"

    def test_returns_log_tail(self):
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: self.BUILD_LOG.encode())
        client = prow.ProwClient(fetcher)
        result = client.fetch_build_log("https://example.com/base", "int")
        self.assertEqual(result["step"], "integration-e2e-parallel")
        self.assertEqual(result["container"], "aro-hcp-test-persistent")
        self.assertEqual(result["total_lines"], 5)
        self.assertEqual(len(result["lines"]), 5)
        self.assertIn("ERROR: something failed", result["lines"][3])
        self.assertNotIn("\x1b", result["lines"][3])

    def test_returns_none_when_not_found(self):
        client = prow.ProwClient(MockFetcher())
        self.assertIsNone(client.fetch_build_log("https://example.com/base", "int"))

    def test_unknown_env_raises(self):
        client = prow.ProwClient(MockFetcher())
        with self.assertRaises(ValueError):
            client.fetch_build_log("https://example.com/base", "xxx")

    def test_provision_step(self):
        captured = {}

        def capture(url, timeout):
            captured["url"] = url
            return b"provision output\n"
        fetcher = MockFetcher(fetch_fn=capture)
        client = prow.ProwClient(fetcher)
        result = client.fetch_build_log("https://example.com/base", "dev", step="provision")
        self.assertEqual(result["container"], "aro-hcp-provision-environment")
        self.assertIn("e2e-parallel", captured["url"])
        self.assertIn("aro-hcp-provision-environment", captured["url"])

    def test_respects_lines_limit(self):
        log = "\n".join(f"line{i}" for i in range(200))
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: log.encode())
        client = prow.ProwClient(fetcher)
        result = client.fetch_build_log("https://example.com/base", "int", lines=10)
        self.assertEqual(len(result["lines"]), 10)
        self.assertEqual(result["total_lines"], 200)
        self.assertEqual(result["lines"][0], "line190")

    def test_fetches_correct_url(self):
        captured = {}

        def capture(url, timeout):
            captured["url"] = url
            return None
        fetcher = MockFetcher(fetch_fn=capture)
        client = prow.ProwClient(fetcher)
        client.fetch_build_log("https://example.com/base", "stg")
        self.assertEqual(
            captured["url"],
            "https://example.com/base/artifacts/stage-e2e-parallel/"
            "aro-hcp-test-persistent/build-log.txt"
        )

    def test_strips_ansi_codes(self):
        log = b"\xef\xbf\xbd[37m[10:00:00]\xef\xbf\xbd[0m ERROR: boom\n"
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: log)
        client = prow.ProwClient(fetcher)
        result = client.fetch_build_log("https://example.com/base", "int")
        self.assertNotIn("\ufffd", result["lines"][0])
        self.assertIn("ERROR: boom", result["lines"][0])

    def test_rejects_html_directory_listing(self):
        """GCS returns HTML directory listing when build-log.txt path is a dir."""
        html = b"<html><body><h1>test-platform-results</h1></body></html>"
        fetcher = MockFetcher(fetch_fn=lambda url, timeout: html)
        client = prow.ProwClient(fetcher)
        result = client.fetch_build_log("https://example.com/base", "int")
        self.assertIsNone(result)


class TestFetchText(unittest.TestCase):
    """Tests for Fetcher.fetch_text — HTML rejection."""

    def test_rejects_html_content_type(self):
        from unittest.mock import MagicMock

        mock_response = MagicMock()
        mock_response.read.return_value = b"<html>directory listing</html>"
        mock_response.headers = {"Content-Type": "text/html; charset=utf-8"}
        mock_response.__enter__ = lambda s: s
        mock_response.__exit__ = MagicMock(return_value=False)

        fetcher = prow.Fetcher()
        with patch("urllib.request.urlopen", return_value=mock_response):
            result = fetcher.fetch_text("https://example.com/some/path")
        self.assertIsNone(result)

    def test_returns_text_for_plain_content(self):
        from unittest.mock import MagicMock

        mock_response = MagicMock()
        mock_response.read.return_value = b"line1\nline2\n"
        mock_response.headers = {"Content-Type": "text/plain"}
        mock_response.__enter__ = lambda s: s
        mock_response.__exit__ = MagicMock(return_value=False)

        fetcher = prow.Fetcher()
        with patch("urllib.request.urlopen", return_value=mock_response):
            result = fetcher.fetch_text("https://example.com/log.txt")
        self.assertEqual(result, "line1\nline2\n")

    def test_returns_none_on_error(self):
        fetcher = prow.Fetcher()
        with patch("urllib.request.urlopen", side_effect=urllib.error.URLError("fail")):
            result = fetcher.fetch_text("https://example.com/bad")
        self.assertIsNone(result)


class TestListJobs(unittest.TestCase):
    """Tests for list_jobs — error cases and happy path with periodic jobs."""

    def _make_periodic_client(self, job_ids, prowjob_fn):
        """Build a ProwClient for periodic list_jobs tests.

        job_ids: list of 19-digit strings to appear in the listing page.
        prowjob_fn: callable(job_id) -> dict or None.
        """
        listing_html = " ".join(
            f'<a href="{jid}/">{jid}</a>' for jid in job_ids
        ).encode()
        fetcher = _periodic_fetcher(listing_html, prowjob_fn)
        return prow.ProwClient(fetcher)

    # --- Error cases ---

    def test_invalid_env_raises(self):
        client = prow.ProwClient(MockFetcher())
        with self.assertRaises(ValueError):
            client.list_jobs("xxx", "periodic")

    def test_dev_periodic_raises(self):
        client = prow.ProwClient(MockFetcher())
        with self.assertRaises(ValueError) as ctx:
            client.list_jobs("dev", "periodic")
        self.assertIn("No periodic", str(ctx.exception))

    def test_empty_on_fetch_failure(self):
        client = prow.ProwClient(MockFetcher())
        self.assertEqual(client.list_jobs("int", "periodic", limit=1), [])

    def test_since_rejects_bad_format(self):
        client = prow.ProwClient(MockFetcher())
        with self.assertRaises(ValueError) as ctx:
            client.list_jobs("int", "periodic", since="2026-3-27")
        self.assertIn("ISO format", str(ctx.exception))

    # --- Happy path ---

    def test_returns_jobs_sorted_newest_first(self):
        ids = ["1234567890123456789", "1234567890123456790", "1234567890123456791"]

        def prowjob(jid):
            return _make_prowjob(
                state="success" if jid.endswith("1") else "failure",
                started=f"2026-03-31T1{jid[-1]}:00:00Z",
            )

        client = self._make_periodic_client(ids, prowjob)
        result = client.list_jobs("int", "periodic", limit=10)
        self.assertEqual(len(result), 3)
        self.assertEqual(result[0]["job_id"], "1234567890123456791")
        self.assertEqual(result[2]["job_id"], "1234567890123456789")
        self.assertEqual(result[0]["state"], "success")
        self.assertIn("base_url", result[0])

    def test_failed_only_filters(self):
        ids = ["1234567890123456789", "1234567890123456790"]

        def prowjob(jid):
            state = "failure" if jid == "1234567890123456789" else "success"
            return _make_prowjob(state=state)

        client = self._make_periodic_client(ids, prowjob)
        result = client.list_jobs("int", "periodic", limit=10, failed_only=True)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["state"], "failure")

    def test_limit_caps_results(self):
        ids = [str(1234567890123456789 + i) for i in range(5)]
        client = self._make_periodic_client(ids, lambda jid: _make_prowjob())
        result = client.list_jobs("int", "periodic", limit=2)
        self.assertEqual(len(result), 2)

    def test_since_filters_by_start_time(self):
        ids = ["1234567890123456789", "1234567890123456790"]

        def prowjob(jid):
            ts = "2026-03-30T10:00:00Z" if jid == "1234567890123456789" else "2026-03-31T10:00:00Z"
            return _make_prowjob(started=ts)

        client = self._make_periodic_client(ids, prowjob)
        result = client.list_jobs("int", "periodic", since="2026-03-31")
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["job_id"], "1234567890123456790")

    def test_since_with_limit(self):
        """When --since is used, --limit still caps the output."""
        ids = [str(1234567890123456789 + i) for i in range(5)]
        client = self._make_periodic_client(
            ids, lambda jid: _make_prowjob(started="2026-03-31T10:00:00Z"))
        result = client.list_jobs("int", "periodic", since="2026-03-31", limit=2)
        self.assertEqual(len(result), 2)

    def test_skips_jobs_with_no_prowjob(self):
        """Jobs whose prowjob.json fetch fails are silently skipped."""
        ids = ["1234567890123456789", "1234567890123456790"]

        def prowjob(jid):
            if jid == "1234567890123456789":
                return None
            return _make_prowjob()

        client = self._make_periodic_client(ids, prowjob)
        result = client.list_jobs("int", "periodic", limit=10)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["job_id"], "1234567890123456790")

    def test_periodic_populates_base_url(self):
        ids = ["1234567890123456789"]
        client = self._make_periodic_client(ids, lambda jid: _make_prowjob())
        result = client.list_jobs("int", "periodic", limit=1)
        job_name = prow.PERIODIC_JOBS["int"]
        self.assertIn(f"/logs/{job_name}/1234567890123456789", result[0]["base_url"])


class TestSampleFailure(unittest.TestCase):
    """Tests for _sample_failure."""

    def _job(self, job_id="123", **kwargs):
        return {"job_id": job_id, "base_url": "https://example.com/base", **kwargs}

    def test_returns_junit_failures_first(self):
        def fetch(url, timeout):
            if "junit.xml" in url and "junit_operator" not in url:
                return JUNIT_TWO_FAILURES
            return None
        client = prow.ProwClient(MockFetcher(fetch_fn=fetch))
        result = client._sample_failure(self._job(), "dev")
        self.assertEqual(result["source"], "junit.xml")
        self.assertEqual(result["base_url"], "https://example.com/base")
        self.assertEqual(result["test"], "TestFailing")
        self.assertIn("503", result["message"])
        self.assertEqual(result["num_failures"], 2)
        self.assertEqual(result["failed_tests"], ["TestFailing", "TestAlsoFailing"])

    def test_falls_back_to_operator_xml(self):
        def fetch(url, timeout):
            if "junit_operator.xml" in url:
                return OPERATOR_ONE_FAILURE
            return None
        client = prow.ProwClient(MockFetcher(fetch_fn=fetch))
        result = client._sample_failure(self._job(), "dev")
        self.assertEqual(result["source"], "junit_operator.xml")
        self.assertIn("quota exceeded", result["message"])

    def test_returns_none_when_no_artifacts(self):
        client = prow.ProwClient(MockFetcher())
        result = client._sample_failure(self._job(), "dev")
        self.assertIsNone(result["message"])
        self.assertIsNone(result["source"])

    def test_preserves_pr_fields(self):
        def fetch(url, timeout):
            if "junit_operator.xml" in url:
                return OPERATOR_ONE_FAILURE
            return None
        client = prow.ProwClient(MockFetcher(fetch_fn=fetch))
        job = self._job(pr=4611, pr_title="Fix thing", pr_author="alice")
        result = client._sample_failure(job, "dev")
        self.assertEqual(result["pr"], 4611)
        self.assertEqual(result["pr_title"], "Fix thing")
        self.assertEqual(result["pr_author"], "alice")

    def test_strips_ansi_codes(self):
        ansi_xml = (
            b'<testsuite><testcase name="step">'
            b'<failure>\xef\xbf\xbd[37m[10:00:00]\xef\xbf\xbd[0m ERROR: quota exceeded</failure>'
            b'</testcase></testsuite>'
        )

        def fetch(url, timeout):
            if "junit_operator.xml" in url:
                return ansi_xml
            return None
        client = prow.ProwClient(MockFetcher(fetch_fn=fetch))
        result = client._sample_failure(self._job(), "dev")
        self.assertNotIn("\ufffd", result["message"])
        self.assertIn("quota exceeded", result["message"])

    def test_prefers_non_gather_steps(self):
        xml = b"""<testsuite>
            <testcase name="gather-extra container">
                <failure>gather failed</failure>
            </testcase>
            <testcase name="e2e-parallel container">
                <failure>real error</failure>
            </testcase>
        </testsuite>"""

        def fetch(url, timeout):
            if "junit_operator.xml" in url:
                return xml
            return None

        client = prow.ProwClient(MockFetcher(fetch_fn=fetch))
        result = client._sample_failure(self._job(), "dev")
        self.assertEqual(result["step"], "e2e-parallel container")
        self.assertIn("real error", result["message"])

    def test_falls_back_to_gather_when_only_option(self):
        xml = b"""<testsuite>
            <testcase name="gather-extra container">
                <failure>gather failed</failure>
            </testcase>
        </testsuite>"""

        def fetch(url, timeout):
            if "junit_operator.xml" in url:
                return xml
            return None

        client = prow.ProwClient(MockFetcher(fetch_fn=fetch))
        result = client._sample_failure(self._job(), "dev")
        self.assertEqual(result["step"], "gather-extra container")


class TestEnvHealth(unittest.TestCase):
    """Tests for env_health."""

    def _make_client(self, jobs, failure_msg="some error"):
        """Build a ProwClient with mocked list_jobs and artifact fetching."""
        operator_xml = (
            f'<testsuite><testcase name="step"><failure>{failure_msg}'
            f'</failure></testcase></testsuite>'
        ).encode()

        def fetch(url, timeout):
            if "junit_operator.xml" in url:
                return operator_xml
            return None

        fetcher = MockFetcher(fetch_fn=fetch)
        client = prow.ProwClient(fetcher)
        client.list_jobs = lambda *args, **kwargs: jobs
        return client

    @staticmethod
    def _job(job_id, state, started="2026-03-31T10:00:00",
             completed="2026-03-31T11:00:00", **kwargs):
        return {
            "job_id": job_id, "state": state,
            "started": started, "completed": completed,
            "base_url": f"https://example.com/{job_id}", **kwargs,
        }

    def test_empty_returns_clean(self):
        client = self._make_client([])
        result = client.env_health("dev", "presubmit")
        self.assertEqual(result["total"], 0)
        self.assertEqual(result["pass_rate"], 1.0)
        self.assertEqual(result["samples"], [])
        self.assertIsNone(result["window"])

    def test_all_passing(self):
        jobs = [self._job("1234567890123456789", "success")]
        client = self._make_client(jobs)
        result = client.env_health("dev", "presubmit")
        self.assertEqual(result["passed"], 1)
        self.assertEqual(result["failed"], 0)
        self.assertEqual(result["pass_rate"], 1.0)
        self.assertEqual(result["samples"], [])

    def test_mixed_results(self):
        jobs = [
            self._job("1234567890123456789", "success", started="2026-03-31T12:00:00"),
            self._job("1234567890123456788", "failure", started="2026-03-31T10:00:00"),
        ]
        client = self._make_client(jobs, failure_msg="quota exceeded")
        result = client.env_health("dev", "presubmit")
        self.assertEqual(result["total"], 2)
        self.assertEqual(result["passed"], 1)
        self.assertEqual(result["failed"], 1)
        self.assertEqual(result["pass_rate"], 0.5)
        self.assertIsNotNone(result["last_success"])
        self.assertEqual(len(result["samples"]), 1)
        self.assertIn("quota exceeded", result["samples"][0]["message"])

    def test_error_state_counted_as_failed(self):
        jobs = [self._job("1234567890123456789", "error")]
        client = self._make_client(jobs)
        result = client.env_health("dev", "presubmit")
        self.assertEqual(result["failed"], 1)
        self.assertEqual(result["pass_rate"], 0.0)

    def test_sample_limits_checked_count(self):
        jobs = [
            self._job(f"123456789012345678{i}", "failure",
                      started=f"2026-03-31T{10+i:02d}:00:00")
            for i in range(10)
        ]
        client = self._make_client(jobs)
        result = client.env_health("dev", "presubmit", sample=3)
        self.assertEqual(result["jobs_checked"], 3)
        self.assertEqual(result["unchecked_failures"], 7)

    def test_sample_zero_no_crash(self):
        jobs = [self._job("1234567890123456789", "failure")]
        client = self._make_client(jobs)
        result = client.env_health("dev", "presubmit", sample=0)
        self.assertEqual(result["jobs_checked"], 0)
        self.assertEqual(result["unchecked_failures"], 1)

    def test_window_uses_first_and_last_job(self):
        jobs = [
            self._job("1234567890123456791", "success", started="2026-03-31T14:00:00"),
            self._job("1234567890123456790", "failure", started="2026-03-31T10:00:00"),
            self._job("1234567890123456789", "success", started="2026-03-30T08:00:00"),
        ]
        client = self._make_client(jobs)
        result = client.env_health("dev", "presubmit")
        self.assertEqual(result["window"]["earliest"], "2026-03-30T08:00:00")
        self.assertEqual(result["window"]["latest"], "2026-03-31T14:00:00")

    def test_last_success_includes_pr_when_present(self):
        jobs = [
            self._job("1234567890123456789", "success", pr=42),
        ]
        client = self._make_client(jobs)
        result = client.env_health("dev", "presubmit")
        self.assertEqual(result["last_success"]["pr"], 42)

    def test_raw_message_not_classified(self):
        jobs = [self._job("1234567890123456789", "failure")]
        client = self._make_client(jobs, failure_msg="panic: nil pointer dereference")
        result = client.env_health("dev", "presubmit")
        sample = result["samples"][0]
        self.assertIn("panic: nil pointer dereference", sample["message"])
        self.assertNotIn("category", sample)
        self.assertNotIn("signature", sample)


class TestPrChecksGh(unittest.TestCase):
    """Tests for pr_checks via gh CLI path."""

    PR_OPEN = {"state": "OPEN", "mergedAt": None, "headRefOid": "abc123"}

    @patch("prow._gh_available", return_value=True)
    @patch("prow._run_gh")
    def test_simple_failure(self, mock_gh, _):
        mock_gh.side_effect = [
            self.PR_OPEN,
            [
                {"name": "ci/prow/e2e-parallel", "state": "FAILURE",
                 "link": "https://prow.ci/view/gs/.../1234567890123456789"},
                {"name": "ci/prow/lint", "state": "SUCCESS", "link": ""},
            ],
        ]
        result = prow.ProwClient().pr_checks(123)
        self.assertEqual(result["pr_state"], "OPEN")
        self.assertEqual(result["head_sha"], "abc123")
        self.assertEqual(len(result["failed"]), 1)
        self.assertEqual(result["failed"][0]["name"], "ci/prow/e2e-parallel")
        self.assertEqual(len(result["in_progress"]), 0)

    @patch("prow._gh_available", return_value=True)
    @patch("prow._run_gh")
    def test_pending_detected(self, mock_gh, _):
        mock_gh.side_effect = [
            self.PR_OPEN,
            [{"name": "ci/prow/e2e", "state": "PENDING", "link": ""}],
        ]
        result = prow.ProwClient().pr_checks(123)
        self.assertEqual(len(result["failed"]), 0)
        self.assertIn("ci/prow/e2e", result["in_progress"])

    @patch("prow._gh_available", return_value=True)
    @patch("prow._run_gh")
    def test_all_passing(self, mock_gh, _):
        mock_gh.side_effect = [
            {"state": "MERGED", "mergedAt": "2026-01-01T00:00:00Z", "headRefOid": "abc"},
            [{"name": "ci/prow/e2e", "state": "SUCCESS", "link": ""}],
        ]
        result = prow.ProwClient().pr_checks(123)
        self.assertEqual(result["pr_state"], "MERGED")
        self.assertEqual(len(result["failed"]), 0)

    @patch("prow._gh_available", return_value=True)
    @patch("prow._run_gh")
    def test_deduplicates_by_name(self, mock_gh, _):
        mock_gh.side_effect = [
            self.PR_OPEN,
            [
                {"name": "ci/prow/e2e", "state": "PENDING", "link": ""},
                {"name": "ci/prow/e2e", "state": "FAILURE", "link": "https://link"},
            ],
        ]
        result = prow.ProwClient().pr_checks(123)
        self.assertEqual(len(result["failed"]), 1)
        self.assertEqual(result["failed"][0]["link"], "https://link")

    @patch("prow._gh_available", return_value=True)
    @patch("prow._run_gh")
    def test_empty_checks(self, mock_gh, _):
        mock_gh.side_effect = [self.PR_OPEN, []]
        result = prow.ProwClient().pr_checks(123)
        self.assertEqual(result["failed"], [])
        self.assertEqual(result["in_progress"], [])

    @patch("prow._gh_available", return_value=True)
    @patch("prow._run_gh")
    def test_null_checks_response(self, mock_gh, _):
        mock_gh.side_effect = [self.PR_OPEN, None]
        result = prow.ProwClient().pr_checks(123)
        self.assertEqual(result["failed"], [])

    @patch("prow._gh_available", return_value=True)
    @patch("prow._run_gh")
    def test_null_pr_data_raises(self, mock_gh, _):
        mock_gh.return_value = None
        with self.assertRaises(RuntimeError):
            prow.ProwClient().pr_checks(123)

    @patch("prow._gh_available", return_value=True)
    @patch("prow._run_gh")
    def test_multiple_fail_states(self, mock_gh, _):
        mock_gh.side_effect = [
            self.PR_OPEN,
            [
                {"name": "check1", "state": "FAILURE", "link": ""},
                {"name": "check2", "state": "ERROR", "link": ""},
                {"name": "check3", "state": "TIMED_OUT", "link": ""},
                {"name": "check4", "state": "CANCELLED", "link": ""},
                {"name": "check5", "state": "SKIPPED", "link": ""},
            ],
        ]
        result = prow.ProwClient().pr_checks(123)
        failed_names = [f["name"] for f in result["failed"]]
        self.assertIn("check1", failed_names)
        self.assertIn("check2", failed_names)
        self.assertIn("check3", failed_names)
        self.assertNotIn("check4", failed_names)
        self.assertNotIn("check5", failed_names)

    @patch("prow._gh_available", return_value=True)
    @patch("prow._run_gh")
    def test_uses_provided_repo(self, mock_gh, _):
        mock_gh.side_effect = [self.PR_OPEN, []]
        prow.ProwClient().pr_checks(42, repo="org/other-repo")
        call_args = mock_gh.call_args_list
        self.assertIn("org/other-repo", call_args[0][0])
        self.assertIn("org/other-repo", call_args[1][0])


class TestPrChecksApi(unittest.TestCase):
    """Tests for pr_checks via GitHub REST API fallback."""

    PR_API_RESPONSE = {
        "state": "open",
        "merged_at": None,
        "head": {"sha": "def456"},
    }

    @patch("prow._gh_available", return_value=False)
    @patch("prow._github_api_get_all")
    @patch("prow._github_api_fetch")
    def test_simple_failure(self, mock_api, mock_api_all, _):
        mock_api.return_value = self.PR_API_RESPONSE
        mock_api_all.side_effect = [
            [],  # check_runs
            [  # statuses (Prow uses commit statuses)
                {"context": "ci/prow/e2e", "state": "failure",
                 "target_url": "https://prow/123", "description": ""},
                {"context": "ci/prow/lint", "state": "success",
                 "target_url": "", "description": ""},
            ],
        ]
        result = prow.ProwClient().pr_checks(123)
        self.assertEqual(result["pr_state"], "OPEN")
        self.assertEqual(result["head_sha"], "def456")
        self.assertEqual(len(result["failed"]), 1)
        f = result["failed"][0]
        self.assertEqual(f["name"], "ci/prow/e2e")
        self.assertEqual(f["link"], "https://prow/123")
        self.assertEqual(f["status"], "fail")
        self.assertEqual(f["source"], "prow")
        self.assertFalse(f["flake"])
        self.assertFalse(f["resolved"])

    @patch("prow._gh_available", return_value=False)
    @patch("prow._github_api_get_all")
    @patch("prow._github_api_fetch")
    def test_merged_pr(self, mock_api, mock_api_all, _):
        mock_api.return_value = {**self.PR_API_RESPONSE, "merged_at": "2026-01-01T00:00:00Z"}
        mock_api_all.side_effect = [[], []]
        result = prow.ProwClient().pr_checks(123)
        self.assertEqual(result["pr_state"], "MERGED")
        self.assertEqual(result["pr_merged_at"], "2026-01-01T00:00:00Z")

    # Classification logic (flake, resolved, cancelled, etc.) is tested
    # exhaustively in TestProcessChecksApi, TestClassifyProwStatuses,
    # and TestClassifyGhaCheckRuns. Only API wiring tests here.


class TestRunGh(unittest.TestCase):
    @patch("subprocess.run")
    def test_file_not_found(self, mock_run):
        mock_run.side_effect = FileNotFoundError()
        with self.assertRaises(RuntimeError) as ctx:
            prow._run_gh("pr", "view", "1")
        self.assertIn("not found", str(ctx.exception))

    @patch("subprocess.run")
    def test_auth_error(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=1, stdout="", stderr="gh auth login required"
        )
        with self.assertRaises(RuntimeError) as ctx:
            prow._run_gh("pr", "view", "1")
        self.assertIn("not authenticated", str(ctx.exception))

    @patch("subprocess.run")
    def test_generic_error(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=1, stdout="", stderr="something broke"
        )
        with self.assertRaises(RuntimeError) as ctx:
            prow._run_gh("pr", "view", "1")
        self.assertIn("something broke", str(ctx.exception))

    @patch("subprocess.run")
    def test_empty_stdout_returns_none(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0, stdout="", stderr=""
        )
        self.assertIsNone(prow._run_gh("api", "endpoint"))

    @patch("subprocess.run")
    def test_parses_json(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0, stdout='{"key": "value"}', stderr=""
        )
        result = prow._run_gh("api", "endpoint")
        self.assertEqual(result, {"key": "value"})

    @patch("subprocess.run")
    def test_timeout_raises(self, mock_run):
        mock_run.side_effect = subprocess.TimeoutExpired(cmd=[], timeout=30)
        with self.assertRaises(subprocess.TimeoutExpired):
            prow._run_gh("pr", "view", "1")


class TestAllPeriodic(unittest.TestCase):
    """Tests for all_periodic and _latest_periodic_status."""

    def test_returns_status_for_all_env_jobs_plus_global(self):
        listing_html = b'<a href="1234567890123456789/">1234567890123456789</a>'

        def fetch(url, timeout):
            if "/logs/" in url and "prowjob" not in url:
                return listing_html
            return None

        def fetch_json(url, timeout):
            if "prowjob.json" in url:
                return _make_prowjob(state="success")
            return None

        fetcher = MockFetcher(fetch_fn=fetch, fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client.all_periodic("int")

        # int has 2 env-specific + 2 global = 4 jobs
        self.assertEqual(len(result), 4)
        names = [r["job_name"] for r in result]
        self.assertTrue(any("integration-e2e-parallel" in n for n in names))
        self.assertTrue(any("delete-expired-integration" in n for n in names))
        self.assertTrue(any("kusto-role" in n for n in names))
        self.assertTrue(any("image-updater" in n for n in names))

    def test_returns_no_data_when_listing_fails(self):
        client = prow.ProwClient(MockFetcher())
        result = client.all_periodic("int")
        for entry in result:
            self.assertEqual(entry["state"], "no_data")

    def test_unknown_env_raises(self):
        client = prow.ProwClient(MockFetcher())
        with self.assertRaises(ValueError):
            client.all_periodic("xxx")

    def test_dev_includes_cleanup_and_global(self):
        listing_html = b'<a href="1234567890123456789/">1234567890123456789</a>'

        def fetch(url, timeout):
            if "/logs/" in url and "prowjob" not in url:
                return listing_html
            return None

        def fetch_json(url, timeout):
            if "prowjob.json" in url:
                return _make_prowjob(state="failure")
            return None

        fetcher = MockFetcher(fetch_fn=fetch, fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client.all_periodic("dev")

        # dev has 1 env-specific + 2 global = 3 jobs
        self.assertEqual(len(result), 3)
        self.assertTrue(all(r["state"] == "failure" for r in result))

    def test_latest_periodic_status_picks_newest_id(self):
        listing_html = b'''
        <a href="1234567890123456789/">1234567890123456789</a>
        <a href="1234567890123456790/">1234567890123456790</a>
        '''
        captured = {}

        def fetch(url, timeout):
            if "prowjob" not in url:
                return listing_html
            return None

        def fetch_json(url, timeout):
            if "prowjob.json" in url:
                captured["url"] = url
                return _make_prowjob()
            return None

        fetcher = MockFetcher(fetch_fn=fetch, fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client._latest_periodic_status("some-job")
        self.assertIn("1234567890123456790", captured["url"])
        self.assertEqual(result["job_id"], "1234567890123456790")

    def test_sorted_by_job_name(self):
        listing_html = b'<a href="1234567890123456789/">1234567890123456789</a>'

        def fetch(url, timeout):
            if "/logs/" in url and "prowjob" not in url:
                return listing_html
            return None

        def fetch_json(url, timeout):
            if "prowjob.json" in url:
                return _make_prowjob()
            return None

        fetcher = MockFetcher(fetch_fn=fetch, fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client.all_periodic("int")
        names = [r["job_name"] for r in result]
        self.assertEqual(names, sorted(names))


class TestGhAvailable(unittest.TestCase):
    """Tests for _gh_available."""

    @patch("subprocess.run")
    def test_returns_true_on_success(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0, stdout="", stderr="")
        self.assertTrue(prow._gh_available())

    @patch("subprocess.run")
    def test_returns_false_on_nonzero(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=1, stdout="", stderr="not logged in")
        self.assertFalse(prow._gh_available())

    @patch("subprocess.run")
    def test_returns_false_on_file_not_found(self, mock_run):
        mock_run.side_effect = FileNotFoundError()
        self.assertFalse(prow._gh_available())

    @patch("subprocess.run")
    def test_returns_false_on_timeout(self, mock_run):
        mock_run.side_effect = subprocess.TimeoutExpired(cmd=[], timeout=10)
        self.assertFalse(prow._gh_available())

    @patch("subprocess.run")
    def test_returns_false_on_os_error(self, mock_run):
        mock_run.side_effect = OSError("broken")
        self.assertFalse(prow._gh_available())


class TestGithubApiRequest(unittest.TestCase):
    """Tests for _github_api_request and _github_api_fetch."""

    @patch("urllib.request.urlopen")
    def test_success(self, mock_urlopen):
        from unittest.mock import MagicMock
        mock_resp = MagicMock()
        mock_resp.read.return_value = b'{"key": "value"}'
        mock_resp.headers = MagicMock()
        mock_resp.headers.get.return_value = ""
        mock_resp.__enter__ = lambda s: s
        mock_resp.__exit__ = MagicMock(return_value=False)
        mock_urlopen.return_value = mock_resp
        result = prow._github_api_fetch("https://api.github.com/repos/Azure/ARO-HCP/pulls/1")
        self.assertEqual(result, {"key": "value"})

    @patch("urllib.request.urlopen")
    def test_403_rate_limit(self, mock_urlopen):
        mock_urlopen.side_effect = urllib.error.HTTPError(
            url="", code=403, msg="Forbidden", hdrs={}, fp=None)
        with self.assertRaises(RuntimeError) as ctx:
            prow._github_api_fetch("https://api.github.com/repos/Azure/ARO-HCP/pulls/1")
        self.assertIn("rate limit", str(ctx.exception))

    @patch("urllib.request.urlopen")
    def test_404_error(self, mock_urlopen):
        mock_urlopen.side_effect = urllib.error.HTTPError(
            url="", code=404, msg="Not Found", hdrs={}, fp=None)
        with self.assertRaises(RuntimeError) as ctx:
            prow._github_api_fetch("https://api.github.com/repos/Azure/ARO-HCP/pulls/1")
        self.assertIn("404", str(ctx.exception))

    @patch("urllib.request.urlopen")
    def test_url_error(self, mock_urlopen):
        mock_urlopen.side_effect = urllib.error.URLError("DNS failed")
        with self.assertRaises(RuntimeError) as ctx:
            prow._github_api_fetch("https://api.github.com/repos/Azure/ARO-HCP/pulls/1")
        self.assertIn("request failed", str(ctx.exception))

    @patch("urllib.request.urlopen")
    def test_timeout_error(self, mock_urlopen):
        mock_urlopen.side_effect = TimeoutError()
        with self.assertRaises(RuntimeError):
            prow._github_api_fetch("https://api.github.com/repos/Azure/ARO-HCP/pulls/1")


class TestGithubApiGetAll(unittest.TestCase):
    """Tests for _github_api_get_all pagination."""

    @patch("urllib.request.urlopen")
    def test_single_page(self, mock_urlopen):
        from unittest.mock import MagicMock
        mock_resp = MagicMock()
        mock_resp.read.return_value = b'[{"id": 1}]'
        mock_resp.headers = MagicMock()
        mock_resp.headers.get.return_value = ""
        mock_resp.__enter__ = lambda s: s
        mock_resp.__exit__ = MagicMock(return_value=False)
        mock_urlopen.return_value = mock_resp
        result = prow._github_api_get_all("/repos/Azure/ARO-HCP/commits/abc/statuses")
        self.assertEqual(result, [{"id": 1}])

    @patch("urllib.request.urlopen")
    def test_with_array_key(self, mock_urlopen):
        from unittest.mock import MagicMock
        mock_resp = MagicMock()
        mock_resp.read.return_value = b'{"check_runs": [{"id": 1}], "total_count": 1}'
        mock_resp.headers = MagicMock()
        mock_resp.headers.get.return_value = ""
        mock_resp.__enter__ = lambda s: s
        mock_resp.__exit__ = MagicMock(return_value=False)
        mock_urlopen.return_value = mock_resp
        result = prow._github_api_get_all(
            "/repos/Azure/ARO-HCP/commits/abc/check-runs",
            array_key="check_runs")
        self.assertEqual(result, [{"id": 1}])

    @patch("urllib.request.urlopen")
    def test_multi_page(self, mock_urlopen):
        """Follows Link rel=next headers to fetch all pages."""
        from unittest.mock import MagicMock

        page1 = MagicMock()
        page1.read.return_value = b'[{"id": 1}, {"id": 2}]'
        page1.headers = MagicMock()
        page1.headers.get.return_value = (
            '<https://api.github.com/repos/Azure/ARO-HCP/statuses?page=2>; rel="next"'
        )
        page1.__enter__ = lambda s: s
        page1.__exit__ = MagicMock(return_value=False)

        page2 = MagicMock()
        page2.read.return_value = b'[{"id": 3}]'
        page2.headers = MagicMock()
        page2.headers.get.return_value = ""
        page2.__enter__ = lambda s: s
        page2.__exit__ = MagicMock(return_value=False)

        mock_urlopen.side_effect = [page1, page2]
        result = prow._github_api_get_all("/repos/Azure/ARO-HCP/statuses")
        self.assertEqual(result, [{"id": 1}, {"id": 2}, {"id": 3}])
        self.assertEqual(mock_urlopen.call_count, 2)

    @patch("urllib.request.urlopen")
    def test_multi_page_with_array_key(self, mock_urlopen):
        """Multi-page pagination with array_key extraction."""
        from unittest.mock import MagicMock

        page1 = MagicMock()
        page1.read.return_value = b'{"check_runs": [{"id": 1}], "total_count": 2}'
        page1.headers = MagicMock()
        page1.headers.get.return_value = (
            '<https://api.github.com/repos/Azure/ARO-HCP/check-runs?page=2>; rel="next"'
        )
        page1.__enter__ = lambda s: s
        page1.__exit__ = MagicMock(return_value=False)

        page2 = MagicMock()
        page2.read.return_value = b'{"check_runs": [{"id": 2}], "total_count": 2}'
        page2.headers = MagicMock()
        page2.headers.get.return_value = ""
        page2.__enter__ = lambda s: s
        page2.__exit__ = MagicMock(return_value=False)

        mock_urlopen.side_effect = [page1, page2]
        result = prow._github_api_get_all(
            "/repos/Azure/ARO-HCP/check-runs",
            array_key="check_runs")
        self.assertEqual(result, [{"id": 1}, {"id": 2}])
        self.assertEqual(mock_urlopen.call_count, 2)


class TestParseNextLink(unittest.TestCase):
    """Tests for _parse_next_link."""

    def test_extracts_next_url(self):
        header = '<https://api.github.com/repos?page=2>; rel="next", <https://api.github.com/repos?page=5>; rel="last"'
        self.assertEqual(prow._parse_next_link(header),
                         "https://api.github.com/repos?page=2")

    def test_returns_none_when_no_next(self):
        header = '<https://api.github.com/repos?page=1>; rel="last"'
        self.assertIsNone(prow._parse_next_link(header))

    def test_returns_none_for_empty(self):
        self.assertIsNone(prow._parse_next_link(""))
        self.assertIsNone(prow._parse_next_link(None))


class TestProcessChecksApi(unittest.TestCase):
    """Standalone tests for _process_checks_api."""

    def test_empty_inputs(self):
        result = prow._process_checks_api("abc", [], [])
        self.assertEqual(result["failed"], [])
        self.assertEqual(result["in_progress"], [])
        self.assertEqual(result["head_sha"], "abc")

    def test_prow_failure_only(self):
        statuses = [
            {"context": "ci/prow/e2e", "state": "failure",
             "target_url": "https://prow/1", "description": ""},
        ]
        result = prow._process_checks_api("abc", [], statuses)
        self.assertEqual(len(result["failed"]), 1)
        self.assertEqual(result["failed"][0]["source"], "prow")
        self.assertFalse(result["failed"][0]["flake"])

    def test_prow_flake(self):
        statuses = [
            {"context": "ci/prow/e2e", "state": "failure",
             "target_url": "https://prow/fail", "description": ""},
            {"context": "ci/prow/e2e", "state": "success",
             "target_url": "", "description": ""},
        ]
        result = prow._process_checks_api("abc", [], statuses)
        self.assertEqual(len(result["failed"]), 1)
        self.assertTrue(result["failed"][0]["flake"])

    def test_prow_resolved(self):
        statuses = [
            {"context": "ci/prow/e2e", "state": "success",
             "target_url": "", "description": ""},
            {"context": "ci/prow/e2e", "state": "failure",
             "target_url": "", "description": ""},
        ]
        result = prow._process_checks_api("abc", [], statuses)
        self.assertEqual(len(result["failed"]), 0)

    def test_prow_pending_with_old_failure(self):
        """Pending re-trigger after failure → still reported as failure with in_progress."""
        statuses = [
            {"context": "ci/prow/e2e", "state": "pending",
             "target_url": "", "description": ""},
            {"context": "ci/prow/e2e", "state": "failure",
             "target_url": "https://prow/fail", "description": ""},
        ]
        result = prow._process_checks_api("abc", [], statuses)
        self.assertEqual(len(result["failed"]), 1)
        self.assertTrue(result["failed"][0]["in_progress"])

    def test_base_sha_extraction(self):
        statuses = [
            {"context": "ci/prow/e2e", "state": "failure",
             "target_url": "https://prow/1",
             "description": "Job failed. BaseSHA:deadbeef123"},
        ]
        result = prow._process_checks_api("abc", [], statuses)
        self.assertEqual(result["failed"][0]["base_sha"], "deadbeef123")

    def test_gha_failure(self):
        check_runs = [
            {"name": "build", "status": "completed", "conclusion": "failure",
             "completed_at": "2026-01-01T00:00:00Z",
             "details_url": "https://gha/1", "html_url": ""},
        ]
        result = prow._process_checks_api("abc", check_runs, [])
        self.assertEqual(len(result["failed"]), 1)
        self.assertEqual(result["failed"][0]["source"], "github-actions")

    def test_gha_resolved(self):
        check_runs = [
            {"name": "build", "status": "completed", "conclusion": "success",
             "completed_at": "2026-01-02T00:00:00Z",
             "details_url": "", "html_url": ""},
            {"name": "build", "status": "completed", "conclusion": "failure",
             "completed_at": "2026-01-01T00:00:00Z",
             "details_url": "", "html_url": ""},
        ]
        result = prow._process_checks_api("abc", check_runs, [])
        self.assertEqual(len(result["failed"]), 0)

    def test_gha_cancelled_no_success(self):
        check_runs = [
            {"name": "build", "status": "completed", "conclusion": "cancelled",
             "completed_at": "2026-01-01T00:00:00Z",
             "details_url": "https://gha/1", "html_url": ""},
        ]
        result = prow._process_checks_api("abc", check_runs, [])
        self.assertEqual(len(result["failed"]), 1)
        self.assertEqual(result["failed"][0]["status"], "cancelled")

    def test_gha_cancelled_with_success(self):
        check_runs = [
            {"name": "build", "status": "completed", "conclusion": "success",
             "completed_at": "2026-01-02T00:00:00Z",
             "details_url": "", "html_url": ""},
            {"name": "build", "status": "completed", "conclusion": "cancelled",
             "completed_at": "2026-01-01T00:00:00Z",
             "details_url": "", "html_url": ""},
        ]
        result = prow._process_checks_api("abc", check_runs, [])
        self.assertEqual(len(result["failed"]), 0)

    def test_prow_status_excludes_duplicate_check_run(self):
        """When same name appears in both statuses and check-runs, statuses win."""
        statuses = [
            {"context": "ci/prow/e2e", "state": "failure",
             "target_url": "https://prow/1", "description": ""},
        ]
        check_runs = [
            {"name": "ci/prow/e2e", "status": "completed", "conclusion": "failure",
             "completed_at": "2026-01-01T00:00:00Z",
             "details_url": "https://gha/1", "html_url": ""},
        ]
        result = prow._process_checks_api("abc", check_runs, statuses)
        # Should only appear once, from statuses
        self.assertEqual(len(result["failed"]), 1)
        self.assertEqual(result["failed"][0]["source"], "prow")

    def test_mixed_prow_and_gha(self):
        statuses = [
            {"context": "ci/prow/e2e", "state": "failure",
             "target_url": "https://prow/1", "description": ""},
        ]
        check_runs = [
            {"name": "build", "status": "completed", "conclusion": "failure",
             "completed_at": "2026-01-01T00:00:00Z",
             "details_url": "https://gha/1", "html_url": ""},
        ]
        result = prow._process_checks_api("abc", check_runs, statuses)
        self.assertEqual(len(result["failed"]), 2)
        sources = {f["source"] for f in result["failed"]}
        self.assertEqual(sources, {"prow", "github-actions"})

    def test_results_sorted_by_name(self):
        statuses = [
            {"context": "ci/prow/zzz", "state": "failure",
             "target_url": "", "description": ""},
            {"context": "ci/prow/aaa", "state": "failure",
             "target_url": "", "description": ""},
        ]
        result = prow._process_checks_api("abc", [], statuses)
        names = [f["name"] for f in result["failed"]]
        self.assertEqual(names, sorted(names))


class TestFetchStatus(unittest.TestCase):
    """Tests for _fetch_status."""

    def test_periodic_path(self):
        def fetch_json(url, timeout):
            if "prowjob.json" in url:
                return _make_prowjob(state="failure")
            return None
        fetcher = MockFetcher(fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client._fetch_status("1234567890123456789", "int", "periodic")
        self.assertEqual(result["state"], "failure")
        self.assertEqual(result["env"], "int")
        self.assertEqual(result["job_id"], "1234567890123456789")
        self.assertIn("integration-e2e-parallel", result["base_url"])

    def test_presubmit_with_pr_fields(self):

        def fetch(url, timeout):
            if ".txt" in url:
                return b"gs://test-platform-results/pr-logs/pull/path/123"
            return None

        def fetch_json(url, timeout):
            if "prowjob.json" in url:
                return _make_prowjob(
                    pulls=[{"number": 42, "title": "Fix", "author": "alice"}])
            return None
        fetcher = MockFetcher(fetch_fn=fetch, fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client._fetch_status("1234567890123456789", "int", "presubmit")
        self.assertEqual(result["pr"], 42)
        self.assertEqual(result["pr_title"], "Fix")
        self.assertEqual(result["pr_author"], "alice")

    def test_missing_prowjob_returns_none(self):
        fetcher = MockFetcher(fetch_json_fn=lambda url, timeout: None)
        client = prow.ProwClient(fetcher)
        result = client._fetch_status("1234567890123456789", "int", "periodic")
        self.assertIsNone(result)

    def test_timestamp_truncation(self):
        def fetch_json(url, timeout):
            if "prowjob.json" in url:
                return _make_prowjob(
                    started="2026-03-31T10:00:00.123456789Z",
                    completed="2026-03-31T11:00:00.987654321Z")
            return None
        fetcher = MockFetcher(fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client._fetch_status("1234567890123456789", "int", "periodic")
        self.assertEqual(result["started"], "2026-03-31T10:00:00")
        self.assertEqual(result["completed"], "2026-03-31T11:00:00")


class TestNormalizeBaseUrl(unittest.TestCase):
    """Tests for _normalize_base_url."""

    def test_prow_dashboard_url(self):
        url = (
            "https://prow.ci.openshift.org/view/gs/"
            "test-platform-results/logs/some-job/1234567890123456789"
        )
        result = prow._normalize_base_url(url)
        self.assertEqual(
            result,
            "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/"
            "test-platform-results/logs/some-job/1234567890123456789")

    def test_prow_dashboard_with_query_params(self):
        url = (
            "https://prow.ci.openshift.org/view/gs/"
            "test-platform-results/logs/job/123?tab=artifacts"
        )
        result = prow._normalize_base_url(url)
        self.assertNotIn("?", result)
        self.assertTrue(result.endswith("/123"))

    def test_prow_dashboard_with_fragment(self):
        url = (
            "https://prow.ci.openshift.org/view/gs/"
            "test-platform-results/logs/job/123#summary"
        )
        result = prow._normalize_base_url(url)
        self.assertNotIn("#", result)

    def test_prow_dashboard_with_trailing_slash(self):
        url = (
            "https://prow.ci.openshift.org/view/gs/"
            "test-platform-results/logs/job/123/"
        )
        result = prow._normalize_base_url(url)
        self.assertFalse(result.endswith("/"))

    def test_gcsweb_url_passes_through(self):
        url = (
            "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/"
            "test-platform-results/logs/job/123"
        )
        self.assertEqual(prow._normalize_base_url(url), url)

    def test_gcsweb_url_strips_query_params(self):
        url = (
            "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/"
            "test-platform-results/logs/job/123?foo=bar"
        )
        result = prow._normalize_base_url(url)
        self.assertNotIn("?", result)

    def test_gcsweb_url_strips_trailing_slash(self):
        url = (
            "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/"
            "test-platform-results/logs/job/123/"
        )
        result = prow._normalize_base_url(url)
        self.assertFalse(result.endswith("/"))

    def test_other_prow_instance_with_view_gs(self):
        url = "https://other-prow.example.com/view/gs/bucket/path/123"
        result = prow._normalize_base_url(url)
        self.assertEqual(
            result,
            "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/"
            "bucket/path/123")


class TestWarnEnvMismatch(unittest.TestCase):
    """Tests for _warn_env_mismatch."""

    def test_warns_on_mismatch(self):
        import io
        with patch("sys.stderr", new_callable=io.StringIO) as mock_err:
            prow._warn_env_mismatch(
                "https://example.com/integration-e2e-parallel/123", "stg")
            output = mock_err.getvalue()
        self.assertIn("warning", output)
        self.assertIn("int", output)

    def test_no_warning_on_match(self):
        import io
        with patch("sys.stderr", new_callable=io.StringIO) as mock_err:
            prow._warn_env_mismatch(
                "https://example.com/integration-e2e-parallel/123", "int")
            output = mock_err.getvalue()
        self.assertEqual(output, "")

    def test_no_warning_on_unknown_url(self):
        import io
        with patch("sys.stderr", new_callable=io.StringIO) as mock_err:
            prow._warn_env_mismatch(
                "https://example.com/some/unknown/path", "int")
            output = mock_err.getvalue()
        self.assertEqual(output, "")


class TestLookupJob(unittest.TestCase):
    """Tests for lookup_job."""

    def test_finds_periodic_job(self):
        def fetch_json(url, timeout):
            if "integration-e2e-parallel/1234567890123456789/prowjob" in url:
                return _make_prowjob(state="failure")
            return None
        fetcher = MockFetcher(fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client.lookup_job("1234567890123456789")
        self.assertIsNotNone(result)
        self.assertEqual(result["job_id"], "1234567890123456789")
        self.assertEqual(result["env"], "int")
        self.assertEqual(result["type"], "periodic")
        self.assertEqual(result["state"], "failure")
        self.assertIn("integration-e2e-parallel", result["base_url"])

    def test_finds_presubmit_job(self):
        def fetch(url, timeout):
            if ".txt" in url and "int" in url:
                return b"gs://test-platform-results/pr-logs/pull/path/123"
            return None

        def fetch_json(url, timeout):
            if "prowjob.json" in url and "pr-logs" in url:
                return _make_prowjob(
                    state="success",
                    pulls=[{"number": 42, "title": "Fix", "author": "bob"}])
            return None
        fetcher = MockFetcher(fetch_fn=fetch, fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client.lookup_job("1234567890123456789")
        self.assertIsNotNone(result)
        self.assertEqual(result["type"], "presubmit")
        self.assertEqual(result["pr"], 42)

    def test_returns_none_when_not_found(self):
        fetcher = MockFetcher()
        client = prow.ProwClient(fetcher)
        result = client.lookup_job("9999999999999999999")
        self.assertIsNone(result)

    def test_finds_global_periodic_job(self):
        def fetch_json(url, timeout):
            if "kusto-role-assignments/1234567890123456789/prowjob" in url:
                return _make_prowjob(state="success")
            return None
        fetcher = MockFetcher(fetch_json_fn=fetch_json)
        client = prow.ProwClient(fetcher)
        result = client.lookup_job("1234567890123456789")
        self.assertIsNotNone(result)
        self.assertEqual(result["env"], "global")
        self.assertEqual(result["type"], "periodic")


class TestCli(unittest.TestCase):
    """CLI integration tests — verify the script is executable and errors are JSON."""

    def _run(self, *args):
        script = os.path.join(os.path.dirname(__file__), "..", "prow.py")
        return subprocess.run(
            [sys.executable, script, *args],
            capture_output=True, text=True, timeout=10,
        )

    def test_no_args_exits_nonzero(self):
        r = self._run()
        self.assertNotEqual(r.returncode, 0)

    def test_unknown_command_exits_nonzero(self):
        r = self._run("bogus")
        self.assertNotEqual(r.returncode, 0)

    def test_error_output_is_json(self):
        r = self._run("resolve-url", "1234567890123456789", "xxx")
        self.assertNotEqual(r.returncode, 0)
        err = json.loads(r.stderr)
        self.assertIn("error", err)

    def test_list_jobs_invalid_env_exits_nonzero(self):
        r = self._run("list-jobs", "dev", "periodic")
        self.assertNotEqual(r.returncode, 0)

    def test_fetch_build_log_unknown_env_exits_nonzero(self):
        r = self._run("fetch-build-log", "https://example.com", "xxx")
        self.assertNotEqual(r.returncode, 0)

    def test_fetch_build_log_missing_args_exits_nonzero(self):
        r = self._run("fetch-build-log")
        self.assertNotEqual(r.returncode, 0)


if __name__ == "__main__":
    unittest.main()
