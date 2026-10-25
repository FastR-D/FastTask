"""Default read-only scheduling. Isolated storage; no real BLE or FastTask calls."""
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

    async def test_missing_pairing_or_device_never_enables_or_runs_schedule(self):
        cases = ({}, {"device_name": PAIRING["device_name"], "address": PAIRING["address"]}, {"panel_token": PAIRING["panel_token"], "pairing_id": PAIRING["pairing_id"]}, {key: value for key, value in PAIRING.items() if key != "pairing_id"})
        for config in cases:
            with self.subTest(config=list(config)), isolated_engine() as engine:
                with patch.object(engine, "fetch_snapshot") as fetch, patch.object(engine, "operation", new_callable=AsyncMock) as operation:
                    result = await engine.configure(config)
                    self.assertFalse(result["config"]["enabled"])
                    self.assertIsNone(engine.health()["next_run"])
                    self.assertIsNone(await engine.run_due_slot(datetime(2026, 10, 25, 21, 0, tzinfo=ph.SHANGHAI)))
                    # Defense in depth: a malformed saved enable flag alone is
                    # never enough to start a scheduled transfer.
                    engine.config["enabled"] = True
                    self.assertIsNone(await engine.run_due_slot(datetime(2026, 10, 25, 21, 0, tzinfo=ph.SHANGHAI)))
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
            self.assertIsNone(restarted.health()["next_run"])

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

    async def test_page_update_keeps_existing_schedule_without_rechecking_token(self):
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


class ScheduleTests(unittest.IsolatedAsyncioTestCase):
    async def test_default_schedule_reads_snapshot_and_preserves_task_statuses(self):
        snapshot = {"profile": {}, "tasks": {"local_date": "2026-10-25", "today_items": [
            {"id": "one", "position": 0, "title": "已完成任务", "status": "satisfied"},
            {"id": "two", "position": 1, "title": "未完成任务", "status": "planned"},
            {"id": "three", "position": 2, "title": "今日未完成", "status": "not_completed"},
        ]}}
        original = copy.deepcopy(snapshot)
        with isolated_engine() as engine:
            with patch.object(engine, "fetch_snapshot", return_value=snapshot), patch.object(ph, "send_raw", new_callable=AsyncMock, return_value={"ok": True, "notification": "ff00000000000000"}) as send:
                await engine.configure(PAIRING)
                slot = await engine.run_due_slot(datetime(2026, 10, 25, 17, 30, tzinfo=ph.SHANGHAI))
                duplicate = await engine.run_due_slot(datetime(2026, 10, 25, 17, 31, tzinfo=ph.SHANGHAI))
            self.assertEqual(slot, "2026-10-25T17:30")
            self.assertIsNone(duplicate)
            send.assert_awaited_once()
            self.assertEqual(snapshot, original)
            self.assertEqual(engine.state["last_status"]["result"]["task_count"], 3)
            self.assertEqual(engine.state["last_status"]["status"], "success")

    async def test_failed_latest_slot_is_persisted_and_never_retried_on_restart(self):
        with isolated_engine() as engine:
            with patch.object(engine, "fetch_snapshot", return_value={}):
                await engine.configure(PAIRING)
            now = datetime(2026, 10, 25, 22, 0, tzinfo=ph.SHANGHAI)

            async def fail(operation, *, slot):
                self.assertEqual(operation, "scheduled")
                self.assertEqual(slot, "2026-10-25T21:00")
                self.assertEqual(json.loads(engine.state_path.read_text())["last_attempted_slot"], slot)
                raise ph.PanelError("设备暂不可用", "device_not_found")

            with patch.object(engine, "operation", side_effect=fail) as operation:
                self.assertEqual(await engine.run_due_slot(now), "2026-10-25T21:00")
                self.assertIsNone(await engine.run_due_slot(now))
            operation.assert_awaited_once()
            restarted = ph.Engine(engine.directory, ORIGIN)
            with patch.object(restarted, "operation", new_callable=AsyncMock) as operation:
                self.assertIsNone(await restarted.run_due_slot(now))
                self.assertIsNone(await restarted.run_due_slot(datetime(2026, 10, 26, 10, 0, tzinfo=ph.SHANGHAI)))
            operation.assert_not_called()


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
