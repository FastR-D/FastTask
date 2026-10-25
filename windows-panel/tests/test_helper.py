import asyncio
import base64
import io
import json
import sys
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from datetime import datetime
from http.server import ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import panel_helper as ph
from badge import render_snapshot, task_rows
from PIL import Image, ImageDraw
from wolink.models import IMAGE_WRITE_UUID
from wolink.protocol import build_image_stream, build_write_packets

NAME, MAC = "WL14B00B1F", "66:66:14:B0:0B:1F"


class FakeScanner:
    @staticmethod
    async def find_device_by_filter(predicate, timeout):
        device = SimpleNamespace(address=MAC, name=NAME)
        return device if predicate(device, SimpleNamespace(local_name=NAME)) else None


class FakeClient:
    instances = []
    notify = ph.SUCCESS_NOTIFICATION
    fail_packet = False

    def __init__(self, *args, **kwargs):
        self.is_connected = False
        self.services = SimpleNamespace(get_service=lambda uuid: object())
        self.callback = None
        self.writes = []
        self.__class__.instances.append(self)

    async def connect(self):
        self.is_connected = True

    async def disconnect(self):
        self.is_connected = False

    async def read_gatt_char(self, uuid):
        return b"a" * 16

    async def write_gatt_char(self, uuid, data, response):
        assert response is True
        self.writes.append((uuid, data))
        if uuid == IMAGE_WRITE_UUID:
            if self.fail_packet:
                raise OSError("mock write failed")
            if data.startswith(b"\x02\xa5") and self.notify is not None:
                self.callback(None, bytearray(self.notify))

    async def start_notify(self, uuid, callback):
        self.callback = callback

    async def stop_notify(self, uuid):
        pass


class CoreTests(unittest.TestCase):
    def test_busy_operation_is_rejected_instead_of_queued(self):
        async def check():
            with tempfile.TemporaryDirectory() as directory, patch.object(ph, "private_directory", lambda p: p.mkdir(parents=True, exist_ok=True)):
                engine = ph.Engine(directory, "http://127.0.0.1:10000")
                await engine.lock.acquire()
                try:
                    with self.assertRaises(ph.PanelError) as raised:
                        await engine.operation("send", {})
                    self.assertEqual(raised.exception.code, "operation_busy")
                    self.assertEqual(raised.exception.http_status, 409)
                finally:
                    engine.lock.release()
        asyncio.run(check())

    def test_name_and_mac_must_both_match(self):
        self.assertEqual(ph.target(NAME.lower(), MAC.lower()), (NAME, MAC))
        with self.assertRaises(ph.PanelError):
            ph.target(NAME, "66:66:14:B0:0B:20")
        with self.assertRaises(ph.PanelError):
            ph.target("OTHER", MAC)

    def test_image_dimensions_and_decode_validation(self):
        buffer = io.BytesIO()
        Image.new("RGB", (800, 480), "white").save(buffer, format="PNG")
        self.assertEqual(len(ph.decode_image(base64.b64encode(buffer.getvalue()).decode())), 96000)
        buffer = io.BytesIO()
        Image.new("RGB", (8, 8), "white").save(buffer, format="PNG")
        for encoded in ["invalid!", base64.b64encode(buffer.getvalue()).decode(), "data:image/svg+xml;base64,eA=="]:
            with self.assertRaises(ph.PanelError):
                ph.decode_image(encoded)

    def test_long_task_complete_continuation(self):
        image = Image.new("RGB", (800, 480), "white")
        text = "长内容和任务说明" * 160
        rows = task_rows([{"title": text, "status": "satisfied"}], ImageDraw.Draw(image))
        self.assertGreater(len(rows), 3)
        self.assertEqual("".join(line for row in rows for line in row["lines"]), text)
        self.assertTrue(all(row["status"] == "已完成" for row in rows))

    def test_snapshot_includes_all_tasks_despite_legacy_selection(self):
        snapshot = {"profile": {"display_name": "测试", "task_overrides": {"id1": {"selected": False}, "id2": {"title": "修正", "position": 0}, "id3": {"position": 10}}}, "tasks": {"local_date": "2026-10-25", "today_items": [{"id": "id" + str(i), "title": "任务" + str(i), "status": "pending", "position": i} for i in range(1, 9)]}}
        image, metadata = render_snapshot(snapshot)
        self.assertEqual(image.size, (800, 480))
        self.assertEqual(metadata["task_count"], 8)
        self.assertEqual(metadata["pages"], 3)
        self.assertEqual(render_snapshot(snapshot, 999)[1]["page"], 0)
        colors = {color for _, color in image.getcolors(384001)}
        self.assertTrue(colors <= {(255, 255, 255), (255, 0, 0), (0, 0, 0)})


class BleTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        FakeClient.instances, FakeClient.notify, FakeClient.fail_packet = [], ph.SUCCESS_NOTIFICATION, False
        self.raw = b"\xff" * 48000 + b"\x00" * 48000

    async def test_success_requires_all_acks_and_exact_notification(self):
        result = await ph.send_raw(NAME, MAC, self.raw, FakeScanner, FakeClient, .01)
        packets = build_write_packets(build_image_stream(self.raw))
        self.assertTrue(result["ok"])
        self.assertEqual(result["notification"], "ff00000000000000")
        self.assertEqual(result["packets"], len(packets))
        writes = FakeClient.instances[-1].writes
        self.assertEqual([data for uuid, data in writes if uuid == IMAGE_WRITE_UUID], packets)
        self.assertFalse(FakeClient.instances[-1].is_connected)

    async def test_missing_or_inexact_notification_is_never_success(self):
        for notification in (None, b"\xff", b"\xff" + b"\x00" * 8):
            FakeClient.notify = notification
            with self.assertRaises(ph.PanelError) as caught:
                await ph.send_raw(NAME, MAC, self.raw, FakeScanner, FakeClient, .001)
            self.assertEqual(caught.exception.code, "completion_unconfirmed")
        self.assertEqual(len(FakeClient.instances), 3)  # No internal retries.

    async def test_write_failure_never_success_and_not_retried(self):
        FakeClient.fail_packet = True
        with self.assertRaises(OSError):
            await ph.send_raw(NAME, MAC, self.raw, FakeScanner, FakeClient, .001)
        self.assertEqual(len(FakeClient.instances), 1)
        self.assertFalse(FakeClient.instances[-1].is_connected)

    async def test_intermittent_advertisement_is_searched_across_multiple_rounds(self):
        class IntermittentScanner:
            calls = 0

            @classmethod
            async def find_device_by_filter(cls, predicate, timeout):
                cls.calls += 1
                self.assertEqual(timeout, ph.SCAN_SECONDS)
                if cls.calls < 3:
                    return None
                return await FakeScanner.find_device_by_filter(predicate, timeout)

        progress = []
        result = await ph.send_raw(NAME, MAC, self.raw, IntermittentScanner, FakeClient, .01, on_progress=progress.append)
        self.assertTrue(result["ok"])
        self.assertEqual(IntermittentScanner.calls, 3)
        self.assertEqual(len(FakeClient.instances), 1)
        self.assertEqual([row["attempt"] for row in progress if row["phase"] == "scanning"], [1, 2, 3])

    async def test_exhausted_search_has_exact_bounded_attempts_and_no_gatt_write(self):
        class SleepingScanner:
            calls = 0

            @classmethod
            async def find_device_by_filter(cls, predicate, timeout):
                cls.calls += 1
                return None

        with self.assertRaises(ph.PanelError) as raised:
            await ph.send_raw(NAME, MAC, self.raw, SleepingScanner, FakeClient, .01)
        self.assertEqual(raised.exception.code, "device_not_found")
        self.assertEqual(SleepingScanner.calls, ph.SCAN_ATTEMPTS)
        self.assertEqual(FakeClient.instances, [])

    async def test_latest_content_is_prepared_after_connect_and_before_first_image_write(self):
        prepared = False

        async def prepare():
            nonlocal prepared
            self.assertTrue(FakeClient.instances[-1].is_connected)
            self.assertEqual(FakeClient.instances[-1].writes, [])
            prepared = True
            return self.raw

        await ph.send_raw(NAME, MAC, self.raw, FakeScanner, FakeClient, .01, prepare_raw=prepare)
        self.assertTrue(prepared)

    async def test_scan_retries_until_matching_advert_and_stops_promptly(self):
        class Discovery:
            calls = 0

            @classmethod
            async def discover(cls, timeout, return_adv):
                cls.calls += 1
                if cls.calls < 2:
                    return {}
                device = SimpleNamespace(name=NAME, address=MAC)
                return {MAC: (device, SimpleNamespace(local_name=NAME, rssi=-42))}

        progress = []
        result = await ph.scan_devices(NAME, Discovery, progress.append)
        self.assertEqual(Discovery.calls, 2)
        self.assertEqual(result, [{"name": NAME, "address": MAC, "rssi": -42}])
        self.assertEqual([row["attempt"] for row in progress], [1, 2])

    async def test_cancel_current_scan_does_not_queue_or_transmit(self):
        started = asyncio.Event()

        async def pending(*args, **kwargs):
            started.set()
            await asyncio.Future()

        with tempfile.TemporaryDirectory() as directory, patch.object(ph, "private_directory", lambda path: path.mkdir(exist_ok=True)):
            engine = ph.Engine(directory, "http://127.0.0.1:10000")
            with patch.object(ph, "scan_devices", side_effect=pending), patch.object(ph, "send_raw") as send:
                operation = asyncio.create_task(engine.operation("scan", {"device_name": NAME}))
                await started.wait()
                await engine.cancel()
                with self.assertRaises(ph.PanelError) as raised:
                    await operation
                self.assertEqual(raised.exception.code, "operation_cancelled")
                self.assertFalse(engine.busy)
                self.assertEqual(engine.state["last_status"]["status"], "cancelled")
                send.assert_not_called()

    async def test_failed_background_refresh_records_failure_without_success(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(ph, "private_directory", lambda path: path.mkdir(exist_ok=True)):
                engine = ph.Engine(directory, "http://127.0.0.1:10000")
            engine.config.update({"device_name": NAME, "address": MAC, "panel_token": "ftp_" + "x" * 32, "pairing_id": "test-pairing"})
            with patch.object(engine, "fetch_snapshot", side_effect=ph.PanelError("已撤销，请配对", "pairing_expired", 401)):
                with self.assertRaises(ph.PanelError):
                    await engine.operation("automatic", engine.expected_target(engine.config))
            self.assertEqual(engine.state["last_status"]["status"], "failed")
            self.assertEqual(engine.state["last_status"]["error"], "pairing_expired")
            self.assertFalse(engine.busy)
            self.assertNotIn(engine.config["panel_token"], engine.state_path.read_text(encoding="utf-8"))


class ApiTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        # Tests use a temporary directory; production additionally enforces Windows ACL.
        with patch.object(ph, "private_directory", lambda directory: directory.mkdir(exist_ok=True)):
            self.engine = ph.Engine(self.temporary.name, "http://127.0.0.1:10000")
        self.loop = asyncio.new_event_loop()
        self.loop_thread = threading.Thread(target=self.loop.run_forever, daemon=True)
        self.loop_thread.start()
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), ph.handler_factory(self.engine, self.loop, 0))
        self.port = self.server.server_port
        # Host validation uses the actual allocated test port.
        self.server.RequestHandlerClass = ph.handler_factory(self.engine, self.loop, self.port)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.loop.call_soon_threadsafe(self.loop.stop)
        self.loop_thread.join()
        self.loop.close()
        self.temporary.cleanup()

    def request(self, path="/health", method="GET", body=None, origin=None, host=None, content="application/json"):
        headers = {"Content-Type": content}
        if origin is not None:
            headers["Origin"] = origin
        if host is not None:
            headers["Host"] = host
        encoded = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(f"http://127.0.0.1:{self.port}" + path, data=encoded, method=method, headers=headers)
        try:
            response = urllib.request.urlopen(request, timeout=5)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            return response.status, response.headers, json.loads(response.read())

    def test_source_and_host_rejection(self):
        self.assertEqual(self.request("/config", "PUT", {"enabled": False})[0], 403)
        self.assertEqual(self.request(origin="https://evil.example")[0], 403)
        self.assertEqual(self.request(host="evil.example")[0], 403)
        self.assertEqual(self.request("/config", "PUT", {"enabled": False}, origin=self.engine.origin, content="text/plain")[0], 415)

    def test_preflight_headers_only_for_allowed_origin(self):
        status, headers, body = self.request("/send", "OPTIONS", origin=self.engine.origin)
        self.assertEqual(status, 200)
        self.assertEqual(headers["Access-Control-Allow-Origin"], self.engine.origin)
        self.assertEqual(headers["Access-Control-Allow-Private-Network"], "true")
        status, headers, body = self.request("/send", "OPTIONS", origin="https://evil.example")
        self.assertEqual(status, 403)
        self.assertIsNone(headers.get("Access-Control-Allow-Origin"))

    def test_configuration_token_redaction_and_upstream_restriction(self):
        token = "ftp_" + "A" * 32
        body = {"enabled": True, "device_name": NAME, "address": MAC, "panel_token": token, "pairing_id": "test-pairing"}
        with patch.object(self.engine, "fetch_snapshot", return_value={}) as fetch:
            status, headers, response = self.request("/config", "PUT", body, origin=self.engine.origin)
        fetch.assert_called_once()
        self.assertEqual(status, 200)
        self.assertNotIn(token, json.dumps(response))
        self.assertTrue(response["config"]["has_token"])
        self.assertEqual(response["config"]["pairing_id"], "test-pairing")
        self.assertNotIn(token, json.dumps(self.request()[2]))
        self.assertEqual(json.loads(self.engine.config_path.read_text())["panel_token"], token)
        body["fasttask_url"] = "https://evil.example"
        self.assertEqual(self.request("/config", "PUT", body, origin=self.engine.origin)[0], 400)

    def test_http_refresh_requires_the_calling_tabs_expected_pairing_before_any_io(self):
        self.engine.config.update({"enabled": True, "device_name": NAME, "address": MAC, "panel_token": "ftp_" + "B" * 32, "pairing_id": "account-b"})
        before = dict(self.engine.config)
        with patch.object(self.engine, "fetch_snapshot") as fetch, patch.object(ph, "send_raw") as send:
            for body in ({"page": 3}, {"expected_pairing_id": "account-a", "page": 3}, {"expected_pairing_id": "account-b", "page": 3}, {"expected_pairing_id": "account-b", "expected_device_name": NAME, "page": 3}, {"expected_pairing_id": "account-b", "expected_address": MAC, "page": 3}, {"expected_pairing_id": "account-b", "expected_device_name": NAME, "expected_address": "", "page": 3}):
                status, _, data = self.request("/refresh", "POST", body, origin=self.engine.origin)
                self.assertEqual(status, 409)
                self.assertEqual(data["error"], "pairing_changed")
            fetch.assert_not_called(); send.assert_not_called()
        self.assertEqual(self.engine.config, before)
        self.assertIsNone(self.engine.state["last_status"])
        self.assertTrue(self.request()[2]["capabilities"]["guarded_refresh"])


if __name__ == "__main__":
    unittest.main()
