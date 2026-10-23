#!/usr/bin/env python3
"""Run the finite A1-A8 ablation inventory against every offline test suite.

This is not exhaustive mutation testing or a performance benchmark. Production
files are never patched in-place. Only a selected Git archive is copied.
"""
import argparse
import concurrent.futures
import hashlib
import io
import json
import os
import pathlib
import re
import subprocess
import tarfile
import tempfile
import time

import ablate

EXTRA = [
    ("success_erases_cooldown", "A1", "defense"),
    ("failure_shortens_cooldown", "A1", "defense"),
    ("release_ignores_revision", "A2", "defense"),
    ("admin_does_not_bump_revision", "A2", "defense"),
    ("lease_can_release_twice", "A2", "defense"),
    ("cache_accepts_old_epoch_write", "A3", "defense"),
    ("invalidation_does_not_advance_epoch", "A3", "defense"),
    ("gate_does_not_join_handlers", "A4", "defense"),
    ("shutdown_timeout_without_cancel_or_close", "A4", "defense"),
    ("rollback_disabled", "A6", "defense"),
    ("recovery_accepts_foreign_container", "A6", "defense"),
    ("ensure_recreates_after_health_failure", "A6", "defense"),
    ("doh_response_limit_relaxed", "A7", "defense"),
    ("redirect_budget_relaxed", "A7", "defense"),
    ("special_purpose_cidrs_ignored", "A7", "defense"),
    ("credential_nonce_not_fresh", "A8", "defense"),
    ("credential_tag_error_ignored", "A8", "defense"),
    ("web_allows_unsafe_links", "A8", "defense"),
    ("web_hides_citation_warning", "A8", "defense"),
    ("inline_key_pool_negative_control", "A8", "negative_control"),
]
AREAS = {"observability": "A5", "deployment": "A6", "network": "A7", "policy": "A8"}
CASES = [(name, AREAS[suite], kind) for name, suite, kind in ablate.CASES] + EXTRA


def modifications(name, read):
    replace = ablate.replace_once
    if name in {case[0] for case in ablate.CASES}:
        return ablate.modifications(name, read)
    path = "internal/app/store.go"
    if name == "success_erases_cooldown":
        return {path: replace(read(path), "SET successes=successes+1,last_error=", "SET successes=successes+1,cooldown_until=0,last_error=")}
    if name == "failure_shortens_cooldown":
        return {path: replace(read(path), "cooldown_until=MAX(cooldown_until,?)", "cooldown_until=?")}
    if name == "release_ignores_revision":
        source = read(path)
        start = source.index("func (a *App) applyKeyOutcome(")
        end = source.index("\n}\n", start) + 2
        body = source[start:end]
        if body.count("WHERE id=? AND revision=?") != 2:
            raise ValueError("revision guards changed")
        body = body.replace("WHERE id=? AND revision=?", "WHERE id=? AND revision=revision+0*?")
        return {path: source[:start] + body + source[end:]}
    if name == "admin_does_not_bump_revision":
        path = "internal/app/http.go"
        return {path: replace(read(path), "revision=revision+CASE WHEN ? OR enabled<>? THEN 1 ELSE 0 END", "revision=revision+CASE WHEN ? OR enabled<>? THEN 0 ELSE 0 END")}
    if name == "lease_can_release_twice":
        path = "internal/app/key_pool.go"
        return {path: replace(read(path), "if k.lease == nil || k.lease.released {", "if k.lease == nil {")}
    if name == "cache_accepts_old_epoch_write":
        path = "internal/app/search.go"
        return {path: replace(read(path), "if epoch == a.cacheEpoch {", "if true {")}
    if name == "invalidation_does_not_advance_epoch":
        return {path: replace(read(path), "a.cacheEpoch++", "a.cacheEpoch += 0")}
    path = "cmd/server/main.go"
    if name == "gate_does_not_join_handlers":
        return {path: replace(read(path), "go func() { g.active.Wait(); close(done) }()", "close(done)")}
    if name == "shutdown_timeout_without_cancel_or_close":
        return {path: replace(read(path), "\t\tcancelRequests()\n\t\t_ = s.Close()", "\t\t_ = s.Addr // ablated both forced cancellation mechanisms")}
    path = "deploy/wslc-common.ps1"
    if name == "rollback_disabled":
        return {path: replace(read(path), "try { Restore-UasPlan $Plan; Write-Warning", "try { Write-Warning")}
    if name == "recovery_accepts_foreign_container":
        source = read(path)
        lines = [line for line in source.splitlines() if "if ($current.Id -ne $Plan.ReplacementID" in line]
        if len(lines) != 1:
            raise ValueError("recovery identity anchor changed")
        return {path: replace(source, lines[0], "    if ($false) { throw 'Recovery refused: container identity changed' }")}
    if name == "ensure_recreates_after_health_failure":
        return {path: replace(read(path), "  throw 'Readiness/identity failed; no container was recreated. Inspect service and forwarding, then explicitly run recreate-wslc.ps1 if needed.'", "  Start-UasPlan $Plan")}
    path = "internal/app/net.go"
    if name == "doh_response_limit_relaxed":
        return {path: replace(read(path), "readBody(res.Body, 64<<10)", "readBody(res.Body, 4<<20)")}
    if name == "redirect_budget_relaxed":
        return {path: replace(read(path), "if len(via) >= 5 {", "if len(via) >= 50 {")}
    if name == "special_purpose_cidrs_ignored":
        return {path: replace(read(path), "if n.Contains(ip) {", "if false && n.Contains(ip) {")}
    path = "internal/credential/cipher.go"
    if name == "credential_nonce_not_fresh":
        return {path: replace(read(path), "\treturn base64.StdEncoding.EncodeToString(c.aead.Seal(", "\tclear(nonce) // ablated nonce freshness\n\treturn base64.StdEncoding.EncodeToString(c.aead.Seal(")}
    if name == "credential_tag_error_ignored":
        return {path: replace(read(path), '\tif err != nil {\n\t\treturn "", errors.New("invalid encrypted key")\n\t}', '\tif false && err != nil {\n\t\treturn "", errors.New("invalid encrypted key")\n\t}')}
    path = "internal/app/web/app.js"
    if name == "web_allows_unsafe_links":
        return {path: replace(read(path), '["http:", "https:"].includes(u.protocol)', 'Boolean(u.protocol)')}
    if name == "web_hides_citation_warning":
        return {path: replace(read(path), 'if (h.basis === "model_citation")', 'if (false && h.basis === "model_citation")')}
    if name == "inline_key_pool_negative_control":
        pool = read("internal/app/key_pool.go")
        body = pool[pool.index("\n)\n") + 3:]
        store = replace(read("internal/app/store.go"), '\t"sync"', '\t"sync"\n\t"sync/atomic"')
        return {"internal/app/key_pool.go": "package app\n", "internal/app/store.go": store + "\n" + body}
    raise ValueError(name)


def command_result(command, root, timeout=240):
    start = time.monotonic()
    try:
        result = subprocess.run(command, cwd=root, env=os.environ.copy(), text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                errors="replace", timeout=timeout)
        return result.returncode, result.stdout, round(time.monotonic() - start, 3)
    except subprocess.TimeoutExpired:
        return None, "stage exceeded outer timeout", round(time.monotonic() - start, 3)


def run_suites(root, powershell):
    results = {}
    code, text, elapsed = command_result(["go", "test", "-race", "-json", "-count=1", "-timeout=150s", "./..."], root)
    events = []
    for line in text.splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    passed = sorted({e["Package"] + "/" + e["Test"] for e in events if e.get("Action") == "pass" and "Test" in e})
    failed = sorted({e["Package"] + "/" + e["Test"] for e in events if e.get("Action") == "fail" and "Test" in e})
    skipped = sorted({e["Package"] + "/" + e["Test"] for e in events if e.get("Action") == "skip" and "Test" in e})
    build_error = "[build failed]" in text or "build failed" in text
    timeout = code is None or "panic: test timed out" in text
    state = "pass" if code == 0 else "assertion_failure" if failed else "runner_error"
    if build_error or timeout or not passed or skipped:
        state = "timeout" if timeout else "build_error" if build_error else "incomplete"
    results["go_race_all"] = {"status": state, "exit_code": code, "seconds": elapsed,
                              "passed_count": len(passed), "failed_tests": failed, "skipped_tests": skipped}
    for label, shell in [("windows_ps51_mock_dpapi", powershell), ("linux_pwsh_mock", "pwsh")]:
        script = root / "deploy/tests/wslc-mock.tests.ps1"
        script_arg = str(script)
        if shell.endswith(".exe"):
            script_arg = subprocess.check_output(["wslpath", "-w", script_arg], text=True).strip()
        code, text, elapsed = command_result([shell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script_arg], root, 60)
        if code is None:
            state = "timeout"
        elif code == 0 and "WSLC mock tests passed" in text:
            state = "pass"
        elif code and "ABLATION_ASSERT:" in text and "WSLC mock tests passed" not in text:
            state = "assertion_failure"
        else:
            state = "runner_error"
        results[label] = {"status": state, "exit_code": code, "seconds": elapsed}
    code, text, elapsed = command_result(["node", "--test", "--test-reporter=tap", "internal/app/web_test/web.test.mjs"], root, 30)
    state = "pass" if code == 0 and "# pass 3" in text else "assertion_failure" if code and "not ok " in text and "ERR_ASSERTION" in text else "runner_error"
    if code is None:
        state = "timeout"
    results["node_dom_all"] = {"status": state, "exit_code": code, "seconds": elapsed,
                               "failed_tests": re.findall(r"^not ok \d+ - (.+)$", text, re.M)}
    code, _, elapsed = command_result(["sh", "-n", "skills/uni-agent-search/scripts/uas.sh"], root, 30)
    results["sh_syntax"] = {"status": "pass" if code == 0 else "runner_error", "exit_code": code, "seconds": elapsed}
    return results


def classify(results):
    invalid = [name for name, result in results.items() if result["status"] not in ("pass", "assertion_failure")]
    if invalid:
        return "invalid_or_incomplete"
    return "detected" if any(result["status"] == "assertion_failure" for result in results.values()) else "survived"


GO_WITNESSES = {
    "readiness_without_database_probe": ["TestReadinessReportsUnavailableDatabase"],
    "without_ingress_accounting": ["TestIngressAccountingIncludesCacheAndFetchFailures"],
    "without_key_persistence_fail_closed": ["TestReadinessAndStorageFailure"],
    "panic_value_in_logs": ["TestPanicLogRedactsValuesAndHasRequestID"],
    "without_whole_dns_answer_validation": ["TestPublicDNSValidatesEntireAnswerBeforeDial", "TestPublicRedirectRevalidatesDestination"],
    "redial_hostname_instead_of_pinned_ip": ["TestPublicDNSPinningRebindingAndFallback", "TestPublicIPv6IsDialledAsPinnedLiteral"],
    "without_provider_key_rotation": ["TestProviderOutcomePolicy", "TestKeyRotationCooldownAndPersistence"],
    "success_erases_cooldown": ["TestAuditCooldownSurvivesConcurrentSuccess", "TestLeaseCooldownCompletionOrders"],
    "failure_shortens_cooldown": ["TestLeaseCooldownCompletionOrders"],
    "release_ignores_revision": ["TestAuditReplacementIgnoresOldFailure", "TestLeaseAdministrativeFences", "TestManualKeyLeaseFencesAndConcurrency"],
    "admin_does_not_bump_revision": ["TestAuditReplacementIgnoresOldFailure", "TestLeaseAdministrativeFences"],
    "lease_can_release_twice": ["TestLeaseCooldownCompletionOrders", "TestLeaseAdministrativeFences"],
    "cache_accepts_old_epoch_write": ["TestCacheEpochFencesInFlightAPI"],
    "invalidation_does_not_advance_epoch": ["TestCacheEpochFencesInFlightAPI"],
    "gate_does_not_join_handlers": ["TestRequestGateWaitsAndRejectsLateHandlers"],
    "shutdown_timeout_without_cancel_or_close": ["TestServerSIGTERMLifecycle/timeout"],
    "doh_response_limit_relaxed": ["TestDoHMalformedBoundedAndContext/oversized", "TestDoHRejectsOversizedValidJSON/over_limit"],
    "redirect_budget_relaxed": ["TestPublicRedirectBudget"],
    "special_purpose_cidrs_ignored": ["TestPublicSpecialPurposePolicy", "TestSSRFAndHTMLReader"],
    "credential_nonce_not_fresh": ["TestLegacyCiphertextCompatibility"],
    "credential_tag_error_ignored": ["TestLegacyCiphertextCompatibility"],
}


def attribute(result):
    """Do not count failures in unrelated timing-sensitive tests as kills."""
    if "suites" not in result:
        result["outcome"] = "invalid_or_incomplete"
        return result
    overall = classify(result["suites"])
    result["any_suite_outcome"] = overall
    if overall == "invalid_or_incomplete" or result["kind"] in ("baseline", "negative_control"):
        result["outcome"] = overall
        return result
    witnesses = []
    for test in result["suites"]["go_race_all"]["failed_tests"]:
        if any(test.endswith("/" + expected) or ("/" + expected + "/") in test
               for expected in GO_WITNESSES.get(result["name"], [])):
            witnesses.append(test)
    if result["area"] == "A6":
        witnesses += [stage for stage in ("windows_ps51_mock_dpapi", "linux_pwsh_mock")
                      if result["suites"][stage]["status"] == "assertion_failure"]
    if result["name"] in ("web_allows_unsafe_links", "web_hides_citation_warning"):
        witnesses += result["suites"]["node_dom_all"]["failed_tests"]
    result["witnesses"] = witnesses
    result["outcome"] = "detected" if witnesses else "unwitnessed_failure" if overall == "detected" else "survived"
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", default="HEAD")
    parser.add_argument("--report", required=True)
    parser.add_argument("--powershell", required=True, help="Windows PowerShell 5.1 executable for DPAPI mocks")
    parser.add_argument("--client-pwsh", required=True, help="Portable Linux pwsh executable (also used by CLI tests)")
    parser.add_argument("--scratch-root", required=True, help="Windows-mounted temp directory")
    parser.add_argument("--workers", type=int, choices=[1, 2], default=2)
    parser.add_argument("--resume", action="store_true", help="Continue checkpoint; serially retry unattributed failures with --workers 1")
    parser.add_argument("--case", action="append", choices=[c[0] for c in CASES], help="Diagnostic rerun only; omitted means full inventory")
    args = parser.parse_args()
    # Do not inherit private service settings or ambient proxy configuration.
    for key in list(os.environ):
        if key.startswith("SAS_") or key.lower() in {"http_proxy", "https_proxy", "all_proxy", "no_proxy"}:
            os.environ.pop(key)
    os.environ["PATH"] = str(pathlib.Path(args.client_pwsh).resolve().parent) + os.pathsep + os.environ["PATH"]
    os.environ["POWERSHELL_TELEMETRY_OPTOUT"] = "1"
    repo = pathlib.Path(__file__).resolve().parent.parent
    revision = subprocess.check_output(["git", "rev-parse", args.revision], cwd=repo, text=True).strip()
    archive = subprocess.check_output(["git", "archive", revision], cwd=repo)
    destination = pathlib.Path(args.report).resolve()
    report = {"revision": revision, "method": "single-factor finite A1-A8 inventory; complete offline suites for each variant",
              "workers": args.workers, "repetitions_per_variant": 1, "baseline": [], "cases": [],
              "environment": {"go": subprocess.check_output(["go", "version"], text=True).strip(),
                              "node": subprocess.check_output(["node", "--version"], text=True).strip(),
                              "client_pwsh": subprocess.check_output(["pwsh", "-Version"], text=True).strip(),
                              "deployment_powershell": "Windows PowerShell 5.1 (DPAPI) plus Linux pwsh"}}

    if args.resume:
        report = json.loads(destination.read_text(encoding="utf-8"))
        if report["revision"] != revision:
            raise ValueError("resume revision mismatch")
        report.setdefault("resume_workers", []).append(args.workers)
        for case in report["cases"]:
            attribute(case)
    elif destination.exists():
        raise ValueError("report already exists; use --resume or a new path")

    def checkpoint():
        destination.parent.mkdir(parents=True, exist_ok=True)
        temp = destination.with_suffix(destination.suffix + ".partial")
        temp.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        temp.replace(destination)

    def trial(case):
        name, area, kind = case
        with tempfile.TemporaryDirectory(prefix="uas-full-ablation-", dir=args.scratch_root) as folder:
            root = pathlib.Path(folder)
            with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
                for member in tar.getmembers():
                    if member.issym() or member.islnk() or not (root / member.name).resolve().is_relative_to(root.resolve()):
                        raise RuntimeError("unsafe archive member")
                tar.extractall(root, filter="data")
            mock = root / "deploy/tests/wslc-mock.tests.ps1"
            mock.write_text(ablate.replace_once(mock.read_text(encoding="utf-8"), "throw $Message", "throw ('ABLATION_ASSERT: '+$Message)"), encoding="utf-8")
            patches = {}
            if kind != "baseline":
                patches = modifications(name, lambda path: (root / path).read_text(encoding="utf-8"))
                for path, source in patches.items():
                    (root / path).write_text(source, encoding="utf-8")
            results = run_suites(root, args.powershell)
            return attribute({"name": name, "area": area, "kind": kind, "outcome": classify(results), "suites": results,
                    "patched_files_sha256": {path: hashlib.sha256(source.encode()).hexdigest() for path, source in patches.items()}})

    if not report["baseline"]:
        report["baseline"].append(trial(("baseline_before", "all", "baseline")))
        checkpoint()
    if report["baseline"][0]["outcome"] != "survived":
        raise RuntimeError("baseline incomplete or failing; inspect report before attributing any failures")
    print("baseline_before: all suites PASS, zero Go skips", flush=True)
    existing = {case["name"]: case for case in report["cases"]}
    selected = [c for c in CASES if (not args.case or c[0] in args.case)
                and (c[0] not in existing or existing[c[0]]["outcome"] in ("unwitnessed_failure", "invalid_or_incomplete")
                     or (c[2] == "negative_control" and existing[c[0]]["outcome"] != "survived"))]
    print("pending: " + ", ".join(c[0] for c in selected), flush=True)
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
        pending = {pool.submit(trial, case): case for case in selected}
        for future in concurrent.futures.as_completed(pending):
            case = pending[future]
            try:
                result = future.result()
            except Exception as error:
                # No raw external/native output or user environment in reports.
                result = {"name": case[0], "area": case[1], "kind": case[2],
                          "outcome": "invalid_or_incomplete", "error_class": type(error).__name__}
            previous = next((old for old in report["cases"] if old["name"] == case[0]), None)
            if previous is not None:
                result["attempt_history"] = previous.get("attempt_history", []) + [{key: value for key, value in previous.items() if key != "attempt_history"}]
                report["cases"].remove(previous)
            report["cases"].append(result)
            checkpoint()
            print(f"{len(report['cases'])}/{len(CASES)} {case[0]}: {result['outcome']}", flush=True)
    report["baseline"].append(trial(("baseline_after", "all", "baseline")))
    report["cases"].sort(key=lambda case: [c[0] for c in CASES].index(case["name"]))
    defenses = [c for c in report["cases"] if c["kind"] == "defense"]
    controls = [c for c in report["cases"] if c["kind"] == "negative_control"]
    report["summary"] = {
        "inventory_total": len(CASES), "executed_variants": len(report["cases"]),
        "full_inventory_executed": {c["name"] for c in report["cases"]} == {c[0] for c in CASES},
        "baselines_all_passed": all(b["outcome"] == "survived" for b in report["baseline"]),
        "defense_variants": len(defenses),
        "detected": sum(c["outcome"] == "detected" for c in defenses),
        "survivors": [c["name"] for c in defenses if c["outcome"] == "survived"],
        "invalid_or_incomplete": [c["name"] for c in report["cases"] if c["outcome"] == "invalid_or_incomplete"],
        "unwitnessed_failures": [c["name"] for c in report["cases"] if c["outcome"] == "unwitnessed_failure"],
        "negative_controls": {c["name"]: c["outcome"] for c in controls},
    }
    checkpoint()
    print(json.dumps(report["summary"], ensure_ascii=False), flush=True)


if __name__ == "__main__":
    main()
