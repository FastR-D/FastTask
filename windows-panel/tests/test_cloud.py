"""Cloud/source-isolation tests. No production state, Bluetooth, or public hosts."""
import asyncio
import contextlib
import io
import json
import ssl
import sys
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import panel_helper as ph


TOKEN = "ftp_" + "T" * 32
LOCAL = "http://127.0.0.1:10000"
CLOUD = "https://tasks.example.edu"


@contextlib.contextmanager
def temporary_engine(origin):
    with tempfile.TemporaryDirectory() as directory:
        # Unit tests use isolated storage. Production enforces the Windows ACL.
        with patch.object(ph, "private_directory", lambda path: path.mkdir(exist_ok=True)):
            yield ph.Engine(directory, origin)


class OriginTests(unittest.TestCase):
    def test_cloud_local_and_browser_canonical_origins(self):
        for supplied, expected in (
            ("https://tasks.example.edu", CLOUD),
            ("HTTPS://TASKS.example.edu:443/", CLOUD),
            ("https://tasks.example.edu:8443/", CLOUD + ":8443"),
            ("https://任务.example/", "https://xn--boqu2g.example"),
            ("http://localhost:10000/", "http://localhost:10000"),
            ("http://127.0.0.1:10000", LOCAL),
            ("http://[::1]:10000/", "http://[::1]:10000"),
            ("http://localhost:80", "http://localhost"),
        ):
            with self.subTest(supplied=supplied):
                self.assertEqual(ph.normalize_origin(supplied), expected)
                self.assertEqual(ph.parse_options(["--origin", supplied]).origin, expected)
                with temporary_engine(supplied) as engine:
                    self.assertEqual(engine.origin, expected)

    def test_invalid_origin_rejected_by_engine_and_cli_before_storage(self):
        invalid = (
            None, "", "http://tasks.example.edu", "http://127.0.0.2", "http://localhost.evil.example",
            "http://127.1", "http://2130706433", "http://[::ffff:127.0.0.1]",
            "file:///tmp/config", "wss://tasks.example.edu", "//tasks.example.edu",
            "https://name:password@tasks.example.edu", "https://@tasks.example.edu",
            CLOUD + "/fasttask", CLOUD + "?", CLOUD + "#", CLOUD + "?next=x", CLOUD + "#x",
            CLOUD + ":", CLOUD + ":bad", CLOUD + ":0", CLOUD + ":65536", CLOUD + ":-1",
            "https://[::1", "https://[::1%25eth0]", "https://bad_host.example", "https://-bad.example",
            "https://host..example", " https://tasks.example.edu", CLOUD + "\n",
            "https://tasks.example.edu\\@evil.example", "https://tasks%2eexample.edu",
        )
        for supplied in invalid:
            with self.subTest(supplied=supplied):
                with patch.object(ph, "private_directory") as storage:
                    with self.assertRaises(ph.PanelError) as error:
                        ph.Engine("unused-unit-test-directory", supplied)
                    self.assertEqual(error.exception.code, "invalid_origin")
                    storage.assert_not_called()
                if isinstance(supplied, str):
                    with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
                        ph.parse_options(["--origin", supplied])
                    self.assertEqual(error.exception.code, 2)

    def test_switching_origin_clears_token_target_schedule_and_old_attempt(self):
        with temporary_engine(LOCAL) as original:
            original.config.update(enabled=True, device_name="WL14B00B1F", address="66:66:14:B0:0B:1F", panel_token=TOKEN, pairing_id="old-account")
            ph.write_private(original.config_path, original.config)
            ph.write_private(original.state_path, {"last_attempted_slot": "2026-10-25T21:00", "last_status": {"status": "success"}})
            switched = ph.Engine(original.directory, CLOUD)
            self.assertFalse(switched.config["enabled"])
            self.assertEqual(switched.config["fasttask_url"], CLOUD)
            for key in ("panel_token", "pairing_id", "device_name", "address"):
                self.assertEqual(switched.config[key], "")
            self.assertNotIn(TOKEN, switched.config_path.read_text(encoding="utf-8"))
            self.assertEqual(switched.state["last_attempted_slot"], "")
            self.assertEqual(switched.state["last_status"]["operation"], "source_changed")
            with patch.object(ph.urllib.request, "build_opener") as opener:
                with self.assertRaises(ph.PanelError) as error:
                    switched.fetch_snapshot()
                self.assertEqual(error.exception.code, "not_paired")
                opener.assert_not_called()
            restarted = ph.Engine(original.directory, CLOUD)
            self.assertFalse(restarted.config["enabled"])
            self.assertEqual(restarted.config["panel_token"], "")

    def test_equivalent_origin_preserves_pairing_and_canonicalizes_storage(self):
        with temporary_engine(CLOUD) as original:
            original.config.update(enabled=True, panel_token=TOKEN, pairing_id="existing-pairing", device_name="WL14B00B1F", address="66:66:14:B0:0B:1F", fasttask_url="HTTPS://TASKS.example.edu:443/")
            ph.write_private(original.config_path, original.config)
            restarted = ph.Engine(original.directory, CLOUD)
            self.assertEqual(restarted.config["panel_token"], TOKEN)
            self.assertTrue(restarted.config["enabled"])
            self.assertEqual(json.loads(restarted.config_path.read_text())["fasttask_url"], CLOUD)

    def test_invalid_saved_origin_cannot_reuse_token(self):
        with temporary_engine(CLOUD) as original:
            original.config.update(enabled=True, panel_token=TOKEN, fasttask_url="file:///untrusted")
            ph.write_private(original.config_path, original.config)
            restarted = ph.Engine(original.directory, CLOUD)
            self.assertFalse(restarted.config["enabled"])
            self.assertEqual(restarted.config["panel_token"], "")

    def test_configuration_cannot_change_upstream(self):
        with temporary_engine(CLOUD) as engine:
            engine.config["panel_token"] = TOKEN
            for origin in ("https://other.example.edu", "http://tasks.example.edu", None):
                with self.subTest(origin=origin), self.assertRaises(ph.PanelError):
                    asyncio.run(engine.configure({"fasttask_url": origin}))
            self.assertEqual(engine.config["panel_token"], TOKEN)
            self.assertEqual(engine.config["fasttask_url"], CLOUD)
            self.assertFalse(engine.config_path.exists())


class CloudApiTests(unittest.TestCase):
    def test_https_page_has_exact_cors_and_private_network_preflight(self):
        with temporary_engine(CLOUD) as engine:
            # OPTIONS and GET never enter the asyncio operation loop or Bluetooth.
            server = ThreadingHTTPServer(("127.0.0.1", 0), ph.handler_factory(engine, None, 0))
            server.RequestHandlerClass = ph.handler_factory(engine, None, server.server_port)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                for origin, status in ((CLOUD, 200), (LOCAL, 403), (CLOUD + ".evil.example", 403), (CLOUD + ":8443", 403), ("null", 403)):
                    request = urllib.request.Request(f"http://127.0.0.1:{server.server_port}/send", method="OPTIONS", headers={"Origin": origin, "Access-Control-Request-Private-Network": "true", "Access-Control-Request-Method": "POST"})
                    try:
                        response = urllib.request.urlopen(request, timeout=5)
                    except urllib.error.HTTPError as error:
                        response = error
                    with response:
                        self.assertEqual(response.status, status)
                        self.assertEqual(response.headers.get("Access-Control-Allow-Origin"), CLOUD if status == 200 else None)
                        self.assertEqual(response.headers.get("Access-Control-Allow-Private-Network"), "true" if status == 200 else None)
                request = urllib.request.Request(f"http://127.0.0.1:{server.server_port}/health", headers={"Origin": CLOUD, "Host": "rebound.example"})
                with self.assertRaises(urllib.error.HTTPError) as error:
                    urllib.request.urlopen(request, timeout=5)
                self.assertEqual(error.exception.code, 403)
                error.exception.close()
            finally:
                server.shutdown()
                server.server_close()
                thread.join()


class UpstreamTests(unittest.TestCase):
    def test_redirect_is_not_followed_or_given_pairing_token(self):
        seen = []

        class Sink(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                seen.append(("sink", self.headers.get("X-Panel-Token")))
                self.send_response(200)
                self.end_headers()

        sink = ThreadingHTTPServer(("127.0.0.1", 0), Sink)

        class Redirect(Sink):
            def do_GET(self):
                seen.append(("source", self.headers.get("X-Panel-Token")))
                self.send_response(302)
                self.send_header("Location", f"http://127.0.0.1:{sink.server_port}/capture")
                self.end_headers()

        source = ThreadingHTTPServer(("127.0.0.1", 0), Redirect)
        threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in (source, sink)]
        for thread in threads:
            thread.start()
        try:
            with temporary_engine(f"http://127.0.0.1:{source.server_port}") as engine:
                engine.config["panel_token"] = TOKEN
                with self.assertRaises(ph.PanelError) as error:
                    engine.fetch_snapshot()
                self.assertEqual(error.exception.code, "snapshot_unavailable")
                self.assertEqual(seen, [("source", TOKEN)])
        finally:
            for server, thread in zip((source, sink), threads):
                server.shutdown()
                server.server_close()
                thread.join()

    def test_cloud_snapshot_uses_fixed_https_origin_and_verified_tls(self):
        with temporary_engine(CLOUD) as engine:
            engine.config["panel_token"] = TOKEN
            snapshot = {"tasks": {"local_date": datetime.now(ph.SHANGHAI).date().isoformat()}}
            response = io.BytesIO(json.dumps(snapshot).encode())
            with patch.object(ph.urllib.request, "build_opener") as factory:
                factory.return_value.open.return_value = response
                self.assertEqual(engine.fetch_snapshot(), snapshot)
                request = factory.return_value.open.call_args.args[0]
                self.assertEqual(request.full_url, CLOUD + "/api/v1/panel/snapshot")
                self.assertEqual(request.get_method(), "GET")
                self.assertIsNone(request.data)
                self.assertEqual(request.get_header("X-panel-token"), TOKEN)
                tls = next(handler for handler in factory.call_args.args if isinstance(handler, urllib.request.HTTPSHandler))
                self.assertTrue(tls._context.check_hostname)
                self.assertEqual(tls._context.verify_mode, ssl.CERT_REQUIRED)
                proxy = next(handler for handler in factory.call_args.args if isinstance(handler, urllib.request.ProxyHandler))
                self.assertEqual(proxy.proxies, {})
            # Defense in depth: even in-memory configuration tampering must fail
            # before sending its token to any upstream.
            engine.config["fasttask_url"] = "https://other.example.edu"
            with patch.object(ph.urllib.request, "build_opener") as factory:
                with self.assertRaises(ph.PanelError):
                    engine.fetch_snapshot()
                factory.assert_not_called()

    def test_untrusted_cloud_certificate_fails_before_http_token(self):
        requests = []

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                requests.append(self.headers.get("X-Panel-Token"))
                self.send_response(200)
                self.end_headers()

        with tempfile.TemporaryDirectory() as directory:
            key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
            subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "localhost")])
            now = datetime.now(timezone.utc)
            certificate = (x509.CertificateBuilder().subject_name(subject).issuer_name(subject).public_key(key.public_key()).serial_number(x509.random_serial_number()).not_valid_before(now - timedelta(days=1)).not_valid_after(now + timedelta(days=1)).sign(key, hashes.SHA256()))
            key_path, cert_path = Path(directory) / "key.pem", Path(directory) / "cert.pem"
            key_path.write_bytes(key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
            cert_path.write_bytes(certificate.public_bytes(serialization.Encoding.PEM))
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.load_cert_chain(cert_path, key_path)
            server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
            server.socket = context.wrap_socket(server.socket, server_side=True)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with temporary_engine(f"https://127.0.0.1:{server.server_port}") as engine:
                    engine.config["panel_token"] = TOKEN
                    with self.assertRaises(ph.PanelError) as error:
                        engine.fetch_snapshot()
                    self.assertEqual(error.exception.code, "snapshot_unavailable")
                    self.assertEqual(requests, [])
            finally:
                server.shutdown()
                server.server_close()
                thread.join()


if __name__ == "__main__":
    unittest.main()
