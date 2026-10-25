"""Loopback-only Windows WOLINK bridge and paired daily FastTask synchronizer."""
from __future__ import annotations

import argparse
import asyncio
import base64
import concurrent.futures
import io
import ipaddress
import json
import os
import re
import ssl
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from PIL import Image
from badge import SHANGHAI, render_snapshot
from wolink.image import image_to_raw
from wolink.models import AUTH_UUID, DONE_NOTIFY_UUID, IMAGE_WRITE_UUID, SERVICE_UUID
from wolink.protocol import SUCCESS_NOTIFICATION, build_image_stream, build_write_packets, encrypt_challenge

VERSION = "1.0.0"
SLOTS = ("14:00", "17:30", "21:00")
MAX_BODY = 1_500_000
MAX_IMAGE = 1_048_576
NAME = re.compile(r"^WL[0-9A-F]{8}$")
ADDRESS = re.compile(r"^(?:[0-9A-F]{2}:){5}[0-9A-F]{2}$")
TOKEN = re.compile(r"ftp_[A-Za-z0-9_-]{20,200}")


class PanelError(Exception):
    def __init__(self, message, code="operation_failed", http_status=400):
        super().__init__(message)
        self.code, self.http_status = code, http_status


def normalize_origin(value):
    """One browser origin, never a URL with credentials or an upstream path."""
    invalid = "FastTask 来源必须是 HTTPS 网站，或本机 HTTP 地址；只填写协议、主机和端口"
    if not isinstance(value, str) or not value or any(char.isspace() or ord(char) < 32 for char in value) or any(char in value for char in "\\%@?#"):
        raise PanelError(invalid, "invalid_origin")
    try:
        parsed = urllib.parse.urlsplit(value)
        if parsed.scheme not in ("http", "https") or not parsed.netloc or parsed.path not in ("", "/") or parsed.username is not None or parsed.password is not None:
            raise ValueError("invalid origin")
        # urlsplit accepts an empty port and some non-browser URL spellings.
        authority = re.fullmatch(r"(?:\[[0-9A-Fa-f:.]+\]|[^:\[\]]+)(?::([0-9]+))?", parsed.netloc)
        if authority is None:
            raise ValueError("invalid authority")
        port = parsed.port
        if port is not None and not 1 <= port <= 65535:
            raise ValueError("invalid port")
        hostname = parsed.hostname
        if not hostname:
            raise ValueError("missing hostname")
        try:
            address = ipaddress.ip_address(hostname)
            host = address.compressed.lower()
            authority_host = f"[{host}]" if address.version == 6 else host
        except ValueError:
            host = hostname.encode("idna").decode("ascii").lower()
            labels = host[:-1].split(".") if host.endswith(".") else host.split(".")
            if len(host) > 253 or any(not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label) for label in labels):
                raise ValueError("invalid hostname")
            # Browsers reinterpret shortened/numeric IPv4 spellings; do not
            # accept one spelling and then authorize a different origin.
            if re.fullmatch(r"[0-9.]+", host):
                raise ValueError("noncanonical IPv4")
            authority_host = host
        if parsed.scheme == "http" and host not in ("localhost", "127.0.0.1", "::1"):
            raise ValueError("HTTP is only allowed on loopback")
        default_port = 443 if parsed.scheme == "https" else 80
        suffix = f":{port}" if port is not None and port != default_port else ""
        return f"{parsed.scheme}://{authority_host}{suffix}"
    except (ValueError, UnicodeError) as exc:
        raise PanelError(invalid, "invalid_origin") from exc


def target(name, address):
    name, address = str(name).strip().upper(), str(address).strip().upper()
    if not NAME.fullmatch(name) or not ADDRESS.fullmatch(address):
        raise PanelError("请提供 WOLINK 名称 WLxxxxxxxx 和完整蓝牙 MAC 地址", "invalid_device")
    if name[2:] != address.replace(":", "")[-8:]:
        raise PanelError("设备名称与蓝牙地址不匹配", "device_mismatch")
    return name, address


def paired(config):
    """A schedule needs an explicit account pairing and one validated device."""
    token, pairing_id = config.get("panel_token"), config.get("pairing_id")
    if not isinstance(token, str) or not TOKEN.fullmatch(token) or not isinstance(pairing_id, str) or not pairing_id.strip():
        return False
    try:
        target(config.get("device_name", ""), config.get("address", ""))
    except PanelError:
        return False
    return True


def due_slot(now, last_attempted=""):
    local = now.astimezone(SHANGHAI)
    today = local.date().isoformat()
    eligible = [today + "T" + slot for slot in SLOTS if slot <= local.strftime("%H:%M")]
    latest = eligible[-1] if eligible else None
    # One latest missed slot for today. No historical backlog and no automatic transfer retries.
    return latest if latest and latest > last_attempted else None


def next_slot(now):
    local = now.astimezone(SHANGHAI)
    for offset in (0, 1):
        day = local.date() + timedelta(days=offset)
        for slot in SLOTS:
            candidate = datetime.fromisoformat(f"{day}T{slot}").replace(tzinfo=SHANGHAI)
            if candidate > local:
                return candidate.isoformat()


def decode_image(encoded):
    if not isinstance(encoded, str):
        raise PanelError("image_base64 必须为 PNG/JPEG 的 base64 字符串", "invalid_image")
    if encoded.startswith("data:"):
        if not encoded.startswith(("data:image/png;base64,", "data:image/jpeg;base64,")):
            raise PanelError("只支持 PNG/JPEG 图像", "invalid_image")
        encoded = encoded.split(",", 1)[1]
    try:
        data = base64.b64decode(encoded, validate=True)
        if len(data) > MAX_IMAGE:
            raise PanelError("图片不能超过 1 MB", "image_too_large", 413)
        with Image.open(io.BytesIO(data)) as source:
            if source.size != (800, 480) or source.format not in ("PNG", "JPEG"):
                raise PanelError("图片必须为 800×480 的 PNG/JPEG", "invalid_image")
            source.load()
            return image_to_raw(source, fit="stretch")
    except (ValueError, OSError) as exc:
        raise PanelError("图片解码失败", "invalid_image") from exc


async def scan_devices(name=""):
    from bleak import BleakScanner
    if name and not NAME.fullmatch(name):
        raise PanelError("蓝牙 ID 格式应为 WLxxxxxxxx", "invalid_device")
    found = await BleakScanner.discover(timeout=30.0, return_adv=True)
    result = []
    for device, adv in found.values():
        advertised = (adv.local_name or device.name or "").upper()
        if not NAME.fullmatch(advertised) or (name and advertised != name):
            continue
        try:
            valid_name, address = target(advertised, device.address)
        except PanelError:
            continue
        result.append({"name": valid_name, "address": address, "rssi": adv.rssi})
    return sorted(result, key=lambda row: row["name"])


async def send_raw(name, address, raw, scanner=None, client_factory=None, done_timeout=60):
    """Never retry a transfer: a timeout may still mean the device received a packet."""
    name, address = target(name, address)
    if scanner is None or client_factory is None:
        from bleak import BleakClient, BleakScanner
        scanner, client_factory = BleakScanner, BleakClient

    def matches(device, advertisement):
        return device.address.upper() == address and (advertisement.local_name or device.name or "").upper() == name

    device = await scanner.find_device_by_filter(matches, timeout=35)
    if device is None:
        raise PanelError("未发现名称和地址均匹配的墨水屏，请检查电源、距离及蓝牙占用", "device_not_found", 503)
    packets = build_write_packets(build_image_stream(raw))
    client = client_factory(device, timeout=25, winrt={"use_cached_services": True})
    notify_started, completed = False, asyncio.Event()
    acknowledged = 0
    try:
        await asyncio.wait_for(client.connect(), 30)
        if not client.is_connected or client.services.get_service(SERVICE_UUID) is None:
            raise PanelError("设备未暴露预期 WOLINK 服务", "wrong_service", 503)
        challenge = bytes(await asyncio.wait_for(client.read_gatt_char(AUTH_UUID), 20))
        await asyncio.wait_for(client.write_gatt_char(AUTH_UUID, encrypt_challenge(challenge), response=True), 20)

        def on_done(_sender, data):
            if bytes(data) == SUCCESS_NOTIFICATION:
                completed.set()

        await asyncio.wait_for(client.start_notify(DONE_NOTIFY_UUID, on_done), 20)
        notify_started = True
        for packet in packets:
            await asyncio.wait_for(client.write_gatt_char(IMAGE_WRITE_UUID, packet, response=True), 20)
            acknowledged += 1
        try:
            await asyncio.wait_for(completed.wait(), done_timeout)
        except asyncio.TimeoutError as exc:
            raise PanelError("数据已发送，但未收到设备准确完成通知；不能确认刷新成功，请检查墨水屏后手动重试", "completion_unconfirmed", 504) from exc
        return {"ok": True, "notification": SUCCESS_NOTIFICATION.hex(), "packets": acknowledged, "device_name": name, "address": address}
    finally:
        if notify_started and client.is_connected:
            try:
                await asyncio.wait_for(client.stop_notify(DONE_NOTIFY_UUID), 5)
            except Exception:
                pass
        try:
            await asyncio.wait_for(client.disconnect(), 10)
        except Exception:
            pass


def private_directory(directory):
    directory.mkdir(parents=True, exist_ok=True)
    if os.name == "nt":
        identity = subprocess.run(["whoami"], capture_output=True, text=True, check=True, creationflags=subprocess.CREATE_NO_WINDOW).stdout.strip()
        result = subprocess.run(["icacls", str(directory), "/inheritance:r", "/grant:r", f"{identity}:(OI)(CI)F"], capture_output=True, creationflags=subprocess.CREATE_NO_WINDOW)
        if result.returncode:
            raise PanelError("无法限制本地配置文件权限，未保存配对凭据", "private_storage_failed", 500)
    else:
        directory.chmod(0o700)


def write_private(path, data):
    temporary = path.with_suffix(".tmp")
    with open(temporary, "w", encoding="utf-8") as handle:
        if os.name != "nt":
            os.chmod(temporary, 0o600)
        json.dump(data, handle, ensure_ascii=False, indent=2)
    os.replace(temporary, path)


class Engine:
    def __init__(self, directory, origin):
        self.directory, self.origin = Path(directory), normalize_origin(origin)
        private_directory(self.directory)
        self.config_path, self.state_path = self.directory / "config.json", self.directory / "status.json"
        self.config = {"enabled": False, "device_name": "", "address": "", "fasttask_url": self.origin, "page": 0, "panel_token": "", "pairing_id": ""}
        self.state = {"last_attempted_slot": "", "last_status": None}
        source_changed = False
        if self.config_path.exists():
            saved = json.loads(self.config_path.read_text(encoding="utf-8"))
            self.config.update(saved)
            try:
                saved_origin = normalize_origin(saved.get("fasttask_url"))
            except PanelError:
                saved_origin = None
            source_changed = saved_origin != self.origin
            if source_changed:
                # A local account's token must never be reused at a new cloud
                # origin, including before the browser has opened its panel.
                self.config.update({"enabled": False, "device_name": "", "address": "", "fasttask_url": self.origin, "page": 0, "panel_token": "", "pairing_id": ""})
            else:
                self.config["fasttask_url"] = self.origin
                # A complete pairing defaults to the fixed daily schedule.
                # Preserve a deliberately saved administrative disable.
                self.config["enabled"] = paired(self.config) and saved.get("enabled", True) is True
            if source_changed or saved.get("fasttask_url") != self.origin or saved.get("enabled") != self.config["enabled"]:
                write_private(self.config_path, self.config)
        if self.state_path.exists():
            self.state.update(json.loads(self.state_path.read_text(encoding="utf-8")))
        if source_changed:
            self.state = {"last_attempted_slot": "", "last_status": {"status": "unpaired", "operation": "source_changed", "message": "FastTask 来源已变更，旧配对已清除；请登录新网站重新选择设备并配对"}}
            write_private(self.state_path, self.state)
        self.lock = asyncio.Lock()
        self.busy = False

    def public_config(self):
        return {key: value for key, value in self.config.items() if key != "panel_token"} | {"has_token": bool(self.config.get("panel_token"))}

    def health(self):
        return {"ok": True, "version": VERSION, "ble_supported": sys.platform == "win32", "busy": self.busy,
                "config": self.public_config(), "last_status": self.state["last_status"],
                "next_run": next_slot(datetime.now(SHANGHAI)) if self.config["enabled"] and paired(self.config) else None,
                "schedule": {"timezone": "Asia/Shanghai", "refresh_times": list(SLOTS)}}

    async def configure(self, values):
        async with self.lock:
            config = self.config | {key: values[key] for key in self.config if key in values}
            if type(config["page"]) is not int or not 0 <= config["page"] <= 999:
                raise PanelError("自动刷新开关或页码不正确", "invalid_config")
            if normalize_origin(config["fasttask_url"]) != self.origin:
                raise PanelError("FastTask 地址必须与启动时允许的页面来源一致", "invalid_origin")
            config["fasttask_url"] = self.origin
            token = config.get("panel_token")
            if token and (not isinstance(token, str) or not TOKEN.fullmatch(token)):
                raise PanelError("配对凭据格式不正确，请重新配对", "invalid_token")
            if not isinstance(config.get("pairing_id"), str) or len(config["pairing_id"]) > 256:
                raise PanelError("配对标识不正确，请重新配对", "invalid_config")
            if config["device_name"] or config["address"]:
                config["device_name"], config["address"] = target(config["device_name"], config["address"])
            if "enabled" not in values:
                config["enabled"] = paired(config)
            if type(config["enabled"]) is not bool:
                raise PanelError("自动刷新开关不正确", "invalid_config")
            if config["enabled"] and not paired(config):
                raise PanelError("请先选择墨水屏并配对 FastTask 账号", "not_paired")
            changed_pairing = any(config[key] != self.config[key] for key in ("panel_token", "pairing_id", "device_name", "address"))
            if config["enabled"] and (changed_pairing or not self.config["enabled"]):
                # Validate a new authorization with a read-only request before
                # committing it. A failed cloud connection keeps the old
                # pairing/configuration intact and never starts a BLE write.
                await asyncio.to_thread(self.fetch_snapshot, config)
            write_private(self.config_path, config)
            self.config = config
            return {"ok": True, "config": self.public_config()}

    def record(self, status):
        self.state["last_status"] = status
        write_private(self.state_path, self.state)

    def fetch_snapshot(self, proposed_config=None):
        config = self.config if proposed_config is None else proposed_config
        if normalize_origin(config.get("fasttask_url")) != self.origin:
            raise PanelError("FastTask 来源已变更，请重新配对", "invalid_origin")
        token = config.get("panel_token")
        if not token:
            raise PanelError("请先在 FastTask 页面配对账号", "not_paired")
        request = urllib.request.Request(self.origin + "/api/v1/panel/snapshot", headers={"X-Panel-Token": token, "Accept": "application/json"})
        # The fixed upstream must not inherit proxies, follow redirects, or
        # weaken certificate/hostname verification for cloud deployments.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                return None
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect(), urllib.request.HTTPSHandler(context=ssl.create_default_context()))
        try:
            with opener.open(request, timeout=15) as response:
                data = response.read(2_000_001)
                if len(data) > 2_000_000:
                    raise PanelError("FastTask 快照超过大小限制", "snapshot_too_large", 502)
                snapshot = json.loads(data)
        except urllib.error.HTTPError as exc:
            exc.close()
            if exc.code in (401, 403):
                raise PanelError("FastTask 配对已失效或被撤销，请在页面重新配对", "pairing_expired", 401) from exc
            raise PanelError(f"FastTask 暂不可用（HTTP {exc.code}）", "snapshot_unavailable", 502) from exc
        except (urllib.error.URLError, OSError, ValueError) as exc:
            raise PanelError("无法读取 FastTask 今日任务，请检查网络、服务及 HTTPS 证书", "snapshot_unavailable", 503) from exc
        if snapshot.get("tasks", {}).get("local_date") != datetime.now(SHANGHAI).date().isoformat():
            raise PanelError("FastTask 返回的任务不是上海时区当日数据，未写入墨水屏", "stale_snapshot", 409)
        return snapshot

    async def operation(self, kind, body=None, slot=None):
        # Never queue another physical transfer behind an operation whose
        # browser request may already have timed out.
        if self.lock.locked():
            raise PanelError("助手仍有操作正在执行，请查询状态并等待完成，勿重复提交", "operation_busy", 409)
        async with self.lock:
            self.busy = True
            started = datetime.now(SHANGHAI).isoformat()
            info = {"status": "running", "operation": kind, "started_at": started, "slot": slot}
            self.record(info)
            try:
                if kind == "scan":
                    return {"ok": True, "devices": await scan_devices(str((body or {}).get("device_name", "")).strip().upper())}
                if kind == "send":
                    body = body or {}
                    name, address = target(body.get("device_name", ""), body.get("address", ""))
                    raw = decode_image(body.get("image_base64"))
                    metadata = {}
                else:
                    name, address = target(self.config["device_name"], self.config["address"])
                    snapshot = await asyncio.to_thread(self.fetch_snapshot)
                    image, metadata = render_snapshot(snapshot, self.config["page"])
                    raw = image_to_raw(image, fit="stretch")
                    info["snapshot_generated_at"] = snapshot.get("generated_at")
                    info["weather_status"] = snapshot.get("weather", {}).get("status", "unavailable")
                result = await send_raw(name, address, raw)
                result.update(metadata)
                if "page" in metadata and self.config["page"] != metadata["page"]:
                    self.config["page"] = metadata["page"]
                    write_private(self.config_path, self.config)
                info.update({"status": "success", "message": "墨水屏已准确确认写入", "result": result})
                return result
            except Exception as exc:
                error = exc if isinstance(exc, PanelError) else PanelError("蓝牙操作失败，请确认 Windows 蓝牙、设备电源及距离后手动重试", "bluetooth_failed", 503)
                info.update({"status": "failed", "error": error.code, "message": str(error)})
                raise error from exc
            finally:
                if kind == "scan" and info["status"] == "running":
                    info.update({"status": "success", "message": "蓝牙扫描完成"})
                info["finished_at"] = datetime.now(SHANGHAI).isoformat()
                self.record(info)
                self.busy = False

    async def run_due_slot(self, now):
        if not self.config["enabled"] or not paired(self.config) or self.lock.locked():
            return None
        slot = due_slot(now, self.state.get("last_attempted_slot", ""))
        if slot:
            # Persist BEFORE any BLE write: power loss must not cause an automatic resend.
            self.state["last_attempted_slot"] = slot
            write_private(self.state_path, self.state)
            try:
                await self.operation("scheduled", slot=slot)
            except PanelError:
                pass
        return slot

    async def scheduler(self):
        while True:
            await self.run_due_slot(datetime.now(SHANGHAI))
            await asyncio.sleep(15)


def handler_factory(engine, loop, port):
    class Handler(BaseHTTPRequestHandler):
        server_version = "FastTaskPanel/" + VERSION

        def log_message(self, fmt, *args):
            # Paths/bodies/tokens and nearby device identifiers never enter the access log.
            return

        def origin_allowed(self):
            return self.headers.get("Origin") == engine.origin

        def verify_source(self, write=False):
            if self.headers.get("Host", "") not in (f"127.0.0.1:{port}", f"localhost:{port}"):
                raise PanelError("Host 不允许", "invalid_host", 403)
            origin = self.headers.get("Origin")
            if (origin and not self.origin_allowed()) or (write and not self.origin_allowed()):
                raise PanelError("仅允许已配置的 FastTask 页面操作本地墨水屏", "invalid_origin", 403)

        def respond(self, code, payload):
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Vary", "Origin")
            if self.origin_allowed():
                self.send_header("Access-Control-Allow-Origin", engine.origin)
                self.send_header("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
                self.send_header("Access-Control-Allow-Headers", "Content-Type")
                self.send_header("Access-Control-Allow-Private-Network", "true")
            self.end_headers()
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                pass

        def dispatch(self):
            self.verify_source(write=self.command in ("POST", "PUT", "OPTIONS"))
            path = urllib.parse.urlsplit(self.path).path
            if self.command == "OPTIONS":
                return self.respond(200, {"ok": True})
            if self.command == "GET":
                if path == "/health":
                    return self.respond(200, engine.health())
                if path == "/config":
                    return self.respond(200, {"ok": True, "config": engine.public_config()})
            if self.command in ("POST", "PUT"):
                if self.headers.get("Content-Type", "").split(";", 1)[0].strip() != "application/json":
                    raise PanelError("必须使用 application/json", "invalid_content_type", 415)
                if self.headers.get("Transfer-Encoding"):
                    raise PanelError("不支持分块请求", "invalid_length", 400)
                try:
                    length = int(self.headers.get("Content-Length", "0"))
                except ValueError as exc:
                    raise PanelError("请求长度不正确", "invalid_length") from exc
                if not 0 < length <= MAX_BODY:
                    raise PanelError("请求为空或超过大小限制", "invalid_length", 413)
                try:
                    body = json.loads(self.rfile.read(length))
                except (ValueError, UnicodeError) as exc:
                    raise PanelError("JSON 格式不正确", "invalid_json") from exc
                if not isinstance(body, dict):
                    raise PanelError("请求必须为 JSON 对象", "invalid_json")
                if self.command == "PUT" and path == "/config":
                    future = asyncio.run_coroutine_threadsafe(engine.configure(body), loop)
                elif self.command == "POST" and path in ("/scan", "/send", "/refresh"):
                    future = asyncio.run_coroutine_threadsafe(engine.operation(path[1:], body), loop)
                else:
                    raise PanelError("接口不存在", "not_found", 404)
                # A client timeout must never imply an automatic retry or a success.
                result = future.result(timeout=300)
                return self.respond(200, result)
            raise PanelError("接口不存在", "not_found", 404)

        def do_GET(self):
            try:
                self.dispatch()
            except PanelError as exc:
                self.respond(exc.http_status, {"ok": False, "error": exc.code, "message": str(exc)})
            except concurrent.futures.TimeoutError:
                self.respond(504, {"ok": False, "error": "operation_pending", "message": "操作仍在执行，请查询状态，勿重复提交"})
            except Exception:
                self.respond(500, {"ok": False, "error": "internal_error", "message": "本地助手异常，请查看状态后手动重试"})

        do_POST = do_GET
        do_PUT = do_GET
        do_OPTIONS = do_GET

    return Handler


def parse_options(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=19743)
    parser.add_argument("--origin", default="http://127.0.0.1:10000")
    parser.add_argument("--state-dir", type=Path, default=Path(__file__).resolve().parent / ".state")
    options = parser.parse_args(argv)
    try:
        options.origin = normalize_origin(options.origin)
    except PanelError as exc:
        parser.error(str(exc))
    if not 1 <= options.port <= 65535:
        parser.error("--port 必须在 1 到 65535 之间")
    return options


def main():
    options = parse_options()
    loop = asyncio.new_event_loop()
    engine = Engine(options.state_dir, options.origin)
    server = ThreadingHTTPServer(("127.0.0.1", options.port), handler_factory(engine, loop, options.port))
    server.daemon_threads = True
    thread = threading.Thread(target=loop.run_forever, daemon=True)
    thread.start()
    task = asyncio.run_coroutine_threadsafe(engine.scheduler(), loop)
    print(f"FastTask Windows Panel {VERSION} http://127.0.0.1:{options.port}; 自动刷新={'开启' if engine.config['enabled'] else '未开启'}", flush=True)
    try:
        server.serve_forever(poll_interval=0.5)
    except KeyboardInterrupt:
        pass
    finally:
        task.cancel()
        server.server_close()
        loop.call_soon_threadsafe(loop.stop)


if __name__ == "__main__":
    main()
