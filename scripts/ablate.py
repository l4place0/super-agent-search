#!/usr/bin/env python3
"""Offline, single-factor ablations in an archived Git revision (never the worktree).

Requires Linux/WSL Go and, for WSLC mocks, PowerShell. All provider/network
requests are test fixtures. No credential/deployment files are copied from the
worktree. This measures selected regression detection, not performance/security
completeness or the general usefulness of a module.
"""
import argparse
import io
import json
import pathlib
import subprocess
import tarfile
import tempfile
import time

SUITES = {
    "observability": r"Test(ReadinessAndStorageFailure|ReadinessReportsUnavailableDatabase|IngressAccountingIncludesCacheAndFetchFailures|PanicLogRedactsValuesAndHasRequestID|KeyReadFailureIsNotAnUpstreamFailure)$",
    "network": r"Test(PublicDNSValidatesEntireAnswerBeforeDial|PublicDNSPinningRebindingAndFallback|PublicIPv6IsDialledAsPinnedLiteral)$",
    "policy": r"Test(ProviderOutcomePolicy|KeyRotationCooldownAndPersistence|SearchAndManualShareProviderOutcome)$",
}


def replace_once(text, old, new):
    if text.count(old) != 1:
        raise ValueError(f"ablation anchor count {text.count(old)} instead of one: {old[:80]}")
    return text.replace(old, new, 1)


def modifications(name, read):
    if name == "readiness_without_database_probe":
        path = "internal/app/observe.go"
        source = read(path)
        start = source.index("func (a *App) ready(ctx context.Context) bool {")
        end = source.index("\n}", start) + 2
        return {path: source[:start] + "func (a *App) ready(ctx context.Context) bool { return !a.poolFault.Load() }" + source[end:]}
    if name == "without_ingress_accounting":
        path = "internal/app/observe.go"
        return {path: replace_once(read(path), 'if operation != "" {', 'if false && operation != "" {')}
    if name == "without_key_persistence_fail_closed":
        path = "internal/app/key_pool.go"
        return {path: replace_once(read(path), "a.poolFault.Store(true)", "a.poolFault.Store(false)")}
    if name == "panic_value_in_logs":
        path = "internal/app/observe.go"
        source = replace_once(read(path), "if recover() != nil {", "if panicValue := recover(); panicValue != nil {")
        source = replace_once(source, 'a.logger.Error("request panic",', 'a.logger.Error("request panic", "panic_value", panicValue,')
        return {path: source}
    if name == "backup_default_instead_of_actual_mount":
        path = "deploy/wslc-common.ps1"
        return {path: replace_once(read(path), '"type=$($Plan.MountType),source=$($Plan.Source),target=$Target"', '"type=$($Plan.MountType),source=sas-data,target=$Target"')}
    if name == "health_without_instance_identity":
        path = "deploy/wslc-common.ps1"
        source = read(path)
        lines = [line for line in source.splitlines() if "return ($inside.status" in line]
        if len(lines) != 1:
            raise ValueError("identity check anchor changed")
        return {path: replace_once(source, lines[0], "    return ($outside.status -eq 'ready')")}
    if name == "without_whole_dns_answer_validation":
        path = "internal/app/net.go"
        return {path: replace_once(read(path), "if !publicIP(ip.IP) {", "if false && !publicIP(ip.IP) {")}
    if name == "redial_hostname_instead_of_pinned_ip":
        path = "internal/app/net.go"
        source = replace_once(read(path), "net.JoinHostPort(ip.IP.String(), port)", "net.JoinHostPort(host, port)")
        source = replace_once(source, "for _, ip := range ips {\n\t\tconn, e := n.dial", "for range ips {\n\t\tconn, e := n.dial")
        return {path: source}
    if name == "without_provider_key_rotation":
        path = "internal/app/provider_outcome.go"
        source = read(path)
        if source.count("out.retry = true") != 2:
            raise ValueError("retry policy anchors changed")
        return {path: source.replace("out.retry = true", "out.retry = false")}
    if name == "duplicate_equivalent_policy_negative_control":
        path = "internal/app/provider_outcome.go"
        source = read(path)
        function = source[source.index("func classifyProviderOutcome("):]
        duplicate = replace_once(function, "func classifyProviderOutcome(", "func manualProviderOutcome(")
        manual_path = "internal/app/key_test_call.go"
        return {path: source + "\n" + duplicate,
                manual_path: replace_once(read(manual_path), "outcome := classifyProviderOutcome(", "outcome := manualProviderOutcome(")}
    raise ValueError(name)


CASES = [
    ("readiness_without_database_probe", "observability", "defense"),
    ("without_ingress_accounting", "observability", "defense"),
    ("without_key_persistence_fail_closed", "observability", "defense"),
    ("panic_value_in_logs", "observability", "defense"),
    ("backup_default_instead_of_actual_mount", "deployment", "defense"),
    ("health_without_instance_identity", "deployment", "defense"),
    ("without_whole_dns_answer_validation", "network", "defense"),
    ("redial_hostname_instead_of_pinned_ip", "network", "defense"),
    ("without_provider_key_rotation", "policy", "defense"),
    ("duplicate_equivalent_policy_negative_control", "policy", "negative_control"),
]

READINESS_PROBE = '''package app
import "testing"
func TestReadinessReportsUnavailableDatabase(t *testing.T) {
 a := testApp(t)
 if err := a.Close(); err != nil { t.Fatal(err) }
 if a.poolFault.Load() { t.Fatal("probe must isolate database failure from KeyPool fault") }
 if w := call(a, "GET", "/healthz", "", ""); w.Code != 200 { t.Fatal("liveness depends on storage") }
 if w := call(a, "GET", "/readyz", "", ""); w.Code != 503 { t.Fatalf("closed database reported ready: HTTP %d", w.Code) }
}
'''


def execute(root, suite, powershell):
    if suite == "deployment":
        script = root / "deploy/tests/wslc-mock.tests.ps1"
        if powershell.endswith(".exe"):
            script_arg = subprocess.check_output(["wslpath", "-w", str(script)], text=True).strip()
        else:
            script_arg = str(script)
        command = [powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script_arg]
    else:
        command = ["go", "test", "-race", "-json", "-count=1", "-timeout=60s", "./internal/app", "-run", SUITES[suite]]
    start = time.monotonic()
    result = subprocess.run(command, cwd=root, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, errors="replace", timeout=90)
    events = []
    if suite != "deployment":
        for line in result.stdout.splitlines():
            try:
                events.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    passed = [e["Test"] for e in events if e.get("Action") == "pass" and "Test" in e]
    failed = [e["Test"] for e in events if e.get("Action") == "fail" and "Test" in e]
    # Compilation/runner faults are not evidence that a defense was detected.
    if suite != "deployment" and not (passed or failed):
        raise RuntimeError(f"{suite} produced no test results:\n{result.stdout[-3000:]}")
    if suite == "deployment" and result.returncode and "WSLC mock tests passed" in result.stdout:
        raise RuntimeError("mock runner failed after all assertions passed; not an ablation result")
    if suite == "deployment" and result.returncode and "ABLATION_ASSERT:" not in result.stdout:
        raise RuntimeError(f"mock failed outside an assertion:\n{result.stdout[-3000:]}")
    return {"exit_code": result.returncode, "passed_tests": passed, "failed_tests": failed,
            "seconds": round(time.monotonic() - start, 3)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", default="HEAD")
    parser.add_argument("--report", required=True)
    parser.add_argument("--powershell", default="pwsh")
    parser.add_argument("--scratch-root", help="Use a Windows-mounted temp directory with powershell.exe")
    args = parser.parse_args()
    repo = pathlib.Path(__file__).resolve().parent.parent
    revision = subprocess.check_output(["git", "rev-parse", args.revision], cwd=repo, text=True).strip()
    archive = subprocess.check_output(["git", "archive", revision], cwd=repo)
    report = {"revision": revision, "method": "single-factor offline regression ablation; not a performance benchmark",
              "baseline": {}, "cases": []}
    with tempfile.TemporaryDirectory(prefix="uas-ablation-", dir=args.scratch_root) as folder:
        root = pathlib.Path(folder)
        with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
            for member in tar.getmembers():
                if member.issym() or member.islnk() or not (root / member.name).resolve().is_relative_to(root.resolve()):
                    raise RuntimeError("unsafe archive member")
            tar.extractall(root)
        # Distinguish an assertion that kills a mutant from PowerShell parser
        # or runtime setup failures. This marker exists only in the temp copy.
        mock = root / "deploy/tests/wslc-mock.tests.ps1"
        mock.write_text(replace_once(mock.read_text(encoding="utf-8"), "throw $Message", "throw ('ABLATION_ASSERT: '+$Message)"), encoding="utf-8")
        read = lambda path: (root / path).read_text(encoding="utf-8")
        for suite in [*SUITES, "deployment"]:
            result = execute(root, suite, args.powershell)
            if result["exit_code"]:
                raise RuntimeError(f"baseline {suite} failed: {result}")
            report["baseline"][suite] = result
            print(f"baseline {suite}: PASS", flush=True)
        for name, suite, kind in CASES:
            changes = modifications(name, read)
            originals = {path: read(path) for path in changes}
            try:
                for path, source in changes.items():
                    (root / path).write_text(source, encoding="utf-8")
                result = execute(root, suite, args.powershell)
                result.update(name=name, suite=suite, kind=kind,
                              outcome="detected" if result["exit_code"] else "survived")
                # If readiness ablation survives, confirm the specific blind
                # spot using a separate, isolated closed-DB regression probe.
                if name == "readiness_without_database_probe" and not result["exit_code"]:
                    probe = root / "internal/app/ablation_readiness_probe_test.go"
                    probe.write_text(READINESS_PROBE, encoding="utf-8")
                    try:
                        result["with_closed_database_probe"] = execute(root, suite, args.powershell)
                        (root / "internal/app/observe.go").write_text(originals["internal/app/observe.go"], encoding="utf-8")
                        result["unmodified_with_closed_database_probe"] = execute(root, suite, args.powershell)
                    finally:
                        probe.unlink()
                report["cases"].append(result)
                print(f"{name}: {result['outcome']}", flush=True)
            finally:
                for path, source in originals.items():
                    (root / path).write_text(source, encoding="utf-8")
    defenses = [case for case in report["cases"] if case["kind"] == "defense"]
    report["summary"] = {
        "defense_variants": len(defenses),
        "detected_by_original_suite": sum(case["exit_code"] != 0 for case in defenses),
        "surviving_defense_variants": [case["name"] for case in defenses if case["exit_code"] == 0],
        "confirmed_by_added_isolation_probe": [case["name"] for case in defenses
            if case.get("with_closed_database_probe", {}).get("exit_code") == 1
            and case.get("unmodified_with_closed_database_probe", {}).get("exit_code") == 0],
        "equivalent_negative_control_passed": all(case["exit_code"] == 0 for case in report["cases"] if case["kind"] == "negative_control"),
    }
    destination = pathlib.Path(args.report).resolve()
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"report: {destination}")


if __name__ == "__main__":
    main()
