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
import math

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "agy-pool"))
GO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
GO_BIN = os.path.join(GO_ROOT, "bin", "agy-pool")

if REPO_ROOT not in sys.path:
    sys.path.insert(0, REPO_ROOT)

os.environ["AGY_TEST_MODE"] = "1"
os.environ["AGY_GEMINI_DIR"] = os.path.join(tempfile.mkdtemp(), ".gemini")

from agy_pool import quota
from agy_pool import scheduler

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

def main():
    test_differential_capacity_cases()
    test_differential_scheduler_scenarios()
    print("ALL DIFFERENTIAL TESTS PASSED!")

if __name__ == "__main__":
    main()
