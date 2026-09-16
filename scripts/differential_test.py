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
import base64

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "agy-pool"))
GO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
GO_BIN = os.path.join(GO_ROOT, "bin", "agy-pool")

if REPO_ROOT not in sys.path:
    sys.path.insert(0, REPO_ROOT)

os.environ["AGY_TEST_MODE"] = "1"
os.environ["AGY_GEMINI_DIR"] = os.path.join(tempfile.mkdtemp(), ".gemini")

from agy_pool import quota
from agy_pool import scheduler
from agy_pool import auth
from agy_pool import accounts

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
    # 1. JWT Payload decoding
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

    # 2. Error classifiers
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

    # Target matching
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

    # Display name formatting
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

        # 1. Python encrypts -> Go decrypts
        py_bundle = accounts.encrypt_bundle(data_bytes, pw)
        go_res = run_go_cmd("eval-crypto", {
            "action": "decrypt",
            "password": pw,
            "bundle": py_bundle
        })
        assert "error" not in go_res, f"Go failed to decrypt Python bundle: {go_res.get('error')}"
        assert go_res["plaintext"] == data_str, "Go decrypted plaintext mismatch"

        # 2. Go encrypts -> Python decrypts
        go_enc_res = run_go_cmd("eval-crypto", {
            "action": "encrypt",
            "password": pw,
            "data": data_str
        })
        go_bundle = go_enc_res["bundle"]
        py_decrypted = accounts.decrypt_bundle(go_bundle, pw)
        assert py_decrypted.decode("utf-8") == data_str, "Python failed to decrypt Go bundle"

    print("  ✓ Bidirectional crypto differential tests passed with 100% parity.")

def main():
    test_differential_capacity_cases()
    test_differential_scheduler_scenarios()
    test_differential_auth()
    test_differential_accounts()
    test_differential_crypto()
    print("\nALL DIFFERENTIAL TESTS PASSED WITH 100% PARITY!")

if __name__ == "__main__":
    main()
