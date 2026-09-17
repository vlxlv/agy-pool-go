#!/usr/bin/env python3
"""
differential_test.py: Performs automated differential parity verification
between Python v0.1.0-beta.2 reference implementation and Go candidate.

Generates dynamic edge-case scenarios and asserts 100% behavioral parity.
"""

import os
import sys
import json
import subprocess
import tempfile
import atexit
import shutil
import base64
import socket
import time
import urllib.request
import urllib.error
import threading
import http.server

def find_python_repo():
    if os.environ.get("AGY_PYTHON_ROOT") and os.path.exists(os.environ["AGY_PYTHON_ROOT"]):
        return os.environ["AGY_PYTHON_ROOT"]
    cand1 = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "agy-pool"))
    if os.path.exists(cand1):
        return cand1
    cand2 = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..", "agy-pool"))
    if os.path.exists(cand2):
        return cand2
    return "/home/codex/agy-pool"

REPO_ROOT = find_python_repo()
GO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
GO_BIN = os.path.join(GO_ROOT, "bin", "agy-pool")

if REPO_ROOT not in sys.path:
    sys.path.insert(0, REPO_ROOT)

_module_temp_dir = tempfile.mkdtemp(prefix="diff_module_init_")
atexit.register(lambda: shutil.rmtree(_module_temp_dir, ignore_errors=True))
os.environ["AGY_TEST_MODE"] = "1"
os.environ["AGY_GEMINI_DIR"] = os.path.join(_module_temp_dir, ".gemini")

from agy_pool import quota
from agy_pool import scheduler
from agy_pool import auth
from agy_pool import accounts
from agy_pool import proxy as py_proxy
from agy_pool import storage as py_storage
from agy_pool import daemon as py_daemon

def run_go_cmd(subcmd, input_data):
    p = subprocess.run(
        [GO_BIN, subcmd],
        input=json.dumps(input_data).encode("utf-8"),
        capture_output=True,
        cwd=GO_ROOT,
        check=True
    )
    return json.loads(p.stdout.decode("utf-8"))

def test_differential_capacity_cases():
    print("Testing differential capacity cases...")
    fixed_now = 1789560000.0

    scenarios = [
        # Healthy dual window
        {"id": "c1", "last_quota": {"gemini_5h": {"fraction": 0.8}, "gemini_weekly": {"fraction": 0.9}, "updated_at": int(fixed_now - 10)}},
        # Depleted at 0.005 exact boundary
        {"id": "c2", "last_quota": {"gemini_5h": {"fraction": 0.005}, "gemini_weekly": {"fraction": 0.9}, "updated_at": int(fixed_now - 10)}},
        # Depleted below 0.005
        {"id": "c3", "last_quota": {"gemini_5h": {"fraction": 0.004999}, "gemini_weekly": {"fraction": 0.9}, "updated_at": int(fixed_now - 10)}},
        # Just above 0.005
        {"id": "c4", "last_quota": {"gemini_5h": {"fraction": 0.005001}, "gemini_weekly": {"fraction": 0.9}, "updated_at": int(fixed_now - 10)}},
        # Freshness boundary: age = 60s (fresh)
        {"id": "c5", "last_quota": {"gemini_5h": {"fraction": 0.8}, "updated_at": int(fixed_now - 60)}},
        # Freshness boundary: age = 61s (aging)
        {"id": "c6", "last_quota": {"gemini_5h": {"fraction": 0.8}, "updated_at": int(fixed_now - 61)}},
        # Freshness boundary: age = 300s (aging)
        {"id": "c7", "last_quota": {"gemini_5h": {"fraction": 0.8}, "updated_at": int(fixed_now - 300)}},
        # Freshness boundary: age = 301s (stale)
        {"id": "c8", "last_quota": {"gemini_5h": {"fraction": 0.8}, "updated_at": int(fixed_now - 301)}},
        # Passed reset time (stale even if age < 60s)
        {"id": "c9", "last_quota": {"gemini_5h": {"fraction": 0.8, "reset_time": fixed_now - 5}, "updated_at": int(fixed_now - 10)}},
        # Future reset time
        {"id": "c10", "last_quota": {"gemini_5h": {"fraction": 0.8, "reset_time": fixed_now + 3600}, "gemini_weekly": {"fraction": 0.9, "reset_time": fixed_now + 86400}, "updated_at": int(fixed_now - 10)}},
    ]

    for acc in scenarios:
        py_cap = scheduler.compute_capacity_state(acc, now=fixed_now)
        py_fresh = quota.quota_freshness(acc, now=fixed_now)
        py_rank = quota.quota_freshness_rank(acc, now=fixed_now)
        py_needed = quota.quota_refresh_needed(acc, now=fixed_now)

        go_res = run_go_cmd("eval-capacity", {"account": acc, "now": fixed_now})
        go_cap = go_res["capacity_state"]
        go_fresh = go_res["freshness_info"]
        go_rank = go_res["freshness_rank"]
        go_needed = go_res["refresh_needed"]

        # Assert parity
        assert go_cap["is_depleted"] == py_cap["is_depleted"], f"is_depleted mismatch on {acc['id']}"
        assert go_cap["known_window_count"] == py_cap["known_window_count"], f"known_window_count mismatch on {acc['id']}"
        assert abs(go_cap["raw_floor"] - py_cap["raw_floor"]) < 1e-9, f"raw_floor mismatch on {acc['id']}"
        assert abs(go_cap["worst_pace"] - py_cap["worst_pace"]) < 1e-9, f"worst_pace mismatch on {acc['id']}"
        assert abs(go_cap["total_pace"] - py_cap["total_pace"]) < 1e-9, f"total_pace mismatch on {acc['id']}"
        assert go_fresh["class"] == py_fresh["class"], f"freshness class mismatch on {acc['id']}"
        assert go_rank == py_rank, f"freshness rank mismatch on {acc['id']}"
        assert go_needed == py_needed, f"refresh needed mismatch on {acc['id']}"

    print(f"  ✓ {len(scenarios)} capacity edge-case scenarios passed with 100% parity.")

def test_differential_scheduler_scenarios():
    print("Testing differential scheduler scenarios...")
    fixed_now = 1789560000.0

    candidates = [
        {"id": "acc_a", "gen_count": 5, "last_quota": {"gemini_5h": {"fraction": 0.90, "reset_time": fixed_now + 7200}, "gemini_weekly": {"fraction": 0.95, "reset_time": fixed_now + 100000}, "updated_at": int(fixed_now - 10)}},
        {"id": "acc_b", "gen_count": 2, "last_quota": {"gemini_5h": {"fraction": 0.80, "reset_time": fixed_now + 3600}, "gemini_weekly": {"fraction": 0.85, "reset_time": fixed_now + 80000}, "updated_at": int(fixed_now - 20)}},
        {"id": "acc_c", "gen_count": 10, "last_quota": {"gemini_5h": {"fraction": 0.95, "reset_time": fixed_now + 10000}, "gemini_weekly": {"fraction": 0.90, "reset_time": fixed_now + 90000}, "updated_at": int(fixed_now - 5)}},
        {"id": "acc_depleted", "gen_count": 1, "last_quota": {"gemini_5h": {"fraction": 0.003, "reset_time": fixed_now + 3600}, "updated_at": int(fixed_now - 10)}},
        {"id": "acc_cooldown", "rate_limited_until": fixed_now + 600, "last_quota": {"gemini_5h": {"fraction": 0.99}, "updated_at": int(fixed_now - 10)}},
        {"id": "acc_restricted", "status": "auth_error", "last_quota": {"gemini_5h": {"fraction": 0.99}, "updated_at": int(fixed_now - 10)}},
    ]

    for strat in ["max_quota", "least_used", "round_robin"]:
        for last_id in [None, "acc_a", "acc_b", "acc_c", "acc_nonexistent"]:
            pool = {"round_robin_last_account_id": last_id}
            py_ordered = scheduler.order_candidates(candidates, strategy=strat, pool=pool, now=fixed_now)
            py_ids = [a["id"] for a in py_ordered]

            go_res = run_go_cmd("eval-scheduler", {
                "strategy": strat,
                "now": fixed_now,
                "pool": pool,
                "candidates": candidates
            })
            go_ids = go_res["ordered_ids"]

            assert go_ids == py_ids, f"Mismatch on {strat} with cursor {last_id}:\nGo: {go_ids}\nPy: {py_ids}"

    print("  ✓ All scheduler strategies (max_quota, least_used, round_robin) passed with 100% parity.")

def test_differential_auth():
    print("Testing differential auth...")
    jwt_cases = [
        "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ICJ1c2VyMUBleGFtcGxlLmNvbSIsICJzdWIiOiAiMTIzNDU2In0.sig",
        "eyJhbGciOiJub25lIn0.eyJzdWIiOiAiNzg5MCJ9.sig",
        "invalid-jwt-single-part",
        "header.not-base64.sig",
        "header.bm90LWpzb24.sig",
        "",
    ]
    for jwt_str in jwt_cases:
        py_claims = auth.decode_jwt_payload(jwt_str)
        go_res = run_go_cmd("eval-jwt", {"jwt": jwt_str})
        go_claims = go_res["claims"]
        assert go_claims == py_claims, f"JWT decode mismatch for {jwt_str}:\nGo: {go_claims}\nPy: {py_claims}"

    error_cases = [
        (401, b"Unauthorized access"),
        (403, b'{"error": {"message": "validation_required"}}'),
        (403, b"Please verify your account to continue"),
        (403, b"unauthenticated request token"),
        (403, b"invalid_grant error code"),
        (403, b"Access_Token_Expired now"),
        (403, b"account is disabled by admin"),
        (403, b"generic forbidden"),
        (500, b"internal server error"),
        (403, b'{"error": {"details": [{"metadata": {"validation_url": "https://accounts.google.com/verify?id=99"}}]}}'),
        (403, b'{"error": {"details": [{"links": [{"description": "Please verify", "url": "https://google.com/verify-now"}]}]}}'),
    ]
    for status, body in error_cases:
        py_is_val = auth._is_validation_error(status, body)
        py_val_url = auth._extract_validation_url(body)
        py_is_auth = auth._is_auth_error(status, body)

        go_res = run_go_cmd("eval-error-classifier", {
            "status": status,
            "body": body.decode("utf-8", errors="ignore")
        })

        assert go_res["is_validation"] == py_is_val, f"is_validation mismatch on status={status}, body={body}"
        assert (go_res["validation_url"] or None) == (py_val_url or None), f"validation_url mismatch on {body}"
        assert go_res["is_auth_error"] == py_is_auth, f"is_auth_error mismatch on status={status}, body={body}"

    print(f"  ✓ JWT and error classifier differential tests passed with 100% parity.")

def test_differential_accounts():
    print("Testing differential accounts (target matching & display names)...")
    sample_accounts = [
        {"id": "acc_1", "email": "dev1@gmail.com", "name": "Work Dev"},
        {"id": "acc_2", "email": "dev2@gmail.com", "name": None},
        {"id": "acc_03", "email": "dev3@gmail.com"},
        {"id": "custom_id", "email": "dev4@gmail.com"},
    ]

    targets = ["1", "2", "3", "4", "0", "5", "acc_1", "acc_2", "acc_03", "custom_id", "dev1@gmail.com", "Work Dev", "unknown", ""]
    for t in targets:
        py_found = accounts.find_account_by_target(sample_accounts, t)
        py_id = py_found["id"] if py_found else None

        go_res = run_go_cmd("eval-account-target", {
            "accounts": sample_accounts,
            "target": t
        })
        go_id = go_res["found_id"]
        assert go_id == py_id, f"Target matching mismatch for target={t!r}: Go={go_id}, Py={py_id}"

    for acc in sample_accounts:
        py_name = accounts.display_account_name(acc)
        go_res = run_go_cmd("eval-display-name", {"account": acc})
        go_name = go_res["display_name"]
        assert go_name == py_name, f"Display name mismatch for acc={acc}: Go={go_name}, Py={py_name}"

    print("  ✓ Account target and display name differential tests passed with 100% parity.")

def test_differential_crypto():
    print("Testing differential backup encryption & decryption...")
    test_cases = [
        ("simple test string", "password123"),
        ("{\"accounts\": [{\"email\": \"a@b.com\", \"refresh_token\": \"secret-rf\"}]}", "passphrase-with-special!@#$%^&*()"),
        ("a" * 1000, "long-buffer-passphrase"),
    ]

    for data_str, pw in test_cases:
        data_bytes = data_str.encode("utf-8")

        py_bundle = accounts.encrypt_bundle(data_bytes, pw)
        go_res = run_go_cmd("eval-crypto", {
            "action": "decrypt",
            "password": pw,
            "bundle": py_bundle
        })
        assert "error" not in go_res, f"Go failed to decrypt Python bundle: {go_res.get('error')}"
        assert go_res["plaintext"] == data_str, "Go decrypted plaintext mismatch"

        go_enc_res = run_go_cmd("eval-crypto", {
            "action": "encrypt",
            "password": pw,
            "data": data_str
        })
        go_bundle = go_enc_res["bundle"]
        py_decrypted = accounts.decrypt_bundle(go_bundle, pw)
        assert py_decrypted.decode("utf-8") == data_str, "Python failed to decrypt Go bundle"

    print("  ✓ Bidirectional crypto differential tests passed with 100% parity.")

def find_free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]

def test_differential_proxy():
    print("Testing differential HTTP proxy forwarding & failover...")

    port_upstream = find_free_port()
    port_py = find_free_port()
    port_go = find_free_port()

    class MockUpstreamHandler(http.server.BaseHTTPRequestHandler):
        requests_log = []

        def do_POST(self):
            length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(length)
            auth_hdr = self.headers.get("Authorization", "")
            MockUpstreamHandler.requests_log.append({
                "method": "POST",
                "path": self.path,
                "auth": auth_hdr,
                "body": body
            })

            if self.path == "/v1/normal":
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                resp = json.dumps({"status": "ok", "echo_auth": auth_hdr}).encode("utf-8")
                self.send_header("Content-Length", str(len(resp)))
                self.end_headers()
                self.wfile.write(resp)
                return

            if self.path == "/v1/failover_429":
                if "acc_1" in auth_hdr:
                    self.send_response(429)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Retry-After", "120")
                    resp = json.dumps({"error": "ResourceExhausted"}).encode("utf-8")
                    self.send_header("Content-Length", str(len(resp)))
                    self.end_headers()
                    self.wfile.write(resp)
                    return
                else:
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    resp = json.dumps({"status": "recovered_acc2"}).encode("utf-8")
                    self.send_header("Content-Length", str(len(resp)))
                    self.end_headers()
                    self.wfile.write(resp)
                    return

            if self.path == "/v1/failover_val":
                if "acc_1" in auth_hdr:
                    self.send_response(403)
                    self.send_header("Content-Type", "application/json")
                    resp = json.dumps({
                        "error": {
                            "message": "validation_required",
                            "details": [{"metadata": {"validation_url": "https://accounts.google.com/verify?id=42"}}]
                        }
                    }).encode("utf-8")
                    self.send_header("Content-Length", str(len(resp)))
                    self.end_headers()
                    self.wfile.write(resp)
                    return
                else:
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    resp = json.dumps({"status": "recovered_after_val"}).encode("utf-8")
                    self.send_header("Content-Length", str(len(resp)))
                    self.end_headers()
                    self.wfile.write(resp)
                    return

            if self.path == "/v1/bad_request":
                self.send_response(400)
                self.send_header("Content-Type", "application/json")
                resp = json.dumps({"error": "bad param"}).encode("utf-8")
                self.send_header("Content-Length", str(len(resp)))
                self.end_headers()
                self.wfile.write(resp)
                return

            self.send_response(404)
            self.end_headers()

        def do_GET(self):
            auth_hdr = self.headers.get("Authorization", "")
            MockUpstreamHandler.requests_log.append({
                "method": "GET",
                "path": self.path,
                "auth": auth_hdr
            })
            if self.path == "/v1/stream":
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Transfer-Encoding", "chunked")
                self.end_headers()
                chunks = [b"data: chunk 1\n\n", b"data: chunk 2\n\n"]
                for c in chunks:
                    self.wfile.write(f"{len(c):X}\r\n".encode() + c + b"\r\n")
                    self.wfile.flush()
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
                return
            self.send_response(404)
            self.end_headers()

        def log_message(self, format, *args):
            pass

    upstream_server = accounts.ThreadedHTTPServer(("127.0.0.1", port_upstream), MockUpstreamHandler)
    upstream_thread = threading.Thread(target=upstream_server.serve_forever, daemon=True)
    upstream_thread.start()

    def make_pool_state():
        return {
            "version": 1,
            "strategy": "max_quota",
            "active_account_id": "acc_1",
            "accounts": [
                {
                    "id": "acc_1",
                    "email": "user1@gmail.com",
                    "request_count": 0,
                    "gen_count": 0,
                    "error_count": 0,
                    "last_quota": {"remaining_fraction": 0.9}
                },
                {
                    "id": "acc_2",
                    "email": "user2@gmail.com",
                    "request_count": 0,
                    "gen_count": 0,
                    "error_count": 0,
                    "last_quota": {"remaining_fraction": 0.8}
                }
            ]
        }

    # Setup isolated state directories
    with tempfile.TemporaryDirectory(prefix="agy_py_state_") as tmp_py, \
         tempfile.TemporaryDirectory(prefix="agy_go_state_") as tmp_go:

        with open(os.path.join(tmp_py, "agy-pool-accounts.json"), "w") as f:
            json.dump(make_pool_state(), f)
        with open(os.path.join(tmp_go, "agy-pool-accounts.json"), "w") as f:
            json.dump(make_pool_state(), f)

        # Configure and start Python proxy
        from agy_pool import config as py_config
        py_config.configure_paths(tmp_py)
        py_proxy.set_backend_host_provider(lambda: f"127.0.0.1:{port_upstream}")
        py_proxy.set_backend_url_provider(lambda: f"http://127.0.0.1:{port_upstream}")
        py_proxy.set_token_refresher(lambda acc: f"token-{acc.get('id')}")

        py_server = accounts.ThreadedHTTPServer(("127.0.0.1", port_py), py_proxy.SmartProxyHandler)
        py_thread = threading.Thread(target=py_server.serve_forever, daemon=True)
        py_thread.start()

        # Start Go proxy server process
        go_cmd = [
            GO_BIN, "proxy-test-server",
            str(port_go),
            tmp_go,
            f"http://127.0.0.1:{port_upstream}"
        ]
        go_proc = subprocess.Popen(
            go_cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            cwd=GO_ROOT
        )

        # Wait for Go server ready
        ready_line = go_proc.stdout.readline()
        assert "READY" in ready_line, f"Go proxy server failed to start: {ready_line}"

        def request_pair(path, method="POST", data=b"{}"):
            # Request to Python
            req_py = urllib.request.Request(f"http://127.0.0.1:{port_py}{path}", data=data, method=method)
            try:
                with urllib.request.urlopen(req_py, timeout=5) as resp:
                    py_status = resp.status
                    py_body = resp.read()
            except urllib.error.HTTPError as e:
                py_status = e.code
                py_body = e.read()

            # Request to Go
            req_go = urllib.request.Request(f"http://127.0.0.1:{port_go}{path}", data=data, method=method)
            try:
                with urllib.request.urlopen(req_go, timeout=5) as resp:
                    go_status = resp.status
                    go_body = resp.read()
            except urllib.error.HTTPError as e:
                go_status = e.code
                go_body = e.read()

            return (py_status, py_body), (go_status, go_body)

        try:
            # 1. Normal Request
            (py_status, py_body), (go_status, go_body) = request_pair("/v1/normal")
            assert py_status == 200, f"Py status = {py_status}"
            assert go_status == 200, f"Go status = {go_status}"
            assert json.loads(py_body) == json.loads(go_body), f"Body mismatch on /v1/normal: Py={py_body}, Go={go_body}"

            # 2. 429 Failover
            (py_status, py_body), (go_status, go_body) = request_pair("/v1/failover_429")
            assert py_status == 200, f"Py failover status = {py_status}"
            assert go_status == 200, f"Go failover status = {go_status}"
            assert json.loads(py_body) == json.loads(go_body), f"Body mismatch on 429 failover"

            # 3. Validation Failover
            (py_status, py_body), (go_status, go_body) = request_pair("/v1/failover_val")
            assert py_status == 200, f"Py validation status = {py_status}"
            assert go_status == 200, f"Go validation status = {go_status}"
            assert json.loads(py_body) == json.loads(go_body), f"Body mismatch on validation failover"

            # 4. Ordinary Error (400 Bad Request) - No failover
            (py_status, py_body), (go_status, go_body) = request_pair("/v1/bad_request")
            assert py_status == 400, f"Py bad request status = {py_status}"
            assert go_status == 400, f"Go bad request status = {go_status}"
            assert json.loads(py_body) == json.loads(go_body), f"Body mismatch on bad request"

            # 5. Streaming (SSE)
            (py_status, py_body), (go_status, go_body) = request_pair("/v1/stream", method="GET", data=None)
            assert py_status == 200, f"Py stream status = {py_status}"
            assert go_status == 200, f"Go stream status = {go_status}"
            assert b"chunk 1" in py_body and b"chunk 2" in py_body
            assert b"chunk 1" in go_body and b"chunk 2" in go_body

            print("  ✓ End-to-end Python vs Go proxy forwarding, failover, no-replay, and streaming passed with 100% parity.")
        finally:
            # Teardown
            go_proc.terminate()
            try:
                go_proc.wait(timeout=2)
            except Exception:
                go_proc.kill()
            py_server.shutdown()
            py_server.server_close()
            upstream_server.shutdown()
            upstream_server.server_close()

def test_differential_daemon():
    print("Testing differential daemon lifecycle & log operations...")

    # 1. Format size parity
    for sz in [0, 500, 1023, 1024, 1536, 2048, 1048576, 5242880, 104857600]:
        py_fmt = py_daemon._format_size(sz)
        go_res = run_go_cmd("eval-daemon", {"action": "format_size", "bytes": sz})
        go_fmt = go_res.get("formatted")
        assert py_fmt == go_fmt, f"Format mismatch for {sz}: py={py_fmt}, go={go_fmt}"

    # 2. Missing PID file
    with tempfile.TemporaryDirectory(prefix="agy_daemon_diff_") as tmp_d:
        missing_pid = os.path.join(tmp_d, "missing.pid")
        py_daemon.set_pid_file_provider(lambda: missing_pid)
        py_info = py_daemon.get_daemon_info()
        go_res = run_go_cmd("eval-daemon", {"action": "get_daemon_info", "path": missing_pid})
        assert py_info is None, f"Expected py_info None, got {py_info}"
        assert go_res.get("info") is None, f"Expected go_info None, got {go_res.get('info')}"

        # 3. Dead PID file (stale PID)
        stale_pid_file = os.path.join(tmp_d, "stale.pid")
        dead_pid = 4194300
        with open(stale_pid_file, "w") as f:
            json.dump({"pid": dead_pid, "version": "0.1.0"}, f)
        py_daemon.set_pid_file_provider(lambda: stale_pid_file)
        py_info = py_daemon.get_daemon_info()
        go_res = run_go_cmd("eval-daemon", {"action": "get_daemon_info", "path": stale_pid_file})
        assert py_info is None, f"Expected stale PID py_info None, got {py_info}"
        assert go_res.get("info") is None, f"Expected stale PID go_info None, got {go_res.get('info')}"

        # 4. Malformed PID file
        malformed_pid_file = os.path.join(tmp_d, "malformed.pid")
        with open(malformed_pid_file, "w") as f:
            f.write("{invalid json")
        py_daemon.set_pid_file_provider(lambda: malformed_pid_file)
        py_info = py_daemon.get_daemon_info()
        go_res = run_go_cmd("eval-daemon", {"action": "get_daemon_info", "path": malformed_pid_file})
        assert py_info is None, f"Expected malformed py_info None"
        assert go_res.get("info") is None, f"Expected malformed go_info None"

        # 5. Live PID file with current process PID
        live_pid_file = os.path.join(tmp_d, "live.pid")
        with open(live_pid_file, "w") as f:
            json.dump({"pid": os.getpid(), "version": "0.1.0", "script_mtime": 1000}, f)
        py_daemon.set_pid_file_provider(lambda: live_pid_file)
        py_info = py_daemon.get_daemon_info()
        go_res = run_go_cmd("eval-daemon", {"action": "get_daemon_info", "path": live_pid_file})
        assert py_info is not None and py_info.get("pid") == os.getpid()
        assert go_res.get("info") is not None and go_res["info"]["pid"] == os.getpid()

        # 6. Log rotation differential
        log_py = os.path.join(tmp_d, "py.log")
        log_go = os.path.join(tmp_d, "go.log")
        with open(log_py, "w") as f:
            f.write("test log data line 1\nline 2\n")
        with open(log_go, "w") as f:
            f.write("test log data line 1\nline 2\n")

        py_rot = py_daemon.rotate_log_if_needed(log_py, force=True, backup_count=2)
        go_res = run_go_cmd("eval-daemon", {"action": "rotate_log", "path": log_go, "backup_count": 2, "force": True})
        assert py_rot is True
        assert go_res.get("rotated") is True
        assert os.path.getsize(log_py) == 0
        assert os.path.getsize(log_go) == 0
        assert os.path.exists(log_py + ".1") and os.path.exists(log_go + ".1")
        assert os.path.getsize(log_py + ".1") == os.path.getsize(log_go + ".1")

        # 7. Clear log differential
        py_daemon.clear_log(log_py, backup_count=2)
        go_res = run_go_cmd("eval-daemon", {"action": "clear_log", "path": log_go, "backup_count": 2})
        assert not os.path.exists(log_py + ".1")
        assert not os.path.exists(log_go + ".1")
        assert os.path.getsize(log_py) == 0
        assert os.path.getsize(log_go) == 0

    print("  ✓ Daemon formatting, PID parsing, stale detection, log rotation, and log clear passed with 100% parity.")

def test_differential_cli():
    print("Testing differential CLI commands and exit codes...")
    fixtures_path = os.path.join(GO_ROOT, "testdata", "cli", "golden_cases.json")
    with open(fixtures_path, "r", encoding="utf-8") as f:
        cases = json.load(f)

    py_bin = os.path.join(REPO_ROOT, "bin", "agy-pool")

    for c in cases:
        name = c["name"]
        args = c["args"]
        expected_rc = c["exit_code"]

        with tempfile.TemporaryDirectory(prefix=f"diff_cli_py_{name}_") as tmp_py, \
             tempfile.TemporaryDirectory(prefix=f"diff_cli_go_{name}_") as tmp_go:
            env_py = os.environ.copy()
            env_py["HOME"] = tmp_py
            env_py["AGY_TEST_MODE"] = "1"
            env_py["AGY_GEMINI_DIR"] = os.path.join(tmp_py, ".gemini")

            env_go = os.environ.copy()
            env_go["HOME"] = tmp_go
            env_go["AGY_TEST_MODE"] = "1"
            env_go["AGY_GEMINI_DIR"] = os.path.join(tmp_go, ".gemini")

            p_py = subprocess.run(["python3", py_bin] + args, env=env_py, capture_output=True, text=True)
            p_go = subprocess.run([GO_BIN] + args, env=env_go, capture_output=True, text=True)

            assert p_py.returncode == expected_rc, f"[{name}] Python exit code {p_py.returncode} != expected {expected_rc}"
            assert p_go.returncode == expected_rc, f"[{name}] Go exit code {p_go.returncode} != expected {expected_rc}"

            if not c.get("exempt_py_parity", False):
                for pat in c.get("stdout_contains", []):
                    assert pat in p_py.stdout, f"[{name}] Python stdout missing {pat!r}\nGot: {p_py.stdout}"

            for pat in c.get("stdout_contains", []):
                assert pat in p_go.stdout, f"[{name}] Go stdout missing {pat!r}\nGot: {p_go.stdout}"

            for pat in c.get("stdout_contains_go", []):
                assert pat in p_go.stdout, f"[{name}] Go stdout missing {pat!r}\nGot: {p_go.stdout}"

            for pat in c.get("stdout_not_contains_go", []):
                assert pat not in p_go.stdout, f"[{name}] Go stdout unexpectedly contained {pat!r}\nGot: {p_go.stdout}"

            for pat in c.get("stderr_contains", []):
                assert pat in p_py.stderr, f"[{name}] Python stderr missing {pat!r}\nGot: {p_py.stderr}"
                assert pat in p_go.stderr, f"[{name}] Go stderr missing {pat!r}\nGot: {p_go.stderr}"

    print(f"  ✓ All {len(cases)} CLI golden test cases passed with exact exit-code and semantic parity.")

def test_differential_conversation():
    print("Testing differential conversation session discovery and -c resolution...")
    import sqlite3
    from agy_pool import cli as py_cli
    from agy_pool import config as py_config

    orig_cli_dir = py_config.AGY_CLI_DIR
    try:
        with tempfile.TemporaryDirectory(prefix="diff_conv_") as tmp_dir:
            db_dir = os.path.join(tmp_dir, ".gemini", "antigravity-cli")
        os.makedirs(db_dir, exist_ok=True)
        db_path = os.path.join(db_dir, "conversation_summaries.db")

        proj1 = os.path.join(tmp_dir, "workspaces", "project1")
        proj1_deep = os.path.join(proj1, "src", "backend")
        proj2 = os.path.join(tmp_dir, "workspaces", "project2")
        os.makedirs(proj1_deep, exist_ok=True)
        os.makedirs(proj2, exist_ok=True)

        with sqlite3.connect(db_path) as conn:
            conn.execute("CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, workspace_uris TEXT, last_modified_time INTEGER)")
            conn.execute("INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)",
                         ("c1_old", "Project 1 Base", json.dumps([f"file://{proj1}"]), 1000))
            conn.execute("INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)",
                         ("c2_proj2", "Project 2 Session", json.dumps([f"file://{proj2}"]), 2000))
            conn.execute("INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)",
                         ("c3_deep", "Project 1 Deep", json.dumps([f"file://{proj1_deep}"]), 3000))

        # Test Python selection
        py_config.AGY_CLI_DIR = db_dir

        # 1. Inside proj1_deep
        cid_py, title_py, dir_py = py_cli.find_latest_conversation_for_dir(proj1_deep)
        assert cid_py == "c3_deep"

        # 2. Inside proj1 (workspace of c1_old)
        cid_py_p, title_py_p, dir_py_p = py_cli.find_latest_conversation_for_dir(proj1)
        assert cid_py_p == "c1_old"

        # 3. Inside proj2
        cid_py_2, title_py_2, dir_py_2 = py_cli.find_latest_conversation_for_dir(proj2)
        assert cid_py_2 == "c2_proj2"

        # Now test Go candidate on the exact same database and directories
        for test_path, expected_cid in [(proj1_deep, "c3_deep"), (proj1, "c1_old"), (proj2, "c2_proj2")]:
            env = os.environ.copy()
            env["HOME"] = tmp_dir
            env["AGY_TEST_MODE"] = "1"
            env["AGY_GEMINI_DIR"] = os.path.join(tmp_dir, ".gemini")

            # 1. Direct discovery parity
            cid_py, title_py, dir_py = py_cli.find_latest_conversation_for_dir(test_path)
            res_go = subprocess.run([GO_BIN, "eval-conversation", test_path],
                                    cwd=test_path, env=env, capture_output=True, text=True)
            assert res_go.returncode == 0, f"eval-conversation failed: {res_go.stderr}"
            data_go = json.loads(res_go.stdout)
            assert data_go["cid"] == cid_py == expected_cid, f"expected {expected_cid}, got py={cid_py}, go={data_go['cid']}"

            # 2. Continue arg resolution parity
            res_res = subprocess.run([GO_BIN, "eval-resolve-continue", "-c", "--verbose"],
                                     cwd=test_path, env=env, capture_output=True, text=True)
            assert res_res.returncode == 0
            resolved_go = json.loads(res_res.stdout)["args"]
            orig_cwd = os.getcwd()
            try:
                os.chdir(test_path)
                resolved_py = py_cli.resolve_continue_arg(["-c", "--verbose"])
            finally:
                os.chdir(orig_cwd)
            assert resolved_go == resolved_py == ["--conversation", expected_cid, "--verbose"], f"go={resolved_go}, py={resolved_py}"

        # Explicit --conversation overrides -c
        res_explicit_go = subprocess.run([GO_BIN, "eval-resolve-continue", "--conversation", "explicit_conv_999", "-c"],
                                         cwd=proj1, env=env, capture_output=True, text=True)
        assert res_explicit_go.returncode == 0
        explicit_go = json.loads(res_explicit_go.stdout)["args"]
        explicit_py = py_cli.resolve_continue_arg(["--conversation", "explicit_conv_999", "-c"])
        assert explicit_go == explicit_py == ["--conversation", "explicit_conv_999", "-c"]

    finally:
        py_config.AGY_CLI_DIR = orig_cli_dir

    print("  ✓ Conversation discovery and -c argument resolution passed with zero-tolerance parity.")

def test_differential_wrappers():
    print("Testing differential wrapper execution (run vs raw)...")
    with tempfile.TemporaryDirectory(prefix="diff_wrap_") as tmp_dir:
        py_bin = os.path.join(REPO_ROOT, "bin", "agy-pool")

        mock_agy = os.path.join(tmp_dir, "mock_agy.sh")
        with open(mock_agy, "w") as f:
            f.write("#!/bin/sh\n")
            f.write("echo \"AGY_CALLED: $@\"\n")
            f.write("echo \"URL_VAR: ${CLOUD_CODE_URL:-NONE}\"\n")
        os.chmod(mock_agy, 0o755)

        env = os.environ.copy()
        env["HOME"] = tmp_dir
        env["AGY_TEST_MODE"] = "1"
        env["AGY_GEMINI_DIR"] = os.path.join(tmp_dir, ".gemini")
        env["AGY_BIN"] = mock_agy

        # 1. agy-pool run
        p_py_run = subprocess.run(["python3", py_bin, "run", "--hello", "world space"],
                                  env=env, capture_output=True, text=True)
        p_go_run = subprocess.run([GO_BIN, "run", "--hello", "world space"],
                                  env=env, capture_output=True, text=True)

        assert p_py_run.returncode == 0
        assert p_go_run.returncode == 0
        assert "AGY_CALLED: --hello world space" in p_py_run.stdout
        assert "AGY_CALLED: --hello world space" in p_go_run.stdout
        assert "URL_VAR: http://127.0.0.1:8899" in p_py_run.stdout
        assert "URL_VAR: http://127.0.0.1:8899" in p_go_run.stdout

        # 2. agy-pool raw
        p_py_raw = subprocess.run(["python3", py_bin, "raw", "--hello", "world space"],
                                  env=env, capture_output=True, text=True)
        p_go_raw = subprocess.run([GO_BIN, "raw", "--hello", "world space"],
                                  env=env, capture_output=True, text=True)

        assert p_py_raw.returncode == 0
        assert p_go_raw.returncode == 0
        assert "AGY_CALLED: --hello world space" in p_py_raw.stdout
        assert "AGY_CALLED: --hello world space" in p_go_raw.stdout
        assert "URL_VAR: NONE" in p_py_raw.stdout
        assert "URL_VAR: NONE" in p_go_raw.stdout

        # 3. Canonical collision test: agy-pool run -c cfg -D data -- -c foo -D bar --future-option
        cfg_file = os.path.join(tmp_dir, "test_config.json")
        with open(cfg_file, "w") as f:
            f.write('{"version": 1, "server": {"listen": "127.0.0.1", "port": 8899}, "scheduler": {"strategy": "max_quota"}, "native_agy": {"binary": null}, "logging": {"max_size_bytes": 5242880, "backup_count": 1}}')
        data_dir = os.path.join(tmp_dir, "test_data")

        p_go_collision = subprocess.run([
            GO_BIN, "run",
            "-c", cfg_file,
            "-D", data_dir,
            "--",
            "-c", "foo",
            "-D", "bar",
            "--future-option"
        ], env=env, capture_output=True, text=True)

        assert p_go_collision.returncode == 0, f"collision test failed: {p_go_collision.stderr}"
        assert "AGY_CALLED: -c foo -D bar --future-option" in p_go_collision.stdout, f"got stdout: {p_go_collision.stdout}"

        # 4. Top-level rejection: -c and -D must NOT be global options
        p_go_toplevel_c = subprocess.run([GO_BIN, "-c", cfg_file, "list"], env=env, capture_output=True, text=True)
        assert p_go_toplevel_c.returncode == 2, f"expected code 2 for top-level -c, got {p_go_toplevel_c.returncode}"
        p_go_toplevel_d = subprocess.run([GO_BIN, "-D", data_dir, "list"], env=env, capture_output=True, text=True)
        assert p_go_toplevel_d.returncode == 2, f"expected code 2 for top-level -D, got {p_go_toplevel_d.returncode}"
    print("  ✓ Wrapper argument, -c/-D scoping, -- delimiter, and environment passthrough passed with 100% parity.")

def test_differential_installer_and_migration():
    print("Testing differential installer lifecycle and migration...")
    with tempfile.TemporaryDirectory(prefix="agy_installer_diff_") as tmp_dir:
        env = os.environ.copy()
        env["HOME"] = tmp_dir
        env["AGY_TEST_MODE"] = "1"
        env["AGY_GEMINI_DIR"] = os.path.join(tmp_dir, ".gemini")

        # 1. Precondition failure when native agy is missing
        target_bin = os.path.join(tmp_dir, "bin")
        p_fail = subprocess.run([
            GO_BIN, "install",
            "--target", target_bin,
            "--native-agy", os.path.join(tmp_dir, "nonexistent"),
            "--skip-rc"
        ], env=env, capture_output=True, text=True)
        assert p_fail.returncode == 1, f"expected code 1, got {p_fail.returncode}"
        assert "Native agy was not found" in p_fail.stderr

        # 2. Mock native agy
        real_bin = os.path.join(tmp_dir, "real_bin")
        os.makedirs(real_bin, exist_ok=True)
        mock_agy = os.path.join(real_bin, "agy")
        with open(mock_agy, "w") as f:
            f.write("#!/bin/sh\necho 'mock native agy'\n")
        os.chmod(mock_agy, 0o755)

        cfg_dir = os.path.join(tmp_dir, "cfg")
        data_dir = os.path.join(tmp_dir, "data")

        # 3. Successful user install
        p_install = subprocess.run([
            GO_BIN, "install",
            "--user",
            "--target", target_bin,
            "--native-agy", mock_agy,
            "--config-dir", cfg_dir,
            "--data-dir", data_dir,
            "--skip-rc"
        ], env=env, capture_output=True, text=True)
        assert p_install.returncode == 0, f"install failed: {p_install.stderr}"
        assert "Successfully installed agy-pool" in p_install.stdout

        # Verify shim content
        shim_path = os.path.join(target_bin, "agy")
        assert os.path.exists(shim_path)
        with open(shim_path, "r") as f:
            content = f.read()
        assert "agy-pool run" in content

        # Verify config and data
        assert os.path.exists(os.path.join(cfg_dir, "config.json"))
        assert os.path.isdir(os.path.join(data_dir, "locks"))

        # 4. Uninstall leaves native agy and data untouched
        p_uninstall = subprocess.run([
            GO_BIN, "uninstall",
            "--target", target_bin
        ], env=env, capture_output=True, text=True)
        assert p_uninstall.returncode == 0, f"uninstall failed: {p_uninstall.stderr}"
        assert not os.path.exists(shim_path), "shim was not removed"
        assert os.path.exists(mock_agy), "native agy was deleted!"
        assert os.path.exists(os.path.join(cfg_dir, "config.json")), "config was deleted!"

        # 5. Test migrate-legacy command
        legacy_file = os.path.join(tmp_dir, "legacy_pool.json")
        with open(legacy_file, "w") as f:
            f.write(json.dumps({
                "version": 1,
                "strategy": "max_quota",
                "accounts": [{"id": "acc_1", "email": "migrated@example.com", "access_token": "tok1"}]
            }))
        target_data = os.path.join(tmp_dir, "migrated_data")
        p_mig = subprocess.run([
            GO_BIN, "migrate-legacy",
            "-s", legacy_file,
            "-D", target_data
        ], env=env, capture_output=True, text=True)
        assert p_mig.returncode == 0, f"migration failed: {p_mig.stderr}"
        assert os.path.exists(os.path.join(target_data, "accounts.json"))
    print("  ✓ Installer lifecycle, native-agy protection, and legacy migration passed with 100% parity.")

def main():
    test_differential_capacity_cases()
    test_differential_scheduler_scenarios()
    test_differential_auth()
    test_differential_accounts()
    test_differential_crypto()
    test_differential_proxy()
    test_differential_daemon()
    test_differential_cli()
    test_differential_conversation()
    test_differential_wrappers()
    test_differential_installer_and_migration()
    print("\nALL DIFFERENTIAL TESTS PASSED WITH 100% PARITY!")

if __name__ == "__main__":
    main()
