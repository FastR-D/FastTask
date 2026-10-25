"""Read-only change synchronization. Isolated storage; no real BLE or FastTask calls."""
import asyncio
import contextlib
import copy
import json
import sys
import tempfile
import unittest
from datetime import datetime
from pathlib import Path
from unittest.mock import AsyncMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import badge
import panel_helper as ph

ORIGIN = "https://tasks.example.edu"
PAIRING = {"panel_token": "ftp_" + "D" * 32, "pairing_id": "panel_test", "device_name": "WL14B00B1F", "address": "66:66:14:B0:0B:1F"}


@contextlib.contextmanager
def isolated_engine():
    with tempfile.TemporaryDirectory() as directory, patch.object(ph, "private_directory", lambda path: path.mkdir(exist_ok=True)):
        yield ph.Engine(directory, ORIGIN)


class DefaultPairingTests(unittest.IsolatedAsyncioTestCase):
    async def test_complete_new_pairing_defaults_on_after_read_only_verification(self):
        with isolated_engine() as engine:
            with patch.object(engine, "fetch_snapshot", return_value={}) as fetch, patch.object(ph, "send_raw", new_callable=AsyncMock) as send:
                response = await engine.configure(PAIRING)
            proposed = fetch.call_args.args[0]
            self.assertTrue(proposed["enabled"])
            self.assertEqual(proposed["panel_token"], PAIRING["panel_token"])
            fetch.assert_called_once()
            send.assert_not_called()
            self.assertTrue(response["config"]["enabled"])
            self.assertNotIn(PAIRING["panel_token"], json.dumps(response))
            restarted = ph.Engine(engine.directory, ORIGIN)
            self.assertTrue(restarted.config["enabled"])
            self.assertEqual(restarted.config["pairing_id"], PAIRING["pairing_id"])

    async def test_missing_pairing_or_device_never_enables_or_runs_monitor(self):
        cases = ({}, {"device_name": PAIRING["device_name"], "address": PAIRING["address"]}, {"panel_token": PAIRING["panel_token"], "pairing_id": PAIRING["pairing_id"]}, {key: value for key, value in PAIRING.items() if key != "pairing_id"})
        for config in cases:
            with self.subTest(config=list(config)), isolated_engine() as engine:
                with patch.object(engine, "fetch_snapshot") as fetch, patch.object(engine, "operation", new_callable=AsyncMock) as operation:
                    result = await engine.configure(config)
                    self.assertFalse(result["config"]["enabled"])
                    self.assertFalse(engine.health()["monitor"]["enabled"])
                    self.assertNotIn("schedule", engine.health())
                    self.assertNotIn("next_run", engine.health())
                    self.assertIsNone(await engine.monitor_tick(0))
                    # Defense in depth: a malformed saved enable flag alone is
                    # never enough to start a background transfer.
                    engine.config["enabled"] = True
                    self.assertIsNone(await engine.monitor_tick(10))
                fetch.assert_not_called()
                operation.assert_not_called()

    async def test_explicit_administrative_disable_persists(self):
        with isolated_engine() as engine:
            with patch.object(engine, "fetch_snapshot") as fetch:
                result = await engine.configure(PAIRING | {"enabled": False})
            fetch.assert_not_called()
            self.assertFalse(result["config"]["enabled"])
            restarted = ph.Engine(engine.directory, ORIGIN)
            self.assertFalse(restarted.config["enabled"])
            self.assertFalse(restarted.health()["monitor"]["enabled"])

    async def test_bad_or_expired_new_pairing_leaves_prior_configuration_intact(self):
        with isolated_engine() as engine:
            with patch.object(engine, "fetch_snapshot", return_value={}):
                await engine.configure(PAIRING)
            before, stored = copy.deepcopy(engine.config), engine.config_path.read_bytes()
            new_token = "ftp_" + "E" * 32

            def reject(proposed):
                self.assertEqual(proposed["panel_token"], new_token)
                self.assertEqual(engine.config, before)
                raise ph.PanelError("配对已过期", "pairing_expired", 401)

            with patch.object(engine, "fetch_snapshot", side_effect=reject), self.assertRaises(ph.PanelError):
                await engine.configure(PAIRING | {"panel_token": new_token, "pairing_id": "panel_new"})
            self.assertEqual(engine.config, before)
            self.assertEqual(engine.config_path.read_bytes(), stored)

        with isolated_engine() as engine:
            with patch.object(engine, "fetch_snapshot", side_effect=ph.PanelError("离线", "snapshot_unavailable")), self.assertRaises(ph.PanelError):
                await engine.configure(PAIRING)
            self.assertFalse(engine.config["enabled"])
            self.assertFalse(engine.config_path.exists())

    async def test_page_update_keeps_existing_monitor_without_rechecking_token(self):
        with isolated_engine() as engine:
            with patch.object(engine, "fetch_snapshot", return_value={}) as fetch:
                await engine.configure(PAIRING)
                await engine.configure({"page": 2})
            fetch.assert_called_once()
            self.assertTrue(engine.config["enabled"])
            self.assertEqual(engine.config["page"], 2)

    async def test_legacy_saved_pairing_defaults_on_only_when_complete(self):
        for complete in (True, False):
            with self.subTest(complete=complete), isolated_engine() as engine:
                saved = engine.config | PAIRING
                del saved["enabled"]
                if not complete:
                    del saved["pairing_id"]
                ph.write_private(engine.config_path, saved)
                restarted = ph.Engine(engine.directory, ORIGIN)
                self.assertEqual(restarted.config["enabled"], complete)


class ChangeSyncTests(unittest.IsolatedAsyncioTestCase):
    def snapshot(self, title="任务", status="planned"):
        return {"profile": {"display_name": "工牌"}, "tasks": {"local_date": "2026-10-25", "today_items": [{"id": "one", "title": title, "status": status, "position": 0}]}, "weather": {"local_date": "2026-10-25", "status": "ok", "summary": "晴 22°C"}}

    async def test_new_pairing_syncs_once_and_restart_does_not_repeat(self):
        snapshot = self.snapshot()
        original = copy.deepcopy(snapshot)
        with isolated_engine() as engine, patch.object(ph, "render_snapshot", return_value=(object(), {"page": 0})), patch.object(ph, "image_to_raw", return_value=b"frame"):
            with patch.object(engine, "fetch_snapshot", return_value=snapshot), patch.object(ph, "send_raw", new_callable=AsyncMock, return_value={"ok": True, "notification": ph.SUCCESS_NOTIFICATION.hex()}) as send:
                await engine.configure(PAIRING)
                self.assertIsNone(await engine.monitor_tick(0))
                self.assertTrue(engine.health()["monitor"]["pending"])
                await engine.monitor_tick(2)
                await engine.monitor_tick(12)
                send.assert_awaited_once()
                self.assertEqual(engine.state["last_status"]["status"], "success")
                restarted = ph.Engine(engine.directory, ORIGIN)
                with patch.object(restarted, "fetch_snapshot", return_value=snapshot):
                    await restarted.monitor_tick(0)
                send.assert_awaited_once()
            self.assertEqual(snapshot, original)

    async def test_fast_changes_are_coalesced_before_transfer(self):
        first, latest = self.snapshot("旧任务"), self.snapshot("新任务", "satisfied")
        with isolated_engine() as engine, patch.object(ph, "image_to_raw", return_value=b"latest"), patch.object(ph, "render_snapshot", return_value=(object(), {"page": 0})) as render:
            with patch.object(engine, "fetch_snapshot", side_effect=[first, first, latest, latest]), patch.object(ph, "send_raw", new_callable=AsyncMock, return_value={"ok": True, "notification": ph.SUCCESS_NOTIFICATION.hex()}) as send:
                await engine.configure(PAIRING)
                await engine.monitor_tick(0)
                await engine.monitor_tick(2)
                await engine.monitor_tick(4)
                send.assert_awaited_once()
                self.assertEqual(render.call_args.args[0], latest)

    async def test_failed_frame_is_persisted_before_attempt_and_manual_retry_recovers(self):
        snapshot = self.snapshot()
        with isolated_engine() as engine, patch.object(ph, "render_snapshot", return_value=(object(), {"page": 0})), patch.object(ph, "image_to_raw", return_value=b"frame"):
            async def fail(*args, **kwargs):
                stored = json.loads(engine.state_path.read_text())
                self.assertTrue(stored["last_attempted_fingerprint"])
                self.assertTrue(stored["last_attempted_source"])
                raise ph.PanelError("设备暂不可用", "device_not_found")
            with patch.object(engine, "fetch_snapshot", return_value=snapshot), patch.object(ph, "send_raw", side_effect=fail) as send:
                await engine.configure(PAIRING)
                await engine.monitor_tick(0)
                await engine.monitor_tick(2)
                await engine.monitor_tick(12)
                send.assert_awaited_once()
            restarted = ph.Engine(engine.directory, ORIGIN)
            with patch.object(restarted, "fetch_snapshot", return_value=snapshot), patch.object(ph, "send_raw", new_callable=AsyncMock, return_value={"ok": True, "notification": ph.SUCCESS_NOTIFICATION.hex()}) as send:
                await restarted.monitor_tick(0)
                send.assert_not_called()
                await restarted.operation("refresh", restarted.expected_target(restarted.config))
                await restarted.monitor_tick(12)
                send.assert_awaited_once()

    async def test_network_failure_backs_off_without_marking_frame_as_sent(self):
        with isolated_engine() as engine:
            engine.config.update(PAIRING | {"enabled": True})
            with patch.object(engine, "fetch_snapshot", side_effect=ph.PanelError("暂时离线", "snapshot_unavailable")) as fetch, patch.object(ph, "send_raw", new_callable=AsyncMock) as send:
                for now in (0, 1, 9, 10, 11, 29):
                    await engine.monitor_tick(now)
                self.assertEqual(fetch.call_count, 2)
                self.assertEqual(engine.next_poll_at, 30)
                self.assertEqual(engine.state["last_attempted_fingerprint"], "")
                send.assert_not_called()

    async def test_manual_page_refresh_is_one_locked_operation_and_monitor_does_not_duplicate_it(self):
        snapshot = self.snapshot()
        with isolated_engine() as engine, patch.object(ph, "render_snapshot", return_value=(object(), {"page": 2})) as render, patch.object(ph, "image_to_raw", return_value=b"page-two"), patch.object(engine, "fetch_snapshot", return_value=snapshot), patch.object(ph, "send_raw", new_callable=AsyncMock, return_value={"ok": True, "notification": ph.SUCCESS_NOTIFICATION.hex()}) as send:
            await engine.configure(PAIRING)
            await engine.operation("refresh", engine.expected_target(engine.config) | {"page": 2})
            self.assertEqual(render.call_args.args[1], 2)
            self.assertEqual(engine.config["page"], 2)
            self.assertEqual(json.loads(engine.config_path.read_text())["page"], 2)
            await engine.monitor_tick(0)
            send.assert_awaited_once()

    async def test_manual_refresh_invalid_page_does_not_change_config_or_start_io(self):
        with isolated_engine() as engine, patch.object(engine, "fetch_snapshot") as fetch, patch.object(ph, "send_raw") as send:
            engine.config.update(PAIRING)
            before = copy.deepcopy(engine.config)
            for page in (-1, 1000, True, "1"):
                with self.subTest(page=page), self.assertRaises(ph.PanelError) as raised:
                    await engine.operation("refresh", engine.expected_target(engine.config) | {"page": page})
                self.assertEqual(raised.exception.code, "invalid_config")
            self.assertEqual(engine.config, before)
            self.assertFalse(engine.busy)
            fetch.assert_not_called()
            send.assert_not_called()

    async def test_pairing_switch_cancels_old_transfer_and_resets_fingerprints(self):
        snapshot = self.snapshot()
        started = asyncio.Event()
        async def pending(*args, **kwargs):
            started.set()
            await asyncio.Future()
        with isolated_engine() as engine, patch.object(ph, "render_snapshot", return_value=(object(), {"page": 0})), patch.object(ph, "image_to_raw", return_value=b"frame"), patch.object(ph, "send_raw", side_effect=pending), patch.object(engine, "fetch_snapshot", return_value=snapshot):
            await engine.configure(PAIRING)
            operation = asyncio.create_task(engine.operation("refresh", engine.expected_target(engine.config)))
            await started.wait()
            await engine.configure(PAIRING | {"pairing_id": "other", "panel_token": "ftp_" + "N" * 32})
            with self.assertRaises(ph.PanelError) as raised:
                await operation
            self.assertEqual(raised.exception.code, "operation_cancelled")
            self.assertEqual(engine.config["pairing_id"], "other")
            self.assertEqual(engine.state["last_attempted_fingerprint"], "")
            self.assertEqual(engine.state["last_success_fingerprint"], "")

    async def test_stale_tab_refresh_cannot_change_new_account_page_or_touch_its_snapshot(self):
        snapshot = self.snapshot("B 的任务")
        with isolated_engine() as engine:
            with patch.object(engine, "fetch_snapshot", return_value=snapshot):
                await engine.configure(PAIRING | {"pairing_id": "account-b", "panel_token": "ftp_" + "B" * 32})
            engine.state.update(last_success_fingerprint="b-frame", last_success_source="b-source", last_attempted_fingerprint="b-attempt")
            ph.write_private(engine.state_path, engine.state)
            before_config, before_state = copy.deepcopy(engine.config), copy.deepcopy(engine.state)
            config_bytes, state_bytes = engine.config_path.read_bytes(), engine.state_path.read_bytes()
            cases = ({}, {"expected_pairing_id": PAIRING["pairing_id"]}, {"expected_pairing_id": True}, {"expected_pairing_id": "account-b", "expected_device_name": "WL00000000"}, {"expected_pairing_id": "account-b", "expected_address": "00:00:00:00:00:00"}, {"expected_pairing_id": "account-b", "expected_address": None})
            with patch.object(engine, "fetch_snapshot") as fetch, patch.object(ph, "send_raw") as send, patch.object(ph, "render_snapshot") as render:
                for body in cases:
                    with self.subTest(body=body), self.assertRaises(ph.PanelError) as raised:
                        await engine.operation("refresh", body | {"page": 9})
                    self.assertEqual((raised.exception.code, raised.exception.http_status), ("pairing_changed", 409))
                fetch.assert_not_called(); send.assert_not_called(); render.assert_not_called()
            self.assertEqual(engine.config, before_config)
            self.assertEqual(engine.state, before_state)
            self.assertEqual(engine.config_path.read_bytes(), config_bytes)
            self.assertEqual(engine.state_path.read_bytes(), state_bytes)
            with patch.object(engine, "fetch_snapshot", return_value=snapshot), patch.object(ph, "render_snapshot", return_value=(object(), {"page": 1})), patch.object(ph, "image_to_raw", return_value=b"new-b-frame"), patch.object(ph, "send_raw", new_callable=AsyncMock, return_value={"ok": True, "notification": ph.SUCCESS_NOTIFICATION.hex()}) as send:
                result = await engine.operation("refresh", engine.expected_target(engine.config) | {"page": 1})
                self.assertTrue(result["ok"])
                self.assertEqual(engine.config["page"], 1)
                send.assert_awaited_once()

    async def test_same_pairing_id_with_changed_credential_cannot_inherit_persisted_frame(self):
        with isolated_engine() as engine:
            with patch.object(engine, "fetch_snapshot", return_value=self.snapshot()):
                await engine.configure(PAIRING)
            old_scope = engine.sync_scope(engine.config)
            engine.state.update(last_success_fingerprint="old-frame", last_success_source="old-source", last_attempted_fingerprint="old-attempt")
            ph.write_private(engine.state_path, engine.state)
            next_config = engine.config | {"panel_token": "ftp_" + "C" * 32}
            self.assertNotEqual(engine.sync_scope(next_config), old_scope)
            ph.write_private(engine.config_path, next_config)
            restarted = ph.Engine(engine.directory, ORIGIN)
            self.assertEqual(restarted.config["pairing_id"], PAIRING["pairing_id"])
            self.assertEqual(restarted.state["last_success_fingerprint"], "")
            self.assertEqual(restarted.state["last_success_source"], "")
            self.assertEqual(restarted.state["last_attempted_fingerprint"], "")
            self.assertNotIn(next_config["panel_token"], json.dumps(restarted.state))
            self.assertNotIn(next_config["panel_token"], json.dumps(restarted.health()))

    def test_fingerprint_tracks_only_visible_fields_and_task_removal(self):
        snapshot = self.snapshot()
        config = PAIRING | {"page": 0}
        signature = ph.snapshot_fingerprint(snapshot, config)
        metadata = copy.deepcopy(snapshot)
        metadata.update(generated_at="later")
        metadata["weather"].update(fetched_at="later", observed_at="later")
        metadata["tasks"].update(plan_revision=99, updated_at="later")
        metadata["profile"].update(seat="legacy", task_overrides={"one": {"selected": False, "position": 99}})
        self.assertEqual(ph.snapshot_fingerprint(metadata, config), signature)
        for mutate in (lambda data: data["tasks"]["today_items"].clear(), lambda data: data["tasks"]["today_items"][0].update(status="satisfied"), lambda data: data["profile"].update(avatar_data_url="new-avatar"), lambda data: data["weather"].update(summary="小雨 18°C")):
            changed = copy.deepcopy(snapshot); mutate(changed)
            self.assertNotEqual(ph.snapshot_fingerprint(changed, config), signature)


class DefaultTasksTests(unittest.TestCase):
    def test_all_current_tasks_keep_source_order_and_real_status(self):
        snapshot = {"profile": {"task_overrides": {
            "two": {"selected": False, "position": -10, "title": "文案修正", "detail": "补充说明", "status": "satisfied"},
            "one": {"position": 100},
        }}, "tasks": {"local_date": "2026-10-25", "today_items": [
            {"id": "three", "position": 2, "title": "跳过任务", "status": "skipped"},
            {"id": "two", "position": 1, "title": "待完成任务", "status": "planned"},
            {"id": "old", "position": 4, "title": "旧版本", "status": "superseded"},
            {"id": "one", "position": 0, "title": "完成任务", "status": "satisfied"},
            {"id": "four", "position": 3, "title": "未完成任务", "status": "not_completed"},
        ]}}
        original = copy.deepcopy(snapshot)
        with patch.object(badge, "task_rows", wraps=badge.task_rows) as rows:
            _, metadata = badge.render_snapshot(snapshot)
        tasks = rows.call_args.args[0]
        self.assertEqual([item["id"] for item in tasks], ["one", "two", "three", "four"])
        self.assertEqual([item["status"] for item in tasks], ["satisfied", "planned", "skipped", "not_completed"])
        self.assertEqual(tasks[1]["title"], "文案修正")
        self.assertEqual(tasks[1]["detail"], "补充说明")
        rendered_rows = badge.task_rows(tasks, badge.ImageDraw.Draw(badge.Image.new("RGB", (800, 480))))
        self.assertEqual([row["status"] for row in rendered_rows], ["已完成", "待完成", "已跳过", "未完成"])
        self.assertEqual(metadata["task_count"], 4)
        self.assertEqual(snapshot, original)


if __name__ == "__main__":
    unittest.main()
