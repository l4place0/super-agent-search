"""Fast unit tests for attribution; never launches mutations or external tools."""
import copy
import unittest

import ablate
import ablate_full


class AttributionTests(unittest.TestCase):
    def result(self, name="doh_response_limit_relaxed", kind="defense"):
        return {
            "name": name, "area": "A7", "kind": kind,
            "suites": {
                "go_race_all": {"status": "pass", "failed_tests": []},
                "windows_ps51_mock_dpapi": {"status": "pass"},
                "linux_pwsh_mock": {"status": "pass"},
                "node_dom_all": {"status": "pass", "failed_tests": []},
                "sh_syntax": {"status": "pass"},
            },
        }

    def test_unrelated_failure_is_not_a_kill(self):
        result = self.result()
        result["suites"]["go_race_all"].update(
            status="assertion_failure",
            failed_tests=["uni-agent-search/internal/app/TestSkillClientsAgainstMockHTTP/pwsh/capabilities"],
        )
        self.assertEqual(ablate_full.attribute(result)["outcome"], "unwitnessed_failure")

    def test_boundary_assertion_is_a_witness(self):
        result = self.result()
        result["suites"]["go_race_all"].update(
            status="assertion_failure",
            failed_tests=["uni-agent-search/internal/app/TestDoHRejectsOversizedValidJSON/over_limit"],
        )
        self.assertEqual(ablate_full.attribute(result)["outcome"], "detected")

    def test_other_doh_assertion_is_not_size_limit_evidence(self):
        result = self.result()
        result["suites"]["go_race_all"].update(
            status="assertion_failure",
            failed_tests=["uni-agent-search/internal/app/TestDoHMalformedBoundedAndContext/valid"],
        )
        self.assertEqual(ablate_full.attribute(result)["outcome"], "unwitnessed_failure")

    def test_build_error_and_incomplete_runs_are_not_kills(self):
        for state in ("build_error", "timeout", "runner_error", "incomplete"):
            with self.subTest(state=state):
                result = self.result()
                result["suites"]["go_race_all"]["status"] = state
                self.assertEqual(ablate_full.attribute(result)["outcome"], "invalid_or_incomplete")

    def test_equivalent_control_must_pass_every_suite(self):
        result = self.result("duplicate_equivalent_policy_negative_control", "negative_control")
        self.assertEqual(ablate_full.attribute(copy.deepcopy(result))["outcome"], "survived")
        result["suites"]["node_dom_all"].update(status="assertion_failure", failed_tests=["fixture"])
        self.assertNotEqual(ablate_full.attribute(result)["outcome"], "survived")

    def test_inventory_covers_all_audit_areas_without_duplicate_names(self):
        self.assertEqual(len(ablate_full.CASES), 30)
        self.assertEqual(len({case[0] for case in ablate_full.CASES}), 30)
        self.assertEqual({case[1] for case in ablate_full.CASES}, {"A" + str(i) for i in range(1, 9)})
        self.assertEqual(sum(case[2] == "negative_control" for case in ablate_full.CASES), 2)

    def test_ambiguous_patch_anchor_is_rejected(self):
        with self.assertRaises(ValueError):
            ablate.replace_once("anchor anchor", "anchor", "mutant")


if __name__ == "__main__":
    unittest.main()
