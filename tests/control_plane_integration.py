"""Isolated multi-process AV control-plane integration, with an actual SFU.

The default database is a temporary SQLite file shared by two API processes and
a worker. Redis is supplied explicitly and never flushed. All business objects,
ports, and subprocesses belong to this invocation. Optional synthetic media is
delegated to the headless browser fixture; otherwise this tests signaling only.
"""
from __future__ import annotations

import argparse
import base64
from contextlib import closing
import hashlib
import hmac
import http.cookies
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


def free_port() -> int:
    with socket.socket() as handle:
        handle.bind(("127.0.0.1", 0))
        return handle.getsockname()[1]


def redacted(value):
    if isinstance(value, dict):
        return {key: "[redacted]" if any(word in key.lower() for word in
                ("token", "credential", "password", "secret", "cookie", "ticket"))
                else redacted(item) for key, item in value.items()}
    if isinstance(value, list):
        return [redacted(item) for item in value]
    return value


def request(base: str, path: str, body=None, token="", organization="", credential="", method=None):
    headers = {"Content-Type": "application/json", "X-Foundation-Request": "1"}
    if token:
        headers["Authorization"] = "Bearer " + token
    if organization:
        headers["X-Organization-ID"] = organization
    if credential:
        headers["X-AV-Resource-Credential"] = credential
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        response = urllib.request.urlopen(req, timeout=6)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        raw = response.read().decode()
        try:
            value = json.loads(raw)
        except json.JSONDecodeError:
            value = raw
        return response.status, value, response.headers


def expect(response, status=200):
    actual, value, _ = response
    assert actual == status, f"expected {status}, got {actual}: {redacted(value)}"
    return value


def eventually(fn, description, timeout=15):
    until = time.monotonic() + timeout
    last = None
    while time.monotonic() < until:
        try:
            value = fn()
            if value:
                return value
        except (OSError, urllib.error.URLError) as error:
            last = type(error).__name__
        time.sleep(0.15)
    raise AssertionError(f"timed out: {description}; last={last}")


def jwt(issuer, secret, fields):
    def encode(value):
        return base64.urlsafe_b64encode(json.dumps(value, separators=(",", ":")).encode()).rstrip(b"=")
    now = int(time.time())
    body = encode({"alg": "HS256", "typ": "JWT"}) + b"." + encode(
        {"iss": issuer, "nbf": now - 1, "exp": now + 60, **fields})
    return (body + b"." + base64.urlsafe_b64encode(
        hmac.new(secret.encode(), body, hashlib.sha256).digest()).rstrip(b"=")).decode()


class WebSocket:
    """Small RFC6455 test client; only control/signaling messages, no media."""

    def __init__(self, base, path, token="", origin=""):
        parsed = urllib.parse.urlparse(base)
        self.socket = socket.create_connection((parsed.hostname, parsed.port), timeout=6)
        self.reader = self.socket.makefile("rb")
        key = base64.b64encode(secrets.token_bytes(16)).decode()
        headers = [f"GET {path} HTTP/1.1", f"Host: {parsed.netloc}", "Upgrade: websocket",
                   "Connection: Upgrade", "Sec-WebSocket-Version: 13", f"Sec-WebSocket-Key: {key}"]
        if token:
            headers.append("Authorization: Bearer " + token)
        if origin:
            headers.append("Origin: " + origin)
        self.socket.sendall(("\r\n".join(headers) + "\r\n\r\n").encode())
        first = self.reader.readline(8192).decode().strip()
        received = {}
        for _ in range(50):
            line = self.reader.readline(8192).decode().strip()
            if not line:
                break
            name, value = line.split(":", 1)
            received[name.lower()] = value.strip()
        if " 101 " not in first:
            self.close()
            raise AssertionError(f"websocket upgrade failed: {first}")
        accepted = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        assert received.get("sec-websocket-accept") == accepted

    def send(self, opcode, value):
        mask = secrets.token_bytes(4)
        size = len(value)
        header = bytes([0x80 | opcode, 0x80 | size]) if size < 126 else bytes([0x80 | opcode, 0x80 | 126]) + struct.pack("!H", size)
        self.socket.sendall(header + mask + bytes(byte ^ mask[index % 4] for index, byte in enumerate(value)))

    def read(self):
        while True:
            header = self.reader.read(2)
            if len(header) != 2:
                return 8, b""
            opcode = header[0] & 15
            size = header[1] & 127
            if size == 126:
                size = struct.unpack("!H", self.reader.read(2))[0]
            elif size == 127:
                size = struct.unpack("!Q", self.reader.read(8))[0]
            assert size <= 2 * 1024 * 1024, "oversized websocket frame"
            mask = self.reader.read(4) if header[1] & 128 else None
            value = self.reader.read(size)
            assert len(value) == size
            if mask:
                value = bytes(byte ^ mask[index % 4] for index, byte in enumerate(value))
            if opcode == 9:
                self.send(10, value)
                continue
            return opcode, value

    def close(self):
        try:
            self.send(8, struct.pack("!H", 1000))
        except OSError:
            pass
        self.reader.close()
        self.socket.close()


class Processes:
    def __init__(self, directory):
        self.directory = directory
        self.items = []

    def start(self, name, command, env):
        log = (self.directory / (name + ".log")).open("w", encoding="utf-8")
        process = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT,
                                   creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
        self.items.append((name, process, log))
        return process

    @staticmethod
    def stop(process):
        if process.poll() is not None:
            return
        if os.name == "nt":
            # Only this invocation's child and its descendants. Killing the
            # browser fixture's parent alone would orphan its Chromium child.
            try:
                subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"],
                               capture_output=True, timeout=10,
                               creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=15)
            return
        process.terminate()
        try:
            process.wait(timeout=6)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)

    def close(self):
        errors = []
        for name, process, log in reversed(self.items):
            try:
                self.stop(process)
            except (OSError, subprocess.TimeoutExpired) as error:
                errors.append(f"{name}: {type(error).__name__}: {error}")
            finally:
                log.close()
        return errors


def run(args):
    report = {"suite": "av-control-plane", "database": "temporary SQLite", "redis": "external isolated test service",
              "checks": [], "real_sfu": True, "real_rtc_signaling": False, "media_packets": False,
              "device_manual_acceptance": False}
    prefix = "/api/v1/av/"
    api_port, api2_port, media_port = free_port(), free_port(), free_port()
    api, api2, media = (f"http://127.0.0.1:{port}" for port in (api_port, api2_port, media_port))
    rtc_secret, management_secret, admission_secret = (secrets.token_hex(32) for _ in range(3))
    password = secrets.token_urlsafe(24)
    # A transient Windows antivirus/log handle must not replace the original
    # assertion with an unrelated temporary-directory cleanup exception.
    with tempfile.TemporaryDirectory(prefix="av-control-e2e-", ignore_cleanup_errors=True) as temporary:
        temp = Path(temporary)
        processes = Processes(temp)
        media_config = temp / "media.json"
        media_config.write_text(json.dumps({"bind": "127.0.0.1", "port": media_port,
                                           "keys": {"rtc": rtc_secret, "management": management_secret}}), encoding="utf-8")
        env = {key: value for key, value in os.environ.items() if not key.startswith(("PF_", "AV_", "LIVEKIT_", "OTEL_"))}
        env.update({"PF_SQLITE_PATH": str(temp / "business.db"), "PF_MODULES": "organizations,jobs,events",
                    "PF_PUBLIC_ORIGIN": api, "PF_ADDR": f"127.0.0.1:{api_port}", "PF_REGISTRATION": "true",
                    "PF_ADMIN_NAME": "integration_owner", "PF_ADMIN_PASSWORD": password,
                    "AV_ORGANIZATION_NAME": "Integration organization", "AV_REDIS_URL": args.redis_url,
                    "AV_MEDIA_NODES": json.dumps([{"id": "integration-node", "api_url": media,
                        "public_url": media.replace("http:", "ws:"), "rtc_key": "rtc", "rtc_secret": rtc_secret,
                        "management_key": "management", "management_secret": management_secret,
                        "admission_secret": admission_secret, "capacity": 100}])})
        media_env = {**env, "AV_MEDIA_MODE": "true", "AV_NODE_ID": "integration-node", "AV_RTC_KEY": "rtc",
                     "AV_MANAGEMENT_KEY": "management", "AV_ADMISSION_SECRET": admission_secret,
                     "AV_ADMISSION_URL": api + "/internal/v1/media/admissions/check", "AV_ALLOW_INSECURE_ADMISSION": "true"}
        sockets = []
        try:
            for command in ("migrate", "bootstrap"):
                result = subprocess.run([str(args.business), command], env=env, capture_output=True, timeout=30,
                                        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
                assert result.returncode == 0, f"{command} failed: {(result.stdout + result.stderr).decode(errors='replace')}"
            processes.start("media", [str(args.media), "--config", str(media_config)], media_env)
            eventually(lambda: request(media, "/healthz")[0] == 200, "SFU startup")
            api1_process = processes.start("api-1", [str(args.business), "api"], env)
            processes.start("api-2", [str(args.business), "api"], {**env, "PF_ADDR": f"127.0.0.1:{api2_port}"})
            processes.start("worker", [str(args.business), "worker"], env)
            eventually(lambda: request(api, "/readyz")[0] == 200 and request(api2, "/readyz")[0] == 200, "two APIs")

            login = request(api, "/api/v1/auth/login", {"name": "integration_owner", "password": password})
            expect(login)
            cookie = http.cookies.SimpleCookie()
            cookie.load(login[2].get("Set-Cookie", ""))
            token = cookie["pf_session"].value
            organizations = expect(request(api2, "/api/v1/organizations", token=token))["items"]
            organization = organizations[0]["id"]
            user = {"token": token, "organization": organization}
            report["checks"].append("Foundation login cookie, bearer reuse on second API, organization context")
            member_registration = request(api2, "/api/v1/auth/register", {
                "name": "integration_member", "password": secrets.token_urlsafe(24)})
            member_id = expect(member_registration)["user"]["id"]
            member_cookie = http.cookies.SimpleCookie()
            member_cookie.load(member_registration[2].get("Set-Cookie", ""))
            member_token = member_cookie["pf_session"].value
            invitation = expect(request(api, "/api/v1/invitations", {
                "username": "integration_member", "role": "av-member"}, **user))
            expect(request(api2, f"/api/v1/invitations/{invitation['id']}/accept", {}, token=member_token))
            report["checks"].append("Second Foundation account joins organization using av-member invitation")

            def command(path, body=None, actor=None, base=api):
                return expect(request(base, prefix + path, {} if body is None else body, **(actor or user)))

            def credentials(grant, actor=user):
                def attempt():
                    response = request(api2, prefix + f"rtc-grants/{grant}/credentials", {}, **actor)
                    if response[0] == 200:
                        return response[1]
                    if response[0] not in (409, 503):
                        expect(response)
                    return None
                return eventually(attempt, "worker creates room and credentials become ready")

            def signal(credential):
                assert request(media, "/rtc/validate", token=credential["media_token"])[0] == 200
                ws = WebSocket(media, "/rtc?protocol=16&sdk=js&version=2.15.6", credential["media_token"])
                sockets.append(ws)
                for _ in range(8):
                    opcode, payload = ws.read()
                    assert opcode != 8, "SFU closed before join"
                    if opcode == 2 and payload and payload[0] == 10:  # SignalResponse.join field 1
                        report["real_rtc_signaling"] = True
                        return ws
                raise AssertionError("SFU join response missing")

            def room_absent(room):
                admin = jwt("management", management_secret, {"video": {"roomList": True}})
                rooms = expect(request(media, "/twirp/livekit.RoomService/ListRooms", {}, token=admin)).get("rooms", [])
                return not any(item.get("name") == room for item in rooms)

            cast = command("screen-cast/sessions", {"title": "Integration cast"})
            paired = expect(
                request(api2, prefix + "screen-cast/redeem", {"code": cast["pairing_code"]}))
            sender = {"credential": paired["credential"]}
            cast_id = cast["id"]
            connect = command(f"screen-cast/sessions/{cast_id}/connect")
            incoming = command(f"screen-cast/sessions/{cast_id}/connect", actor=sender, base=api2)
            receiver_media, sender_media = credentials(connect["grant_id"]), credentials(incoming["grant_id"], sender)
            signal(receiver_media)
            signal(sender_media)
            assert request(api2, prefix + f"screen-cast/sessions/{cast_id}", token=token, organization="wrong-org")[0] == 403
            assert request(api2, prefix + "screen-cast/redeem", {"code": cast["pairing_code"]})[0] in (404, 409)
            report["checks"].append("Cast pair on API2, single-use code, tenant isolation, worker room creation, real SFU signaling")

            ticket = command("realtime/tickets", {"resource_id": cast_id})["ticket"]
            event_socket = WebSocket(api2, prefix + "realtime/ws?ticket=" + ticket, origin=api)
            sockets.append(event_socket)
            opcode, raw = event_socket.read()
            assert opcode == 1 and json.loads(raw)["resource_id"] == cast_id
            assert request(api2, prefix + "realtime/ws?ticket=" + ticket)[0] == 401
            report["checks"].append("Single-use WebSocket ticket across API replicas and durable event replay")

            processes.stop(api1_process)
            assert expect(request(api2, prefix + f"screen-cast/sessions/{cast_id}", **user))["id"] == cast_id
            processes.start("api-1-restarted", [str(args.business), "api"], env)
            eventually(lambda: request(api, "/readyz")[0] == 200, "API restart")
            assert request(media, "/rtc/validate", token=receiver_media["media_token"])[0] == 200
            report["checks"].append("API restart preserves persisted session, credentials and room binding")

            stopped = command(f"screen-cast/sessions/{cast_id}/stop", base=api2)
            assert stopped["accepted"] and stopped["operation_ids"]
            assert request(media, "/rtc/validate", token=receiver_media["media_token"])[0] == 401
            eventually(lambda: room_absent(receiver_media["room"]), "cast cleanup removes actual SFU room")
            def cleanup_succeeded():
                operations = expect(request(api2, prefix + "operations?resource_id=" + cast_id, **user))["items"]
                relevant = [item for item in operations if item["id"] in stopped["operation_ids"]]
                return (len(relevant) == len(stopped["operation_ids"])
                        and all(item["state"] in ("succeeded", "superseded") for item in relevant)
                        and any(item["action"] == "DeleteRoom" and item["state"] == "succeeded" for item in relevant))
            eventually(cleanup_succeeded, "public operation status confirms durable cleanup")
            assert request(api2, prefix + "operations/" + stopped["operation_ids"][0], token=token, organization="wrong-org")[0] == 403
            with closing(sqlite3.connect(temp / "business.db")) as db:
                assert db.execute("SELECT COUNT(*) FROM av_media_operations WHERE resource_id=? AND action='DeleteRoom' AND state='succeeded'", (cast_id,)).fetchone()[0] == 1
            report["checks"].append("End rejects old media JWT; scoped operation status confirms worker deletion of SFU room")

            assist = command("remote-control/sessions", {"title": "Integration assistance"})
            helper = expect(request(api2, prefix + "remote-control/redeem", {"code": assist["pairing_code"], "name": "Helper"}))
            helper_auth = {"credential": helper["credential"]}
            aid = assist["id"]
            grant = command(f"remote-control/sessions/{aid}/request", actor=helper_auth, base=api2)
            approval = command(f"control-grants/{grant['id']}/approve", {"local_consent": True})
            rtc = command(f"remote-control/sessions/{aid}/connect", {"epoch": approval["epoch"]})
            remote_media = credentials(rtc["grant_id"])
            assert request(media, "/rtc/validate", token=remote_media["media_token"])[0] == 200
            until = time.monotonic() + 10.5
            while time.monotonic() < until:
                command(f"remote-control/sessions/{aid}/heartbeat")
                command(f"remote-control/sessions/{aid}/heartbeat", actor=helper_auth, base=api2)
                time.sleep(min(2, max(0.01, until - time.monotonic())))
            assert request(api2, prefix + f"control-grants/{grant['id']}/renew", {"epoch": approval["epoch"]}, **user)[0] in (403, 409)
            assert request(media, "/rtc/validate", token=remote_media["media_token"])[0] == 401
            def new_control():
                response = request(api2, prefix + f"remote-control/sessions/{aid}/request", {}, **helper_auth)
                return response[1] if response[0] == 200 else None
            new_request = eventually(new_control, "expired control grant transitions before new approval")
            assert new_request["epoch"] > approval["epoch"]
            command(f"control-grants/{new_request['id']}/approve", {"local_consent": True})
            command(f"remote-control/sessions/{aid}/stop")
            report["checks"].append("Remote lease expires despite fresh presence; old RTC grant denied; new local approval advances epoch")

            meeting = command("conference/meetings", {"title": "Integration meeting"})
            mid = meeting["id"]
            attendee = expect(request(api2, prefix + "conference/join", {"number": meeting["number"], "name": "Guest"}))
            attendee_auth = {"credential": attendee["credential"]}
            assert attendee["state"] == "waiting"
            assert request(api2, prefix + f"conference/meetings/{mid}/connect", {}, **attendee_auth)[0] == 403
            command(f"conference/meetings/{mid}/admit", {"target": attendee["subject_id"]})
            participant = command(f"conference/meetings/{mid}/connect", actor=attendee_auth, base=api2)
            meeting_media = credentials(participant["grant_id"], attendee_auth)
            signal(meeting_media)
            command(f"conference/meetings/{mid}/mute", {"target": attendee["subject_id"], "enabled": True})
            assert request(media, "/rtc/validate", token=meeting_media["media_token"])[0] == 401
            updated = command(f"conference/meetings/{mid}/connect", actor=attendee_auth)
            updated_media = credentials(updated["grant_id"], attendee_auth)
            assert "microphone" not in updated_media["granted_capabilities"]["sources"]
            command(f"conference/meetings/{mid}/end")
            report["checks"].append("Meeting waiting room approval, real signaling, policy revision revokes prior publishing token")

            room = command("live-studio/rooms", {"title": "Integration live"})
            live = command(f"live-studio/rooms/{room['id']}/start")
            bid = live["id"]
            viewer = expect(request(api2, prefix + f"live-studio/broadcasts/{bid}/watch", {}))
            viewer_auth = {"credential": viewer["credential"]}
            live_media = credentials(viewer["grant_id"], viewer_auth)
            signal(live_media)
            command(f"live-studio/broadcasts/{bid}/end")
            assert request(media, "/rtc/validate", token=live_media["media_token"])[0] == 401
            eventually(lambda: room_absent(live_media["room"]), "live cleanup")
            report["checks"].append("WebRTC live broadcast program/stage creation, anonymous watch, signaling and end cleanup")

            if args.media_script:
                media_report = temp / "synthetic-media.json"
                media_test = processes.start("synthetic-media", [args.node, str(args.media_script)], {**env, "AV_SMOKE_API": api,
                    "AV_SMOKE_API2": api2, "AV_SMOKE_MEDIA": media.replace("http:", "ws:"), "AV_SMOKE_TOKEN": token,
                    "AV_SMOKE_ORGANIZATION": organization, "AV_SMOKE_MEMBER_TOKEN": member_token,
                    "AV_SMOKE_MEMBER_ID": member_id, "AV_SMOKE_OUTPUT": str(media_report)})
                try:
                    media_test.wait(timeout=240)
                except subprocess.TimeoutExpired:
                    processes.stop(media_test)
                    raise
                assert media_test.returncode == 0, "synthetic media fixture failed: " + (temp / "synthetic-media.log").read_text(encoding="utf-8", errors="replace")[-5000:]
                report["synthetic_media"] = json.loads(media_report.read_text(encoding="utf-8"))
                report["media_packets"] = True
                report["checks"].append("Synthetic WebRTC audio/video packets and decoded frames, verified by headless browser fixture")
            report["passed"] = True
        except BaseException as error:
            report["passed"] = False
            report["error"] = f"{type(error).__name__}: {error}"
            partial_media = temp / "synthetic-media.json"
            if partial_media.exists():
                try:
                    report["synthetic_media"] = json.loads(partial_media.read_text(encoding="utf-8"))
                except (OSError, json.JSONDecodeError):
                    pass
            if args.output:
                args.output.parent.mkdir(parents=True, exist_ok=True)
                args.output.write_text(json.dumps(report, indent=2, ensure_ascii=False), encoding="utf-8")
            # Logs carry request paths but no bodies, cookies, or signing keys.
            for name, process, log in processes.items:
                log.flush()
                print(f"{name}: exit={process.poll()}")
                lines = (temp / (name + ".log")).read_text(encoding="utf-8", errors="replace").splitlines()
                print("\n".join(lines[-12:]))
            raise
        finally:
            for ws in sockets:
                ws.close()
            report["process_cleanup_errors"] = processes.close()
            if report["process_cleanup_errors"]:
                report["passed"] = False
                report.setdefault("error", "owned process cleanup failed")
            if args.output and not report.get("passed", False):
                args.output.parent.mkdir(parents=True, exist_ok=True)
                args.output.write_text(json.dumps(report, indent=2, ensure_ascii=False), encoding="utf-8")
    report["temporary_directory_removed"] = not Path(temporary).exists()
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2, ensure_ascii=False), encoding="utf-8")
    print(json.dumps(report, indent=2, ensure_ascii=False))
    assert report["passed"], report.get("error", "integration failed")


if __name__ == "__main__":
    root = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser()
    parser.add_argument("--business", type=Path, default=root / "out/business-api.exe")
    parser.add_argument("--media", type=Path, default=root.parent / "livekit-cpp/out/windows-x64/Release/livekit-server.exe")
    parser.add_argument("--redis-url", default=os.environ.get("AV_TEST_REDIS_URL", ""))
    parser.add_argument("--media-script", type=Path)
    parser.add_argument("--node", default="node")
    parser.add_argument("--output", type=Path, default=root / "out/control-plane-integration.json")
    options = parser.parse_args()
    if not options.redis_url:
        parser.error("pass --redis-url or AV_TEST_REDIS_URL for an isolated Redis test service")
    options.business, options.media = options.business.resolve(), options.media.resolve()
    if options.media_script:
        options.media_script = options.media_script.resolve()
    run(options)
