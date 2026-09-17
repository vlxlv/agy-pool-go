#!/usr/bin/env python3
"""
generate_fixtures.py: Generates golden testdata fixtures directly from
authoritative Python v0.1.0-beta.2 reference implementation.

Strictly isolated: does not access ~/.gemini, does not call Google API.
"""

import os
import sys
import json
import tempfile
import atexit
import shutil

# Ensure Python modules from reference repository are loadable
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
if REPO_ROOT not in sys.path:
    sys.path.insert(0, REPO_ROOT)

# Run under isolated test mode
os.environ["AGY_TEST_MODE"] = "1"
temp_dir = tempfile.mkdtemp(prefix="agy_fixture_gen_")
atexit.register(lambda: shutil.rmtree(temp_dir, ignore_errors=True))
os.environ["AGY_GEMINI_DIR"] = os.path.join(temp_dir, ".gemini")

from agy_pool import config
from agy_pool import quota
from agy_pool import scheduler
from agy_pool import storage

TESTDATA_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "testdata"))

def write_json(rel_path, data):
    full_path = os.path.join(TESTDATA_DIR, rel_path)
    os.makedirs(os.path.dirname(full_path), exist_ok=True)
    with open(full_path, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)
        f.write("\n")
    print(f"Generated: {rel_path}")

def main():
    fixed_now = 1789560000.0  # Reference epoch timestamp

    # 1. Pool fixtures
    empty_pool = {
        "version": 1,
        "strategy": "max_quota",
        "active_account_id": None,
        "accounts": []
    }
    write_json("pool/empty.json", empty_pool)

    valid_3_accounts = {
        "version": 1,
        "strategy": "max_quota",
        "active_account_id": "acc_1",
        "round_robin_last_account_id": "acc_2",
        "accounts": [
            {
                "id": "acc_1",
                "name": "Alpha Work",
                "email": "user1@example.com",
                "access_token": "token-1",
                "refresh_token": "refresh-1",
                "id_token": "idtoken-1",
                "token_expiry": fixed_now + 3600.0,
                "updated_at": int(fixed_now - 30),
                "status": "ready",
                "rate_limited_until": 0.0,
                "request_count": 100,
                "gen_count": 30,
                "error_count": 0,
                "last_used_at": int(fixed_now - 100),
                "last_quota": {
                    "gemini_5h": {"fraction": 0.80, "reset_time": "2026-09-16T15:00:00Z"},
                    "gemini_weekly": {"fraction": 0.90, "reset_time": "2026-09-23T04:00:00Z"},
                    "remaining_fraction": 0.80,
                    "reset_time": "2026-09-16T15:00:00Z",
                    "updated_at": int(fixed_now - 30)
                }
            },
            {
                "id": "acc_2",
                "name": "Beta Personal",
                "email": "user2@example.com",
                "access_token": "token-2",
                "refresh_token": "refresh-2",
                "id_token": "idtoken-2",
                "token_expiry": fixed_now + 3600.0,
                "updated_at": int(fixed_now - 10),
                "status": "ready",
                "rate_limited_until": 0.0,
                "request_count": 50,
                "gen_count": 15,
                "error_count": 0,
                "last_used_at": int(fixed_now - 200),
                "last_quota": {
                    "gemini_5h": {"fraction": 0.95, "reset_time": "2026-09-16T16:00:00Z"},
                    "gemini_weekly": {"fraction": 0.85, "reset_time": "2026-09-23T05:00:00Z"},
                    "remaining_fraction": 0.85,
                    "reset_time": "2026-09-23T05:00:00Z",
                    "updated_at": int(fixed_now - 10)
                }
            },
            {
                "id": "acc_3",
                "name": "Gamma Backup",
                "email": "user3@example.com",
                "access_token": "token-3",
                "refresh_token": "refresh-3",
                "id_token": "idtoken-3",
                "token_expiry": fixed_now + 3600.0,
                "updated_at": int(fixed_now - 50),
                "status": "ready",
                "rate_limited_until": 0.0,
                "request_count": 10,
                "gen_count": 2,
                "error_count": 0,
                "last_used_at": int(fixed_now - 500),
                "last_quota": {
                    "gemini_5h": {"fraction": 0.40, "reset_time": "2026-09-16T14:30:00Z"},
                    "gemini_weekly": {"fraction": 0.70, "reset_time": "2026-09-23T06:00:00Z"},
                    "remaining_fraction": 0.40,
                    "reset_time": "2026-09-16T14:30:00Z",
                    "updated_at": int(fixed_now - 50)
                }
            }
        ]
    }
    write_json("pool/valid_3_accounts.json", valid_3_accounts)

    legacy_alpha9 = {
        "version": 1,
        "strategy": "max_quota",
        "active_account_id": "acc_legacy",
        "accounts": [
            {
                "id": "acc_legacy",
                "name": "Legacy Alpha Account",
                "email": "legacy@example.com",
                "access_token": "tok-legacy",
                "refresh_token": "ref-legacy",
                "token_expiry": fixed_now + 3600.0,
                "updated_at": int(fixed_now - 120),
                "status": "ready",
                "request_count": 25,
                "gen_count": 8,
                "error_count": 0,
                "last_quota": {
                    "remaining_fraction": 0.65,
                    "reset_time": "2026-09-16T15:30:00Z",
                    "updated_at": int(fixed_now - 120)
                }
            }
        ]
    }
    write_json("pool/legacy_alpha9.json", legacy_alpha9)

    with open(os.path.join(TESTDATA_DIR, "pool/corrupt.json"), "w") as f:
        f.write('{"version": 1, "strategy": "max_quota", "accounts": [ {"id": "acc_incomplete"')
    print("Generated: pool/corrupt.json")

    # 2. Quota Capacity State Fixtures
    test_cases_quota = [
        {
            "id": "dual_window_healthy",
            "account": {
                "id": "acc_dual",
                "last_quota": {
                    "gemini_5h": {"fraction": 0.75, "reset_time": "2026-09-16T16:00:00Z"},
                    "gemini_weekly": {"fraction": 0.90, "reset_time": "2026-09-23T12:00:00Z"},
                    "updated_at": int(fixed_now - 20)
                }
            }
        },
        {
            "id": "single_window_5h",
            "account": {
                "id": "acc_5h_only",
                "last_quota": {
                    "gemini_5h": {"fraction": 0.50, "reset_time": "2026-09-16T15:30:00Z"},
                    "updated_at": int(fixed_now - 40)
                }
            }
        },
        {
            "id": "depleted_below_threshold",
            "account": {
                "id": "acc_depleted",
                "last_quota": {
                    "gemini_5h": {"fraction": 0.004, "reset_time": "2026-09-16T15:00:00Z"},
                    "gemini_weekly": {"fraction": 0.50, "reset_time": "2026-09-23T12:00:00Z"},
                    "updated_at": int(fixed_now - 10)
                }
            }
        },
        {
            "id": "depleted_boundary_exact",
            "account": {
                "id": "acc_boundary",
                "last_quota": {
                    "gemini_5h": {"fraction": 0.005, "reset_time": "2026-09-16T15:00:00Z"},
                    "gemini_weekly": {"fraction": 0.50, "reset_time": "2026-09-23T12:00:00Z"},
                    "updated_at": int(fixed_now - 10)
                }
            }
        },
        {
            "id": "expired_reset_time",
            "account": {
                "id": "acc_expired_reset",
                "last_quota": {
                    "gemini_5h": {"fraction": 0.80, "reset_time": "2026-09-16T10:00:00Z"},
                    "gemini_weekly": {"fraction": 0.90, "reset_time": "2026-09-23T12:00:00Z"},
                    "updated_at": int(fixed_now - 10)
                }
            }
        },
        {
            "id": "legacy_quota_state",
            "account": {
                "id": "acc_legacy",
                "last_quota": {
                    "remaining_fraction": 0.60,
                    "reset_time": "2026-09-16T16:00:00Z",
                    "updated_at": int(fixed_now - 50)
                }
            }
        }
    ]

    quota_results = []
    for tc in test_cases_quota:
        acc = tc["account"]
        cap = scheduler.compute_capacity_state(acc, now=fixed_now)
        fresh = quota.quota_freshness(acc, now=fixed_now)
        rank = quota.quota_freshness_rank(acc, now=fixed_now)
        needed = quota.quota_refresh_needed(acc, now=fixed_now)
        quota_results.append({
            "test_id": tc["id"],
            "account": acc,
            "now": fixed_now,
            "expected_capacity": cap,
            "expected_freshness": fresh,
            "expected_rank": rank,
            "expected_refresh_needed": needed
        })
    write_json("quota/capacity_cases.json", quota_results)

    # 3. Scheduler Ranking Fixtures
    scheduler_scenarios = [
        {
            "id": "all_healthy_max_quota",
            "strategy": "max_quota",
            "pool": valid_3_accounts,
            "candidates": valid_3_accounts["accounts"],
            "now": fixed_now
        },
        {
            "id": "all_healthy_least_used",
            "strategy": "least_used",
            "pool": valid_3_accounts,
            "candidates": valid_3_accounts["accounts"],
            "now": fixed_now
        },
        {
            "id": "round_robin_with_cursor",
            "strategy": "round_robin",
            "pool": valid_3_accounts,
            "candidates": valid_3_accounts["accounts"],
            "now": fixed_now
        },
        {
            "id": "round_robin_no_cursor",
            "strategy": "round_robin",
            "pool": {**valid_3_accounts, "round_robin_last_account_id": None},
            "candidates": valid_3_accounts["accounts"],
            "now": fixed_now
        },
        {
            "id": "round_robin_stale_cursor",
            "strategy": "round_robin",
            "pool": {**valid_3_accounts, "round_robin_last_account_id": "acc_deleted"},
            "candidates": valid_3_accounts["accounts"],
            "now": fixed_now
        },
        {
            "id": "multi_tier_fallback",
            "strategy": "max_quota",
            "pool": {},
            "candidates": [
                {
                    "id": "acc_restricted",
                    "status": "validation_required",
                    "last_quota": {"gemini_5h": {"fraction": 0.99, "reset_time": "2026-09-16T16:00:00Z"}, "updated_at": int(fixed_now - 10)}
                },
                {
                    "id": "acc_cooldown",
                    "rate_limited_until": fixed_now + 300,
                    "last_quota": {"gemini_5h": {"fraction": 0.95, "reset_time": "2026-09-16T16:00:00Z"}, "updated_at": int(fixed_now - 10)}
                },
                {
                    "id": "acc_depleted",
                    "last_quota": {"gemini_5h": {"fraction": 0.002, "reset_time": "2026-09-16T16:00:00Z"}, "updated_at": int(fixed_now - 10)}
                },
                {
                    "id": "acc_eligible_low",
                    "gen_count": 20,
                    "last_quota": {"gemini_5h": {"fraction": 0.30, "reset_time": "2026-09-16T16:00:00Z"}, "updated_at": int(fixed_now - 10)}
                },
                {
                    "id": "acc_eligible_high",
                    "gen_count": 5,
                    "last_quota": {"gemini_5h": {"fraction": 0.85, "reset_time": "2026-09-16T16:00:00Z"}, "updated_at": int(fixed_now - 10)}
                }
            ],
            "now": fixed_now
        },
        {
            "id": "least_used_tie_breaking",
            "strategy": "least_used",
            "pool": {},
            "candidates": [
                {
                    "id": "acc_tie_1",
                    "gen_count": 10,
                    "last_quota": {"gemini_5h": {"fraction": 0.60, "reset_time": "2026-09-16T16:00:00Z"}, "gemini_weekly": {"fraction": 0.70, "reset_time": "2026-09-23T12:00:00Z"}, "updated_at": int(fixed_now - 10)}
                },
                {
                    "id": "acc_tie_2",
                    "gen_count": 10,
                    "last_quota": {"gemini_5h": {"fraction": 0.80, "reset_time": "2026-09-16T16:00:00Z"}, "gemini_weekly": {"fraction": 0.90, "reset_time": "2026-09-23T12:00:00Z"}, "updated_at": int(fixed_now - 10)}
                },
                {
                    "id": "acc_lowest_hits",
                    "gen_count": 2,
                    "last_quota": {"gemini_5h": {"fraction": 0.20, "reset_time": "2026-09-16T16:00:00Z"}, "updated_at": int(fixed_now - 10)}
                }
            ],
            "now": fixed_now
        }
    ]

    scheduler_results = []
    for sc in scheduler_scenarios:
        ordered = scheduler.order_candidates(sc["candidates"], strategy=sc["strategy"], pool=sc["pool"], now=sc["now"])
        ordered_ids = [a.get("id") for a in ordered]
        scheduler_results.append({
            "test_id": sc["id"],
            "strategy": sc["strategy"],
            "now": sc["now"],
            "pool": sc["pool"],
            "candidates": sc["candidates"],
            "expected_ordered_ids": ordered_ids
        })
    write_json("scheduler/ordering_cases.json", scheduler_results)

    # 4. CLI Golden Cases
    cli_cases = [
        {
            "name": "version",
            "args": ["--version"],
            "exit_code": 0,
            "stdout_contains": ["agy-pool"],
            "stderr_contains": []
        },
        {
            "name": "help",
            "args": ["--help"],
            "exit_code": 0,
            "stdout_contains": ["usage: agy-pool"],
            "stderr_contains": []
        },
        {
            "name": "list_empty",
            "args": ["list"],
            "exit_code": 0,
            "stdout_contains": ["No accounts in pool yet."],
            "stderr_contains": []
        },
        {
            "name": "doctor_check",
            "args": ["doctor"],
            "exit_code": 0,
            "stdout_contains": ["Antigravity System Doctor"],
            "stderr_contains": []
        },
        {
            "name": "status_summary",
            "args": ["status"],
            "exit_code": 0,
            "stdout_contains": ["Gateway :"],
            "stdout_contains_go": ["State   :", "Strategy:"],
            "stdout_not_contains_go": ["Gemini 5-Hour:", "Gemini Weekly:"],
            "exempt_py_parity": True,
            "stderr_contains": []
        },
        {
            "name": "quota_empty",
            "args": ["quota"],
            "exit_code": 0,
            "stdout_contains": ["No accounts in pool yet."],
            "stderr_contains": []
        },
        {
            "name": "strategy_view",
            "args": ["strategy"],
            "exit_code": 0,
            "stdout_contains": ["Current load balancing strategy:"],
            "stderr_contains": []
        },
        {
            "name": "strategy_invalid",
            "args": ["strategy", "invalid_strat_choice"],
            "exit_code": 1,
            "stdout_contains": ["[Error] Invalid strategy 'invalid_strat_choice'"],
            "stderr_contains": []
        },
        {
            "name": "rename_missing",
            "args": ["rename"],
            "exit_code": 2,
            "stdout_contains": [],
            "stderr_contains": ["usage: agy-pool rename", "error: the following arguments are required"]
        },
        {
            "name": "rename_empty_name",
            "args": ["rename", "1", ""],
            "exit_code": 1,
            "stdout_contains": ["[Error] Account name cannot be empty."],
            "stderr_contains": []
        },
        {
            "name": "rename_not_found",
            "args": ["rename", "nonexistent", "NewName"],
            "exit_code": 1,
            "stdout_contains": ["[Error] Account 'nonexistent' not found."],
            "stderr_contains": []
        },
        {
            "name": "remove_missing",
            "args": ["remove"],
            "exit_code": 2,
            "stdout_contains": [],
            "stderr_contains": ["usage: agy-pool remove", "error: the following arguments are required"]
        },
        {
            "name": "remove_not_found",
            "args": ["remove", "nonexistent"],
            "exit_code": 0,
            "stdout_contains": ["[Error] Account 'nonexistent' not found."],
            "stderr_contains": []
        },
        {
            "name": "switch_not_found",
            "args": ["switch", "nonexistent"],
            "exit_code": 0,
            "stdout_contains": ["[Error] Account pool is empty."],
            "stderr_contains": []
        },
        {
            "name": "export_empty",
            "args": ["export", "backup.json"],
            "exit_code": 0,
            "stdout_contains": ["No accounts in pool to export."],
            "stderr_contains": []
        },
        {
            "name": "import_missing_file",
            "args": ["import"],
            "exit_code": 2,
            "stdout_contains": [],
            "stderr_contains": ["usage: agy-pool import", "error: the following arguments are required"]
        },
        {
            "name": "invalid_command",
            "args": ["nonexistent_command"],
            "exit_code": 2,
            "stdout_contains": [],
            "stderr_contains": ["invalid choice: 'nonexistent_command'"]
        }
    ]
    write_json("cli/golden_cases.json", cli_cases)

if __name__ == "__main__":
    main()
